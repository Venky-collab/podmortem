// Command podmortem investigates failing Kubernetes pods with an AI agent
// and writes a root-cause report.
//
//	podmortem investigate <namespace>/<pod>    one-off investigation, report to stdout
//	podmortem serve [--watch]                  webhook + watcher + web UI
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Venky-collab/podmortem/internal/agent"
	"github.com/Venky-collab/podmortem/internal/kube"
	"github.com/Venky-collab/podmortem/internal/llm"
	"github.com/Venky-collab/podmortem/internal/server"
)

const usage = `podmortem: AI root-cause analysis for failing Kubernetes pods

Usage:
  podmortem investigate <namespace>/<pod>   investigate one pod and print the report
  podmortem serve [flags]                   run the webhook server (and pod watcher with --watch)

Common flags:
  --kubeconfig PATH   kubeconfig (default: in-cluster, then $KUBECONFIG or ~/.kube/config)
  --model NAME        Ollama model (default $PODMORTEM_MODEL or qwen2.5:1.5b)
  --ollama-url URL    Ollama server (default $OLLAMA_HOST or http://localhost:11434)
  --max-steps N       tool-calling rounds before the agent must answer (default 4)
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "investigate":
		err = runInvestigate(ctx, os.Args[2:])
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", os.Args[1], usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type common struct {
	kubeconfig, model, ollamaURL string
	maxSteps                     int
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "path to kubeconfig")
	fs.StringVar(&c.model, "model", envOr("PODMORTEM_MODEL", "qwen2.5:1.5b"), "Ollama model")
	fs.StringVar(&c.ollamaURL, "ollama-url", envOr("OLLAMA_HOST", "http://localhost:11434"), "Ollama server URL")
	fs.IntVar(&c.maxSteps, "max-steps", 4, "tool-calling rounds before the agent must answer")
}

func (c *common) build() (*agent.Agent, kubernetes.Interface, error) {
	cfg, err := kubeConfig(c.kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	url := c.ollamaURL
	if !strings.HasPrefix(url, "http") {
		url = "http://" + url // OLLAMA_HOST is often just host:port
	}
	return &agent.Agent{
		LLM:      llm.NewOllama(url, c.model),
		Kube:     kube.NewToolbox(client),
		MaxSteps: c.maxSteps,
	}, client, nil
}

func runInvestigate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("investigate", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	ns, pod, ok := strings.Cut(fs.Arg(0), "/")
	if !ok || ns == "" || pod == "" {
		return errors.New("usage: podmortem investigate <namespace>/<pod>")
	}
	a, _, err := c.build()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Investigating %s/%s with %s...\n", ns, pod, a.LLM.Name())
	r := a.Investigate(ctx, ns, pod, "manual")
	fmt.Println(r.Markdown())
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.register(fs)
	addr := fs.String("addr", ":8080", "listen address")
	watch := fs.Bool("watch", false, "watch pods and investigate new failures automatically")
	namespace := fs.String("namespace", "", "namespace to watch (default: all)")
	workers := fs.Int("workers", 1, "parallel investigations")
	dedupe := fs.Duration("dedupe", 10*time.Minute, "ignore repeat triggers for the same pod within this window")
	reportsDir := fs.String("reports-dir", "", "also write each report as Markdown into this directory")
	_ = fs.Parse(args)

	a, client, err := c.build()
	if err != nil {
		return err
	}
	if *reportsDir != "" {
		if err := os.MkdirAll(*reportsDir, 0o755); err != nil {
			return err
		}
	}
	srv := server.New(a, server.Options{Dedupe: *dedupe, ReportsDir: *reportsDir})
	go srv.Run(ctx, *workers)

	if *watch {
		go func() {
			slog.Info("watching pods", "namespace", orAll(*namespace))
			err := kube.Watch(ctx, client, *namespace, func(ns, pod, reason string) {
				srv.Enqueue(ns, pod, "watcher: "+reason)
			})
			if err != nil {
				slog.Error("watcher stopped", "err", err)
			}
		}()
	}

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	slog.Info("podmortem listening", "addr", *addr, "model", a.LLM.Name())
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// kubeConfig prefers in-cluster credentials, then the usual kubeconfig.
func kubeConfig(path string) (*rest.Config, error) {
	if path == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orAll(ns string) string {
	if ns == "" {
		return "all"
	}
	return ns
}

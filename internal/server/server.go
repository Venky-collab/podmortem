// Package server exposes podmortem over HTTP: an Alertmanager webhook that
// queues investigations, a small web UI and JSON API for reports, and
// Prometheus metrics.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Venky-collab/podmortem/internal/agent"
)

// Investigator is the part of the agent the server needs; tests swap it out.
type Investigator interface {
	Investigate(ctx context.Context, ns, pod, trigger string) *agent.Report
}

type job struct{ ns, pod, trigger string }

// Server queues investigations and serves their reports.
type Server struct {
	agent      Investigator
	queue      chan job
	dedupe     time.Duration
	reportsDir string // when set, each report is also written as Markdown

	mu      sync.Mutex
	recent  map[string]time.Time // ns/pod -> last time it was queued
	reports []*agent.Report      // newest first

	received, queued, skipped, done, failed atomic.Int64
}

type Options struct {
	QueueSize  int
	Dedupe     time.Duration // ignore repeat triggers for the same pod within this window
	ReportsDir string
}

func New(a Investigator, opts Options) *Server {
	if opts.QueueSize == 0 {
		opts.QueueSize = 100
	}
	return &Server{
		agent:      a,
		queue:      make(chan job, opts.QueueSize),
		dedupe:     opts.Dedupe,
		reportsDir: opts.ReportsDir,
		recent:     map[string]time.Time{},
	}
}

// Enqueue schedules an investigation unless the same pod was queued within
// the dedupe window or the queue is full. It reports whether it was queued.
func (s *Server) Enqueue(ns, pod, trigger string) bool {
	s.received.Add(1)
	key := ns + "/" + pod
	s.mu.Lock()
	if last, ok := s.recent[key]; ok && time.Since(last) < s.dedupe {
		s.mu.Unlock()
		s.skipped.Add(1)
		return false
	}
	s.recent[key] = time.Now()
	s.mu.Unlock()

	select {
	case s.queue <- job{ns, pod, trigger}:
		s.queued.Add(1)
		slog.Info("queued investigation", "pod", key, "trigger", trigger)
		return true
	default:
		s.skipped.Add(1)
		slog.Warn("queue full, dropping", "pod", key)
		return false
	}
}

// Run processes the queue with `workers` goroutines until ctx is done. One
// worker suits a laptop-sized local model; more only help with a hosted one.
func (s *Server) Run(ctx context.Context, workers int) {
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-s.queue:
					s.handle(ctx, j)
				}
			}
		}()
	}
	wg.Wait()
}

func (s *Server) handle(ctx context.Context, j job) {
	ictx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	r := s.agent.Investigate(ictx, j.ns, j.pod, j.trigger)
	if r.Error != "" {
		s.failed.Add(1)
		slog.Error("investigation failed", "pod", j.ns+"/"+j.pod, "err", r.Error)
	} else {
		s.done.Add(1)
		slog.Info("investigation done", "pod", j.ns+"/"+j.pod, "report", r.ID, "took", r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
	}
	s.mu.Lock()
	s.reports = append([]*agent.Report{r}, s.reports...)
	if len(s.reports) > 200 {
		s.reports = s.reports[:200]
	}
	s.mu.Unlock()
	if s.reportsDir != "" {
		path := filepath.Join(s.reportsDir, r.ID+".md")
		if err := os.WriteFile(path, []byte(r.Markdown()), 0o644); err != nil {
			slog.Error("writing report", "path", path, "err", err)
		}
	}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /alerts", s.handleAlerts)
	mux.HandleFunc("POST /investigate", s.handleInvestigate)
	mux.HandleFunc("GET /api/reports", s.handleListReports)
	mux.HandleFunc("GET /api/reports/{id}", s.handleGetReport)
	mux.HandleFunc("GET /reports/{id}", s.handleReportPage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

// alertmanagerPayload is the subset of Alertmanager's webhook body we use.
type alertmanagerPayload struct {
	Alerts []struct {
		Status string            `json:"status"`
		Labels map[string]string `json:"labels"`
	} `json:"alerts"`
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	var p alertmanagerPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		http.Error(w, "bad alertmanager payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	queued := 0
	for _, a := range p.Alerts {
		ns, pod := a.Labels["namespace"], a.Labels["pod"]
		if a.Status != "firing" || ns == "" || pod == "" {
			continue // only firing alerts about a specific pod
		}
		if s.Enqueue(ns, pod, "alert "+a.Labels["alertname"]) {
			queued++
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": queued})
}

func (s *Server) handleInvestigate(w http.ResponseWriter, r *http.Request) {
	var body struct{ Namespace, Pod string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.Namespace == "" || body.Pod == "" {
		http.Error(w, `want {"namespace": "...", "pod": "..."}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": s.Enqueue(body.Namespace, body.Pod, "manual request")})
}

func (s *Server) handleListReports(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type row struct {
		ID, Namespace, Pod, Trigger, Error string
		FinishedAt                         time.Time
	}
	rows := make([]row, 0, len(s.reports))
	for _, r := range s.reports {
		rows = append(rows, row{r.ID, r.Namespace, r.Pod, r.Trigger, r.Error, r.FinishedAt})
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) find(id string) *agent.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reports {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	rep := s.find(r.PathValue("id"))
	if rep == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>podmortem</title>
<style>body{font:15px system-ui;max-width:860px;margin:2rem auto;padding:0 1rem}td,th{padding:.4rem .6rem;text-align:left;border-bottom:1px solid #ddd}.err{color:#b00}</style>
<h1>podmortem</h1><p>AI root-cause reports for failing pods. Newest first.</p>
<table><tr><th>When</th><th>Pod</th><th>Trigger</th><th></th></tr>
{{range .}}<tr><td>{{.FinishedAt.Format "Jan 2 15:04:05"}}</td><td>{{.Namespace}}/{{.Pod}}</td><td>{{.Trigger}}</td>
<td><a href="/reports/{{.ID}}">report</a>{{if .Error}} <span class="err">failed</span>{{end}}</td></tr>
{{else}}<tr><td colspan=4>No investigations yet. Break a pod and wait a few seconds.</td></tr>{{end}}</table>`))

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	reports := append([]*agent.Report(nil), s.reports...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = indexTmpl.Execute(w, reports)
}

func (s *Server) handleReportPage(w http.ResponseWriter, r *http.Request) {
	rep := s.find(r.PathValue("id"))
	if rep == nil {
		http.NotFound(w, r)
		return
	}
	// Plain Markdown keeps the server dependency-free; browsers show it as text.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, rep.Markdown())
}

// handleMetrics writes counters in the Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	metric := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	metric("podmortem_triggers_received_total", "Investigation triggers received (alerts, watcher, manual).", s.received.Load())
	metric("podmortem_investigations_queued_total", "Investigations queued.", s.queued.Load())
	metric("podmortem_triggers_skipped_total", "Triggers skipped as duplicates or because the queue was full.", s.skipped.Load())
	metric("podmortem_investigations_completed_total", "Investigations that produced a report.", s.done.Load())
	metric("podmortem_investigations_failed_total", "Investigations that ended in an error.", s.failed.Load())
	fmt.Fprintf(w, "# HELP podmortem_queue_depth Investigations waiting.\n# TYPE podmortem_queue_depth gauge\npodmortem_queue_depth %d\n", len(s.queue))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

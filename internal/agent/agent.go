// Package agent runs one investigation: collect evidence, let the model ask
// for more with read-only tools, then write a root-cause report.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Venky-collab/podmortem/internal/kube"
	"github.com/Venky-collab/podmortem/internal/llm"
)

const systemPrompt = `You are podmortem, an SRE assistant that finds the root cause of a failing Kubernetes pod.
You can only read the cluster, never change it.

You are given evidence that was already collected: pod status, events, logs and rollout history,
plus "signals" computed from that evidence. Only call a tool if this evidence is not enough.

When you are done, reply with exactly these four sections in Markdown:
## Summary
One sentence: what is broken and why.
## Root cause
The underlying cause, in 2-4 sentences.
## Evidence
Bullet points quoting the specific log lines, events or status fields you relied on.
## Suggested fix
Concrete steps: the manifest field or kubectl command to change, and how to verify.

Rules: never invent log lines, events or values that are not in the evidence. If the evidence
is not enough to be sure, say what is missing.`

// Agent investigates failing pods.
type Agent struct {
	LLM      llm.Client
	Kube     *kube.Toolbox
	MaxSteps int // tool-calling rounds before the agent must answer
}

// Step is one tool call the model made.
type Step struct {
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Output string         `json:"output"`
}

// Report is the result of one investigation.
type Report struct {
	ID         string    `json:"id"`
	Namespace  string    `json:"namespace"`
	Pod        string    `json:"pod"`
	Trigger    string    `json:"trigger"`
	Model      string    `json:"model"`
	Signals    []string  `json:"signals"`
	Evidence   string    `json:"evidence"`
	Steps      []Step    `json:"steps"`
	Analysis   string    `json:"analysis"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

// Investigate diagnoses one pod. It always returns a report; on failure the
// report carries the error and whatever evidence was gathered.
func (a *Agent) Investigate(ctx context.Context, ns, pod, trigger string) *Report {
	r := &Report{ID: newID(), Namespace: ns, Pod: pod, Trigger: trigger, Model: a.LLM.Name(), StartedAt: time.Now()}
	defer func() { r.FinishedAt = time.Now() }()

	ev, err := a.Kube.Collect(ctx, ns, pod)
	if err != nil {
		r.Error = "collecting evidence: " + err.Error()
		return r
	}
	r.Signals = ev.Signals
	r.Evidence = ev.Text

	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: initialPrompt(ns, pod, trigger, ev)},
	}
	tools := toolSpecs()

	for step := 0; step < a.MaxSteps; step++ {
		reply, err := a.LLM.Chat(ctx, messages, tools)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		messages = append(messages, reply)
		if len(reply.ToolCalls) == 0 {
			r.Analysis = strings.TrimSpace(reply.Content)
			return r
		}
		for _, call := range reply.ToolCalls {
			out := a.runTool(ctx, ns, call)
			r.Steps = append(r.Steps, Step{Tool: call.Name, Args: call.Args, Output: out})
			messages = append(messages, llm.Message{Role: "tool", ToolName: call.Name, Content: out})
		}
	}

	// Out of tool rounds: ask for the report with no tools on offer.
	messages = append(messages, llm.Message{Role: "user", Content: "Stop investigating. Write the report now using the four sections."})
	reply, err := a.LLM.Chat(ctx, messages, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Analysis = strings.TrimSpace(reply.Content)
	return r
}

func initialPrompt(ns, pod, trigger string, ev *kube.Evidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Investigate pod %s in namespace %s.", pod, ns)
	if trigger != "" {
		fmt.Fprintf(&b, " Trigger: %s.", trigger)
	}
	b.WriteString("\n\n## Signals\n")
	if len(ev.Signals) == 0 {
		b.WriteString("- none detected automatically\n")
	}
	for _, s := range ev.Signals {
		b.WriteString("- " + s + "\n")
	}
	b.WriteString("\n## Evidence\n" + ev.Text)
	return b.String()
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

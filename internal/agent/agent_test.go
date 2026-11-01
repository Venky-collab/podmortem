package agent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Venky-collab/podmortem/internal/kube"
	"github.com/Venky-collab/podmortem/internal/llm"
)

// scriptedLLM replays canned replies and records what it was sent.
type scriptedLLM struct {
	replies []llm.Message
	seen    [][]llm.Message
	tools   [][]llm.ToolSpec
}

func (s *scriptedLLM) Name() string { return "scripted" }

func (s *scriptedLLM) Chat(_ context.Context, msgs []llm.Message, tools []llm.ToolSpec) (llm.Message, error) {
	s.seen = append(s.seen, append([]llm.Message(nil), msgs...))
	s.tools = append(s.tools, tools)
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

func newAgent(model llm.Client, objects ...any) *Agent {
	client := fake.NewClientset()
	for _, o := range objects {
		switch v := o.(type) {
		case *corev1.Pod:
			_, _ = client.CoreV1().Pods(v.Namespace).Create(context.Background(), v, metav1.CreateOptions{})
		}
	}
	return &Agent{LLM: model, Kube: kube.NewToolbox(client), MaxSteps: 3}
}

func failingPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:broken"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Image: "app:broken",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}},
	}
}

func TestInvestigateRunsToolsThenReports(t *testing.T) {
	model := &scriptedLLM{replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{Name: "pod_events", Args: map[string]any{"pod": "web"}}}},
		{Role: "assistant", Content: "## Summary\nImage tag app:broken does not exist."},
	}}
	a := newAgent(model, failingPod("shop", "web"))

	r := a.Investigate(context.Background(), "shop", "web", "test")
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if !strings.Contains(r.Analysis, "app:broken does not exist") {
		t.Errorf("analysis = %q", r.Analysis)
	}
	if len(r.Steps) != 1 || r.Steps[0].Tool != "pod_events" {
		t.Fatalf("steps = %+v", r.Steps)
	}
	// The first prompt carries the signals computed in code.
	first := model.seen[0][1].Content
	if !strings.Contains(first, "cannot pull image \"app:broken\"") {
		t.Errorf("initial prompt missing image-pull signal:\n%s", first)
	}
	// The tool result goes back to the model as a tool message.
	last := model.seen[1][len(model.seen[1])-1]
	if last.Role != "tool" || last.ToolName != "pod_events" {
		t.Errorf("last message = %+v", last)
	}
}

func TestInvestigateForcesAnswerAfterMaxSteps(t *testing.T) {
	loop := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{Name: "list_pods"}}}
	model := &scriptedLLM{replies: []llm.Message{loop, loop, loop, {Role: "assistant", Content: "final"}}}
	a := newAgent(model, failingPod("shop", "web"))

	r := a.Investigate(context.Background(), "shop", "web", "")
	if r.Analysis != "final" || len(r.Steps) != 3 {
		t.Fatalf("analysis=%q steps=%d", r.Analysis, len(r.Steps))
	}
	if model.tools[3] != nil {
		t.Error("the forced final call must not offer tools")
	}
}

func TestToolsArePinnedToTheInvestigatedNamespace(t *testing.T) {
	a := newAgent(&scriptedLLM{}, failingPod("shop", "web"), failingPod("kube-system", "secret-pod"))
	out := a.runTool(context.Background(), "shop", llm.ToolCall{Name: "pod_status", Args: map[string]any{"pod": "secret-pod", "namespace": "kube-system"}})
	if !strings.Contains(out, "not found") {
		t.Errorf("expected the kube-system pod to be invisible from namespace shop, got:\n%s", out)
	}
}

func TestToolErrorsGoBackToTheModel(t *testing.T) {
	a := newAgent(&scriptedLLM{}, failingPod("shop", "web"))
	for call, want := range map[string]string{
		"pod_logs":        `missing required argument "pod"`,
		"delete_pod":      `unknown tool "delete_pod"`,
		"rollout_history": `missing required argument "pod"`,
	} {
		if out := a.runTool(context.Background(), "shop", llm.ToolCall{Name: call, Args: map[string]any{}}); !strings.Contains(out, want) {
			t.Errorf("%s: got %q, want it to contain %q", call, out, want)
		}
	}
}

func TestMissingPodGivesReportWithError(t *testing.T) {
	r := newAgent(&scriptedLLM{}).Investigate(context.Background(), "shop", "ghost", "")
	if !strings.Contains(r.Error, "not found") {
		t.Fatalf("error = %q", r.Error)
	}
	if md := r.Markdown(); !strings.Contains(md, "Investigation failed") {
		t.Errorf("markdown should show the failure:\n%s", md)
	}
}

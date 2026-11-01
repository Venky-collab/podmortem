package agent

import (
	"context"
	"fmt"

	"github.com/Venky-collab/podmortem/internal/llm"
)

func toolSpecs() []llm.ToolSpec {
	podParam := map[string]any{
		"type":       "object",
		"properties": map[string]any{"pod": map[string]any{"type": "string", "description": "pod name"}},
		"required":   []string{"pod"},
	}
	return []llm.ToolSpec{
		{Name: "list_pods", Description: "List all pods in the namespace with readiness, restarts and failure reason. Use it to see if other pods fail the same way.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "pod_status", Description: "Current status of a pod: container states, exit codes, restarts, limits, conditions.", Parameters: podParam},
		{Name: "pod_events", Description: "Recent Kubernetes events for a pod (scheduling, pulls, probe failures, kills).", Parameters: podParam},
		{Name: "pod_logs", Description: "Last lines of a container's logs. Set previous=true to read the run before the last crash.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pod":       map[string]any{"type": "string"},
					"container": map[string]any{"type": "string", "description": "container name; optional when the pod has one container"},
					"previous":  map[string]any{"type": "boolean"},
				},
				"required": []string{"pod"},
			}},
		{Name: "rollout_history", Description: "The pod's Deployment and its last revisions with images, to spot a bad deploy.", Parameters: podParam},
	}
}

// runTool executes one tool call. Calls are pinned to the namespace under
// investigation, and errors go back to the model as text so it can recover.
func (a *Agent) runTool(ctx context.Context, ns string, call llm.ToolCall) string {
	pod, _ := call.Args["pod"].(string)
	needPod := func() string {
		if pod == "" {
			return "error: missing required argument \"pod\""
		}
		return ""
	}

	var out string
	var err error
	switch call.Name {
	case "list_pods":
		out, err = a.Kube.ListPods(ctx, ns)
	case "pod_status":
		if out = needPod(); out == "" {
			out, err = a.Kube.PodStatus(ctx, ns, pod)
		}
	case "pod_events":
		if out = needPod(); out == "" {
			out, err = a.Kube.Events(ctx, ns, pod)
		}
	case "pod_logs":
		if out = needPod(); out == "" {
			container, _ := call.Args["container"].(string)
			previous, _ := call.Args["previous"].(bool)
			out, err = a.Kube.Logs(ctx, ns, pod, container, previous)
		}
	case "rollout_history":
		if out = needPod(); out == "" {
			out, err = a.Kube.Rollout(ctx, ns, pod)
		}
	default:
		return fmt.Sprintf("error: unknown tool %q", call.Name)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

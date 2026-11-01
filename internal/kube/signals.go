package kube

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// FailureReason returns why a pod is unhealthy ("CrashLoopBackOff",
// "OOMKilled", "ImagePullBackOff", ...) or "" when it looks fine. The
// watcher uses it to decide when to start an investigation.
func FailureReason(pod *corev1.Pod) string {
	for _, cs := range allStatuses(pod) {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
				"CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return w.Reason
			}
		}
		if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
			return "OOMKilled"
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == "Unschedulable" {
			return "Unschedulable"
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		return "Failed"
	}
	return ""
}

// Signals turns well-known failure patterns into plain-language hints. They
// are computed in code, not by the model, so the diagnosis starts from facts
// even when a small local model is doing the reasoning.
func Signals(pod *corev1.Pod) []string {
	var out []string
	limits := map[string]string{}
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if m, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			limits[c.Name] = m.String()
		}
	}

	for _, cs := range allStatuses(pod) {
		name := cs.Name
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff":
				out = append(out, fmt.Sprintf("container %q is crash-looping (%d restarts); its previous logs show why it exited", name, cs.RestartCount))
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				out = append(out, fmt.Sprintf("container %q cannot pull image %q: wrong name/tag, missing registry credentials, or private image", name, cs.Image))
			case "CreateContainerConfigError":
				out = append(out, fmt.Sprintf("container %q config error: %s (usually a missing ConfigMap, Secret or key)", name, w.Message))
			}
		}
		for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
			if t == nil {
				continue
			}
			switch {
			case t.Reason == "OOMKilled":
				hint := fmt.Sprintf("container %q was OOMKilled (out of memory)", name)
				if l, ok := limits[name]; ok {
					hint += fmt.Sprintf("; memory limit is %s", l)
				}
				out = append(out, hint)
			case t.ExitCode != 0:
				out = append(out, fmt.Sprintf("container %q exited with code %d (%s)%s", name, t.ExitCode, t.Reason, exitCodeMeaning(t.ExitCode)))
			}
			break // the current state wins over the last one
		}
	}

	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			out = append(out, "pod cannot be scheduled: "+c.Message)
		}
		if c.Type == corev1.ContainersReady && c.Status == corev1.ConditionFalse && FailureReason(pod) == "" && pod.Status.Phase == corev1.PodRunning {
			out = append(out, "containers are running but not ready: check the readiness probe")
		}
	}
	return dedupe(out)
}

func exitCodeMeaning(code int32) string {
	switch code {
	case 1:
		return ": application error"
	case 126, 127:
		return ": command not found or not executable"
	case 137:
		return ": killed by SIGKILL (often OOM or a failed liveness probe)"
	case 139:
		return ": segmentation fault"
	case 143:
		return ": terminated by SIGTERM"
	}
	return ""
}

func allStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	return append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

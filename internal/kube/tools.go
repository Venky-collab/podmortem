// Package kube holds podmortem's read-only view of the cluster: the evidence
// gathered for every investigation and the tools the agent can call.
// Nothing here creates, updates or deletes anything.
package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Toolbox reads pods, events, logs and rollouts.
type Toolbox struct {
	Client      kubernetes.Interface
	LogLines    int64 // tail length for logs
	MaxEvents   int
	OutputLimit int // characters per tool result, to keep prompts small
}

func NewToolbox(client kubernetes.Interface) *Toolbox {
	return &Toolbox{Client: client, LogLines: 40, MaxEvents: 15, OutputLimit: 4000}
}

// Evidence is what podmortem collects before the model sees anything.
type Evidence struct {
	Pod     *corev1.Pod
	Signals []string
	Text    string // status, events, logs and rollout, ready to put in a prompt
}

// Collect gathers the standard first-look evidence for a pod: status, recent
// events, logs from each unhealthy container, and its Deployment rollout.
func (t *Toolbox) Collect(ctx context.Context, ns, name string) (*Evidence, error) {
	pod, err := t.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	section := func(title, body string) { fmt.Fprintf(&b, "### %s\n%s\n\n", title, strings.TrimSpace(body)) }

	section("Pod status", describePod(pod))
	if ev, err := t.Events(ctx, ns, name); err == nil {
		section("Events", ev)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready && cs.RestartCount == 0 {
			continue
		}
		// After a crash the current container is often empty or brand new;
		// the previous one holds the error.
		previous := cs.RestartCount > 0
		logs, err := t.Logs(ctx, ns, name, cs.Name, previous)
		if err != nil {
			logs = "(no logs: " + err.Error() + ")"
		}
		label := "Logs of " + cs.Name
		if previous {
			label += " (previous run)"
		}
		section(label, logs)
	}
	if ro, err := t.Rollout(ctx, ns, name); err == nil {
		section("Rollout", ro)
	}
	return &Evidence{Pod: pod, Signals: Signals(pod), Text: b.String()}, nil
}

// PodStatus returns a one-pod summary.
func (t *Toolbox) PodStatus(ctx context.Context, ns, name string) (string, error) {
	pod, err := t.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return t.clip(describePod(pod)), nil
}

// ListPods lists the namespace's pods with phase, readiness and restarts.
func (t *Toolbox) ListPods(ctx context.Context, ns string) (string, error) {
	pods, err := t.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range pods.Items {
		ready, restarts := 0, int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		reason := FailureReason(&p)
		if reason == "" {
			reason = string(p.Status.Phase)
		}
		fmt.Fprintf(&b, "%s  ready=%d/%d  restarts=%d  %s\n", p.Name, ready, len(p.Spec.Containers), restarts, reason)
	}
	if b.Len() == 0 {
		return "no pods in namespace " + ns, nil
	}
	return t.clip(b.String()), nil
}

// Events returns the most recent events about one object.
func (t *Toolbox) Events(ctx context.Context, ns, name string) (string, error) {
	list, err := t.Client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name,
	})
	if err != nil {
		return "", err
	}
	var events []corev1.Event
	for _, e := range list.Items {
		if e.InvolvedObject.Name == name { // fake clients ignore field selectors
			events = append(events, e)
		}
	}
	sort.Slice(events, func(i, j int) bool { return eventTime(events[i]).Before(eventTime(events[j])) })
	if len(events) > t.MaxEvents {
		events = events[len(events)-t.MaxEvents:]
	}
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%s %s: %s", e.Type, e.Reason, strings.TrimSpace(e.Message))
		if e.Count > 1 {
			fmt.Fprintf(&b, " (x%d)", e.Count)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "no events for " + name, nil
	}
	return t.clip(b.String()), nil
}

// Logs returns the tail of a container's logs. previous=true reads the last
// terminated run, which is where a crash's error usually is.
func (t *Toolbox) Logs(ctx context.Context, ns, pod, container string, previous bool) (string, error) {
	opts := &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &t.LogLines}
	raw, err := t.Client.CoreV1().Pods(ns).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "(empty)", nil
	}
	// Keep the end: the last lines before a crash matter most.
	s := string(raw)
	if len(s) > t.OutputLimit {
		s = "..." + s[len(s)-t.OutputLimit:]
	}
	return s, nil
}

// Rollout shows the pod's Deployment and its recent revisions, to spot a
// bad deploy (new image, changed config) right before the failure.
func (t *Toolbox) Rollout(ctx context.Context, ns, podName string) (string, error) {
	pod, err := t.Client.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	rsName := ownerName(pod.OwnerReferences, "ReplicaSet")
	if rsName == "" {
		return "pod is not managed by a Deployment", nil
	}
	rs, err := t.Client.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	depName := ownerName(rs.OwnerReferences, "Deployment")
	if depName == "" {
		return "ReplicaSet " + rsName + " has no Deployment", nil
	}
	dep, err := t.Client.AppsV1().Deployments(ns).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	all, err := t.Client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Deployment %s: %d/%d ready, %d updated\n", dep.Name, dep.Status.ReadyReplicas, ptrOr(dep.Spec.Replicas, 1), dep.Status.UpdatedReplicas)
	for _, c := range dep.Status.Conditions {
		fmt.Fprintf(&b, "condition %s=%s %s\n", c.Type, c.Status, c.Message)
	}
	var revs []appsv1.ReplicaSet
	for _, r := range all.Items {
		if ownerName(r.OwnerReferences, "Deployment") == dep.Name {
			revs = append(revs, r)
		}
	}
	sort.Slice(revs, func(i, j int) bool { return revision(revs[i]) > revision(revs[j]) })
	if len(revs) > 3 {
		revs = revs[:3]
	}
	for _, r := range revs {
		var images []string
		for _, c := range r.Spec.Template.Spec.Containers {
			images = append(images, c.Image)
		}
		fmt.Fprintf(&b, "revision %d (%s ago): images %s, %d/%d ready\n",
			revision(r), since(r.CreationTimestamp.Time), strings.Join(images, ", "), r.Status.ReadyReplicas, ptrOr(r.Spec.Replicas, 0))
	}
	return t.clip(b.String()), nil
}

func describePod(pod *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pod %s/%s phase=%s node=%s age=%s\n", pod.Namespace, pod.Name, pod.Status.Phase, orNone(pod.Spec.NodeName), since(pod.CreationTimestamp.Time))
	specs := map[string]corev1.Container{}
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		specs[c.Name] = c
	}
	for _, cs := range allStatuses(pod) {
		fmt.Fprintf(&b, "container %s image=%s ready=%t restarts=%d state=%s", cs.Name, cs.Image, cs.Ready, cs.RestartCount, stateString(cs.State))
		if cs.LastTerminationState.Terminated != nil {
			fmt.Fprintf(&b, " last=%s", stateString(cs.LastTerminationState))
		}
		if c, ok := specs[cs.Name]; ok {
			if lim := c.Resources.Limits; len(lim) > 0 {
				fmt.Fprintf(&b, " limits=cpu:%s,memory:%s", lim.Cpu(), lim.Memory())
			}
		}
		b.WriteString("\n")
	}
	for _, c := range pod.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			fmt.Fprintf(&b, "condition %s=%s %s %s\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	return b.String()
}

func stateString(s corev1.ContainerState) string {
	switch {
	case s.Waiting != nil:
		return fmt.Sprintf("waiting(%s: %s)", s.Waiting.Reason, s.Waiting.Message)
	case s.Terminated != nil:
		return fmt.Sprintf("terminated(%s, exit %d)", s.Terminated.Reason, s.Terminated.ExitCode)
	case s.Running != nil:
		return "running"
	}
	return "unknown"
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func ownerName(refs []metav1.OwnerReference, kind string) string {
	for _, r := range refs {
		if r.Kind == kind {
			return r.Name
		}
	}
	return ""
}

func revision(rs appsv1.ReplicaSet) int {
	n, _ := strconv.Atoi(rs.Annotations["deployment.kubernetes.io/revision"])
	return n
}

func since(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return time.Since(t).Round(time.Second).String()
}

func ptrOr(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func (t *Toolbox) clip(s string) string {
	if len(s) > t.OutputLimit {
		return s[:t.OutputLimit] + "\n...(truncated)"
	}
	return s
}

package kube

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func crashLoopPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-7d9f-abcde", Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-7d9f"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "api", Image: "shop/api:v2",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "api", Image: "shop/api:v2", RestartCount: 5,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
			}},
		},
	}
}

func TestFailureReason(t *testing.T) {
	cases := map[string]struct {
		pod  *corev1.Pod
		want string
	}{
		"crashloop": {crashLoopPod(), "CrashLoopBackOff"},
		"image pull": {&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}}}, "ImagePullBackOff"},
		"unschedulable": {&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		}}}}, "Unschedulable"},
		"healthy": {&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}}}, ""},
		"still creating": {&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}}}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FailureReason(tc.pod); got != tc.want {
				t.Errorf("FailureReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSignalsExplainOOMWithLimit(t *testing.T) {
	signals := strings.Join(Signals(crashLoopPod()), "\n")
	for _, want := range []string{"crash-looping (5 restarts)", "OOMKilled", "memory limit is 64Mi"} {
		if !strings.Contains(signals, want) {
			t.Errorf("signals missing %q:\n%s", want, signals)
		}
	}
}

func TestCollectGathersStatusEventsLogsAndRollout(t *testing.T) {
	two := int32(2)
	client := fake.NewClientset(
		crashLoopPod(),
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Name: "api-7d9f-abcde"},
			Type:           "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 7,
		},
		&corev1.Event{ // about another pod: must be filtered out
			ObjectMeta:     metav1.ObjectMeta{Name: "e2", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Name: "other"},
			Type:           "Normal", Reason: "Pulled", Message: "unrelated",
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "api-7d9f", Namespace: "shop",
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": "3"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}},
			},
			Spec: appsv1.ReplicaSetSpec{Replicas: &two, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "api", Image: "shop/api:v2"}},
			}}},
		},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &two}},
	)

	ev, err := NewToolbox(client).Collect(context.Background(), "shop", "api-7d9f-abcde")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"waiting(CrashLoopBackOff",
		"limits=cpu:0,memory:64Mi",
		"Warning BackOff: Back-off restarting failed container (x7)",
		"Logs of api (previous run)",
		"Deployment api: 0/2 ready",
		"revision 3",
		"images shop/api:v2",
	} {
		if !strings.Contains(ev.Text, want) {
			t.Errorf("evidence missing %q:\n%s", want, ev.Text)
		}
	}
	if strings.Contains(ev.Text, "unrelated") {
		t.Errorf("evidence includes another pod's event:\n%s", ev.Text)
	}
}

func TestRolloutWithoutDeployment(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lonely", Namespace: "default"}}
	out, err := NewToolbox(fake.NewClientset(pod)).Rollout(context.Background(), "default", "lonely")
	if err != nil || out != "pod is not managed by a Deployment" {
		t.Fatalf("Rollout = %q, %v", out, err)
	}
}

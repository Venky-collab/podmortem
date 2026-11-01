package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Watch calls onFailure whenever a pod becomes unhealthy (healthy -> failing,
// or appears already failing). It blocks until ctx is done. An empty
// namespace watches the whole cluster.
func Watch(ctx context.Context, client kubernetes.Interface, namespace string, onFailure func(ns, pod, reason string)) error {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(namespace))
	pods := factory.Core().V1().Pods().Informer()

	_, err := pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if p, ok := obj.(*corev1.Pod); ok {
				if r := FailureReason(p); r != "" {
					onFailure(p.Namespace, p.Name, r)
				}
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, ok1 := oldObj.(*corev1.Pod)
			newPod, ok2 := newObj.(*corev1.Pod)
			if !ok1 || !ok2 {
				return
			}
			// Only fire on the transition into a failure state, not on every
			// status update of a pod that's already failing.
			if r := FailureReason(newPod); r != "" && r != FailureReason(oldPod) {
				onFailure(newPod.Namespace, newPod.Name, r)
			}
		},
	})
	if err != nil {
		return err
	}
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	<-ctx.Done()
	factory.Shutdown()
	return nil
}

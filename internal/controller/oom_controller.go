/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/sartorproj/sartor/internal/oom"
)

// OOMController watches for OOMKilled pods and patches workloads.
type OOMController struct {
	client.Client
	Scheme      *runtime.Scheme
	Interceptor *oom.Interceptor
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch

// Reconcile handles pod events and checks for OOMKilled containers.
func (r *OOMController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Pod
	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		// Pod might have been deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Detect OOM events
	oomEvents := r.Interceptor.DetectOOMKilled(pod)
	if len(oomEvents) == 0 {
		return ctrl.Result{}, nil
	}

	logger.Info("Detected OOM events", "pod", pod.Name, "count", len(oomEvents))

	// Handle each OOM event
	for _, event := range oomEvents {
		result, err := r.Interceptor.HandleOOMEvent(ctx, event)
		if err != nil {
			logger.Error(err, "Failed to handle OOM event",
				"container", event.ContainerName,
				"owner", event.OwnerName)
			continue
		}

		if result.Success {
			logger.Info("Successfully patched workload after OOM",
				"container", event.ContainerName,
				"owner", event.OwnerName,
				"newLimit", result.NewMemoryLimit.String())

			// If there's an associated Tailoring, it will pick up the change
			// on next reconcile and create a Cut to persist to Git
			if result.TailoringName != "" {
				logger.Info("Associated Tailoring will persist change to Git",
					"tailoring", result.TailoringName)
			}
		} else {
			logger.Info("Skipped OOM patch", "reason", result.Message)
		}
	}

	// Requeue to check for new OOM events
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OOMController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			// Only watch pods that have failed or are in error state
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return false
			}

			// Check for OOMKilled in container statuses
			for _, status := range pod.Status.ContainerStatuses {
				if status.LastTerminationState.Terminated != nil &&
					status.LastTerminationState.Terminated.Reason == "OOMKilled" {
					return true
				}
			}
			return false
		})).
		Named("oom").
		Complete(r)
}

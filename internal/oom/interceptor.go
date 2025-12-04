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

package oom

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

const (
	// DefaultMemoryIncrease is the default percentage to increase memory after OOM.
	DefaultMemoryIncrease = 20

	// OOMAnnotationKey is the annotation key for OOM patching history.
	OOMAnnotationKey = "sartor.sartorproj.io/oom-patched"

	// OOMPatchCooldown is the minimum time between OOM patches for the same container.
	OOMPatchCooldown = 10 * time.Minute
)

// InterceptorConfig holds configuration for the OOM Interceptor.
type InterceptorConfig struct {
	// MemoryIncreasePercent is the percentage to increase memory after OOM.
	MemoryIncreasePercent int
	// MaxMemory is the maximum memory limit to set.
	MaxMemory *resource.Quantity
	// Enabled indicates if the interceptor is active.
	Enabled bool
}

// Interceptor watches for OOMKilled events and patches workloads.
type Interceptor struct {
	client client.Client
	config InterceptorConfig
}

// NewInterceptor creates a new OOM Interceptor.
func NewInterceptor(c client.Client, config InterceptorConfig) *Interceptor {
	if config.MemoryIncreasePercent == 0 {
		config.MemoryIncreasePercent = DefaultMemoryIncrease
	}
	return &Interceptor{
		client: c,
		config: config,
	}
}

// OOMEvent represents an OOM kill event.
type OOMEvent struct {
	// Namespace is the pod's namespace.
	Namespace string
	// PodName is the name of the pod that was OOMKilled.
	PodName string
	// ContainerName is the name of the container that was OOMKilled.
	ContainerName string
	// OwnerKind is the kind of the owner (Deployment, StatefulSet, etc.).
	OwnerKind string
	// OwnerName is the name of the owner.
	OwnerName string
	// CurrentMemoryLimit is the current memory limit.
	CurrentMemoryLimit *resource.Quantity
	// Timestamp is when the OOM occurred.
	Timestamp time.Time
}

// PatchResult contains the result of an OOM patch operation.
type PatchResult struct {
	// Success indicates if the patch was applied.
	Success bool
	// NewMemoryLimit is the new memory limit after patching.
	NewMemoryLimit *resource.Quantity
	// Message contains any error or status message.
	Message string
	// TailoringName is the name of the Tailoring to notify (if any).
	TailoringName string
}

// DetectOOMKilled checks if a pod has any OOMKilled containers.
func (i *Interceptor) DetectOOMKilled(pod *corev1.Pod) []OOMEvent {
	events := make([]OOMEvent, 0)

	for _, status := range pod.Status.ContainerStatuses {
		if status.LastTerminationState.Terminated != nil &&
			status.LastTerminationState.Terminated.Reason == "OOMKilled" {

			// Find the owner
			ownerKind, ownerName := i.findOwner(pod)

			// Find current memory limit
			var memLimit *resource.Quantity
			for _, container := range pod.Spec.Containers {
				if container.Name == status.Name {
					if limit, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
						memLimit = &limit
					}
					break
				}
			}

			events = append(events, OOMEvent{
				Namespace:          pod.Namespace,
				PodName:            pod.Name,
				ContainerName:      status.Name,
				OwnerKind:          ownerKind,
				OwnerName:          ownerName,
				CurrentMemoryLimit: memLimit,
				Timestamp:          status.LastTerminationState.Terminated.FinishedAt.Time,
			})
		}
	}

	return events
}

// findOwner finds the owner of a pod.
func (i *Interceptor) findOwner(pod *corev1.Pod) (kind, name string) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			// Check if it's a ReplicaSet (owned by Deployment)
			if ref.Kind == "ReplicaSet" {
				// Try to find the Deployment that owns this ReplicaSet
				return "Deployment", extractDeploymentName(ref.Name)
			}
			return ref.Kind, ref.Name
		}
	}
	return "", ""
}

// extractDeploymentName extracts the Deployment name from a ReplicaSet name.
// ReplicaSet names are typically <deployment-name>-<hash>.
func extractDeploymentName(rsName string) string {
	// Find the last dash followed by a hash
	for i := len(rsName) - 1; i >= 0; i-- {
		if rsName[i] == '-' {
			return rsName[:i]
		}
	}
	return rsName
}

// HandleOOMEvent processes an OOM event and patches the workload if appropriate.
func (i *Interceptor) HandleOOMEvent(ctx context.Context, event OOMEvent) (*PatchResult, error) {
	logger := log.FromContext(ctx)

	if !i.config.Enabled {
		return &PatchResult{
			Success: false,
			Message: "OOM Interceptor is disabled",
		}, nil
	}

	logger.Info("Handling OOM event",
		"namespace", event.Namespace,
		"pod", event.PodName,
		"container", event.ContainerName,
		"owner", fmt.Sprintf("%s/%s", event.OwnerKind, event.OwnerName))

	// Check cooldown
	if i.shouldSkipDueToCooldown(ctx, event) {
		return &PatchResult{
			Success: false,
			Message: "Skipped due to cooldown",
		}, nil
	}

	// Calculate new memory limit
	newLimit := i.calculateNewMemoryLimit(event.CurrentMemoryLimit)
	if newLimit == nil {
		return &PatchResult{
			Success: false,
			Message: "Unable to calculate new memory limit",
		}, nil
	}

	// Apply the patch
	if err := i.patchWorkload(ctx, event, newLimit); err != nil {
		return &PatchResult{
			Success: false,
			Message: fmt.Sprintf("Failed to patch workload: %v", err),
		}, err
	}

	// Find associated Tailoring
	tailoringName := i.findTailoring(ctx, event)

	return &PatchResult{
		Success:        true,
		NewMemoryLimit: newLimit,
		Message:        fmt.Sprintf("Memory limit increased from %s to %s", event.CurrentMemoryLimit.String(), newLimit.String()),
		TailoringName:  tailoringName,
	}, nil
}

// shouldSkipDueToCooldown checks if we should skip patching due to cooldown.
func (i *Interceptor) shouldSkipDueToCooldown(ctx context.Context, event OOMEvent) bool {
	// Check annotation on the workload
	switch event.OwnerKind {
	case "Deployment":
		deployment := &appsv1.Deployment{}
		if err := i.client.Get(ctx, types.NamespacedName{
			Name:      event.OwnerName,
			Namespace: event.Namespace,
		}, deployment); err != nil {
			return false
		}

		if lastPatch, ok := deployment.Annotations[OOMAnnotationKey]; ok {
			lastPatchTime, err := time.Parse(time.RFC3339, lastPatch)
			if err == nil && time.Since(lastPatchTime) < OOMPatchCooldown {
				return true
			}
		}

	case "StatefulSet":
		statefulSet := &appsv1.StatefulSet{}
		if err := i.client.Get(ctx, types.NamespacedName{
			Name:      event.OwnerName,
			Namespace: event.Namespace,
		}, statefulSet); err != nil {
			return false
		}

		if lastPatch, ok := statefulSet.Annotations[OOMAnnotationKey]; ok {
			lastPatchTime, err := time.Parse(time.RFC3339, lastPatch)
			if err == nil && time.Since(lastPatchTime) < OOMPatchCooldown {
				return true
			}
		}
	}

	return false
}

// calculateNewMemoryLimit calculates the new memory limit after OOM.
func (i *Interceptor) calculateNewMemoryLimit(current *resource.Quantity) *resource.Quantity {
	if current == nil {
		// Default to 256Mi if no current limit
		return resource.NewQuantity(256*1024*1024, resource.BinarySI)
	}

	currentBytes := current.Value()
	increaseBytes := currentBytes * int64(i.config.MemoryIncreasePercent) / 100
	newBytes := currentBytes + increaseBytes

	// Apply max limit if configured
	if i.config.MaxMemory != nil {
		maxBytes := i.config.MaxMemory.Value()
		if newBytes > maxBytes {
			newBytes = maxBytes
		}
	}

	return resource.NewQuantity(newBytes, resource.BinarySI)
}

// patchWorkload patches the workload with new memory limits.
func (i *Interceptor) patchWorkload(ctx context.Context, event OOMEvent, newLimit *resource.Quantity) error {
	switch event.OwnerKind {
	case "Deployment":
		return i.patchDeployment(ctx, event, newLimit)
	case "StatefulSet":
		return i.patchStatefulSet(ctx, event, newLimit)
	case "DaemonSet":
		return i.patchDaemonSet(ctx, event, newLimit)
	default:
		return fmt.Errorf("unsupported owner kind: %s", event.OwnerKind)
	}
}

// patchDeployment patches a Deployment with new memory limits.
func (i *Interceptor) patchDeployment(ctx context.Context, event OOMEvent, newLimit *resource.Quantity) error {
	deployment := &appsv1.Deployment{}
	if err := i.client.Get(ctx, types.NamespacedName{
		Name:      event.OwnerName,
		Namespace: event.Namespace,
	}, deployment); err != nil {
		return fmt.Errorf("failed to get Deployment: %w", err)
	}

	// Update container resources
	for i, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == event.ContainerName {
			if deployment.Spec.Template.Spec.Containers[i].Resources.Limits == nil {
				deployment.Spec.Template.Spec.Containers[i].Resources.Limits = make(corev1.ResourceList)
			}
			deployment.Spec.Template.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory] = *newLimit

			// Also update requests if they're set
			if deployment.Spec.Template.Spec.Containers[i].Resources.Requests != nil {
				if _, ok := deployment.Spec.Template.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory]; ok {
					// Set requests to match limits for OOM scenarios
					deployment.Spec.Template.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory] = *newLimit
				}
			}
			break
		}
	}

	// Add annotation
	if deployment.Annotations == nil {
		deployment.Annotations = make(map[string]string)
	}
	deployment.Annotations[OOMAnnotationKey] = time.Now().Format(time.RFC3339)
	deployment.Annotations["sartor.sartorproj.io/oom-container"] = event.ContainerName
	deployment.Annotations["sartor.sartorproj.io/oom-previous-limit"] = event.CurrentMemoryLimit.String()
	deployment.Annotations["sartor.sartorproj.io/oom-new-limit"] = newLimit.String()

	if err := i.client.Update(ctx, deployment); err != nil {
		return fmt.Errorf("failed to update Deployment: %w", err)
	}

	return nil
}

// patchStatefulSet patches a StatefulSet with new memory limits.
func (i *Interceptor) patchStatefulSet(ctx context.Context, event OOMEvent, newLimit *resource.Quantity) error {
	statefulSet := &appsv1.StatefulSet{}
	if err := i.client.Get(ctx, types.NamespacedName{
		Name:      event.OwnerName,
		Namespace: event.Namespace,
	}, statefulSet); err != nil {
		return fmt.Errorf("failed to get StatefulSet: %w", err)
	}

	// Update container resources
	for i, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == event.ContainerName {
			if statefulSet.Spec.Template.Spec.Containers[i].Resources.Limits == nil {
				statefulSet.Spec.Template.Spec.Containers[i].Resources.Limits = make(corev1.ResourceList)
			}
			statefulSet.Spec.Template.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory] = *newLimit

			if statefulSet.Spec.Template.Spec.Containers[i].Resources.Requests != nil {
				if _, ok := statefulSet.Spec.Template.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory]; ok {
					statefulSet.Spec.Template.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory] = *newLimit
				}
			}
			break
		}
	}

	// Add annotation
	if statefulSet.Annotations == nil {
		statefulSet.Annotations = make(map[string]string)
	}
	statefulSet.Annotations[OOMAnnotationKey] = time.Now().Format(time.RFC3339)

	if err := i.client.Update(ctx, statefulSet); err != nil {
		return fmt.Errorf("failed to update StatefulSet: %w", err)
	}

	return nil
}

// patchDaemonSet patches a DaemonSet with new memory limits.
func (i *Interceptor) patchDaemonSet(ctx context.Context, event OOMEvent, newLimit *resource.Quantity) error {
	daemonSet := &appsv1.DaemonSet{}
	if err := i.client.Get(ctx, types.NamespacedName{
		Name:      event.OwnerName,
		Namespace: event.Namespace,
	}, daemonSet); err != nil {
		return fmt.Errorf("failed to get DaemonSet: %w", err)
	}

	// Update container resources
	for i, container := range daemonSet.Spec.Template.Spec.Containers {
		if container.Name == event.ContainerName {
			if daemonSet.Spec.Template.Spec.Containers[i].Resources.Limits == nil {
				daemonSet.Spec.Template.Spec.Containers[i].Resources.Limits = make(corev1.ResourceList)
			}
			daemonSet.Spec.Template.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory] = *newLimit

			if daemonSet.Spec.Template.Spec.Containers[i].Resources.Requests != nil {
				if _, ok := daemonSet.Spec.Template.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory]; ok {
					daemonSet.Spec.Template.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory] = *newLimit
				}
			}
			break
		}
	}

	// Add annotation
	if daemonSet.Annotations == nil {
		daemonSet.Annotations = make(map[string]string)
	}
	daemonSet.Annotations[OOMAnnotationKey] = time.Now().Format(time.RFC3339)

	if err := i.client.Update(ctx, daemonSet); err != nil {
		return fmt.Errorf("failed to update DaemonSet: %w", err)
	}

	return nil
}

// findTailoring finds the Tailoring associated with the workload.
func (i *Interceptor) findTailoring(ctx context.Context, event OOMEvent) string {
	tailoringList := &autoscalingv1alpha1.TailoringList{}
	if err := i.client.List(ctx, tailoringList, client.InNamespace(event.Namespace)); err != nil {
		return ""
	}

	for _, tailoring := range tailoringList.Items {
		if tailoring.Spec.Target.Name == event.OwnerName &&
			tailoring.Spec.Target.Kind == event.OwnerKind {
			return tailoring.Name
		}
	}

	return ""
}

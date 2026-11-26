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
	"slices"
	"strings"
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

	// kindDeployment is the Kubernetes Deployment kind.
	kindDeployment = "Deployment"
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
				return kindDeployment, deploymentNameFromReplicaSet(ref.Name, pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey])
			}
			return ref.Kind, ref.Name
		}
	}
	return "", ""
}

// deploymentNameFromReplicaSet derives the Deployment name from a ReplicaSet
// name (<deployment>-<pod-template-hash>), using the pod's hash label when
// present.
func deploymentNameFromReplicaSet(rsName, podTemplateHash string) string {
	if podTemplateHash != "" {
		if name, ok := strings.CutSuffix(rsName, "-"+podTemplateHash); ok {
			return name
		}
	}
	return extractDeploymentName(rsName)
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

// skipped returns a PatchResult for an event that was deliberately not handled.
func skipped(format string, args ...interface{}) *PatchResult {
	return &PatchResult{Success: false, Message: fmt.Sprintf(format, args...)}
}

// HandleOOMEvent processes an OOM event and patches the workload if appropriate.
// Only workloads targeted by a non-paused Tailoring are patched, and never for
// containers the Tailoring excludes.
func (i *Interceptor) HandleOOMEvent(ctx context.Context, event OOMEvent) (*PatchResult, error) {
	logger := log.FromContext(ctx)

	if !i.config.Enabled {
		return skipped("OOM Interceptor is disabled"), nil
	}

	tailoring, err := i.findTailoring(ctx, event)
	if err != nil {
		return skipped("Failed to look up Tailoring: %v", err), err
	}
	if tailoring == nil {
		return skipped("No Tailoring targets %s/%s", event.OwnerKind, event.OwnerName), nil
	}
	if tailoring.Spec.Paused {
		return skipped("Tailoring %s is paused", tailoring.Name), nil
	}
	if slices.Contains(tailoring.Spec.ExcludeContainers, event.ContainerName) {
		return skipped("Container %s is excluded by Tailoring %s", event.ContainerName, tailoring.Name), nil
	}

	logger.Info("Handling OOM event",
		"namespace", event.Namespace,
		"pod", event.PodName,
		"container", event.ContainerName,
		"owner", fmt.Sprintf("%s/%s", event.OwnerKind, event.OwnerName),
		"tailoring", tailoring.Name)

	obj, podSpec, err := i.getWorkload(ctx, event)
	if err != nil {
		return &PatchResult{
			Success: false,
			Message: fmt.Sprintf("Failed to patch workload: %v", err),
		}, err
	}

	if lastPatch, ok := lastPatchTime(obj); ok {
		if time.Since(lastPatch) < OOMPatchCooldown {
			return skipped("Skipped due to cooldown"), nil
		}
		// LastTerminationState keeps reporting the same OOM until the next
		// restart; it must not trigger another increase.
		if !event.Timestamp.IsZero() && !event.Timestamp.After(lastPatch) {
			return skipped("OOM at %s was already handled", event.Timestamp.Format(time.RFC3339)), nil
		}
	}

	// Calculate new memory limit
	newLimit := i.calculateNewMemoryLimit(event.CurrentMemoryLimit)
	if newLimit == nil {
		return skipped("Unable to calculate new memory limit"), nil
	}

	if err := i.patchWorkload(ctx, obj, podSpec, event, newLimit); err != nil {
		return &PatchResult{
			Success: false,
			Message: fmt.Sprintf("Failed to patch workload: %v", err),
		}, err
	}

	return &PatchResult{
		Success:        true,
		NewMemoryLimit: newLimit,
		Message:        fmt.Sprintf("Memory limit increased from %s to %s", event.CurrentMemoryLimit.String(), newLimit.String()),
		TailoringName:  tailoring.Name,
	}, nil
}

// lastPatchTime returns when the interceptor last patched the workload.
func lastPatchTime(obj client.Object) (time.Time, bool) {
	value, ok := obj.GetAnnotations()[OOMAnnotationKey]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// getWorkload fetches the event's owner workload and returns it with its pod spec.
func (i *Interceptor) getWorkload(ctx context.Context, event OOMEvent) (client.Object, *corev1.PodSpec, error) {
	key := types.NamespacedName{Name: event.OwnerName, Namespace: event.Namespace}
	var obj client.Object
	var podSpec *corev1.PodSpec
	switch event.OwnerKind {
	case kindDeployment:
		d := &appsv1.Deployment{}
		obj, podSpec = d, &d.Spec.Template.Spec
	case "StatefulSet":
		s := &appsv1.StatefulSet{}
		obj, podSpec = s, &s.Spec.Template.Spec
	case "DaemonSet":
		d := &appsv1.DaemonSet{}
		obj, podSpec = d, &d.Spec.Template.Spec
	default:
		return nil, nil, fmt.Errorf("unsupported owner kind: %s", event.OwnerKind)
	}
	if err := i.client.Get(ctx, key, obj); err != nil {
		return nil, nil, fmt.Errorf("failed to get %s: %w", event.OwnerKind, err)
	}
	return obj, podSpec, nil
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

// patchWorkload raises the container's memory limit (and memory request, if
// set) in the workload's pod template and records the patch in annotations.
func (i *Interceptor) patchWorkload(ctx context.Context, obj client.Object, podSpec *corev1.PodSpec, event OOMEvent, newLimit *resource.Quantity) error {
	for idx := range podSpec.Containers {
		c := &podSpec.Containers[idx]
		if c.Name != event.ContainerName {
			continue
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = make(corev1.ResourceList)
		}
		c.Resources.Limits[corev1.ResourceMemory] = *newLimit
		if _, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			// Set requests to match limits for OOM scenarios
			c.Resources.Requests[corev1.ResourceMemory] = *newLimit
		}
		break
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[OOMAnnotationKey] = time.Now().Format(time.RFC3339)
	annotations["sartor.sartorproj.io/oom-container"] = event.ContainerName
	annotations["sartor.sartorproj.io/oom-previous-limit"] = event.CurrentMemoryLimit.String()
	annotations["sartor.sartorproj.io/oom-new-limit"] = newLimit.String()
	obj.SetAnnotations(annotations)

	if err := i.client.Update(ctx, obj); err != nil {
		return fmt.Errorf("failed to update %s: %w", event.OwnerKind, err)
	}
	return nil
}

// findTailoring returns the Tailoring targeting the event's workload, or nil.
// If several Tailorings target it, a paused one wins so pausing any of them
// stops patching.
func (i *Interceptor) findTailoring(ctx context.Context, event OOMEvent) (*autoscalingv1alpha1.Tailoring, error) {
	tailoringList := &autoscalingv1alpha1.TailoringList{}
	if err := i.client.List(ctx, tailoringList, client.InNamespace(event.Namespace)); err != nil {
		return nil, err
	}

	var found *autoscalingv1alpha1.Tailoring
	for idx := range tailoringList.Items {
		tailoring := &tailoringList.Items[idx]
		if tailoring.Spec.Target.Name != event.OwnerName || tailoring.Spec.Target.Kind != event.OwnerKind {
			continue
		}
		// A Tailoring that targets another namespace does not own this workload,
		// and OOM patching never follows a target into another namespace.
		if tailoring.TargetNamespace() != event.Namespace {
			continue
		}
		if tailoring.Spec.Paused {
			return tailoring, nil
		}
		if found == nil {
			found = tailoring
		}
	}

	return found, nil
}

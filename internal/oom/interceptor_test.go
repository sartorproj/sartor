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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func TestNewInterceptor(t *testing.T) {
	tests := []struct {
		name                    string
		config                  InterceptorConfig
		expectedMemoryIncreaseP int
	}{
		{
			name: "default memory increase",
			config: InterceptorConfig{
				Enabled: true,
			},
			expectedMemoryIncreaseP: DefaultMemoryIncrease,
		},
		{
			name: "custom memory increase",
			config: InterceptorConfig{
				MemoryIncreasePercent: 50,
				Enabled:               true,
			},
			expectedMemoryIncreaseP: 50,
		},
		{
			name: "with max memory",
			config: InterceptorConfig{
				MemoryIncreasePercent: 20,
				MaxMemory:             resource.NewQuantity(1024*1024*1024, resource.BinarySI),
				Enabled:               true,
			},
			expectedMemoryIncreaseP: 20,
		},
		{
			name: "disabled",
			config: InterceptorConfig{
				Enabled: false,
			},
			expectedMemoryIncreaseP: DefaultMemoryIncrease,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interceptor := NewInterceptor(nil, tt.config)
			assert.NotNil(t, interceptor)
			assert.Equal(t, tt.expectedMemoryIncreaseP, interceptor.config.MemoryIncreasePercent)
			assert.Equal(t, tt.config.Enabled, interceptor.config.Enabled)
		})
	}
}

func TestInterceptor_DetectOOMKilled(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		expectedEvents int
		checkEvent     func(t *testing.T, event OOMEvent)
	}{
		{
			name: "pod with OOMKilled container",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "OOMKilled",
									FinishedAt: metav1.Now(),
								},
							},
						},
					},
				},
			},
			expectedEvents: 1,
			checkEvent: func(t *testing.T, event OOMEvent) {
				assert.Equal(t, "app", event.ContainerName)
				assert.Equal(t, "test-pod", event.PodName)
				assert.NotNil(t, event.CurrentMemoryLimit)
				assert.Equal(t, int64(100*1024*1024), event.CurrentMemoryLimit.Value())
			},
		},
		{
			name: "pod without OOMKilled",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "Completed",
									FinishedAt: metav1.Now(),
								},
							},
						},
					},
				},
			},
			expectedEvents: 0,
		},
		{
			name: "pod with multiple containers - one OOMKilled",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-container-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("200Mi"),
								},
							},
						},
						{
							Name: "sidecar",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("50Mi"),
								},
							},
						},
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "OOMKilled",
									FinishedAt: metav1.Now(),
								},
							},
						},
						{
							Name: "sidecar",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "Completed",
									FinishedAt: metav1.Now(),
								},
							},
						},
					},
				},
			},
			expectedEvents: 1,
			checkEvent: func(t *testing.T, event OOMEvent) {
				assert.Equal(t, "app", event.ContainerName)
				assert.Equal(t, int64(200*1024*1024), event.CurrentMemoryLimit.Value())
			},
		},
		{
			name: "pod with OOMKilled but no memory limit set",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-limit-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							// No resources set
						},
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "OOMKilled",
									FinishedAt: metav1.Now(),
								},
							},
						},
					},
				},
			},
			expectedEvents: 1,
			checkEvent: func(t *testing.T, event OOMEvent) {
				assert.Equal(t, "app", event.ContainerName)
				assert.Nil(t, event.CurrentMemoryLimit)
			},
		},
		{
			name: "pod with running container (no termination state)",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-pod",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{
									StartedAt: metav1.Now(),
								},
							},
						},
					},
				},
			},
			expectedEvents: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interceptor := NewInterceptor(nil, InterceptorConfig{
				MemoryIncreasePercent: 20,
				Enabled:               true,
			})

			events := interceptor.DetectOOMKilled(tt.pod)
			assert.Len(t, events, tt.expectedEvents)

			if tt.checkEvent != nil && len(events) > 0 {
				tt.checkEvent(t, events[0])
			}
		})
	}
}

func TestInterceptor_DetectOOMKilled_WithOwnerReferences(t *testing.T) {
	isController := true

	tests := []struct {
		name              string
		ownerRefs         []metav1.OwnerReference
		expectedOwnerKind string
		expectedOwnerName string
	}{
		{
			name: "ReplicaSet owner (Deployment)",
			ownerRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "my-deployment-abc123",
					Controller: &isController,
				},
			},
			expectedOwnerKind: "Deployment",
			expectedOwnerName: "my-deployment",
		},
		{
			name: "StatefulSet owner",
			ownerRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "my-statefulset",
					Controller: &isController,
				},
			},
			expectedOwnerKind: "StatefulSet",
			expectedOwnerName: "my-statefulset",
		},
		{
			name: "DaemonSet owner",
			ownerRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "DaemonSet",
					Name:       "my-daemonset",
					Controller: &isController,
				},
			},
			expectedOwnerKind: "DaemonSet",
			expectedOwnerName: "my-daemonset",
		},
		{
			name:              "No owner",
			ownerRefs:         nil,
			expectedOwnerKind: "",
			expectedOwnerName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "default",
					OwnerReferences: tt.ownerRefs,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "OOMKilled",
									FinishedAt: metav1.Now(),
								},
							},
						},
					},
				},
			}

			interceptor := NewInterceptor(nil, InterceptorConfig{Enabled: true})
			events := interceptor.DetectOOMKilled(pod)

			require.Len(t, events, 1)
			assert.Equal(t, tt.expectedOwnerKind, events[0].OwnerKind)
			assert.Equal(t, tt.expectedOwnerName, events[0].OwnerName)
		})
	}
}

func TestExtractDeploymentName(t *testing.T) {
	tests := []struct {
		rsName          string
		expectedDepName string
	}{
		{"my-deployment-abc123", "my-deployment"},
		{"deployment-with-dashes-xyz789", "deployment-with-dashes"},
		{"nodash", "nodash"},
		{"single-", "single"},
		{"-leading", ""},
	}

	for _, tt := range tests {
		t.Run(tt.rsName, func(t *testing.T) {
			result := extractDeploymentName(tt.rsName)
			assert.Equal(t, tt.expectedDepName, result)
		})
	}
}

func TestInterceptor_CalculateNewMemoryLimit(t *testing.T) {
	tests := []struct {
		name            string
		currentLimit    *resource.Quantity
		increasePercent int
		maxMemory       *resource.Quantity
		expectedResult  int64
	}{
		{
			name:            "20% increase from 100Mi",
			currentLimit:    resource.NewQuantity(100*1024*1024, resource.BinarySI),
			increasePercent: 20,
			expectedResult:  120 * 1024 * 1024,
		},
		{
			name:            "50% increase from 100Mi",
			currentLimit:    resource.NewQuantity(100*1024*1024, resource.BinarySI),
			increasePercent: 50,
			expectedResult:  150 * 1024 * 1024,
		},
		{
			name:            "nil current limit defaults to 256Mi",
			currentLimit:    nil,
			increasePercent: 20,
			expectedResult:  256 * 1024 * 1024,
		},
		{
			name:            "capped by max memory",
			currentLimit:    resource.NewQuantity(900*1024*1024, resource.BinarySI),
			increasePercent: 20,
			maxMemory:       resource.NewQuantity(1024*1024*1024, resource.BinarySI),
			expectedResult:  1024 * 1024 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interceptor := NewInterceptor(nil, InterceptorConfig{
				MemoryIncreasePercent: tt.increasePercent,
				MaxMemory:             tt.maxMemory,
				Enabled:               true,
			})

			result := interceptor.calculateNewMemoryLimit(tt.currentLimit)
			require.NotNil(t, result)
			assert.Equal(t, tt.expectedResult, result.Value())
		})
	}
}

func TestInterceptor_HandleOOMEvent_Disabled(t *testing.T) {
	interceptor := NewInterceptor(nil, InterceptorConfig{
		Enabled: false,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "my-deployment",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "OOM Interceptor is disabled", result.Message)
}

func TestInterceptor_HandleOOMEvent_NilCurrentLimit(t *testing.T) {
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deployment",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app"},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(deployment)
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "my-deployment",
		CurrentMemoryLimit: nil, // No limit set
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.True(t, result.Success)
	// Should default to 256Mi
	assert.Equal(t, int64(256*1024*1024), result.NewMemoryLimit.Value())
}

func TestInterceptor_HandleOOMEvent_DeploymentPatch(t *testing.T) {
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deployment",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(deployment)
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "my-deployment",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.NotNil(t, result.NewMemoryLimit)
	assert.Equal(t, int64(120*1024*1024), result.NewMemoryLimit.Value())

	// Verify deployment was updated
	updatedDeployment := &appsv1.Deployment{}
	err = fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-deployment"}, updatedDeployment)
	require.NoError(t, err)

	// Check annotations
	assert.Contains(t, updatedDeployment.Annotations, OOMAnnotationKey)
	assert.Contains(t, updatedDeployment.Annotations, "sartor.sartorproj.io/oom-container")
	assert.Equal(t, "app", updatedDeployment.Annotations["sartor.sartorproj.io/oom-container"])

	// Check resource limits updated
	newLimit := updatedDeployment.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	assert.Equal(t, int64(120*1024*1024), newLimit.Value())
}

func TestInterceptor_HandleOOMEvent_StatefulSetPatch(t *testing.T) {
	replicas := int32(1)
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-statefulset",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(statefulSet)
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "StatefulSet",
		OwnerName:          "my-statefulset",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.True(t, result.Success)

	// Verify StatefulSet was updated
	updatedStatefulSet := &appsv1.StatefulSet{}
	err = fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-statefulset"}, updatedStatefulSet)
	require.NoError(t, err)

	assert.Contains(t, updatedStatefulSet.Annotations, OOMAnnotationKey)
	newLimit := updatedStatefulSet.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	assert.Equal(t, int64(120*1024*1024), newLimit.Value())
}

func TestInterceptor_HandleOOMEvent_DaemonSetPatch(t *testing.T) {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-daemonset",
			Namespace: "default",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(daemonSet)
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "DaemonSet",
		OwnerName:          "my-daemonset",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.True(t, result.Success)

	// Verify DaemonSet was updated
	updatedDaemonSet := &appsv1.DaemonSet{}
	err = fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-daemonset"}, updatedDaemonSet)
	require.NoError(t, err)

	assert.Contains(t, updatedDaemonSet.Annotations, OOMAnnotationKey)
}

func TestInterceptor_HandleOOMEvent_UnsupportedOwnerKind(t *testing.T) {
	fakeClient := newFakeClient()
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "CronJob", // Unsupported
		OwnerName:          "my-cronjob",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	assert.Error(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "unsupported owner kind")
}

func TestInterceptor_HandleOOMEvent_Cooldown(t *testing.T) {
	// Create deployment with recent OOM annotation
	recentPatchTime := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deployment",
			Namespace: "default",
			Annotations: map[string]string{
				OOMAnnotationKey: recentPatchTime,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app"},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(deployment)
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "my-deployment",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "Skipped due to cooldown", result.Message)
}

func TestInterceptor_HandleOOMEvent_ExpiredCooldown(t *testing.T) {
	// Create deployment with old OOM annotation (past cooldown)
	oldPatchTime := time.Now().Add(-15 * time.Minute).Format(time.RFC3339)
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deployment",
			Namespace: "default",
			Annotations: map[string]string{
				OOMAnnotationKey: oldPatchTime,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(deployment)
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "my-deployment",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.True(t, result.Success)
}

func TestInterceptor_FindTailoring(t *testing.T) {
	tailoring := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-tailoring",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target: autoscalingv1alpha1.TargetRef{
				Kind: "Deployment",
				Name: "my-deployment",
			},
		},
	}

	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deployment",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(tailoring, deployment)
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "my-deployment",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "my-tailoring", result.TailoringName)
}

func TestInterceptor_DeploymentNotFound(t *testing.T) {
	fakeClient := newFakeClient() // Empty client, no deployment
	interceptor := NewInterceptor(fakeClient, InterceptorConfig{
		MemoryIncreasePercent: 20,
		Enabled:               true,
	})

	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "non-existent-deployment",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	ctx := context.Background()
	result, err := interceptor.HandleOOMEvent(ctx, event)

	assert.Error(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "Failed to patch workload")
}

func TestOOMEvent_Structure(t *testing.T) {
	event := OOMEvent{
		Namespace:          "default",
		PodName:            "test-pod",
		ContainerName:      "app",
		OwnerKind:          "Deployment",
		OwnerName:          "my-deployment",
		CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		Timestamp:          time.Now(),
	}

	assert.Equal(t, "default", event.Namespace)
	assert.Equal(t, "test-pod", event.PodName)
	assert.Equal(t, "app", event.ContainerName)
	assert.Equal(t, "Deployment", event.OwnerKind)
	assert.Equal(t, "my-deployment", event.OwnerName)
	assert.NotNil(t, event.CurrentMemoryLimit)
	assert.False(t, event.Timestamp.IsZero())
}

func TestPatchResult_Structure(t *testing.T) {
	result := PatchResult{
		Success:        true,
		NewMemoryLimit: resource.NewQuantity(120*1024*1024, resource.BinarySI),
		Message:        "Memory limit increased",
		TailoringName:  "my-tailoring",
	}

	assert.True(t, result.Success)
	assert.NotNil(t, result.NewMemoryLimit)
	assert.Equal(t, "Memory limit increased", result.Message)
	assert.Equal(t, "my-tailoring", result.TailoringName)
}

func TestInterceptorConfig_Structure(t *testing.T) {
	config := InterceptorConfig{
		MemoryIncreasePercent: 30,
		MaxMemory:             resource.NewQuantity(2*1024*1024*1024, resource.BinarySI),
		Enabled:               true,
	}

	assert.Equal(t, 30, config.MemoryIncreasePercent)
	assert.NotNil(t, config.MaxMemory)
	assert.Equal(t, int64(2*1024*1024*1024), config.MaxMemory.Value())
	assert.True(t, config.Enabled)
}

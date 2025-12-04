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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewInterceptor(t *testing.T) {
	tests := []struct {
		name   string
		config InterceptorConfig
	}{
		{
			name: "default config",
			config: InterceptorConfig{
				MemoryIncreasePercent: 20,
				Enabled:               true,
			},
		},
		{
			name: "custom config",
			config: InterceptorConfig{
				MemoryIncreasePercent: 50,
				Enabled:               true,
			},
		},
		{
			name: "disabled",
			config: InterceptorConfig{
				Enabled: false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interceptor := NewInterceptor(nil, tt.config)
			assert.NotNil(t, interceptor)
			assert.Equal(t, tt.config.Enabled, interceptor.config.Enabled)
		})
	}
}

func TestInterceptor_DetectOOMKilled(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected int
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
			expected: 1,
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
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interceptor := NewInterceptor(nil, InterceptorConfig{
				MemoryIncreasePercent: 20,
				Enabled:               true,
			})

			events := interceptor.DetectOOMKilled(tt.pod)
			assert.Len(t, events, tt.expected)

			if tt.expected > 0 {
				assert.Equal(t, "app", events[0].ContainerName)
				assert.Equal(t, "test-pod", events[0].PodName)
			}
		})
	}
}

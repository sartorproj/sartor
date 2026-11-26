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
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

func gatingDeployment(annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")},
					},
				}, {
					Name: "sidecar",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")},
					},
				}}},
			},
		},
	}
}

func TestInterceptor_HandleOOMEvent_Gating(t *testing.T) {
	now := time.Now()
	otherNamespace := tailoringFor("Deployment", "web", false)
	otherNamespace.Namespace = "other"

	tests := []struct {
		name        string
		enabled     bool
		tailorings  []*autoscalingv1alpha1.Tailoring
		annotations map[string]string
		container   string
		oomAt       time.Time
		wantPatched bool
		wantMessage string
	}{
		{
			name:        "interceptor disabled",
			enabled:     false,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("Deployment", "web", false)},
			wantMessage: "OOM Interceptor is disabled",
		},
		{
			name:        "no Tailoring for the workload",
			enabled:     true,
			wantMessage: "No Tailoring targets Deployment/web",
		},
		{
			name:        "paused Tailoring",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("Deployment", "web", true)},
			wantMessage: "is paused",
		},
		{
			name:    "one of two Tailorings paused",
			enabled: true,
			tailorings: []*autoscalingv1alpha1.Tailoring{
				tailoringFor("Deployment", "web", false),
				func() *autoscalingv1alpha1.Tailoring {
					tl := tailoringFor("Deployment", "web", true)
					tl.Name = "z-paused"
					return tl
				}(),
			},
			wantMessage: "is paused",
		},
		{
			name:        "Tailoring targets a same-named DaemonSet",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("DaemonSet", "web", false)},
			wantMessage: "No Tailoring targets",
		},
		{
			name:        "Tailoring in another namespace",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{otherNamespace},
			wantMessage: "No Tailoring targets",
		},
		{
			name:        "container excluded by Tailoring",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("Deployment", "web", false, "sidecar")},
			container:   "sidecar",
			wantMessage: "excluded",
		},
		{
			name:        "OOM older than last patch is not re-applied",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("Deployment", "web", false)},
			annotations: map[string]string{OOMAnnotationKey: now.Add(-15 * time.Minute).Format(time.RFC3339)},
			oomAt:       now.Add(-20 * time.Minute),
			wantMessage: "already handled",
		},
		{
			name:        "recent patch is in cooldown",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("Deployment", "web", false)},
			annotations: map[string]string{OOMAnnotationKey: now.Add(-time.Minute).Format(time.RFC3339)},
			wantMessage: "cooldown",
		},
		{
			name:        "new OOM after an expired cooldown is patched",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("Deployment", "web", false)},
			annotations: map[string]string{OOMAnnotationKey: now.Add(-15 * time.Minute).Format(time.RFC3339)},
			oomAt:       now.Add(-time.Minute),
			wantPatched: true,
		},
		{
			name:        "active Tailoring",
			enabled:     true,
			tailorings:  []*autoscalingv1alpha1.Tailoring{tailoringFor("Deployment", "web", false, "sidecar")},
			wantPatched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{gatingDeployment(tt.annotations)}
			for _, tl := range tt.tailorings {
				objs = append(objs, tl)
			}
			c := newFakeClient(objs...)
			interceptor := NewInterceptor(c, InterceptorConfig{Enabled: tt.enabled})

			container := tt.container
			if container == "" {
				container = "app"
			}
			oomAt := tt.oomAt
			if oomAt.IsZero() {
				oomAt = now
			}
			result, err := interceptor.HandleOOMEvent(context.Background(), OOMEvent{
				Namespace:          "default",
				PodName:            "web-7d9f8c6b5-x2bqz",
				ContainerName:      container,
				OwnerKind:          "Deployment",
				OwnerName:          "web",
				CurrentMemoryLimit: resource.NewQuantity(100*1024*1024, resource.BinarySI),
				Timestamp:          oomAt,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantPatched, result.Success, result.Message)
			if tt.wantMessage != "" {
				assert.Contains(t, result.Message, tt.wantMessage)
			}

			got := &appsv1.Deployment{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "web"}, got))
			var limit resource.Quantity
			for _, ctr := range got.Spec.Template.Spec.Containers {
				if ctr.Name == container {
					limit = ctr.Resources.Limits[corev1.ResourceMemory]
				}
			}
			if tt.wantPatched {
				assert.Equal(t, int64(120*1024*1024), limit.Value())
			} else {
				assert.Equal(t, int64(100*1024*1024), limit.Value(), "workload must not be patched")
				assert.Equal(t, tt.annotations[OOMAnnotationKey], got.Annotations[OOMAnnotationKey])
			}
		})
	}
}

func TestDeploymentNameFromReplicaSet(t *testing.T) {
	tests := []struct {
		name   string
		rsName string
		hash   string
		want   string
	}{
		{"hash label strips exact suffix", "web-api-7d9f8c6b5", "7d9f8c6b5", "web-api"},
		{"hash label mismatch falls back", "web-api-7d9f8c6b5", "zzz", "web-api"},
		{"no hash label falls back to last dash", "web-api-7d9f8c6b5", "", "web-api"},
		{"name ending in hash-like segment", "web-v2-5b4", "5b4", "web-v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, deploymentNameFromReplicaSet(tt.rsName, tt.hash))
		})
	}
}

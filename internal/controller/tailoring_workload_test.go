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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

func TestParseWorkloadSource(t *testing.T) {
	for _, v := range []string{"kubernetes", "metrics"} {
		got, err := ParseWorkloadSource(v)
		require.NoError(t, err)
		assert.Equal(t, WorkloadSource(v), got)
	}
	_, err := ParseWorkloadSource("Metrics")
	assert.Error(t, err)
	_, err = ParseWorkloadSource("")
	assert.Error(t, err)
}

func unstructuredObj(gvk schema.GroupVersionKind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("blue")
	u.SetName(name)
	return u
}

func autoscalerScheme(t *testing.T, withCRDs bool) *runtime.Scheme {
	s := wbScheme(t)
	if withCRDs {
		for _, list := range []schema.GroupVersionKind{gvkVPAList, gvkScaledObjectList} {
			item := list.GroupVersion().WithKind(list.Kind[:len(list.Kind)-len("List")])
			s.AddKnownTypeWithName(item, &unstructured.Unstructured{})
			s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
		}
	}
	return s
}

var (
	gvkVPA          = schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler"}
	gvkScaledObject = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}
)

func hpa(ns, name, target string, labels map[string]string, metrics ...autoscalingv2.MetricSpec) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: target, APIVersion: "apps/v1"},
			MaxReplicas:    5,
			Metrics:        metrics,
		},
	}
}

func resourceMetric(name corev1.ResourceName, target autoscalingv2.MetricTargetType) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type:     autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{Name: name, Target: autoscalingv2.MetricTarget{Type: target}},
	}
}

// TestDetectAutoscalers_Kubernetes covers HPA and VPA on the same workload,
// KEDA ownership, Utilization vs AverageValue HPA targets, VPA update modes
// and autoscalers of other workloads or namespaces.
func TestDetectAutoscalers_Kubernetes(t *testing.T) {
	objs := []client.Object{
		hpa("blue", "web", "web", nil,
			resourceMetric(corev1.ResourceCPU, autoscalingv2.UtilizationMetricType),
			resourceMetric(corev1.ResourceMemory, autoscalingv2.AverageValueMetricType)),
		hpa("blue", "keda-hpa-web", "web", map[string]string{labelKEDAScaledObject: "web"},
			autoscalingv2.MetricSpec{Type: autoscalingv2.ContainerResourceMetricSourceType, ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
				Name: corev1.ResourceMemory, Container: "app", Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType},
			}}),
		hpa("blue", "other", "other", nil, resourceMetric(corev1.ResourceCPU, autoscalingv2.UtilizationMetricType)),
		hpa("green", "web", "web", nil, resourceMetric(corev1.ResourceCPU, autoscalingv2.UtilizationMetricType)),
		unstructuredObj(gvkVPA, "web-auto", map[string]interface{}{"targetRef": map[string]interface{}{"kind": "Deployment", "name": "web"}}),
		unstructuredObj(gvkVPA, "web-off", map[string]interface{}{
			"targetRef":    map[string]interface{}{"kind": "Deployment", "name": "web"},
			"updatePolicy": map[string]interface{}{"updateMode": "Off"},
		}),
		unstructuredObj(gvkVPA, "sts", map[string]interface{}{"targetRef": map[string]interface{}{"kind": "StatefulSet", "name": "web"}}),
		unstructuredObj(gvkScaledObject, "web", map[string]interface{}{"scaleTargetRef": map[string]interface{}{"name": "web"}}),
	}
	c := fake.NewClientBuilder().WithScheme(autoscalerScheme(t, true)).WithObjects(objs...).Build()
	r := &TailoringReconciler{Client: c}

	got := r.detectAutoscalers(context.Background(), "blue", autoscalingv1alpha1.TargetRef{Kind: "Deployment", Name: "web"}, logr.Discard())
	assert.Equal(t, []autoscalingv1alpha1.DetectedAutoscaler{
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "keda-hpa-web", UtilizationResources: []string{"memory"}, ManagedBy: "web"},
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "web", UtilizationResources: []string{"cpu"}},
		{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "web"},
		{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "web-auto", UpdateMode: "Auto"},
		{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "web-off", UpdateMode: "Off"},
	}, got)
}

func TestDetectAutoscalers_CRDsNotInstalled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(autoscalerScheme(t, false)).
		WithObjects(hpa("blue", "web", "web", nil, resourceMetric(corev1.ResourceCPU, autoscalingv2.UtilizationMetricType))).Build()
	r := &TailoringReconciler{Client: c}
	got := r.detectAutoscalers(context.Background(), "blue", autoscalingv1alpha1.TargetRef{Kind: "Deployment", Name: "web"}, logr.Discard())
	assert.Equal(t, []autoscalingv1alpha1.DetectedAutoscaler{
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "web", UtilizationResources: []string{"cpu"}},
	}, got)
}

// TestReadWorkload_KubernetesTargetNamespace: the workload is read from
// spec.target.namespace when set, from the Tailoring's namespace otherwise.
func TestReadWorkload_KubernetesTargetNamespace(t *testing.T) {
	deploy := func(ns, cpu string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
			}}}}},
		}
	}
	c := fake.NewClientBuilder().WithScheme(autoscalerScheme(t, false)).
		WithObjects(deploy("apps", "100m"), deploy("blue", "300m")).Build()
	r := &TailoringReconciler{Client: c}
	tl := mmTailoring("web")

	view, err := r.readWorkload(context.Background(), tl, nil, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, "100m", view.current["app"].Requests.CPU.String())

	tl.Spec.Target.Namespace = "blue"
	view, err = r.readWorkload(context.Background(), tl, nil, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, "300m", view.current["app"].Requests.CPU.String())

	tl.Spec.Target.Namespace = "green"
	_, err = r.readWorkload(context.Background(), tl, nil, logr.Discard())
	assert.ErrorIs(t, err, errTargetNotFound)
}

func TestApplyOOMFloor(t *testing.T) {
	recs := []strategy.Recommendation{
		{ContainerName: "app", Requests: strategy.ResourceValues{Memory: q("100Mi")}, Limits: strategy.ResourceValues{Memory: q("2Gi")}},
		{ContainerName: "sidecar", Requests: strategy.ResourceValues{Memory: q("10Mi")}},
	}
	current := map[string]autoscalingv1alpha1.ResourceRequirements{
		"app":     {Requests: &autoscalingv1alpha1.ResourceValues{Memory: q("512Mi")}, Limits: &autoscalingv1alpha1.ResourceValues{Memory: q("1Gi")}},
		"sidecar": {Requests: &autoscalingv1alpha1.ResourceValues{Memory: q("64Mi")}},
	}
	applyOOMFloor(recs, current, []string{"app"})
	assert.Equal(t, "512Mi", recs[0].Requests.Memory.String(), "raised to the current request")
	assert.Equal(t, "2Gi", recs[0].Limits.Memory.String(), "a higher recommendation is kept")
	assert.Equal(t, "10Mi", recs[1].Requests.Memory.String(), "containers without OOM kills are unaffected")
	assert.Equal(t, "512Mi", current["app"].Requests.Memory.String(), "current values are not aliased")
}

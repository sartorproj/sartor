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
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender"
	"github.com/sartorproj/sartor/internal/recommender/scaling"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

func int32p(v int32) *int32 { return &v }

func TestHPAMetricDetails(t *testing.T) {
	util := func(v int32) autoscalingv2.MetricTarget {
		return autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &v}
	}
	avg := autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType}
	metrics := []autoscalingv2.MetricSpec{
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory, Target: util(80)}},
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU, Target: util(70)}},
		{Type: autoscalingv2.ContainerResourceMetricSourceType, ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
			Name: corev1.ResourceCPU, Container: "app", Target: util(50)}},
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU, Target: avg}},
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU, Target: util(0)}},
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: "ephemeral-storage", Target: util(50)}},
		{Type: autoscalingv2.PodsMetricSourceType, Pods: &autoscalingv2.PodsMetricSource{Metric: autoscalingv2.MetricIdentifier{Name: "rps"}}},
		{Type: autoscalingv2.ObjectMetricSourceType, Object: &autoscalingv2.ObjectMetricSource{Metric: autoscalingv2.MetricIdentifier{Name: "hits"}}},
		{Type: autoscalingv2.ExternalMetricSourceType, External: &autoscalingv2.ExternalMetricSource{Metric: autoscalingv2.MetricIdentifier{Name: "queue"}}},
		{Type: autoscalingv2.ExternalMetricSourceType},
	}
	targets, other := hpaMetricDetails(metrics)
	assert.Equal(t, []autoscalingv1alpha1.UtilizationTarget{
		{Resource: "cpu", AverageUtilization: 70},
		{Resource: "cpu", Container: "app", AverageUtilization: 50},
		{Resource: "memory", AverageUtilization: 80},
	}, targets)
	assert.Equal(t, []string{"External", "External queue", "Object hits", "Pods rps", "cpu AverageValue", "ephemeral-storage Utilization"}, other)

	targets, other = hpaMetricDetails([]autoscalingv2.MetricSpec{metrics[8]})
	assert.Empty(t, targets, "external metrics only")
	assert.Equal(t, []string{"External queue"}, other)
}

func TestScaledObjectDetails(t *testing.T) {
	obj := map[string]interface{}{"spec": map[string]interface{}{
		"minReplicaCount": int64(2),
		"maxReplicaCount": int64(30),
		"triggers": []interface{}{
			map[string]interface{}{"type": "cpu", "metricType": "Utilization", "metadata": map[string]interface{}{"value": "60"}},
			map[string]interface{}{"type": "memory", "metadata": map[string]interface{}{"type": "Utilization", "value": "80", "containerName": "app"}},
			map[string]interface{}{"type": "memory", "metricType": "AverageValue", "metadata": map[string]interface{}{"value": "300Mi"}},
			map[string]interface{}{"type": "cpu", "metricType": "Utilization", "metadata": map[string]interface{}{"value": "NaN"}},
			map[string]interface{}{"type": "cpu", "metricType": "Utilization", "metadata": map[string]interface{}{"value": "0"}},
			map[string]interface{}{"type": "rabbitmq", "metadata": map[string]interface{}{"value": "20"}},
			"not a trigger",
		},
	}}
	d := scaledObjectDetails("web", obj)
	assert.Equal(t, int32(2), *d.MinReplicas)
	assert.Equal(t, int32(30), *d.MaxReplicas)
	assert.Equal(t, []string{"cpu", "memory"}, d.UtilizationResources)
	assert.Equal(t, []autoscalingv1alpha1.UtilizationTarget{
		{Resource: "cpu", AverageUtilization: 60},
		{Resource: "memory", Container: "app", AverageUtilization: 80},
	}, d.UtilizationTargets)
	assert.Equal(t, []string{"cpu Utilization", "cpu Utilization", "memory AverageValue", "rabbitmq"}, d.OtherMetrics)

	d = scaledObjectDetails("web", map[string]interface{}{"spec": map[string]interface{}{
		"minReplicaCount": float64(-3), "maxReplicaCount": float64(math.MaxInt64),
	}})
	assert.Equal(t, int32(kedaDefaultMinReplicas), *d.MinReplicas, "invalid counts fall back to the KEDA defaults")
	assert.Equal(t, int32(kedaDefaultMaxReplicas), *d.MaxReplicas)
	assert.Empty(t, d.UtilizationTargets)
}

func TestVPAPolicies(t *testing.T) {
	obj := map[string]interface{}{"spec": map[string]interface{}{"resourcePolicy": map[string]interface{}{"containerPolicies": []interface{}{
		map[string]interface{}{
			"containerName":       "*",
			"mode":                "Auto",
			"controlledResources": []interface{}{"memory"},
			"minAllowed":          map[string]interface{}{"memory": "100Mi"},
			"maxAllowed":          map[string]interface{}{"cpu": int64(2), "memory": "-1Gi"},
		},
		map[string]interface{}{"containerName": "app", "mode": "Off", "minAllowed": map[string]interface{}{"cpu": "bogus"}},
		map[string]interface{}{"mode": "Auto"},
		"not a policy",
	}}}}
	got := vpaPolicies(obj)
	require.Len(t, got, 2)
	assert.Equal(t, "*", got[0].ContainerName)
	assert.Equal(t, []string{"memory"}, got[0].ControlledResources)
	assert.Equal(t, "100Mi", got[0].MinAllowed.Memory.String())
	assert.Equal(t, "2", got[0].MaxAllowed.CPU.String())
	assert.Nil(t, got[0].MaxAllowed.Memory, "negative bound dropped")
	assert.Equal(t, "Off", got[1].Mode)
	assert.Nil(t, got[1].MinAllowed, "unparsable bound dropped")
	assert.Empty(t, vpaPolicies(map[string]interface{}{}))
}

func TestHorizontalAndVerticalAutoscalers(t *testing.T) {
	detected := []autoscalingv1alpha1.DetectedAutoscaler{
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "keda-hpa-web", ManagedBy: "web", MaxReplicas: int32p(9)},
		{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "web"},
		{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "lonely", MinReplicas: int32p(0)},
		{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "web", UpdateMode: "Auto"},
	}
	h := horizontalAutoscalers(detected)
	require.Len(t, h, 2, "a ScaledObject whose HPA was detected is not counted twice")
	assert.Equal(t, "keda-hpa-web", h[0].Name)
	assert.Equal(t, int32(9), *h[0].MaxReplicas)
	assert.Equal(t, "lonely", h[1].Name)

	policies := map[string][]scaling.VPAPolicy{"web": {{ContainerName: "*"}}}
	v := verticalAutoscalers(detected, policies)
	require.Len(t, v, 1)
	assert.Equal(t, scaling.VPA{Name: "web", UpdateMode: "Auto", Policies: policies["web"]}, v[0])
}

func TestMetricsVPAPolicies(t *testing.T) {
	q := resource.MustParse("100m")
	mem := resource.MustParse("1Gi")
	got := metricsVPAPolicies(prometheus.VPAInfo{
		Name:       "web",
		MinAllowed: map[string]prometheus.ResourceValues{"b": {CPU: &q}},
		MaxAllowed: map[string]prometheus.ResourceValues{"a": {Memory: &mem}, "b": {}},
	})
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ContainerName)
	assert.Nil(t, got[0].MinAllowed)
	assert.Equal(t, "1Gi", got[0].MaxAllowed.Memory.String())
	assert.Equal(t, "100m", got[1].MinAllowed.CPU.String())
	assert.Nil(t, got[1].MaxAllowed)
	assert.Empty(t, metricsVPAPolicies(prometheus.VPAInfo{}))
}

type fakeSignals struct {
	signals *prometheus.ScalingSignals
	err     error
	calls   int
}

func (f *fakeSignals) GetScalingSignals(context.Context, string, prometheus.Workload, time.Duration) (*prometheus.ScalingSignals, error) {
	f.calls++
	return f.signals, f.err
}

func fptr(v float64) *float64 { return &v }

func scalingRecs() []strategy.Recommendation {
	cpu, mem := resource.MustParse("300m"), resource.MustParse("400Mi")
	return []strategy.Recommendation{{ContainerName: "app", Requests: strategy.ResourceValues{CPU: &cpu, Memory: &mem}}}
}

func TestApplyScaling(t *testing.T) {
	ctx := context.Background()
	spiky := &prometheus.ScalingSignals{
		Containers: map[string]prometheus.UsageSignals{"app": {CPU: prometheus.UsageStats{P50: fptr(0.1), Max: fptr(1)}}},
	}
	p99 := resource.MustParse("800m")
	metrics := []prometheus.ContainerMetrics{{ContainerName: "app", P99CPU: &p99}}
	tl := func(wb autoscalingv1alpha1.WriteBackType) *autoscalingv1alpha1.Tailoring {
		return mmTailoring("web", func(t *autoscalingv1alpha1.Tailoring) { t.Spec.WriteBack.Type = wb })
	}
	vpaView := &workloadView{autoscalers: []autoscalingv1alpha1.DetectedAutoscaler{{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "web", UpdateMode: "Auto"}}}

	t.Run("no autoscalers: no signal query", func(t *testing.T) {
		f := &fakeSignals{signals: spiky}
		s := applyScaling(ctx, tl(autoscalingv1alpha1.WriteBackTypeRaw), f, &workloadView{}, metrics, scalingRecs(), recommender.SafetyRails{}, time.Hour, logr.Discard())
		assert.Zero(t, f.calls)
		assert.Empty(t, s.Autoscalers)
	})
	t.Run("raw write-back writes the VPA bounds", func(t *testing.T) {
		f := &fakeSignals{signals: spiky}
		recs := scalingRecs()
		s := applyScaling(ctx, tl(autoscalingv1alpha1.WriteBackTypeRaw), f, vpaView, metrics, recs, recommender.SafetyRails{}, time.Hour, logr.Discard())
		assert.Equal(t, 1, f.calls)
		require.Len(t, s.VPA, 1)
		assert.True(t, s.VPA[0].WriteBack)
		assert.Equal(t, "120m", recs[0].Requests.CPU.String())
	})
	t.Run("other write-back types only recommend the bounds", func(t *testing.T) {
		f := &fakeSignals{signals: spiky}
		s := applyScaling(ctx, tl(autoscalingv1alpha1.WriteBackTypeHelm), f, vpaView, metrics, scalingRecs(), recommender.SafetyRails{}, time.Hour, logr.Discard())
		require.Len(t, s.VPA, 1)
		assert.False(t, s.VPA[0].WriteBack)
		assert.Contains(t, s.VPA[0].Reason, "Only raw write-back edits VPA objects")
	})
	t.Run("signal errors keep per-pod sizing", func(t *testing.T) {
		f := &fakeSignals{err: errors.New("backend down")}
		recs := scalingRecs()
		s := applyScaling(ctx, tl(autoscalingv1alpha1.WriteBackTypeRaw), f, vpaView, metrics, recs, recommender.SafetyRails{}, time.Hour, logr.Discard())
		assert.Equal(t, "300m", recs[0].Requests.CPU.String())
		assert.Empty(t, s.VPA)
		assert.NotEmpty(t, s.Reason)
	})
}

func TestSetConflictCondition(t *testing.T) {
	r := &TailoringReconciler{}
	tl := &autoscalingv1alpha1.Tailoring{}

	tl.Status.Scaling = &autoscalingv1alpha1.ScalingSummary{
		Autoscalers: []string{"HorizontalPodAutoscaler/web", "VerticalPodAutoscaler/web"},
		Conflicts:   []autoscalingv1alpha1.ScalingConflict{{Resource: "cpu", Message: "both act on cpu"}, {Message: "two VPAs"}},
	}
	r.setConflictCondition(tl)
	c := condition(tl, ConditionTypeAutoscalerConflict)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "AutoscalersConflict", c.Reason)
	assert.Equal(t, "both act on cpu; two VPAs", c.Message)

	tl.Status.Scaling.Conflicts = nil
	r.setConflictCondition(tl)
	c = condition(tl, ConditionTypeAutoscalerConflict)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "NoConflict", c.Reason)

	tl.Status.Scaling = &autoscalingv1alpha1.ScalingSummary{}
	r.setConflictCondition(tl)
	assert.Nil(t, condition(tl, ConditionTypeAutoscalerConflict), "removed without autoscalers")
}

func TestVPAWriteBackBounds(t *testing.T) {
	tl := &autoscalingv1alpha1.Tailoring{}
	assert.Nil(t, vpaWriteBackBounds(tl))
	assert.Nil(t, alignedResources(tl))

	cpu := resource.MustParse("1")
	tl.Status.Scaling = &autoscalingv1alpha1.ScalingSummary{
		AlignedResources: []string{"memory"},
		VPA: []autoscalingv1alpha1.VPARecommendation{
			{Name: "off", WriteBack: false, Containers: []autoscalingv1alpha1.VPAContainerBounds{{Name: "app", MaxAllowed: &autoscalingv1alpha1.ResourceValues{CPU: &cpu}}}},
			{Name: "empty", WriteBack: true},
			{Name: "web", WriteBack: true, Containers: []autoscalingv1alpha1.VPAContainerBounds{{Name: "app", MaxAllowed: &autoscalingv1alpha1.ResourceValues{CPU: &cpu}}}},
		},
	}
	got := vpaWriteBackBounds(tl)
	require.Len(t, got, 1)
	assert.Nil(t, got["web"]["app"].MinAllowed)
	assert.Equal(t, "1", got["web"]["app"].MaxAllowed.CPU.String())
	assert.Equal(t, []string{"memory"}, alignedResources(tl))
}

// signalSamples answers the scaling signal queries of the fake backend.
func signalSamples(cpu, memory, replicas map[string]float64) func(q string) []promSample {
	return func(q string) []promSample {
		values, container := replicas, ""
		switch {
		case strings.Contains(q, "container_memory_working_set_bytes"):
			values, container = memory, "app"
		case strings.Contains(q, "container_cpu_usage_seconds_total"):
			values, container = cpu, "app"
		}
		out := make([]promSample, 0, len(values))
		for agg, v := range values {
			l := map[string]string{"sartor_agg": agg}
			if container != "" {
				l["container"] = container
			}
			out = append(out, promSample{labels: l, value: v})
		}
		return out
	}
}

// TestMetricsMode_HPAAlignedRequests: an HPA (managed by a ScaledObject)
// targets 60% cpu. 3 replicas typically share 270m, so 90m per pod: the cpu
// request is sized to 90m / 60% = 150m, and the cpu trigger is kept as is.
func TestMetricsMode_HPAAlignedRequests(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	snap := agreeing()
	snap.extra = append(hpaSamples("keda-hpa-web", []string{"cpu"}, "web"), vpaSamples("Off")...)
	fx.backend.snapshots[mmTailoringNS] = snap
	fx.backend.signals = signalSamples(
		map[string]float64{"p50": 0.1, "max": 0.2, "total_median": 0.27},
		map[string]float64{"p50": 500 * mi, "max": 560 * mi, "total_median": 1500 * mi},
		map[string]float64{"min": 2, "p50": 3, "max": 6},
	)
	fx.git.SetFile("main", mmPath, strings.Replace(mmManifest, "updateMode: Initial", "updateMode: Off", 1))

	_, tl := fx.reconcile(t, "web")

	require.NotNil(t, tl.Status.Scaling)
	s := tl.Status.Scaling
	assert.Equal(t, []string{"cpu"}, s.AlignedResources)
	assert.Equal(t, &autoscalingv1alpha1.ReplicaRange{Min: 2, Typical: 3, Max: 6, Source: autoscalingv1alpha1.ReplicaSourceHistory}, s.Replicas)
	assert.Empty(t, s.Conflicts, "a VPA with updateMode Off does not fight the HPA")
	require.Len(t, s.Containers, 1)
	assert.Equal(t, "150m", s.Containers[0].CPU.Request.String())
	assert.Equal(t, autoscalingv1alpha1.ScalingSizingHPAUtilization, s.Containers[0].CPU.Sizing)
	assert.Contains(t, s.Reason, "Requests for cpu sized for the Utilization target of HorizontalPodAutoscaler keda-hpa-web at 3 replicas (2-6)")

	file, ok := fx.git.File("sartor/apps/web", mmPath)
	require.True(t, ok)
	assert.Contains(t, file, "cpu: 150m")
	assert.Contains(t, file, `value: "60"`, "the cpu trigger is not rescaled for an aligned request")

	conflict := condition(tl, ConditionTypeAutoscalerConflict)
	require.NotNil(t, conflict)
	assert.Equal(t, metav1.ConditionFalse, conflict.Status)
	shifted := condition(tl, ConditionTypeScalingPointShifted)
	require.NotNil(t, shifted)
	assert.Equal(t, metav1.ConditionFalse, shifted.Status)
	assert.Equal(t, "SizedForUtilizationTarget", shifted.Reason)

	body := fx.git.Body(tl.Status.PRNumber)
	assert.Contains(t, body, "The cpu requests are sized so the Utilization target matches the typical per-pod usage")
	assert.NotContains(t, body, "scaling point shifts")
}

// TestMetricsMode_VPASpikyWritesBounds: cpu idles at 100m and peaks at 1
// core. With a VPA in Auto the request is lowered to the baseline and the
// VPA bounds written so it covers the peaks.
func TestMetricsMode_VPASpikyWritesBounds(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	snap := agreeing()
	snap.extra = append(vpaSamples("Auto"), promSample{labels: map[string]string{
		"sartor_source": "vpa_bound", "verticalpodautoscaler": "web", "container": "app", "sartor_bound": "maxallowed_cpu",
	}, value: 0.3})
	fx.backend.snapshots[mmTailoringNS] = snap
	fx.backend.signals = signalSamples(
		map[string]float64{"p50": 0.1, "max": 1, "total_median": 0.3},
		map[string]float64{"p50": 550 * mi, "max": 600 * mi, "total_median": 1650 * mi},
		map[string]float64{"min": 3, "p50": 3, "max": 3},
	)
	fx.git.SetFile("main", mmPath, strings.Replace(mmManifest, "updateMode: Initial", "updateMode: Auto", 1))

	_, tl := fx.reconcile(t, "web")

	s := tl.Status.Scaling
	require.NotNil(t, s)
	require.Len(t, s.VPA, 1)
	v := s.VPA[0]
	assert.Equal(t, "Auto", v.UpdateMode)
	assert.True(t, v.WriteBack)
	require.Len(t, v.Containers, 1)
	assert.Equal(t, "120m", v.Containers[0].MinAllowed.CPU.String())
	assert.Equal(t, "1200m", v.Containers[0].MaxAllowed.CPU.String())
	require.NotNil(t, v.Containers[0].CurrentMaxAllowed)
	assert.Equal(t, "300m", v.Containers[0].CurrentMaxAllowed.CPU.String(), "the current bound reported by kube-state-metrics")
	c := s.Containers[0]
	assert.True(t, c.CPU.Spiky)
	assert.Equal(t, autoscalingv1alpha1.ScalingSizingVPABaseline, c.CPU.Sizing)
	assert.False(t, c.Memory.Spiky)

	file, ok := fx.git.File("sartor/apps/web", mmPath)
	require.True(t, ok)
	assert.Contains(t, file, "cpu: 125m", "the baseline 120m, limited by maxChangePercent to 50% of 250m")
	assert.Contains(t, file, "maxAllowed:\n          cpu: 1200m")
	assert.Contains(t, file, "minAllowed:\n          cpu: 120m")
	body := fx.git.Body(tl.Status.PRNumber)
	assert.Contains(t, body, "maxAllowed.cpu 300m -> 1200m")
}

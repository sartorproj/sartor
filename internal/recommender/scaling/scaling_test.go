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

package scaling

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

const mi = 1 << 20

func q(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

func f(v float64) *float64 { return &v }

func i32(v int32) *int32 { return &v }

// perPod returns a per-pod recommendation of 300m/400Mi requests with
// 600m/800Mi limits for container app.
func perPod() []strategy.Recommendation {
	return []strategy.Recommendation{{
		ContainerName: "app",
		Requests:      strategy.ResourceValues{CPU: q("300m"), Memory: q("400Mi")},
		Limits:        strategy.ResourceValues{CPU: q("600m"), Memory: q("800Mi")},
	}}
}

// steadySignals: 4 replicas typical (2-8), summed cpu 1.4 cores (350m per
// pod) and memory 1600Mi (400Mi per pod), busiest pod at 400m/450Mi.
func steadySignals() *prometheus.ScalingSignals {
	return &prometheus.ScalingSignals{
		Containers: map[string]prometheus.UsageSignals{"app": {
			CPU:    prometheus.UsageStats{P50: f(0.4), Max: f(0.5), TotalMedian: f(1.4)},
			Memory: prometheus.UsageStats{P50: f(450 * mi), Max: f(500 * mi), TotalMedian: f(1600 * mi)},
		}},
		Replicas: prometheus.ReplicaHistory{Min: f(2), Median: f(4), Max: f(8)},
	}
}

// spikySignals: cpu baseline 100m, peaks at 1.5 cores (P99 600m).
func spikySignals() *prometheus.ScalingSignals {
	return &prometheus.ScalingSignals{
		Containers: map[string]prometheus.UsageSignals{"app": {
			CPU:    prometheus.UsageStats{P50: f(0.1), Max: f(1.5), TotalMedian: f(0.3)},
			Memory: prometheus.UsageStats{P50: f(390 * mi), Max: f(420 * mi), TotalMedian: f(1170 * mi)},
		}},
		Replicas: prometheus.ReplicaHistory{Min: f(3), Median: f(3), Max: f(3)},
	}
}

func metrics(p99CPU, p99Mem string) []prometheus.ContainerMetrics {
	return []prometheus.ContainerMetrics{{ContainerName: "app", P99CPU: q(p99CPU), P99Memory: q(p99Mem)}}
}

func cpuHPA(pct int32) HPA {
	return HPA{
		Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "web", MinReplicas: i32(2), MaxReplicas: i32(10),
		Targets: []autoscalingv1alpha1.UtilizationTarget{{Resource: "cpu", AverageUtilization: pct}},
	}
}

// appScaling returns the summary of container app.
func appScaling(t *testing.T, s *autoscalingv1alpha1.ScalingSummary) autoscalingv1alpha1.ContainerScaling {
	t.Helper()
	for _, c := range s.Containers {
		if c.Name == "app" {
			return c
		}
	}
	t.Fatalf("container app not in summary %+v", s.Containers)
	return autoscalingv1alpha1.ContainerScaling{}
}

func TestApply_NoAutoscalers(t *testing.T) {
	recs := perPod()
	s := Apply(recs, Input{Signals: steadySignals()})
	assert.Equal(t, "300m", recs[0].Requests.CPU.String())
	assert.Empty(t, s.Containers)
	assert.Contains(t, s.Reason, "No autoscaler")
}

// An HPA on cpu at 70%: per-pod usage at the typical 4 replicas is
// 1.4/4 = 350m, so the request is 350m/0.7 = 500m. Memory has no target.
func TestApply_HPACPUUtilization(t *testing.T) {
	recs := perPod()
	s := Apply(recs, Input{
		WorkloadKind: "Deployment", HPAs: []HPA{cpuHPA(70)}, Signals: steadySignals(),
		Metrics: metrics("450m", "480Mi"),
	})
	assert.Equal(t, "500m", recs[0].Requests.CPU.String())
	assert.Equal(t, "600m", recs[0].Limits.CPU.String(), "a limit above the request is kept")
	assert.Equal(t, "400Mi", recs[0].Requests.Memory.String(), "memory has no target: per pod")
	assert.Equal(t, []string{"cpu"}, s.AlignedResources)
	assert.Equal(t, &autoscalingv1alpha1.ReplicaRange{Min: 2, Typical: 4, Max: 8, Source: autoscalingv1alpha1.ReplicaSourceHistory}, s.Replicas)

	c := appScaling(t, s)
	require.NotNil(t, c.CPU)
	assert.Equal(t, autoscalingv1alpha1.ScalingSizingHPAUtilization, c.CPU.Sizing)
	assert.Equal(t, "300m", c.CPU.PerPodRequest.String())
	assert.Equal(t, "500m", c.CPU.Request.String())
	assert.Equal(t, "350m", c.CPU.PerPodUsage.String())
	assert.Equal(t, int32(70), *c.CPU.TargetUtilization)
	assert.Contains(t, c.CPU.Reason, "at 4 replicas")
	assert.Equal(t, autoscalingv1alpha1.ScalingSizingPerPod, c.Memory.Sizing)
	assert.Contains(t, s.Reason, "Requests for cpu sized for the Utilization target of HorizontalPodAutoscaler web at 4 replicas (2-8)")
	assert.Equal(t, []string{"HorizontalPodAutoscaler/web"}, s.Autoscalers)
}

func TestApply_HPAAlignedRequestRaisesLimit(t *testing.T) {
	recs := perPod()
	Apply(recs, Input{HPAs: []HPA{cpuHPA(50)}, Signals: steadySignals()})
	assert.Equal(t, "700m", recs[0].Requests.CPU.String())
	assert.Equal(t, "700m", recs[0].Limits.CPU.String(), "limits never end below requests")
}

func TestApply_HPACPUAndMemoryTargets(t *testing.T) {
	h := cpuHPA(70)
	h.Targets = append(h.Targets, autoscalingv1alpha1.UtilizationTarget{Resource: "memory", AverageUtilization: 80})
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{h}, Signals: steadySignals()})
	assert.Equal(t, "500m", recs[0].Requests.CPU.String())
	assert.Equal(t, "500Mi", recs[0].Requests.Memory.String(), "400Mi per pod / 80%")
	assert.Equal(t, []string{"cpu", "memory"}, s.AlignedResources)
}

func TestApply_ContainerTargetBeatsPodTarget(t *testing.T) {
	h := cpuHPA(70)
	h.Targets = append(h.Targets, autoscalingv1alpha1.UtilizationTarget{Resource: "cpu", Container: "app", AverageUtilization: 50})
	h.Targets = append(h.Targets, autoscalingv1alpha1.UtilizationTarget{Resource: "cpu", Container: "other", AverageUtilization: 10})
	recs := perPod()
	Apply(recs, Input{HPAs: []HPA{h}, Signals: steadySignals()})
	assert.Equal(t, "700m", recs[0].Requests.CPU.String(), "the container target (50%) applies, not the pod one or another container's")
}

func TestApply_HPAExternalMetricsOnly(t *testing.T) {
	h := HPA{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "web", MaxReplicas: i32(10), OtherMetrics: []string{"External queue_depth"}}
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{h}, Signals: steadySignals()})
	assert.Equal(t, "300m", recs[0].Requests.CPU.String())
	assert.Empty(t, s.AlignedResources)
	c := appScaling(t, s)
	assert.Equal(t, autoscalingv1alpha1.ScalingSizingPerPod, c.CPU.Sizing)
	assert.Contains(t, c.CPU.Reason, "External queue_depth")
	assert.Contains(t, s.Reason, "none needed an adjustment")
}

func TestApply_KEDAMixedTriggers(t *testing.T) {
	so := HPA{
		Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "web", MinReplicas: i32(1), MaxReplicas: i32(20),
		Targets:      []autoscalingv1alpha1.UtilizationTarget{{Resource: "cpu", AverageUtilization: 70}},
		OtherMetrics: []string{"cpu AverageValue", "rabbitmq"},
	}
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{so}, Signals: steadySignals()})
	assert.Equal(t, "500m", recs[0].Requests.CPU.String(), "the cpu Utilization trigger is aligned")
	assert.Contains(t, appScaling(t, s).CPU.Reason, "rabbitmq", "other triggers also drive the replica count")
	assert.Equal(t, []string{"ScaledObject/web"}, s.Autoscalers)
}

func TestApply_KEDAManagedHPANamesBoth(t *testing.T) {
	h := cpuHPA(70)
	h.ManagedBy = "web-so"
	s := Apply(perPod(), Input{HPAs: []HPA{h}, Signals: steadySignals()})
	assert.Equal(t, []string{"HorizontalPodAutoscaler/web", "ScaledObject/web-so"}, s.Autoscalers)
}

func TestApply_HPAMinEqualsMax(t *testing.T) {
	h := cpuHPA(70)
	h.MinReplicas, h.MaxReplicas = i32(3), i32(3)
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{h}, Signals: steadySignals()})
	assert.Equal(t, "300m", recs[0].Requests.CPU.String())
	assert.Equal(t, &autoscalingv1alpha1.ReplicaRange{Min: 3, Typical: 3, Max: 3, Source: autoscalingv1alpha1.ReplicaSourceFixed}, s.Replicas)
	assert.Contains(t, appScaling(t, s).CPU.Reason, "replicas are fixed")
}

func TestApply_MissingReplicaHistory(t *testing.T) {
	sig := steadySignals()
	sig.Replicas = prometheus.ReplicaHistory{}

	t.Run("current count is used", func(t *testing.T) {
		recs := perPod()
		s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70)}, Signals: sig, CurrentReplicas: i32(7)})
		assert.Equal(t, autoscalingv1alpha1.ReplicaSourceCurrent, s.Replicas.Source)
		assert.Equal(t, int32(7), s.Replicas.Typical)
		assert.Equal(t, "286m", recs[0].Requests.CPU.String(), "1.4 cores / 7 / 70%")
	})
	t.Run("nothing known: per pod", func(t *testing.T) {
		recs := perPod()
		s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70)}, Signals: sig})
		assert.Nil(t, s.Replicas)
		assert.Equal(t, "300m", recs[0].Requests.CPU.String())
		assert.Contains(t, appScaling(t, s).CPU.Reason, "no replica history or count")
	})
	t.Run("no signals at all: per pod", func(t *testing.T) {
		recs := perPod()
		s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70)}, CurrentReplicas: i32(4)})
		assert.Equal(t, "300m", recs[0].Requests.CPU.String())
		assert.Contains(t, appScaling(t, s).CPU.Reason, "no cpu usage is known")
	})
}

func TestApply_TotalMissingFallsBackToBusiestPod(t *testing.T) {
	sig := steadySignals()
	u := sig.Containers["app"]
	u.CPU.TotalMedian = nil
	sig.Containers["app"] = u
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{cpuHPA(80)}, Signals: sig})
	assert.Equal(t, "500m", recs[0].Requests.CPU.String(), "busiest pod P50 400m / 80%")
	assert.Contains(t, appScaling(t, s).CPU.Reason, "busiest pod")
}

func TestApply_ReplicaRangeWithinHPABounds(t *testing.T) {
	sig := steadySignals()
	sig.Replicas = prometheus.ReplicaHistory{Min: f(0), Median: f(1e9), Max: f(2e9)}
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70)}, Signals: sig})
	assert.Equal(t, &autoscalingv1alpha1.ReplicaRange{Min: 2, Typical: 10, Max: 10, Source: autoscalingv1alpha1.ReplicaSourceHistory}, s.Replicas)
	assert.Equal(t, "200m", recs[0].Requests.CPU.String(), "1.4 cores / 10 / 70%")
}

func TestApply_ScaledToZeroTypically(t *testing.T) {
	so := HPA{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "web", MinReplicas: i32(0), MaxReplicas: i32(5),
		Targets: []autoscalingv1alpha1.UtilizationTarget{{Resource: "cpu", AverageUtilization: 70}}}
	sig := steadySignals()
	sig.Replicas = prometheus.ReplicaHistory{Min: f(0), Median: f(0), Max: f(3)}
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{so}, Signals: sig})
	assert.Equal(t, "300m", recs[0].Requests.CPU.String())
	assert.Contains(t, appScaling(t, s).CPU.Reason, "scaled to zero")
}

func vpa(mode string, policies ...VPAPolicy) VPA {
	return VPA{Name: "web", UpdateMode: mode, Policies: policies}
}

func TestApply_VPAModes(t *testing.T) {
	tests := []struct {
		mode        string
		wantRequest string
		wantSizing  autoscalingv1alpha1.ScalingSizing
		wantWrite   bool
	}{
		{"Off", "300m", autoscalingv1alpha1.ScalingSizingPerPod, false},
		{"Initial", "300m", autoscalingv1alpha1.ScalingSizingPerPod, true},
		{"Auto", "120m", autoscalingv1alpha1.ScalingSizingVPABaseline, true},
		{"Recreate", "120m", autoscalingv1alpha1.ScalingSizingVPABaseline, true},
		{"InPlaceOrRecreate", "120m", autoscalingv1alpha1.ScalingSizingVPABaseline, true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			recs := perPod()
			s := Apply(recs, Input{VPAs: []VPA{vpa(tt.mode)}, Signals: spikySignals(), Metrics: metrics("600m", "410Mi")})
			assert.Equal(t, tt.wantRequest, recs[0].Requests.CPU.String())
			assert.Equal(t, "400Mi", recs[0].Requests.Memory.String(), "memory is steady: per pod")
			c := appScaling(t, s)
			assert.Equal(t, tt.wantSizing, c.CPU.Sizing)
			assert.True(t, c.CPU.Spiky)
			assert.Equal(t, "6.00", c.CPU.PeakToBaseline)
			assert.False(t, c.Memory.Spiky)

			require.Len(t, s.VPA, 1)
			assert.Equal(t, tt.wantWrite, s.VPA[0].WriteBack)
			require.Len(t, s.VPA[0].Containers, 1)
			b := s.VPA[0].Containers[0]
			assert.Equal(t, "120m", b.MinAllowed.CPU.String(), "baseline P50 100m + 20%")
			assert.Equal(t, "1800m", b.MaxAllowed.CPU.String(), "peak 1.5 cores + 20%")
			assert.Nil(t, b.MinAllowed.Memory, "steady memory gets no bounds")
		})
	}
}

func TestApply_VPAExistingBoundsReported(t *testing.T) {
	recs := perPod()
	s := Apply(recs, Input{
		VPAs: []VPA{vpa("Auto", VPAPolicy{
			ContainerName: "app",
			MinAllowed:    &autoscalingv1alpha1.ResourceValues{CPU: q("50m")},
			MaxAllowed:    &autoscalingv1alpha1.ResourceValues{CPU: q("4")},
		})},
		Signals: spikySignals(), Metrics: metrics("600m", "410Mi"),
	})
	require.Len(t, s.VPA, 1)
	b := s.VPA[0].Containers[0]
	assert.Equal(t, "50m", b.CurrentMinAllowed.CPU.String())
	assert.Equal(t, "4", b.CurrentMaxAllowed.CPU.String())
	assert.Equal(t, "1800m", b.MaxAllowed.CPU.String(), "the recommendation is reported as computed; write-back keeps the wider maxAllowed")
}

func TestApply_VPASteady(t *testing.T) {
	recs := perPod()
	s := Apply(recs, Input{VPAs: []VPA{vpa("Auto")}, Signals: steadySignals(), Metrics: metrics("450m", "480Mi")})
	assert.Equal(t, "300m", recs[0].Requests.CPU.String())
	assert.Empty(t, s.VPA)
	assert.Contains(t, appScaling(t, s).CPU.Reason, "steady")
}

func TestApply_VPAPolicyOffOrNotControlled(t *testing.T) {
	for name, p := range map[string]VPAPolicy{
		"container mode Off":         {ContainerName: "app", Mode: "Off"},
		"cpu not controlled":         {ContainerName: "*", ControlledResources: []string{"memory"}},
		"named policy beats a * one": {ContainerName: "app", ControlledResources: []string{"memory"}},
	} {
		t.Run(name, func(t *testing.T) {
			recs := perPod()
			policies := []VPAPolicy{p}
			if p.ContainerName == "app" && p.Mode == "" {
				policies = append(policies, VPAPolicy{ContainerName: "*"})
			}
			s := Apply(recs, Input{VPAs: []VPA{vpa("Auto", policies...)}, Signals: spikySignals(), Metrics: metrics("600m", "410Mi")})
			assert.Equal(t, "300m", recs[0].Requests.CPU.String())
			assert.Empty(t, s.VPA)
			assert.Contains(t, appScaling(t, s).CPU.Reason, "does not control cpu")
		})
	}
}

func TestApply_VPAAndHPAOnSameResource(t *testing.T) {
	for _, mode := range []string{"Auto", "Initial", "Recreate"} {
		t.Run(mode, func(t *testing.T) {
			recs := perPod()
			s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70)}, VPAs: []VPA{vpa(mode)}, Signals: spikySignals(), Metrics: metrics("600m", "410Mi")})
			assert.Equal(t, "300m", recs[0].Requests.CPU.String(), "not aligned, not lowered")
			require.Len(t, s.Conflicts, 1)
			assert.Equal(t, "cpu", s.Conflicts[0].Resource)
			assert.Equal(t, []string{"HorizontalPodAutoscaler/web", "VerticalPodAutoscaler/web"}, s.Conflicts[0].Autoscalers)
			assert.Empty(t, s.AlignedResources)
			assert.Empty(t, s.VPA, "no bounds for the conflicting resource")
			assert.True(t, strings.HasPrefix(s.Reason, "1 autoscaler conflict(s) flagged"), s.Reason)
		})
	}
}

func TestApply_VPAOffWithHPAIsNoConflict(t *testing.T) {
	recs := perPod()
	s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70)}, VPAs: []VPA{vpa("Off")}, Signals: steadySignals()})
	assert.Empty(t, s.Conflicts)
	assert.Equal(t, "500m", recs[0].Requests.CPU.String())
}

// The common split: HPA on cpu, VPA on memory only.
func TestApply_HPACPUWithVPAMemory(t *testing.T) {
	sig := spikySignals()
	u := sig.Containers["app"]
	u.Memory = prometheus.UsageStats{P50: f(100 * mi), Max: f(900 * mi), TotalMedian: f(300 * mi)}
	sig.Containers["app"] = u
	recs := perPod()
	s := Apply(recs, Input{
		HPAs:    []HPA{cpuHPA(50)},
		VPAs:    []VPA{vpa("Auto", VPAPolicy{ContainerName: "*", ControlledResources: []string{"memory"}})},
		Signals: sig, Metrics: metrics("600m", "700Mi"),
	})
	assert.Empty(t, s.Conflicts)
	assert.Equal(t, []string{"cpu"}, s.AlignedResources)
	assert.Equal(t, "200m", recs[0].Requests.CPU.String(), "0.3 cores / 3 / 50%")
	assert.Equal(t, "120Mi", recs[0].Requests.Memory.String(), "spiky memory lowered to the baseline")
	require.Len(t, s.VPA, 1)
	assert.Equal(t, "1080Mi", s.VPA[0].Containers[0].MaxAllowed.Memory.String())
	assert.Nil(t, s.VPA[0].Containers[0].MaxAllowed.CPU)
}

func TestApply_SeveralAutoscalersOfAKind(t *testing.T) {
	recs := perPod()
	second := cpuHPA(50)
	second.Name = "web-2"
	s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70), second}, VPAs: []VPA{vpa("Auto"), {Name: "web-b", UpdateMode: "Auto"}}, Signals: spikySignals()})
	assert.Equal(t, "300m", recs[0].Requests.CPU.String())
	assert.Len(t, s.Conflicts, 2)
	assert.Empty(t, s.VPA)
}

func TestApply_DaemonSet(t *testing.T) {
	recs := perPod()
	s := Apply(recs, Input{
		WorkloadKind: "DaemonSet", HPAs: []HPA{cpuHPA(70)}, VPAs: []VPA{vpa("Auto")},
		Signals: spikySignals(), Metrics: metrics("600m", "410Mi"), CurrentReplicas: i32(12),
	})
	assert.Empty(t, s.AlignedResources, "an HPA cannot scale a DaemonSet")
	assert.Empty(t, s.Conflicts)
	assert.Equal(t, "120m", recs[0].Requests.CPU.String(), "the VPA still applies")
	assert.Contains(t, s.Reason, "cannot scale a DaemonSet")
}

func TestApply_OOMKilledMemoryNeverLowered(t *testing.T) {
	current := map[string]autoscalingv1alpha1.ResourceRequirements{
		"app": {Requests: &autoscalingv1alpha1.ResourceValues{Memory: q("1Gi")}},
	}
	h := cpuHPA(70)
	h.Targets = []autoscalingv1alpha1.UtilizationTarget{{Resource: "memory", AverageUtilization: 90}}

	t.Run("HPA", func(t *testing.T) {
		recs := perPod()
		recs[0].Requests.Memory = q("1Gi")
		Apply(recs, Input{HPAs: []HPA{h}, Signals: steadySignals(), Current: current, OOMKilled: []string{"app"}})
		assert.Equal(t, "1Gi", recs[0].Requests.Memory.String())
	})
	t.Run("VPA baseline", func(t *testing.T) {
		sig := spikySignals()
		u := sig.Containers["app"]
		u.Memory = prometheus.UsageStats{P50: f(100 * mi), Max: f(2000 * mi)}
		sig.Containers["app"] = u
		recs := perPod()
		recs[0].Requests.Memory = q("1Gi")
		s := Apply(recs, Input{VPAs: []VPA{vpa("Auto")}, Signals: sig, Metrics: metrics("600m", "1500Mi"), Current: current, OOMKilled: []string{"app"}})
		assert.Equal(t, "1Gi", recs[0].Requests.Memory.String())
		require.Len(t, s.VPA, 1)
		assert.Equal(t, "1Gi", s.VPA[0].Containers[0].MinAllowed.Memory.String())
	})
}

func TestApply_SafetyRails(t *testing.T) {
	recs := perPod()
	Apply(recs, Input{HPAs: []HPA{cpuHPA(10)}, Signals: steadySignals(), Rails: Rails{MaxCPU: q("1")}})
	assert.Equal(t, "1", recs[0].Requests.CPU.String(), "3.5 cores capped at the rail")

	recs = perPod()
	Apply(recs, Input{VPAs: []VPA{vpa("Auto")}, Signals: spikySignals(), Metrics: metrics("600m", "410Mi"), Rails: Rails{MinCPU: q("250m")}})
	assert.Equal(t, "250m", recs[0].Requests.CPU.String(), "baseline raised to the rail")
}

func TestApply_MissingRequestsAndContainers(t *testing.T) {
	recs := []strategy.Recommendation{
		{ContainerName: "app"},
		{ContainerName: "sidecar", Requests: strategy.ResourceValues{CPU: q("10m")}},
	}
	s := Apply(recs, Input{HPAs: []HPA{cpuHPA(70)}, VPAs: []VPA{vpa("Off")}, Signals: steadySignals()})
	assert.Nil(t, recs[0].Requests.CPU, "no request is invented")
	require.Len(t, s.Containers, 1)
	assert.Equal(t, "sidecar", s.Containers[0].Name)
	assert.Contains(t, s.Containers[0].CPU.Reason, "no cpu usage is known", "no signals for the sidecar")
}

// TestApply_Pathological feeds degenerate usage and replica series: the
// result must never hold negative values, and must not panic.
func TestApply_Pathological(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	series := map[string]prometheus.UsageStats{
		"flat":          {P50: f(0.2), Max: f(0.2), TotalMedian: f(0.6)},
		"single spike":  {P50: f(0.2), Max: f(50), TotalMedian: f(0.6)},
		"all zeros":     {P50: f(0), Max: f(0), TotalMedian: f(0)},
		"zero baseline": {P50: f(0), Max: f(2), TotalMedian: f(0)},
		"NaN":           {P50: &nan, Max: &nan, TotalMedian: &nan},
		"Inf":           {P50: &inf, Max: &inf, TotalMedian: &inf},
		"negative":      {P50: f(-1), Max: f(-5), TotalMedian: f(-3)},
		"huge":          {P50: f(1e300), Max: f(1e308), TotalMedian: f(1e308)},
		"missing":       {},
	}
	replicas := map[string]prometheus.ReplicaHistory{
		"none":     {},
		"NaN":      {Min: &nan, Median: &nan, Max: &nan},
		"absurd":   {Min: f(-5), Median: f(1e12), Max: f(inf)},
		"inverted": {Min: f(9), Median: f(5), Max: f(1)},
		"zero":     {Min: f(0), Median: f(0), Max: f(0)},
	}
	for sName, st := range series {
		for rName, rh := range replicas {
			t.Run(sName+"/"+rName, func(t *testing.T) {
				sig := &prometheus.ScalingSignals{Containers: map[string]prometheus.UsageSignals{"app": {CPU: st, Memory: st}}, Replicas: rh}
				run := func() ([]strategy.Recommendation, *autoscalingv1alpha1.ScalingSummary) {
					recs := perPod()
					h := cpuHPA(70)
					h.Targets = append(h.Targets, autoscalingv1alpha1.UtilizationTarget{Resource: "memory", AverageUtilization: 70})
					s := Apply(recs, Input{
						HPAs: []HPA{h}, VPAs: []VPA{vpa("Auto", VPAPolicy{ContainerName: "*", ControlledResources: []string{"memory"}})},
						Signals: sig, Metrics: metrics("300m", "300Mi"), CurrentReplicas: i32(3),
					})
					return recs, s
				}
				recs, s := run()
				assertSane(t, recs, s)
				recs2, s2 := run()
				assert.Equal(t, mustJSON(t, s), mustJSON(t, s2), "stable output")
				assert.Equal(t, recs[0].Requests.CPU.String(), recs2[0].Requests.CPU.String())
			})
		}
	}
}

// TestApply_Random checks random inputs for panics and invalid values.
func TestApply_Random(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pick := func() *float64 {
		switch rng.Intn(6) {
		case 0:
			return nil
		case 1:
			v := math.NaN()
			return &v
		case 2:
			return f(0)
		case 3:
			return f(-rng.Float64())
		default:
			return f(rng.ExpFloat64() * math.Pow(10, float64(rng.Intn(12))))
		}
	}
	modes := []string{"Off", "Initial", "Auto", "Recreate", "", "Weird"}
	for i := 0; i < 2000; i++ {
		stats := func() prometheus.UsageStats {
			return prometheus.UsageStats{P50: pick(), Max: pick(), TotalMedian: pick()}
		}
		sig := &prometheus.ScalingSignals{
			Containers: map[string]prometheus.UsageSignals{"app": {CPU: stats(), Memory: stats()}},
			Replicas:   prometheus.ReplicaHistory{Min: pick(), Median: pick(), Max: pick()},
		}
		h := HPA{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "web", MinReplicas: i32(int32(rng.Intn(5)) - 1), MaxReplicas: i32(int32(rng.Intn(1000)) - 1),
			Targets: []autoscalingv1alpha1.UtilizationTarget{{Resource: "cpu", AverageUtilization: int32(rng.Intn(300)) - 10}}}
		var hpas []HPA
		if rng.Intn(2) == 0 {
			hpas = []HPA{h}
		}
		var vpas []VPA
		if rng.Intn(2) == 0 {
			vpas = []VPA{vpa(modes[rng.Intn(len(modes))])}
		}
		var current *int32
		if rng.Intn(2) == 0 {
			current = i32(int32(rng.Intn(1 << 30)))
		}
		recs := perPod()
		s := Apply(recs, Input{HPAs: hpas, VPAs: vpas, Signals: sig, CurrentReplicas: current, Metrics: metrics("200m", "300Mi"), SpikeRatio: rng.Float64() * 4})
		assertSane(t, recs, s)
	}
}

func assertSane(t *testing.T, recs []strategy.Recommendation, s *autoscalingv1alpha1.ScalingSummary) {
	t.Helper()
	require.NotNil(t, s)
	assert.NotEmpty(t, s.Reason)
	nonNegative := func(what string, q *resource.Quantity) {
		if q != nil {
			assert.GreaterOrEqual(t, q.Sign(), 0, what)
			v := q.AsApproximateFloat64()
			assert.False(t, math.IsNaN(v) || math.IsInf(v, 0), what)
		}
	}
	for _, r := range recs {
		nonNegative("cpu request", r.Requests.CPU)
		nonNegative("memory request", r.Requests.Memory)
		if r.Requests.CPU != nil && r.Limits.CPU != nil {
			assert.GreaterOrEqual(t, r.Limits.CPU.Cmp(*r.Requests.CPU), 0, "cpu limit >= request")
		}
		if r.Requests.Memory != nil && r.Limits.Memory != nil {
			assert.GreaterOrEqual(t, r.Limits.Memory.Cmp(*r.Requests.Memory), 0, "memory limit >= request")
		}
	}
	if s.Replicas != nil {
		assert.GreaterOrEqual(t, s.Replicas.Min, int32(0))
		assert.LessOrEqual(t, s.Replicas.Min, s.Replicas.Typical)
		assert.LessOrEqual(t, s.Replicas.Typical, s.Replicas.Max)
	}
	for _, c := range s.Containers {
		for _, rs := range []*autoscalingv1alpha1.ResourceScaling{c.CPU, c.Memory} {
			if rs == nil {
				continue
			}
			nonNegative("request", rs.Request)
			nonNegative("per-pod usage", rs.PerPodUsage)
			assert.NotContains(t, rs.PeakToBaseline, "NaN")
			assert.NotContains(t, rs.PeakToBaseline, "Inf")
		}
	}
	for _, v := range s.VPA {
		for _, b := range v.Containers {
			for _, res := range []string{"cpu", "memory"} {
				lo, hi := pickValues(b.MinAllowed, res), pickValues(b.MaxAllowed, res)
				nonNegative("minAllowed", lo)
				nonNegative("maxAllowed", hi)
				if lo != nil && hi != nil {
					assert.LessOrEqual(t, lo.Cmp(*hi), 0, "minAllowed <= maxAllowed")
				}
			}
		}
	}
	_ = mustJSON(t, s)
}

func pickValues(v *autoscalingv1alpha1.ResourceValues, res string) *resource.Quantity {
	if v == nil {
		return nil
	}
	if res == "cpu" {
		return v.CPU
	}
	return v.Memory
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

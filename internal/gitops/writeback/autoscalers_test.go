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

package writeback

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider/fake"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

const vpaForAPI = `---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: api
spec:
  targetRef:
    kind: Deployment
    name: api
  updatePolicy:
    updateMode: Auto
  resourcePolicy:
    containerPolicies:
      - containerName: api
        maxAllowed:
          cpu: 200m
`

func TestChangedRequests(t *testing.T) {
	current := map[string]*writer.ResourceValues{"api": {CPU: q("100m"), Memory: q("128Mi")}}
	assert.Equal(t, []string{"cpu"}, ChangedRequests(cpuUpdate("150m"), current))
	assert.Empty(t, ChangedRequests(cpuUpdate("0.1"), current), "equal quantities are not a change")
	both := []writer.ResourceUpdate{{ContainerName: "api", Requests: &writer.ResourceValues{CPU: q("150m"), Memory: q("256Mi")}}}
	assert.Equal(t, []string{"cpu", "memory"}, ChangedRequests(both, current))
	limitsOnly := []writer.ResourceUpdate{{ContainerName: "api", Limits: &writer.ResourceValues{CPU: q("1")}}}
	assert.Empty(t, ChangedRequests(limitsOnly, current))
}

func TestWithoutResources(t *testing.T) {
	assert.Equal(t, []string{"memory"}, WithoutResources([]string{"cpu", "memory"}, []string{"cpu"}))
	assert.Empty(t, WithoutResources([]string{"cpu"}, []string{"cpu", "memory"}))
	assert.Equal(t, []string{"cpu"}, WithoutResources([]string{"cpu"}, nil))
}

func TestShiftedHPAs(t *testing.T) {
	detected := []autoscalingv1alpha1.DetectedAutoscaler{
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "cpu-hpa", UtilizationResources: []string{"cpu"}},
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "mem-hpa", UtilizationResources: []string{"memory"}},
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "custom-hpa"},
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "keda-hpa-api", UtilizationResources: []string{"cpu"}, ManagedBy: "api"},
	}
	assert.Equal(t, map[string][]string{"cpu-hpa": {"cpu"}, "keda-hpa-api": {"cpu"}}, ShiftedHPAs(detected, []string{"cpu"}, nil))
	assert.Equal(t, map[string][]string{"cpu-hpa": {"cpu"}}, ShiftedHPAs(detected, []string{"cpu"}, map[string]bool{"api": true}),
		"an HPA whose ScaledObject was rescaled keeps its scaling point")
}

// TestApplyReportsAutoscalers covers HPA and VPA on the same workload: the VPA
// in the file is adjusted, the HPA's shifted scaling point is flagged.
func TestApplyReportsAutoscalers(t *testing.T) {
	p := fake.New("main", map[string]string{"deploy/api.yaml": deployment + vpaForAPI})
	req := baseRequest(autoscalingv1alpha1.WriteBackTypeRaw, "deploy/api.yaml", "")
	req.Updates = cpuUpdate("300m")
	req.Current = map[string]*writer.ResourceValues{"api": {CPU: q("100m")}}
	req.Autoscalers = []autoscalingv1alpha1.DetectedAutoscaler{
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "api", UtilizationResources: []string{"cpu"}},
		{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "api", UpdateMode: "Auto"},
	}

	res, err := Apply(context.Background(), p, req)
	require.NoError(t, err)
	require.Equal(t, OutcomeCreated, res.Outcome)
	require.Len(t, res.Autoscalers, 1)
	assert.True(t, res.Autoscalers[0].Adjusted)

	file, _ := p.File(req.Branch, "deploy/api.yaml")
	assert.Contains(t, file, "cpu: 300m\n", "VPA maxAllowed widened to the new request")
	body := p.Body(res.PR.Number)
	assert.Contains(t, body, "### Autoscalers")
	assert.Contains(t, body, "VerticalPodAutoscaler `api` (in `deploy/api.yaml`): adjusted: maxAllowed.cpu 200m -> 300m (container api).")
	assert.Contains(t, body, "**HorizontalPodAutoscaler `api`: scaling point shifts.** It targets cpu utilization")
	assert.NotContains(t, body, "read from pods", "only metrics-only mode reads values from pods")
}

func TestAutoscalingSection(t *testing.T) {
	tests := []struct {
		name     string
		req      Request
		rendered rendered
		want     []string
		notWant  []string
	}{
		{
			name: "nothing to report",
			req:  Request{Updates: cpuUpdate("300m")},
		},
		{
			name: "detected but not in the file",
			req: Request{Path: "values.yaml", Updates: cpuUpdate("300m"), Autoscalers: []autoscalingv1alpha1.DetectedAutoscaler{
				{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "api", UpdateMode: "Off"},
				{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "api"},
			}},
			want: []string{
				"VerticalPodAutoscaler `api`: detected but not adjusted",
				"ScaledObject `api`: detected but not adjusted",
			},
		},
		{
			name: "KEDA-managed HPA kept by the adjusted ScaledObject",
			req: Request{Path: "a.yaml", Updates: cpuUpdate("300m"), Autoscalers: []autoscalingv1alpha1.DetectedAutoscaler{
				{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "keda-hpa-api", UtilizationResources: []string{"cpu"}, ManagedBy: "api"},
				{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "api"},
			}},
			rendered: rendered{autoscalers: []writer.AutoscalerNote{
				{Kind: writer.KindScaledObject, Name: "api", Adjusted: true, KeepsScalingPoint: true, Details: []string{"trigger 0 (cpu): Utilization 60% -> 20%"}},
			}},
			want:    []string{"ScaledObject `api` (in `a.yaml`): adjusted: trigger 0 (cpu): Utilization 60% -> 20%."},
			notWant: []string{"scaling point shifts", "detected but not adjusted"},
		},
		{
			name: "metrics-only: VPA-set pod values and skipped sidecars",
			req: Request{Path: "a.yaml", LiveFromPods: true, Updates: cpuUpdate("300m"), Autoscalers: []autoscalingv1alpha1.DetectedAutoscaler{
				{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "api", UpdateMode: "Initial"},
			}},
			rendered: rendered{
				autoscalers: []writer.AutoscalerNote{{Kind: writer.KindVerticalPodAutoscaler, Name: "api", Details: []string{"bounds already contain the new requests"}}},
				skipped:     []string{"injected-proxy"},
			},
			want: []string{
				"VerticalPodAutoscaler `api` has updateMode Initial: the current values were read from pods",
				"### Skipped containers",
				"`injected-proxy`: runs in the pods but is not in `a.yaml`",
			},
		},
		{
			name: "requests sized for the HPA Utilization target",
			req: Request{Path: "a.yaml", Updates: cpuUpdate("300m"), Current: map[string]*writer.ResourceValues{"api": {CPU: q("100m")}},
				AlignedResources: []string{"cpu"},
				Autoscalers: []autoscalingv1alpha1.DetectedAutoscaler{
					{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "api", UtilizationResources: []string{"cpu"}},
				}},
			want:    []string{"The cpu requests are sized so the Utilization target matches the typical per-pod usage"},
			notWant: []string{"scaling point shifts"},
		},
		{
			name: "recommended VPA bounds for a VPA outside the file",
			req: Request{Path: "values.yaml", Updates: cpuUpdate("300m"),
				VPABounds: map[string]map[string]writer.VPABounds{"api": {"api": {MaxAllowed: &writer.ResourceValues{CPU: q("1")}}}},
				Autoscalers: []autoscalingv1alpha1.DetectedAutoscaler{
					{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "api", UpdateMode: "Auto"},
				}},
			want: []string{"VerticalPodAutoscaler `api`: minAllowed/maxAllowed are recommended for its spiky usage (see the Tailoring status `scaling.vpa`), but it is not in `values.yaml`."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := autoscalingSection(tt.req, &tt.rendered)
			if len(tt.want) == 0 {
				assert.Empty(t, got)
			}
			for _, w := range tt.want {
				assert.Contains(t, got, w)
			}
			for _, w := range tt.notWant {
				assert.NotContains(t, got, w)
			}
		})
	}
}

func TestApplyLiveFromPodsSkipsMissingContainers(t *testing.T) {
	p := fake.New("main", map[string]string{"deploy/api.yaml": deployment})
	req := baseRequest(autoscalingv1alpha1.WriteBackTypeRaw, "deploy/api.yaml", "")
	req.Updates = append(cpuUpdate("300m"), writer.ResourceUpdate{ContainerName: "injected-proxy", Requests: &writer.ResourceValues{CPU: q("50m")}})

	_, err := Apply(context.Background(), p, req)
	require.Error(t, err, "Kubernetes mode keeps failing on containers missing from Git")

	req.LiveFromPods = true
	res, err := Apply(context.Background(), p, req)
	require.NoError(t, err)
	assert.Equal(t, []string{"injected-proxy"}, res.Skipped)
	assert.Contains(t, p.Body(res.PR.Number), "`injected-proxy`: runs in the pods")
}

// TestApplyLiveFromPodsHelmNeedsOneContainer: Helm write-back has one values
// path, so with pod-level values (which may include sidecars) more than one
// container fails closed instead of writing a sidecar's values.
func TestApplyLiveFromPodsHelmNeedsOneContainer(t *testing.T) {
	p := fake.New("main", map[string]string{"values.yaml": helmValues})
	req := baseRequest(autoscalingv1alpha1.WriteBackTypeHelm, "values.yaml", "api.resources")
	req.LiveFromPods = true
	req.Updates = append([]writer.ResourceUpdate{{ContainerName: "envoy", Requests: &writer.ResourceValues{CPU: q("50m")}}}, cpuUpdate("300m")...)

	_, err := Apply(context.Background(), p, req)
	require.ErrorContains(t, err, "spec.excludeContainers")
	assert.Zero(t, p.Commits())

	req.Updates = cpuUpdate("300m")
	res, err := Apply(context.Background(), p, req)
	require.NoError(t, err)
	assert.Equal(t, OutcomeCreated, res.Outcome)
}

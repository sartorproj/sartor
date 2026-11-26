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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

func q(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

func reqs(cpu, mem string) *autoscalingv1alpha1.ResourceValues {
	v := &autoscalingv1alpha1.ResourceValues{}
	if cpu != "" {
		v.CPU = q(cpu)
	}
	if mem != "" {
		v.Memory = q(mem)
	}
	return v
}

func str(v *resource.Quantity) string {
	if v == nil {
		return ""
	}
	return v.String()
}

func TestBuildPlan(t *testing.T) {
	type want struct {
		reqCPU, reqMem, limCPU, limMem string
	}
	tests := []struct {
		name      string
		live      autoscalingv1alpha1.ResourceRequirements
		rec       autoscalingv1alpha1.ResourceRequirements
		guard     Guard
		want      *want // nil = no update for the container
		wantNotes []string
	}{
		{
			name:  "within maxChangePercent is written as-is (rounded)",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "100Mi")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("120m", "110000000")},
			guard: Guard{MaxChangePercent: 50},
			want:  &want{reqCPU: "120m", reqMem: "105Mi"},
		},
		{
			name:      "increase capped at maxChangePercent",
			live:      autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "")},
			rec:       autoscalingv1alpha1.ResourceRequirements{Requests: reqs("1", "")},
			guard:     Guard{MaxChangePercent: 50},
			want:      &want{reqCPU: "150m"},
			wantNotes: []string{"`app`: requests.cpu: capped at +50% (maxChangePercent)"},
		},
		{
			name:      "decrease capped at maxChangePercent",
			live:      autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "1Gi")},
			rec:       autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "64Mi")},
			guard:     Guard{MaxChangePercent: 30},
			want:      &want{reqMem: "717Mi"},
			wantNotes: []string{"`app`: requests.memory: capped at -30% (maxChangePercent)"},
		},
		{
			name:      "rounding to Mi never overshoots the cap",
			live:      autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "1000000")},
			rec:       autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "10Mi")},
			guard:     Guard{MaxChangePercent: 50},
			want:      &want{reqMem: "1Mi"},
			wantNotes: []string{"`app`: requests.memory: capped at +50% (maxChangePercent)"},
		},
		{
			name:  "maxChangePercent >= 100 disables the cap",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("2", "")},
			guard: Guard{MaxChangePercent: 100},
			want:  &want{reqCPU: "2"},
		},
		{
			name:      "safety rail maximum wins over the recommendation",
			live:      autoscalingv1alpha1.ResourceRequirements{Requests: reqs("1", "")},
			rec:       autoscalingv1alpha1.ResourceRequirements{Requests: reqs("1400m", "")},
			guard:     Guard{MaxChangePercent: 50, MaxCPU: q("1200m")},
			want:      &want{reqCPU: "1200m"},
			wantNotes: []string{"`app`: requests.cpu: lowered to safety rail maximum 1200m"},
		},
		{
			name:      "safety rail minimum wins over the cap",
			live:      autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "100Mi")},
			rec:       autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "10Mi")},
			guard:     Guard{MaxChangePercent: 50, MinMemory: q("64Mi")},
			want:      &want{reqMem: "64Mi"},
			wantNotes: []string{"`app`: requests.memory: capped at -50% (maxChangePercent)", "`app`: requests.memory: raised to safety rail minimum 64Mi"},
		},
		{
			name:      "limits are never introduced",
			live:      autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "")},
			rec:       autoscalingv1alpha1.ResourceRequirements{Requests: reqs("110m", ""), Limits: reqs("500m", "1Gi")},
			guard:     Guard{MaxChangePercent: 50},
			want:      &want{reqCPU: "110m"},
			wantNotes: []string{"`app`: limits.cpu: not set on the workload, left unset", "`app`: limits.memory: not set on the workload, left unset"},
		},
		{
			name:      "request above the live limit raises the limit",
			live:      autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", ""), Limits: reqs("120m", "")},
			rec:       autoscalingv1alpha1.ResourceRequirements{Requests: reqs("140m", "")},
			guard:     Guard{MaxChangePercent: 50},
			want:      &want{reqCPU: "140m", limCPU: "140m"},
			wantNotes: []string{"`app`: limits.cpu: raised to match the request"},
		},
		{
			name:  "unchanged values produce no update",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "128Mi")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("99800u", "128Mi")},
			guard: Guard{MaxChangePercent: 50},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := BuildPlan(
				[]autoscalingv1alpha1.ContainerRecommendation{{Name: "app", Recommended: tt.rec}},
				map[string]autoscalingv1alpha1.ResourceRequirements{"app": tt.live},
				tt.guard,
			)
			assert.Equal(t, tt.wantNotes, plan.Notes)
			if tt.want == nil {
				assert.Empty(t, plan.Updates)
				return
			}
			require.Len(t, plan.Updates, 1)
			u := plan.Updates[0]
			got := want{}
			if u.Requests != nil {
				got.reqCPU, got.reqMem = str(u.Requests.CPU), str(u.Requests.Memory)
			}
			if u.Limits != nil {
				got.limCPU, got.limMem = str(u.Limits.CPU), str(u.Limits.Memory)
			}
			assert.Equal(t, *tt.want, got)
		})
	}
}

func TestBuildPlanSkipsContainersNotInWorkloadSpec(t *testing.T) {
	plan := BuildPlan(
		[]autoscalingv1alpha1.ContainerRecommendation{
			{Name: "istio-proxy", Recommended: autoscalingv1alpha1.ResourceRequirements{Requests: reqs("50m", "")}},
			{Name: "app", Recommended: autoscalingv1alpha1.ResourceRequirements{Requests: reqs("120m", "")}},
		},
		map[string]autoscalingv1alpha1.ResourceRequirements{"app": {Requests: reqs("100m", "")}},
		Guard{MaxChangePercent: 50},
	)
	require.Len(t, plan.Updates, 1)
	assert.Equal(t, "app", plan.Updates[0].ContainerName)
	assert.Equal(t, []string{"`istio-proxy`: skipped, not a container of the workload spec"}, plan.Notes)
}

func TestMaxRequestChangePercent(t *testing.T) {
	plan := BuildPlan(
		[]autoscalingv1alpha1.ContainerRecommendation{
			{Name: "a", Recommended: autoscalingv1alpha1.ResourceRequirements{Requests: reqs("110m", "")}},
			{Name: "b", Recommended: autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "75Mi")}},
		},
		map[string]autoscalingv1alpha1.ResourceRequirements{
			"a": {Requests: reqs("100m", "")},
			"b": {Requests: reqs("", "100Mi")},
		},
		Guard{MaxChangePercent: 100},
	)
	assert.InDelta(t, 25.0, plan.MaxRequestChangePercent(), 0.01)
}

// TestBuildPlanNeverExceedsMaxChange tries to push values past the cap with
// arbitrary live values, recommendations and percentages.
func TestBuildPlanNeverExceedsMaxChange(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		pct := float64(rng.Intn(99) + 1)
		liveCPU := resource.NewMilliQuantity(int64(rng.Intn(4000)+1), resource.DecimalSI)
		liveMem := resource.NewQuantity(int64(rng.Intn(4<<30)+1), resource.BinarySI)
		recCPU := resource.NewMilliQuantity(int64(rng.Intn(16000)), resource.DecimalSI)
		recMem := resource.NewQuantity(int64(rng.Intn(16<<30)), resource.BinarySI)

		plan := BuildPlan(
			[]autoscalingv1alpha1.ContainerRecommendation{{Name: "app", Recommended: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: recCPU, Memory: recMem},
			}}},
			map[string]autoscalingv1alpha1.ResourceRequirements{"app": {
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: liveCPU, Memory: liveMem},
			}},
			Guard{MaxChangePercent: pct},
		)
		for _, u := range plan.Updates {
			for _, pair := range [][2]*resource.Quantity{{liveCPU, u.Requests.CPU}, {liveMem, u.Requests.Memory}} {
				if pair[1] == nil {
					continue
				}
				change := changePercent(pair[0], pair[1])
				if change > pct+1e-9 {
					t.Fatalf("case %d: %s -> %s is %.4f%%, cap %.0f%%", i, pair[0], pair[1], change, pct)
				}
			}
		}
	}
}

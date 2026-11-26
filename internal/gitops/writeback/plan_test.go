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
		name  string
		live  autoscalingv1alpha1.ResourceRequirements
		rec   autoscalingv1alpha1.ResourceRequirements
		guard Guard
		want  *want // nil = no update for the container
	}{
		{
			name:  "within maxChangePercent is written as-is",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "100Mi")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("120m", "105Mi")},
			guard: Guard{MaxChangePercent: 50},
			want:  &want{reqCPU: "120m", reqMem: "105Mi"},
		},
		{
			name:  "increase capped at maxChangePercent",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("1", "")},
			guard: Guard{MaxChangePercent: 50},
			want:  &want{reqCPU: "150m"},
		},
		{
			name:  "decrease capped at maxChangePercent",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "1Gi")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "64Mi")},
			guard: Guard{MaxChangePercent: 30},
			want:  &want{reqMem: "751619277"},
		},
		{
			name:  "memory cap is computed in whole bytes",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "1000001")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "10Mi")},
			guard: Guard{MaxChangePercent: 50},
			want:  &want{reqMem: "1500001"},
		},
		{
			name:  "maxChangePercent >= 100 disables the cap",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("2", "")},
			guard: Guard{MaxChangePercent: 100},
			want:  &want{reqCPU: "2"},
		},
		{
			name:  "safety rail maximum wins over the recommendation",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("1", "")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("1400m", "")},
			guard: Guard{MaxChangePercent: 50, MaxCPU: q("1200m")},
			want:  &want{reqCPU: "1200m"},
		},
		{
			name:  "safety rail minimum wins over the cap",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "100Mi")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("", "10Mi")},
			guard: Guard{MaxChangePercent: 50, MinMemory: q("64Mi")},
			want:  &want{reqMem: "64Mi"},
		},
		{
			name:  "limits are never introduced",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", "")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("110m", ""), Limits: reqs("500m", "1Gi")},
			guard: Guard{MaxChangePercent: 50},
			want:  &want{reqCPU: "110m"},
		},
		{
			name:  "request above the live limit raises the limit",
			live:  autoscalingv1alpha1.ResourceRequirements{Requests: reqs("100m", ""), Limits: reqs("120m", "")},
			rec:   autoscalingv1alpha1.ResourceRequirements{Requests: reqs("140m", "")},
			guard: Guard{MaxChangePercent: 50},
			want:  &want{reqCPU: "140m", limCPU: "140m"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := BuildPlan(
				[]autoscalingv1alpha1.ContainerRecommendation{{Name: "app", Recommended: tt.rec}},
				map[string]autoscalingv1alpha1.ResourceRequirements{"app": tt.live},
				tt.guard,
			)
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
}

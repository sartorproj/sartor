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

// Package writeback turns Tailoring recommendations into GitOps pull requests.
package writeback

import (
	"math"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

// Guard bounds the values Sartor is allowed to write to Git. It is applied to
// the final values regardless of which strategy produced the recommendation.
type Guard struct {
	// MaxChangePercent caps the relative change of every value versus the live
	// workload. Zero, or values >= 100, disable the cap, matching the percentile strategy.
	MaxChangePercent float64
	MinCPU           *resource.Quantity
	MaxCPU           *resource.Quantity
	MinMemory        *resource.Quantity
	MaxMemory        *resource.Quantity
}

// Plan is the set of resource changes to write for one Tailoring.
type Plan struct {
	Updates []writer.ResourceUpdate
	// Current holds the live values of the containers in Updates, for PR descriptions.
	Current map[string]*writer.ResourceValues
}

// BuildPlan converts analysis results into guarded resource updates.
//
// Safety properties:
//   - only containers present in the live workload spec are written (injected
//     sidecars that only appear in metrics are skipped);
//   - limits are only resized, never introduced where the workload has none;
//   - every value is capped to MaxChangePercent of the live value, then clamped
//     to the safety rails;
//   - a request never ends above its limit.
func BuildPlan(
	recommendations []autoscalingv1alpha1.ContainerRecommendation,
	live map[string]autoscalingv1alpha1.ResourceRequirements,
	guard Guard,
) Plan {
	plan := Plan{Current: make(map[string]*writer.ResourceValues)}

	recs := append([]autoscalingv1alpha1.ContainerRecommendation(nil), recommendations...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })

	for _, rec := range recs {
		current, ok := live[rec.Name]
		if !ok {
			continue
		}

		requests := &writer.ResourceValues{}
		limits := &writer.ResourceValues{}
		cur := &writer.ResourceValues{CPU: reqCPU(current.Requests), Memory: reqMemory(current.Requests)}
		curLimits := &writer.ResourceValues{CPU: reqCPU(current.Limits), Memory: reqMemory(current.Limits)}

		requests.CPU = guard.value(reqCPU(rec.Recommended.Requests), cur.CPU, guard.MinCPU, guard.MaxCPU, cpuUnits)
		requests.Memory = guard.value(reqMemory(rec.Recommended.Requests), cur.Memory, guard.MinMemory, guard.MaxMemory, memoryUnits)
		if curLimits.CPU != nil {
			limits.CPU = guard.value(reqCPU(rec.Recommended.Limits), curLimits.CPU, guard.MinCPU, guard.MaxCPU, cpuUnits)
		}
		if curLimits.Memory != nil {
			limits.Memory = guard.value(reqMemory(rec.Recommended.Limits), curLimits.Memory, guard.MinMemory, guard.MaxMemory, memoryUnits)
		}

		keepRequestBelowLimit(requests.CPU, &limits.CPU, curLimits.CPU)
		keepRequestBelowLimit(requests.Memory, &limits.Memory, curLimits.Memory)

		update := writer.ResourceUpdate{ContainerName: rec.Name}
		if requests.CPU != nil || requests.Memory != nil {
			update.Requests = requests
		}
		if limits.CPU != nil || limits.Memory != nil {
			update.Limits = limits
		}
		if update.Requests == nil && update.Limits == nil {
			continue
		}
		plan.Updates = append(plan.Updates, update)
		plan.Current[rec.Name] = cur
	}
	return plan
}

// units converts between float values and the integer unit a resource is written in.
type units struct {
	perValue float64
	quantity func(n int64) *resource.Quantity
}

var (
	cpuUnits    = units{perValue: 1000, quantity: func(n int64) *resource.Quantity { return resource.NewMilliQuantity(n, resource.DecimalSI) }}
	memoryUnits = units{perValue: 1, quantity: func(n int64) *resource.Quantity { return resource.NewQuantity(n, resource.BinarySI) }}
)

// floatSlack absorbs float error such as 0.1*1.5 = 0.15000000000000002.
const floatSlack = 1e-6

func (u units) ceil(v float64) *resource.Quantity {
	return u.quantity(int64(math.Max(0, math.Ceil(v*u.perValue-floatSlack))))
}

func (u units) floor(v float64) *resource.Quantity {
	return u.quantity(int64(math.Max(0, math.Floor(v*u.perValue+floatSlack))))
}

func (g Guard) value(recommended, current, minRail, maxRail *resource.Quantity, u units) *resource.Quantity {
	if recommended == nil {
		return nil
	}
	q := u.ceil(recommended.AsApproximateFloat64())

	// Bounds are computed in whole units so that rounding can never exceed the cap.
	if current != nil && !current.IsZero() && g.MaxChangePercent > 0 && g.MaxChangePercent < 100 {
		cur := current.AsApproximateFloat64()
		if upper := u.floor(cur * (1 + g.MaxChangePercent/100)); q.Cmp(*upper) > 0 {
			q = upper
		}
		if lower := u.ceil(cur * (1 - g.MaxChangePercent/100)); q.Cmp(*lower) < 0 {
			q = lower
		}
	}

	if minRail != nil && q.Cmp(*minRail) < 0 {
		m := minRail.DeepCopy()
		q = &m
	}
	if maxRail != nil && q.Cmp(*maxRail) > 0 {
		m := maxRail.DeepCopy()
		q = &m
	}
	return q
}

// keepRequestBelowLimit raises the limit to the request if the request would exceed it.
func keepRequestBelowLimit(request *resource.Quantity, limit **resource.Quantity, liveLimit *resource.Quantity) {
	if request == nil {
		return
	}
	effective := *limit
	if effective == nil {
		effective = liveLimit
	}
	if effective == nil || request.Cmp(*effective) <= 0 {
		return
	}
	r := request.DeepCopy()
	*limit = &r
}

func reqCPU(v *autoscalingv1alpha1.ResourceValues) *resource.Quantity {
	if v == nil {
		return nil
	}
	return v.CPU
}

func reqMemory(v *autoscalingv1alpha1.ResourceValues) *resource.Quantity {
	if v == nil {
		return nil
	}
	return v.Memory
}

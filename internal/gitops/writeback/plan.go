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
	"fmt"
	"math"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

// DefaultMaxChangePercent caps a single write-back when the FitProfile sets no maxChangePercent.
const DefaultMaxChangePercent = 50.0

// Guard bounds the values Sartor is allowed to write to Git. It is applied to
// the final values regardless of which strategy produced the recommendation.
type Guard struct {
	// MaxChangePercent caps the relative change of every value versus the live
	// workload. Values >= 100 disable the cap, matching the percentile strategy.
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
	// Notes explains values that were capped, skipped or adjusted.
	Notes []string
}

// MaxRequestChangePercent returns the largest absolute relative change of a
// request in the plan, used for the minChangePercent threshold. Requests added
// where none existed count as 100%.
func (p Plan) MaxRequestChangePercent() float64 {
	maxChange := 0.0
	for _, u := range p.Updates {
		if u.Requests == nil {
			continue
		}
		cur := p.Current[u.ContainerName]
		for _, pair := range [][2]*resource.Quantity{
			{currentCPU(cur), u.Requests.CPU},
			{currentMemory(cur), u.Requests.Memory},
		} {
			if pair[1] == nil {
				continue
			}
			maxChange = math.Max(maxChange, changePercent(pair[0], pair[1]))
		}
	}
	return maxChange
}

func currentCPU(v *writer.ResourceValues) *resource.Quantity {
	if v == nil {
		return nil
	}
	return v.CPU
}

func currentMemory(v *writer.ResourceValues) *resource.Quantity {
	if v == nil {
		return nil
	}
	return v.Memory
}

func changePercent(current, recommended *resource.Quantity) float64 {
	if current == nil || current.IsZero() {
		return 100
	}
	cur := current.AsApproximateFloat64()
	return math.Abs(recommended.AsApproximateFloat64()-cur) / cur * 100
}

// BuildPlan converts analysis results into guarded resource updates.
//
// Safety properties:
//   - only containers present in the live workload spec are written (injected
//     sidecars that only appear in metrics are skipped);
//   - limits are only resized, never introduced where the workload has none;
//   - every value is capped to MaxChangePercent of the live value, then clamped
//     to the safety rails, then rounded up (CPU to 1m, memory to 1Mi);
//   - a request never ends above its limit;
//   - values equal to the live value are omitted.
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
			plan.Notes = append(plan.Notes, fmt.Sprintf("`%s`: skipped, not a container of the workload spec", rec.Name))
			continue
		}

		var notes []string
		requests := &writer.ResourceValues{}
		limits := &writer.ResourceValues{}
		cur := &writer.ResourceValues{CPU: reqCPU(current.Requests), Memory: reqMemory(current.Requests)}
		curLimits := &writer.ResourceValues{CPU: reqCPU(current.Limits), Memory: reqMemory(current.Limits)}

		requests.CPU = guard.value("requests.cpu", reqCPU(rec.Recommended.Requests), cur.CPU, guard.MinCPU, guard.MaxCPU, roundCPU, &notes)
		requests.Memory = guard.value("requests.memory", reqMemory(rec.Recommended.Requests), cur.Memory, guard.MinMemory, guard.MaxMemory, roundMemory, &notes)
		if curLimits.CPU != nil {
			limits.CPU = guard.value("limits.cpu", reqCPU(rec.Recommended.Limits), curLimits.CPU, guard.MinCPU, guard.MaxCPU, roundCPU, &notes)
		} else if reqCPU(rec.Recommended.Limits) != nil {
			notes = append(notes, "limits.cpu: not set on the workload, left unset")
		}
		if curLimits.Memory != nil {
			limits.Memory = guard.value("limits.memory", reqMemory(rec.Recommended.Limits), curLimits.Memory, guard.MinMemory, guard.MaxMemory, roundMemory, &notes)
		} else if reqMemory(rec.Recommended.Limits) != nil {
			notes = append(notes, "limits.memory: not set on the workload, left unset")
		}

		keepRequestBelowLimit("cpu", requests.CPU, &limits.CPU, curLimits.CPU, &notes)
		keepRequestBelowLimit("memory", requests.Memory, &limits.Memory, curLimits.Memory, &notes)

		requests.CPU = dropUnchanged(requests.CPU, cur.CPU)
		requests.Memory = dropUnchanged(requests.Memory, cur.Memory)
		limits.CPU = dropUnchanged(limits.CPU, curLimits.CPU)
		limits.Memory = dropUnchanged(limits.Memory, curLimits.Memory)

		for _, n := range notes {
			plan.Notes = append(plan.Notes, fmt.Sprintf("`%s`: %s", rec.Name, n))
		}

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

func (g Guard) value(
	field string,
	recommended, current, minRail, maxRail *resource.Quantity,
	round rounder,
	notes *[]string,
) *resource.Quantity {
	if recommended == nil {
		return nil
	}
	v := recommended.AsApproximateFloat64()
	format := recommended.Format

	var lower, upper *resource.Quantity
	if current != nil && !current.IsZero() && g.MaxChangePercent < 100 {
		cur := current.AsApproximateFloat64()
		upper = floorMilli(cur*(1+g.MaxChangePercent/100), format)
		lower = ceilMilli(cur*(1-g.MaxChangePercent/100), format)
		if v > upper.AsApproximateFloat64() {
			*notes = append(*notes, fmt.Sprintf("%s: capped at +%.0f%% (maxChangePercent)", field, g.MaxChangePercent))
		} else if v < lower.AsApproximateFloat64() {
			*notes = append(*notes, fmt.Sprintf("%s: capped at -%.0f%% (maxChangePercent)", field, g.MaxChangePercent))
		}
	}

	exact := ceilMilli(v, format)
	if upper != nil && exact.Cmp(*upper) > 0 {
		exact = upper
	}
	if lower != nil && exact.Cmp(*lower) < 0 {
		exact = lower
	}

	// Round to a readable unit without leaving the [lower, upper] window.
	q := round(exact, true)
	if upper != nil && q.Cmp(*upper) > 0 {
		q = round(exact, false)
	}
	if lower != nil && q.Cmp(*lower) < 0 {
		q = exact
	}

	if minRail != nil && q.Cmp(*minRail) < 0 {
		*notes = append(*notes, fmt.Sprintf("%s: raised to safety rail minimum %s", field, minRail.String()))
		m := minRail.DeepCopy()
		q = &m
	}
	if maxRail != nil && q.Cmp(*maxRail) > 0 {
		*notes = append(*notes, fmt.Sprintf("%s: lowered to safety rail maximum %s", field, maxRail.String()))
		m := maxRail.DeepCopy()
		q = &m
	}
	return q
}

// rounder rounds a quantity up or down to a readable unit.
type rounder func(q *resource.Quantity, up bool) *resource.Quantity

// floatSlack absorbs float error such as 0.1*1.5 = 0.15000000000000002.
const floatSlack = 1e-6

func ceilMilli(v float64, format resource.Format) *resource.Quantity {
	return resource.NewMilliQuantity(int64(math.Max(0, math.Ceil(v*1000-floatSlack))), format)
}

func floorMilli(v float64, format resource.Format) *resource.Quantity {
	return resource.NewMilliQuantity(int64(math.Max(0, math.Floor(v*1000+floatSlack))), format)
}

// roundCPU rounds to whole millicores and prints whole cores without a suffix.
func roundCPU(q *resource.Quantity, _ bool) *resource.Quantity {
	milli := q.MilliValue()
	if milli%1000 == 0 {
		return resource.NewQuantity(milli/1000, resource.DecimalSI)
	}
	return resource.NewMilliQuantity(milli, resource.DecimalSI)
}

// roundMemory rounds to whole mebibytes.
func roundMemory(q *resource.Quantity, up bool) *resource.Quantity {
	const mi = 1024 * 1024
	milli := q.MilliValue()
	mib := milli / (mi * 1000)
	if up && milli%(mi*1000) != 0 {
		mib++
	}
	return resource.NewQuantity(mib*mi, resource.BinarySI)
}

// keepRequestBelowLimit raises the limit to the request if the request would exceed it.
func keepRequestBelowLimit(name string, request *resource.Quantity, limit **resource.Quantity, liveLimit *resource.Quantity, notes *[]string) {
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
	*notes = append(*notes, fmt.Sprintf("limits.%s: raised to match the request", name))
}

func dropUnchanged(v, current *resource.Quantity) *resource.Quantity {
	if v == nil || (current != nil && v.Cmp(*current) == 0) {
		return nil
	}
	return v
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

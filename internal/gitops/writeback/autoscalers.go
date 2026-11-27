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
	"fmt"
	"slices"
	"strings"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

// ChangedRequests returns the resources ("cpu", "memory") whose request
// changes for at least one container.
func ChangedRequests(updates []writer.ResourceUpdate, current map[string]*writer.ResourceValues) []string {
	var out []string
	add := func(res string) {
		if !slices.Contains(out, res) {
			out = append(out, res)
		}
	}
	for _, u := range updates {
		if u.Requests == nil {
			continue
		}
		var cur *writer.ResourceValues
		if current != nil {
			cur = current[u.ContainerName]
		}
		if u.Requests.CPU != nil && (cur == nil || cur.CPU == nil || cur.CPU.Cmp(*u.Requests.CPU) != 0) {
			add("cpu")
		}
		if u.Requests.Memory != nil && (cur == nil || cur.Memory == nil || cur.Memory.Cmp(*u.Requests.Memory) != 0) {
			add("memory")
		}
	}
	slices.Sort(out)
	return out
}

// WithoutResources returns changed without the resources in drop.
func WithoutResources(changed, drop []string) []string {
	var out []string
	for _, r := range changed {
		if !slices.Contains(drop, r) {
			out = append(out, r)
		}
	}
	return out
}

// ShiftedHPAs returns the HPAs whose cpu/memory Utilization target is
// relative to a changed request, so the same percentage now means a different
// absolute usage. HPAs managed by a ScaledObject listed in keptBy are excluded.
func ShiftedHPAs(detected []autoscalingv1alpha1.DetectedAutoscaler, changed []string, keptBy map[string]bool) map[string][]string {
	out := map[string][]string{}
	for _, a := range detected {
		if a.Kind != autoscalingv1alpha1.AutoscalerKindHPA {
			continue
		}
		if a.ManagedBy != "" && keptBy[a.ManagedBy] {
			continue
		}
		var res []string
		for _, r := range a.UtilizationResources {
			if slices.Contains(changed, r) {
				res = append(res, r)
			}
		}
		if len(res) > 0 {
			out[a.Name] = res
		}
	}
	return out
}

// autoscalingSection renders the PR body section about autoscalers and
// skipped containers. It is empty when there is nothing to report.
func autoscalingSection(req Request, r *rendered) string {
	var lines []string
	inFile := map[string]bool{}
	keptBy := map[string]bool{}
	for _, n := range r.autoscalers {
		inFile[n.Kind+"/"+n.Name] = true
		state := "left unchanged"
		if n.Adjusted {
			state = "adjusted"
		}
		if n.Kind == writer.KindScaledObject && n.KeepsScalingPoint {
			keptBy[n.Name] = true
		}
		lines = append(lines, fmt.Sprintf("- %s `%s` (in `%s`): %s: %s.", n.Kind, n.Name, req.Path, state, strings.Join(n.Details, "; ")))
	}

	for _, a := range req.Autoscalers {
		switch a.Kind {
		case autoscalingv1alpha1.AutoscalerKindVPA, autoscalingv1alpha1.AutoscalerKindScaledObject:
			if !inFile[string(a.Kind)+"/"+a.Name] {
				lines = append(lines, fmt.Sprintf("- %s `%s`: detected but not adjusted: only a %s document in `%s` (raw write-back) is edited.",
					a.Kind, a.Name, a.Kind, req.Path))
			}
			if a.Kind == autoscalingv1alpha1.AutoscalerKindVPA && req.LiveFromPods && a.UpdateMode != "" && a.UpdateMode != "Off" {
				lines = append(lines, fmt.Sprintf("- %s `%s` has updateMode %s: the current values were read from pods and may already be set by the VPA.",
					a.Kind, a.Name, a.UpdateMode))
			}
		}
	}

	changed := ChangedRequests(req.Updates, req.Current)
	shifted := ShiftedHPAs(req.Autoscalers, WithoutResources(changed, req.AlignedResources), keptBy)
	for _, a := range req.Autoscalers {
		if res, ok := shifted[a.Name]; ok && a.Kind == autoscalingv1alpha1.AutoscalerKindHPA {
			lines = append(lines, fmt.Sprintf("- **HorizontalPodAutoscaler `%s`: scaling point shifts.** It targets %s utilization, which is relative to requests; with the new requests the same percentage means a different absolute usage. Review its targets.",
				a.Name, strings.Join(res, "/")))
		}
	}
	if len(req.AlignedResources) > 0 {
		lines = append(lines, fmt.Sprintf("- The %s requests are sized so the Utilization target matches the typical per-pod usage at the typical replica count; Utilization targets are kept as they are.",
			strings.Join(req.AlignedResources, "/")))
	}
	for _, name := range sortedKeys(req.VPABounds) {
		if !inFile[string(autoscalingv1alpha1.AutoscalerKindVPA)+"/"+name] {
			lines = append(lines, fmt.Sprintf("- VerticalPodAutoscaler `%s`: minAllowed/maxAllowed are recommended for its spiky usage (see the Tailoring status `scaling.vpa`), but it is not in `%s`.",
				name, req.Path))
		}
	}

	return renderAutoscalingSection(lines, r, req.Path)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func renderAutoscalingSection(lines []string, r *rendered, path string) string {
	var b strings.Builder
	if len(lines) > 0 {
		b.WriteString("\n\n### Autoscalers\n\n")
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n")
	}
	if len(r.skipped) > 0 {
		b.WriteString("\n\n### Skipped containers\n\n")
		for _, c := range r.skipped {
			fmt.Fprintf(&b, "- `%s`: runs in the pods but is not in `%s` (for example an injected sidecar); not changed.\n", c, path)
		}
	}
	return b.String()
}

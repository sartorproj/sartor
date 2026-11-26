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

package writer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Autoscaler kinds edited alongside the workload in raw manifests.
const (
	KindVerticalPodAutoscaler = "VerticalPodAutoscaler"
	KindScaledObject          = "ScaledObject"
)

const (
	resourceCPU    = "cpu"
	resourceMemory = "memory"
)

// AutoscalerNote reports what was done to an autoscaler found in the same
// manifest file as the workload.
type AutoscalerNote struct {
	Kind string
	Name string
	// Adjusted is true when the autoscaler document was changed.
	Adjusted bool
	// KeepsScalingPoint is true for a ScaledObject whose cpu/memory
	// Utilization triggers all follow the written requests (changed now or
	// already consistent), so its HPA's scaling point does not move.
	KeepsScalingPoint bool
	// Details lists the individual changes, or why nothing was changed.
	Details []string
}

// RawOptions configures RawWriter.Update.
type RawOptions struct {
	// SkipMissingContainers skips updates for containers that are not in the
	// manifest instead of failing (pod-level values may include injected sidecars).
	SkipMissingContainers bool
}

// RawResult is the result of RawWriter.Update.
type RawResult struct {
	Content []byte
	// Skipped lists containers that were not found in the manifest.
	Skipped []string
	// Autoscalers reports the VerticalPodAutoscaler and ScaledObject documents
	// in the file that target the workload.
	Autoscalers []AutoscalerNote
}

// Update writes the resource updates into the workload document matching
// target and keeps the autoscalers in the same file consistent with them:
//   - VerticalPodAutoscaler: minAllowed/maxAllowed are widened so that each new
//     request stays within them (updateMode Off is left untouched);
//   - KEDA ScaledObject: cpu/memory triggers with metricType Utilization are
//     rescaled so the absolute usage at which it scales stays the same.
//     AverageValue and non-cpu/memory triggers are left untouched.
func (w *RawWriter) Update(content []byte, target ManifestTarget, updates []ResourceUpdate, opts RawOptions) (*RawResult, error) {
	f, err := parseYAMLFile(content)
	if err != nil {
		return nil, err
	}
	containers, err := findTargetContainers(f, target)
	if err != nil {
		return nil, err
	}

	before := containerRequests(containers)
	res := &RawResult{}
	for _, update := range updates {
		if opts.SkipMissingContainers && !hasContainer(containers, update.ContainerName) {
			res.Skipped = append(res.Skipped, update.ContainerName)
			continue
		}
		if err := updateContainer(f, containers, update); err != nil {
			return nil, fmt.Errorf("failed to update container %s: %w", update.ContainerName, err)
		}
	}
	after := containerRequests(containers)

	for i := range f.docs {
		doc, err := f.rootMapping(i)
		if err != nil {
			continue
		}
		kind := scalarValue(findKey(doc, "kind"))
		name := scalarValue(findKey(findKey(doc, "metadata"), "name"))
		spec := findKey(doc, "spec")
		switch {
		case kind == KindVerticalPodAutoscaler && refMatches(findKey(spec, "targetRef"), target, ""):
			res.Autoscalers = append(res.Autoscalers, adjustVPA(f, spec, name, before, after))
		case kind == KindScaledObject && refMatches(findKey(spec, "scaleTargetRef"), target, "Deployment"):
			res.Autoscalers = append(res.Autoscalers, adjustScaledObject(f, spec, name, before, after))
		}
	}

	out, err := f.render()
	if err != nil {
		return nil, err
	}
	res.Content = out
	return res, nil
}

func hasContainer(containers *yaml.Node, name string) bool {
	for _, c := range containers.Content {
		if n := findKey(c, "name"); n != nil && n.Value == name {
			return true
		}
	}
	return false
}

func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// refMatches reports whether a targetRef/scaleTargetRef names the workload.
func refMatches(ref *yaml.Node, target ManifestTarget, defaultKind string) bool {
	if ref == nil || scalarValue(findKey(ref, "name")) != target.Name {
		return false
	}
	kind := scalarValue(findKey(ref, "kind"))
	if kind == "" {
		kind = defaultKind
	}
	return target.Kind == "" || kind == target.Kind
}

// containerRequests reads each container's requests from the manifest.
func containerRequests(containers *yaml.Node) map[string]*ResourceValues {
	out := map[string]*ResourceValues{}
	for _, c := range containers.Content {
		name := scalarValue(findKey(c, "name"))
		if name == "" {
			continue
		}
		requests := findKey(findKey(c, resourcesKey), "requests")
		out[name] = &ResourceValues{
			CPU:    parseQuantity(findKey(requests, resourceCPU)),
			Memory: parseQuantity(findKey(requests, resourceMemory)),
		}
	}
	return out
}

func parseQuantity(n *yaml.Node) *resource.Quantity {
	v := scalarValue(n)
	if v == "" {
		return nil
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return nil
	}
	return &q
}

func pick(v *ResourceValues, res string) *resource.Quantity {
	if v == nil {
		return nil
	}
	if res == resourceCPU {
		return v.CPU
	}
	return v.Memory
}

func requestChanged(before, after map[string]*ResourceValues, container, res string) bool {
	b, a := pick(before[container], res), pick(after[container], res)
	if b == nil || a == nil {
		return b != a
	}
	return b.Cmp(*a) != 0
}

// adjustVPA widens minAllowed/maxAllowed so every changed request stays in bounds.
func adjustVPA(f *yamlFile, spec *yaml.Node, name string, before, after map[string]*ResourceValues) AutoscalerNote {
	note := AutoscalerNote{Kind: KindVerticalPodAutoscaler, Name: name}
	mode := scalarValue(findKey(findKey(spec, "updatePolicy"), "updateMode"))
	if mode == "" {
		mode = "Auto"
	}
	if mode == "Off" {
		note.Details = append(note.Details, "updateMode is Off: the VPA only recommends, bounds left unchanged")
		return note
	}
	policies := findKey(findKey(spec, "resourcePolicy"), "containerPolicies")
	if policies == nil || policies.Kind != yaml.SequenceNode {
		note.Details = append(note.Details, fmt.Sprintf("updateMode %s: no containerPolicies, no bounds to adjust", mode))
		return note
	}

	named := map[string]bool{}
	for _, p := range policies.Content {
		if n := scalarValue(findKey(p, "containerName")); n != "" && n != "*" {
			named[n] = true
		}
	}
	for _, p := range policies.Content {
		if p.Kind != yaml.MappingNode || scalarValue(findKey(p, "mode")) == "Off" {
			continue
		}
		policyName := scalarValue(findKey(p, "containerName"))
		controlled := []string{resourceCPU, resourceMemory}
		if cr := findKey(p, "controlledResources"); cr != nil && cr.Kind == yaml.SequenceNode {
			controlled = nil
			for _, r := range cr.Content {
				controlled = append(controlled, r.Value)
			}
		}
		for _, container := range sortedNames(after) {
			if policyName != container && (policyName != "*" || named[container]) {
				continue
			}
			for _, res := range controlled {
				if res != resourceCPU && res != resourceMemory {
					continue
				}
				req := pick(after[container], res)
				if req == nil || !requestChanged(before, after, container, res) {
					continue
				}
				if d := widen(f, p, "minAllowed", res, req, -1); d != "" {
					note.Details = append(note.Details, fmt.Sprintf("%s (container %s)", d, container))
				}
				if d := widen(f, p, "maxAllowed", res, req, 1); d != "" {
					note.Details = append(note.Details, fmt.Sprintf("%s (container %s)", d, container))
				}
			}
		}
	}
	note.Adjusted = len(note.Details) > 0
	if !note.Adjusted {
		note.Details = append(note.Details, fmt.Sprintf("updateMode %s: bounds already contain the new requests", mode))
	}
	return note
}

// widen moves policy[bound][res] to req when req lies outside it (sign -1 for
// minAllowed, +1 for maxAllowed). It returns a description of the change.
func widen(f *yamlFile, policy *yaml.Node, bound, res string, req *resource.Quantity, sign int) string {
	b := findKey(policy, bound)
	cur := parseQuantity(findKey(b, res))
	if cur == nil || req.Cmp(*cur)*sign <= 0 {
		return ""
	}
	f.setScalar(b, res, req.String())
	return fmt.Sprintf("%s.%s %s -> %s", bound, res, cur.String(), req.String())
}

// adjustScaledObject rescales cpu/memory Utilization triggers so the absolute
// scaling point stays where it was.
func adjustScaledObject(f *yamlFile, spec *yaml.Node, name string, before, after map[string]*ResourceValues) AutoscalerNote {
	note := AutoscalerNote{Kind: KindScaledObject, Name: name}
	triggers := findKey(spec, "triggers")
	if triggers == nil || triggers.Kind != yaml.SequenceNode {
		note.Details = append(note.Details, "no triggers")
		return note
	}
	tracked, untracked := 0, 0
	for i, t := range triggers.Content {
		typ := scalarValue(findKey(t, "type"))
		if typ != resourceCPU && typ != resourceMemory {
			continue
		}
		meta := findKey(t, "metadata")
		metricType := scalarValue(findKey(t, "metricType"))
		if metricType == "" {
			metricType = scalarValue(findKey(meta, "type"))
		}
		label := fmt.Sprintf("trigger %d (%s)", i, typ)
		switch metricType {
		case "Utilization":
		case "AverageValue":
			note.Details = append(note.Details, label+": AverageValue is an absolute target, left unchanged")
			continue
		default:
			untracked++
			note.Details = append(note.Details, fmt.Sprintf("%s: metric type %q, left unchanged", label, metricType))
			continue
		}
		valueNode := findKey(meta, "value")
		value, err := strconv.ParseFloat(strings.TrimSpace(scalarValue(valueNode)), 64)
		if err != nil || value <= 0 {
			untracked++
			note.Details = append(note.Details, label+": no numeric value, left unchanged")
			continue
		}
		oldReq, newReq, why := utilizationBase(before, after, scalarValue(findKey(meta, "containerName")), typ)
		if why != "" {
			untracked++
			note.Details = append(note.Details, label+": "+why+", left unchanged")
			continue
		}
		tracked++
		if oldReq == newReq {
			continue
		}
		next := int64(math.Max(1, math.Round(value*oldReq/newReq)))
		newValue := strconv.FormatInt(next, 10)
		if newValue == scalarValue(valueNode) {
			continue
		}
		f.setScalar(meta, "value", newValue)
		note.Adjusted = true
		note.Details = append(note.Details, fmt.Sprintf("%s: Utilization %s%% -> %s%%", label, scalarValue(valueNode), newValue))
	}
	note.KeepsScalingPoint = tracked > 0 && untracked == 0
	if len(note.Details) == 0 {
		note.Details = append(note.Details, "no cpu/memory Utilization trigger depends on the changed requests")
	}
	return note
}

// utilizationBase returns the requests a Utilization target is relative to,
// before and after the update: the named container's, or the sum over all
// containers of the pod (as the HPA computes it).
func utilizationBase(before, after map[string]*ResourceValues, container, res string) (float64, float64, string) {
	names := []string{container}
	if container == "" {
		names = sortedNames(after)
	}
	var oldSum, newSum float64
	for _, n := range names {
		b, a := pick(before[n], res), pick(after[n], res)
		if b == nil || a == nil {
			return 0, 0, fmt.Sprintf("container %q has no %s request", n, res)
		}
		oldSum += b.AsApproximateFloat64()
		newSum += a.AsApproximateFloat64()
	}
	if oldSum <= 0 || newSum <= 0 {
		return 0, 0, fmt.Sprintf("zero %s request", res)
	}
	return oldSum, newSum, ""
}

func sortedNames(m map[string]*ResourceValues) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

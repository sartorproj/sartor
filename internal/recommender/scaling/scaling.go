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

// Package scaling adjusts per-pod recommendations to the autoscalers of a
// workload: requests are sized for HPA (and KEDA) cpu/memory Utilization
// targets at the typical replica count, and spiky workloads with a VPA get a
// baseline request and VPA bounds that cover the peaks.
package scaling

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

// Resource names.
const (
	ResourceCPU    = "cpu"
	ResourceMemory = "memory"
)

const (
	// DefaultSpikeRatio is the P99 to P50 usage ratio from which usage is spiky.
	DefaultSpikeRatio = 2.0
	// baselineBufferPercent is added to the P50 usage for a baseline request.
	baselineBufferPercent = 20
	// peakHeadroomPercent is added to the peak usage for VPA maxAllowed.
	peakHeadroomPercent = 20

	// maxCores and maxBytes bound computed values so quantities stay finite.
	maxCores = 1 << 20
	maxBytes = 1 << 50

	updateModeOff = "Off"
	kindDaemonSet = "DaemonSet"
)

// HPA is a horizontal autoscaler of the workload: an HPA, or a KEDA
// ScaledObject whose HPA was not detected.
type HPA struct {
	Kind autoscalingv1alpha1.AutoscalerKind
	Name string
	// ManagedBy is the ScaledObject owning the HPA, if any.
	ManagedBy string
	// MinReplicas defaults to 1 when nil; MaxReplicas is unbounded when nil.
	MinReplicas, MaxReplicas *int32
	Targets                  []autoscalingv1alpha1.UtilizationTarget
	OtherMetrics             []string
}

// VPAPolicy is a VPA container policy. ContainerName "*" is the default policy.
type VPAPolicy struct {
	ContainerName string
	// Mode "Off" disables the VPA for the container.
	Mode string
	// ControlledResources defaults to cpu and memory when empty.
	ControlledResources []string
	MinAllowed          *autoscalingv1alpha1.ResourceValues
	MaxAllowed          *autoscalingv1alpha1.ResourceValues
}

// VPA is a VerticalPodAutoscaler of the workload.
type VPA struct {
	Name       string
	UpdateMode string
	Policies   []VPAPolicy
}

// Rails are the safety rails applied to every adjusted value.
type Rails struct {
	MinCPU, MaxCPU, MinMemory, MaxMemory *resource.Quantity
}

// Input is what Apply works from.
type Input struct {
	WorkloadKind string
	Current      map[string]autoscalingv1alpha1.ResourceRequirements
	// Metrics holds the P95/P99 usage the per-pod recommendation used.
	Metrics []prometheus.ContainerMetrics
	// Signals is nil when the scaling signals could not be read.
	Signals         *prometheus.ScalingSignals
	CurrentReplicas *int32
	HPAs            []HPA
	VPAs            []VPA
	OOMKilled       []string
	Rails           Rails
	// SpikeRatio defaults to DefaultSpikeRatio.
	SpikeRatio float64
}

// applier carries the state of one Apply call.
type applier struct {
	in       Input
	hpa      *HPA
	vpa      *VPA
	replicas *autoscalingv1alpha1.ReplicaRange
	p99      map[string]prometheus.ContainerMetrics
	out      *autoscalingv1alpha1.ScalingSummary
	vpaRecs  map[string]*autoscalingv1alpha1.VPAContainerBounds
	notes    []string
	aligned  map[string]bool
	spiky    map[string]bool
}

// Apply adjusts recs in place to the workload's autoscalers and returns the
// summary for the Tailoring status. It never returns nil.
func Apply(recs []strategy.Recommendation, in Input) *autoscalingv1alpha1.ScalingSummary {
	if in.SpikeRatio <= 0 || math.IsNaN(in.SpikeRatio) || math.IsInf(in.SpikeRatio, 0) {
		in.SpikeRatio = DefaultSpikeRatio
	}
	a := &applier{
		in:      in,
		p99:     map[string]prometheus.ContainerMetrics{},
		out:     &autoscalingv1alpha1.ScalingSummary{},
		vpaRecs: map[string]*autoscalingv1alpha1.VPAContainerBounds{},
		aligned: map[string]bool{},
		spiky:   map[string]bool{},
	}
	for _, m := range in.Metrics {
		a.p99[m.ContainerName] = m
	}
	a.out.Autoscalers = autoscalerNames(in.HPAs, in.VPAs)
	if len(a.out.Autoscalers) == 0 {
		a.out.Reason = "No autoscaler targets the workload: requests are sized per pod from usage."
		return a.out
	}
	a.pickAutoscalers()
	a.replicas = replicaRange(in.Signals, in.CurrentReplicas, a.hpa)
	a.out.Replicas = a.replicas

	order := make([]int, len(recs))
	for i := range recs {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return recs[order[i]].ContainerName < recs[order[j]].ContainerName })
	for _, i := range order {
		rec := &recs[i]
		cs := autoscalingv1alpha1.ContainerScaling{Name: rec.ContainerName}
		cs.CPU = a.sizeResource(rec, ResourceCPU)
		cs.Memory = a.sizeResource(rec, ResourceMemory)
		strategy.EnsureLimitsCoverRequests(rec)
		if cs.CPU != nil || cs.Memory != nil {
			a.out.Containers = append(a.out.Containers, cs)
		}
	}
	a.finishVPA()
	for r := range a.aligned {
		a.out.AlignedResources = append(a.out.AlignedResources, r)
	}
	sort.Strings(a.out.AlignedResources)
	a.out.Reason = a.reason()
	return a.out
}

func autoscalerNames(hpas []HPA, vpas []VPA) []string {
	var out []string
	for _, h := range hpas {
		out = append(out, string(h.Kind)+"/"+h.Name)
		if h.ManagedBy != "" {
			out = append(out, string(autoscalingv1alpha1.AutoscalerKindScaledObject)+"/"+h.ManagedBy)
		}
	}
	for _, v := range vpas {
		out = append(out, string(autoscalingv1alpha1.AutoscalerKindVPA)+"/"+v.Name)
	}
	sort.Strings(out)
	return dedupe(out)
}

// pickAutoscalers selects the horizontal and vertical autoscaler the
// recommendation follows. Several of a kind fight each other: that is
// flagged and the kind is ignored.
func (a *applier) pickAutoscalers() {
	hpas := a.in.HPAs
	if len(hpas) > 0 && a.in.WorkloadKind == kindDaemonSet {
		a.notes = append(a.notes, "horizontal autoscalers cannot scale a DaemonSet and are ignored")
		hpas = nil
	}
	switch {
	case len(hpas) == 1:
		a.hpa = &hpas[0]
	case len(hpas) > 1:
		names := make([]string, 0, len(hpas))
		for _, h := range hpas {
			names = append(names, string(h.Kind)+"/"+h.Name)
		}
		a.conflict("", names, fmt.Sprintf("%d horizontal autoscalers target the workload and compete for its replica count; requests are sized per pod", len(hpas)))
	}
	switch {
	case len(a.in.VPAs) == 1:
		a.vpa = &a.in.VPAs[0]
	case len(a.in.VPAs) > 1:
		names := make([]string, 0, len(a.in.VPAs))
		for _, v := range a.in.VPAs {
			names = append(names, string(autoscalingv1alpha1.AutoscalerKindVPA)+"/"+v.Name)
		}
		a.conflict("", names, fmt.Sprintf("%d VerticalPodAutoscalers target the workload and compete for its requests; no VPA bounds are recommended", len(a.in.VPAs)))
	}
}

func (a *applier) conflict(res string, names []string, msg string) {
	sort.Strings(names)
	for _, c := range a.out.Conflicts {
		if c.Resource == res && strings.Join(c.Autoscalers, ",") == strings.Join(names, ",") {
			return
		}
	}
	a.out.Conflicts = append(a.out.Conflicts, autoscalingv1alpha1.ScalingConflict{Resource: res, Autoscalers: names, Message: msg})
}

// sizeResource adjusts one request of rec and explains it. It returns nil
// when the per-pod recommendation has no request for res.
func (a *applier) sizeResource(rec *strategy.Recommendation, res string) *autoscalingv1alpha1.ResourceScaling {
	perPod := request(rec, res)
	if perPod == nil {
		return nil
	}
	rs := &autoscalingv1alpha1.ResourceScaling{
		Sizing:        autoscalingv1alpha1.ScalingSizingPerPod,
		PerPodRequest: copyQ(perPod),
		Request:       copyQ(perPod),
	}
	baseline, p99, peak := a.usage(rec.ContainerName, res)
	ratio, spiky := spikiness(baseline, p99, a.in.SpikeRatio)
	rs.PeakToBaseline, rs.Spiky = ratio, spiky

	target := a.target(rec.ContainerName, res)
	policy := a.vpaPolicy(rec.ContainerName, res)
	switch {
	case target != nil && policy != nil && a.vpa.UpdateMode != updateModeOff:
		names := []string{string(a.hpa.Kind) + "/" + a.hpa.Name, string(autoscalingv1alpha1.AutoscalerKindVPA) + "/" + a.vpa.Name}
		a.conflict(res, names, fmt.Sprintf("VPA %s (updateMode %s) and %s %s both act on %s: the VPA changes the requests the HPA utilization is relative to; requests are sized per pod and the autoscalers are left as they are",
			a.vpa.Name, a.vpa.UpdateMode, a.hpa.Kind, a.hpa.Name, res))
		rs.Reason = fmt.Sprintf("VPA %s and %s %s both act on %s (conflict): sized per pod.", a.vpa.Name, a.hpa.Kind, a.hpa.Name, res)
	case target != nil:
		a.alignHPA(rec, res, rs, *target)
	case a.vpa != nil:
		a.sizeForVPA(rec, res, rs, policy, baseline, peak)
	case a.hpa != nil:
		rs.Reason = fmt.Sprintf("%s %s does not scale on %s Utilization%s: sized per pod.", a.hpa.Kind, a.hpa.Name, res, otherMetrics(a.hpa))
	default:
		rs.Reason = "No autoscaler acts on " + res + ": sized per pod."
	}
	return rs
}

func otherMetrics(h *HPA) string {
	if len(h.OtherMetrics) == 0 {
		return ""
	}
	return " (it scales on " + strings.Join(h.OtherMetrics, ", ") + ")"
}

// usage returns the baseline (P50), the P99 and the highest (max or P99)
// usage of a container's busiest pod; nil when unknown. Spikiness is judged
// on the P99 so that one outlier does not make a workload spiky, while VPA
// maxAllowed covers the highest usage seen.
func (a *applier) usage(container, res string) (baseline, p99, highest *float64) {
	if a.in.Signals != nil {
		if u, ok := a.in.Signals.Containers[container]; ok {
			st := u.CPU
			if res == ResourceMemory {
				st = u.Memory
			}
			baseline, highest = valid(st.P50), valid(st.Max)
		}
	}
	m := a.p99[container]
	q := m.P99CPU
	if res == ResourceMemory {
		q = m.P99Memory
	}
	if q != nil {
		v := q.AsApproximateFloat64()
		p99 = valid(&v)
	}
	switch {
	case p99 == nil:
		p99 = highest
	case highest == nil || *highest < *p99:
		highest = p99
	}
	return baseline, p99, highest
}

// spikiness returns the peak to baseline ratio (formatted, empty when the
// baseline is zero or unknown) and whether usage is spiky.
func spikiness(baseline, peak *float64, threshold float64) (string, bool) {
	if baseline == nil || peak == nil {
		return "", false
	}
	if *baseline <= 0 {
		return "", *peak > 0
	}
	ratio := *peak / *baseline
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return "", false
	}
	return fmt.Sprintf("%.2f", ratio), ratio >= threshold
}

// target returns the Utilization target of the selected HPA that applies to
// the container's res: a container target before a pod-level one, the
// lowest when several apply.
func (a *applier) target(container, res string) *autoscalingv1alpha1.UtilizationTarget {
	if a.hpa == nil {
		return nil
	}
	var best *autoscalingv1alpha1.UtilizationTarget
	for i := range a.hpa.Targets {
		t := &a.hpa.Targets[i]
		if t.Resource != res || t.AverageUtilization <= 0 || (t.Container != "" && t.Container != container) {
			continue
		}
		switch {
		case best == nil:
			best = t
		case (t.Container != "") != (best.Container != ""):
			if t.Container != "" {
				best = t
			}
		case t.AverageUtilization < best.AverageUtilization:
			best = t
		}
	}
	return best
}

// vpaPolicy returns the policy of the selected VPA that controls the
// container's res, or nil when the VPA leaves it alone.
func (a *applier) vpaPolicy(container, res string) *VPAPolicy {
	if a.vpa == nil {
		return nil
	}
	var p *VPAPolicy
	for i := range a.vpa.Policies {
		switch a.vpa.Policies[i].ContainerName {
		case container:
			p = &a.vpa.Policies[i]
		case "*":
			if p == nil {
				p = &a.vpa.Policies[i]
			}
		}
	}
	if p == nil {
		p = &VPAPolicy{ContainerName: container}
	}
	if p.Mode == updateModeOff {
		return nil
	}
	if len(p.ControlledResources) > 0 && !contains(p.ControlledResources, res) {
		return nil
	}
	return p
}

// alignHPA sizes the request so that the Utilization target matches the
// typical per-pod usage at the typical replica count.
func (a *applier) alignHPA(rec *strategy.Recommendation, res string, rs *autoscalingv1alpha1.ResourceScaling, t autoscalingv1alpha1.UtilizationTarget) {
	h := a.hpa
	pct := t.AverageUtilization
	rs.TargetUtilization = &pct
	switch {
	case a.replicas != nil && a.replicas.Source == autoscalingv1alpha1.ReplicaSourceFixed:
		rs.Reason = fmt.Sprintf("%s %s has minReplicas = maxReplicas = %d: replicas are fixed, sized per pod.", h.Kind, h.Name, a.replicas.Typical)
		return
	case a.replicas == nil:
		rs.Reason = fmt.Sprintf("%s %s targets %d%% %s Utilization but no replica history or count is known: sized per pod.", h.Kind, h.Name, pct, res)
		return
	case a.replicas.Typical < 1:
		rs.Reason = fmt.Sprintf("%s %s: the workload is typically scaled to zero: sized per pod.", h.Kind, h.Name)
		return
	}
	perPodUsage, source := a.perPodUsage(rec.ContainerName, res, a.replicas.Typical)
	if perPodUsage <= 0 {
		rs.Reason = fmt.Sprintf("%s %s targets %d%% %s Utilization but no %s usage is known: sized per pod.", h.Kind, h.Name, pct, res, res)
		return
	}
	aligned := a.clamp(res, perPodUsage*100/float64(pct))
	aligned = a.oomFloor(rec.ContainerName, res, aligned)
	setRequest(rec, res, aligned)
	usage := quantity(res, perPodUsage)
	rs.Sizing = autoscalingv1alpha1.ScalingSizingHPAUtilization
	rs.PerPodUsage = usage
	rs.Request = copyQ(aligned)
	a.aligned[res] = true
	rs.Reason = fmt.Sprintf("Sized so the %d%% %s Utilization target of %s %s matches the typical per-pod usage (%s, %s) at %d replicas%s.",
		pct, res, h.Kind, h.Name, usage.String(), source, a.replicas.Typical, otherMetrics(h))
}

// perPodUsage returns the median usage summed over pods divided by the
// typical replica count, else the median usage of the busiest pod.
func (a *applier) perPodUsage(container, res string, typical int32) (float64, string) {
	if a.in.Signals == nil {
		return 0, ""
	}
	u, ok := a.in.Signals.Containers[container]
	if !ok {
		return 0, ""
	}
	st := u.CPU
	if res == ResourceMemory {
		st = u.Memory
	}
	if total := valid(st.TotalMedian); total != nil && *total > 0 {
		return *total / float64(typical), "median of the summed usage per replica"
	}
	if p50 := valid(st.P50); p50 != nil {
		return *p50, "median of the busiest pod"
	}
	return 0, ""
}

func (a *applier) clamp(res string, v float64) *resource.Quantity {
	q := quantity(res, v)
	minQ, maxQ := a.in.Rails.MinCPU, a.in.Rails.MaxCPU
	floor := resource.NewMilliQuantity(1, resource.DecimalSI)
	if res == ResourceMemory {
		minQ, maxQ = a.in.Rails.MinMemory, a.in.Rails.MaxMemory
		floor = resource.NewQuantity(1<<20, resource.BinarySI)
	}
	if q.Cmp(*floor) < 0 {
		q = floor
	}
	if minQ != nil && q.Cmp(*minQ) < 0 {
		q = copyQ(minQ)
	}
	if maxQ != nil && q.Cmp(*maxQ) > 0 {
		q = copyQ(maxQ)
	}
	return q
}

// oomFloor keeps the memory of an OOM-killed container at least at its
// current request.
func (a *applier) oomFloor(container, res string, q *resource.Quantity) *resource.Quantity {
	if res != ResourceMemory || !contains(a.in.OOMKilled, container) {
		return q
	}
	cur := a.in.Current[container]
	if cur.Requests == nil || cur.Requests.Memory == nil || q.Cmp(*cur.Requests.Memory) >= 0 {
		return q
	}
	return copyQ(cur.Requests.Memory)
}

// sizeForVPA lowers the request of a spiky workload to its baseline when the
// VPA updates running pods, and recommends VPA bounds that cover the peaks.
func (a *applier) sizeForVPA(
	rec *strategy.Recommendation,
	res string,
	rs *autoscalingv1alpha1.ResourceScaling,
	policy *VPAPolicy,
	baseline, peak *float64,
) {
	v := a.vpa
	switch {
	case policy == nil:
		rs.Reason = fmt.Sprintf("VPA %s does not control %s of this container: sized per pod.", v.Name, res)
		return
	case baseline == nil || peak == nil:
		rs.Reason = fmt.Sprintf("VPA %s: no usage baseline or peak is known: sized per pod.", v.Name)
		return
	case !rs.Spiky:
		rs.Reason = fmt.Sprintf("VPA %s: %s usage is steady%s: sized per pod.", v.Name, res, ratioText(rs.PeakToBaseline))
		return
	}
	a.spiky[res] = true
	perPod := request(rec, res)
	base := a.oomFloor(rec.ContainerName, res, a.clamp(res, *baseline*(1+baselineBufferPercent/100.0)))
	maxAllowed := a.clamp(res, *peak*(1+peakHeadroomPercent/100.0))
	for _, floor := range []*resource.Quantity{perPod, base} {
		if floor != nil && maxAllowed.Cmp(*floor) < 0 {
			maxAllowed = copyQ(floor)
		}
	}
	a.addBounds(rec.ContainerName, res, policy, base, maxAllowed)

	switch {
	case v.UpdateMode == updateModeOff:
		rs.Reason = fmt.Sprintf("Spiky %s usage%s; VPA %s has updateMode Off, so the request stays sized per pod and the VPA bounds are only recommended.",
			res, ratioText(rs.PeakToBaseline), v.Name)
	case !updatesRunningPods(v.UpdateMode):
		rs.Reason = fmt.Sprintf("Spiky %s usage%s; VPA %s (updateMode %s) only sets requests at pod creation, so the request stays sized per pod; VPA bounds recommended.",
			res, ratioText(rs.PeakToBaseline), v.Name, v.UpdateMode)
	case perPod != nil && base.Cmp(*perPod) < 0:
		setRequest(rec, res, base)
		rs.Sizing = autoscalingv1alpha1.ScalingSizingVPABaseline
		rs.Request = copyQ(base)
		rs.Reason = fmt.Sprintf("Spiky %s usage%s: request lowered to the baseline; VPA %s (updateMode %s) covers peaks up to maxAllowed %s.",
			res, ratioText(rs.PeakToBaseline), v.Name, v.UpdateMode, maxAllowed.String())
	default:
		rs.Reason = fmt.Sprintf("Spiky %s usage%s: the per-pod request is already at the baseline; VPA %s covers peaks up to maxAllowed %s.",
			res, ratioText(rs.PeakToBaseline), v.Name, maxAllowed.String())
	}
}

func ratioText(ratio string) string {
	if ratio == "" {
		return ""
	}
	return " (P99/P50 = " + ratio + ")"
}

// updatesRunningPods reports whether a VPA update mode changes the requests
// of running pods (evicting or resizing them).
func updatesRunningPods(mode string) bool {
	switch mode {
	case "Auto", "Recreate", "InPlaceOrRecreate", "":
		return true
	}
	return false
}

func (a *applier) addBounds(container, res string, policy *VPAPolicy, minQ, maxQ *resource.Quantity) {
	b := a.vpaRecs[container]
	if b == nil {
		b = &autoscalingv1alpha1.VPAContainerBounds{
			Name:              container,
			CurrentMinAllowed: copyValues(policy.MinAllowed),
			CurrentMaxAllowed: copyValues(policy.MaxAllowed),
		}
		a.vpaRecs[container] = b
	}
	if b.MinAllowed == nil {
		b.MinAllowed = &autoscalingv1alpha1.ResourceValues{}
	}
	if b.MaxAllowed == nil {
		b.MaxAllowed = &autoscalingv1alpha1.ResourceValues{}
	}
	if res == ResourceCPU {
		b.MinAllowed.CPU, b.MaxAllowed.CPU = copyQ(minQ), copyQ(maxQ)
	} else {
		b.MinAllowed.Memory, b.MaxAllowed.Memory = copyQ(minQ), copyQ(maxQ)
	}
}

func (a *applier) finishVPA() {
	if a.vpa == nil || len(a.vpaRecs) == 0 {
		return
	}
	rec := autoscalingv1alpha1.VPARecommendation{
		Name:       a.vpa.Name,
		UpdateMode: a.vpa.UpdateMode,
		WriteBack:  a.vpa.UpdateMode != updateModeOff,
	}
	names := make([]string, 0, len(a.vpaRecs))
	for n := range a.vpaRecs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rec.Containers = append(rec.Containers, *a.vpaRecs[n])
	}
	if rec.WriteBack {
		rec.Reason = "minAllowed is the usage baseline and maxAllowed the observed peak with headroom, so the VPA covers the peaks."
	} else {
		rec.Reason = "updateMode Off: the VPA only recommends, so these bounds are not written."
	}
	a.out.VPA = append(a.out.VPA, rec)
}

// reason summarizes the adjustments in one sentence.
func (a *applier) reason() string {
	var parts []string
	if len(a.out.Conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("%d autoscaler conflict(s) flagged", len(a.out.Conflicts)))
	}
	if len(a.out.AlignedResources) > 0 && a.hpa != nil && a.replicas != nil {
		parts = append(parts, fmt.Sprintf("requests for %s sized for the Utilization target of %s %s at %d replicas (%d-%d)",
			strings.Join(a.out.AlignedResources, "/"), a.hpa.Kind, a.hpa.Name, a.replicas.Typical, a.replicas.Min, a.replicas.Max))
	}
	if len(a.spiky) > 0 {
		spiky := make([]string, 0, len(a.spiky))
		for r := range a.spiky {
			spiky = append(spiky, r)
		}
		sort.Strings(spiky)
		what := "VPA bounds recommended to cover the peaks"
		if a.vpa.UpdateMode == updateModeOff {
			what = "VPA bounds recommended only (updateMode Off)"
		} else if a.lowered() {
			what = "baseline requests with VPA bounds covering the peaks"
		}
		parts = append(parts, fmt.Sprintf("spiky %s usage: %s", strings.Join(spiky, "/"), what))
	}
	parts = append(parts, a.notes...)
	if len(parts) == 0 {
		return "Autoscalers detected, but none needed an adjustment: requests are sized per pod."
	}
	s := strings.Join(parts, "; ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

func (a *applier) lowered() bool {
	for _, c := range a.out.Containers {
		for _, rs := range []*autoscalingv1alpha1.ResourceScaling{c.CPU, c.Memory} {
			if rs != nil && rs.Sizing == autoscalingv1alpha1.ScalingSizingVPABaseline {
				return true
			}
		}
	}
	return false
}

// replicaRange derives the replica range from the history, else the current
// count, within the HPA bounds. It returns nil when nothing is known.
func replicaRange(signals *prometheus.ScalingSignals, current *int32, h *HPA) *autoscalingv1alpha1.ReplicaRange {
	lo, hi := int64(0), int64(math.MaxInt32)
	if h != nil {
		lo = 1
		if h.MinReplicas != nil {
			lo = int64(max(*h.MinReplicas, 0))
		}
		if h.MaxReplicas != nil && *h.MaxReplicas >= 0 {
			hi = int64(*h.MaxReplicas)
		}
		if hi < lo {
			hi = lo
		}
		if h.MinReplicas != nil && h.MaxReplicas != nil && *h.MinReplicas == *h.MaxReplicas {
			n := int32(lo)
			return &autoscalingv1alpha1.ReplicaRange{Min: n, Typical: n, Max: n, Source: autoscalingv1alpha1.ReplicaSourceFixed}
		}
	}
	clamp := func(v float64) int32 {
		return int32(min(max(int64(math.Round(v)), lo), hi))
	}
	if signals != nil {
		if med := validReplicas(signals.Replicas.Median); med != nil {
			mn, mx := *med, *med
			if v := validReplicas(signals.Replicas.Min); v != nil {
				mn = math.Min(*v, *med)
			}
			if v := validReplicas(signals.Replicas.Max); v != nil {
				mx = math.Max(*v, *med)
			}
			return &autoscalingv1alpha1.ReplicaRange{Min: clamp(mn), Typical: clamp(*med), Max: clamp(mx), Source: autoscalingv1alpha1.ReplicaSourceHistory}
		}
	}
	if current != nil && *current >= 0 {
		n := clamp(float64(*current))
		return &autoscalingv1alpha1.ReplicaRange{Min: n, Typical: n, Max: n, Source: autoscalingv1alpha1.ReplicaSourceCurrent}
	}
	return nil
}

func validReplicas(v *float64) *float64 {
	if v = valid(v); v == nil || *v > math.MaxInt32 {
		return nil
	}
	return v
}

// valid returns v unless it is nil, NaN, infinite or negative.
func valid(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
		return nil
	}
	return v
}

// quantity converts cores or bytes to a quantity, rounded up to whole
// millicores or MiB and bounded so it cannot overflow.
func quantity(res string, v float64) *resource.Quantity {
	if math.IsNaN(v) || v < 0 {
		v = 0
	}
	if res == ResourceCPU {
		return resource.NewMilliQuantity(int64(ceil(math.Min(v, maxCores)*1000)), resource.DecimalSI)
	}
	mib := ceil(math.Min(v, maxBytes) / (1 << 20))
	return resource.NewQuantity(int64(mib)<<20, resource.BinarySI)
}

// ceil rounds up, ignoring float error so that 150.00000000000003 is 150.
func ceil(v float64) float64 {
	return math.Ceil(v - 1e-6)
}

func request(rec *strategy.Recommendation, res string) *resource.Quantity {
	if res == ResourceCPU {
		return rec.Requests.CPU
	}
	return rec.Requests.Memory
}

func setRequest(rec *strategy.Recommendation, res string, q *resource.Quantity) {
	if res == ResourceCPU {
		rec.Requests.CPU = copyQ(q)
	} else {
		rec.Requests.Memory = copyQ(q)
	}
}

func copyQ(q *resource.Quantity) *resource.Quantity {
	if q == nil {
		return nil
	}
	c := q.DeepCopy()
	return &c
}

func copyValues(v *autoscalingv1alpha1.ResourceValues) *autoscalingv1alpha1.ResourceValues {
	if v == nil || (v.CPU == nil && v.Memory == nil) {
		return nil
	}
	return &autoscalingv1alpha1.ResourceValues{CPU: copyQ(v.CPU), Memory: copyQ(v.Memory)}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

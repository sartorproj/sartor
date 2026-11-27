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

package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

const (
	defaultTopGainsLimit = 20
	maxTopGainsLimit     = 200

	bytesPerGiB = 1 << 30

	// Requests above these bounds are treated as invalid, which keeps the
	// per-container products exact and the sums finite.
	maxRequestCores = 1 << 20
	maxRequestBytes = 1 << 50
	// maxPrice bounds the price parameters so that cost figures stay finite;
	// JSON cannot encode infinite values.
	maxPrice = 1e9
)

// SavingsCounts tells which Tailorings an analytics result covers.
type SavingsCounts struct {
	// Total is the number of Tailorings.
	Total int `json:"total"`
	// Included Tailorings have a last analysis, a replica count and at least
	// one container with both a current and a recommended request.
	Included int `json:"included"`
	// SkippedNoAnalysis Tailorings have no last analysis yet.
	SkippedNoAnalysis int `json:"skippedNoAnalysis"`
	// SkippedNoReplicas Tailorings have an analysis without a replica count.
	SkippedNoReplicas int `json:"skippedNoReplicas"`
	// SkippedNoCurrentRequest Tailorings have no container with both a
	// current and a recommended CPU or memory request.
	SkippedNoCurrentRequest int `json:"skippedNoCurrentRequest"`
}

// CPUSavings is a CPU request change in cores.
type CPUSavings struct {
	// ReclaimableCores sums the request decreases (current above recommended).
	ReclaimableCores float64 `json:"reclaimableCores"`
	// IncreaseCores sums the request increases (recommended above current).
	IncreaseCores float64 `json:"increaseCores"`
	// NetCores is ReclaimableCores - IncreaseCores.
	NetCores float64 `json:"netCores"`
	// TotalRequestedCores sums the current CPU requests x replicas of every
	// container of the included Tailorings that has one.
	TotalRequestedCores float64 `json:"totalRequestedCores"`
	// NetPercent is NetCores as a percentage of TotalRequestedCores; null
	// when the total is zero.
	NetPercent *float64 `json:"netPercent"`
}

// MemorySavings is a memory request change in GiB.
type MemorySavings struct {
	ReclaimableGiB float64 `json:"reclaimableGiB"`
	IncreaseGiB    float64 `json:"increaseGiB"`
	NetGiB         float64 `json:"netGiB"`
	// TotalRequestedGiB sums the current memory requests x replicas of every
	// container of the included Tailorings that has one.
	TotalRequestedGiB float64 `json:"totalRequestedGiB"`
	// NetPercent is NetGiB as a percentage of TotalRequestedGiB; null when
	// the total is zero.
	NetPercent *float64 `json:"netPercent"`
}

// CostSavings prices a request change with the prices given in the request.
type CostSavings struct {
	ReclaimablePerHour float64 `json:"reclaimablePerHour"`
	IncreasePerHour    float64 `json:"increasePerHour"`
	NetPerHour         float64 `json:"netPerHour"`
}

// SavingsAmounts is the request change of a set of containers, multiplied by
// the replica count of their workloads.
type SavingsAmounts struct {
	CPU    CPUSavings    `json:"cpu"`
	Memory MemorySavings `json:"memory"`
	// Cost is only present when a price is given.
	Cost *CostSavings `json:"cost,omitempty"`
}

// SavingsGroup is the request change of the Tailorings of one cluster, or of
// one workload namespace in a cluster.
type SavingsGroup struct {
	// Cluster is spec.target.cluster; empty when the Tailorings set none.
	Cluster string `json:"cluster"`
	// Namespace is the workload namespace (byNamespace only).
	Namespace string `json:"namespace,omitempty"`
	// Tailorings is the number of included Tailorings in the group.
	Tailorings int `json:"tailorings"`
	SavingsAmounts
}

// SavingsPricing echoes the prices used for the cost figures.
type SavingsPricing struct {
	CPUPricePerCoreHour *float64 `json:"cpuPricePerCoreHour,omitempty"`
	MemPricePerGiBHour  *float64 `json:"memPricePerGiBHour,omitempty"`
}

// SavingsReport is the data returned by GetSavings.
type SavingsReport struct {
	Totals      SavingsAmounts  `json:"totals"`
	ByCluster   []SavingsGroup  `json:"byCluster"`
	ByNamespace []SavingsGroup  `json:"byNamespace"`
	Tailorings  SavingsCounts   `json:"tailorings"`
	Pricing     *SavingsPricing `json:"pricing,omitempty"`
}

// ContainerGain is one container of a top gains list.
type ContainerGain struct {
	Cluster string `json:"cluster"`
	// Namespace and Tailoring name the Tailoring.
	Namespace         string `json:"namespace"`
	Tailoring         string `json:"tailoring"`
	WorkloadKind      string `json:"workloadKind"`
	WorkloadName      string `json:"workloadName"`
	WorkloadNamespace string `json:"workloadNamespace"`
	Container         string `json:"container"`
	Replicas          int32  `json:"replicas"`
	// CurrentRequests and RecommendedRequests are the per-pod requests.
	CurrentRequests     *autoscalingv1alpha1.ResourceValues `json:"currentRequests,omitempty"`
	RecommendedRequests *autoscalingv1alpha1.ResourceValues `json:"recommendedRequests,omitempty"`
	// CPUSavingCores and MemorySavingGiB are (current - recommended) x
	// replicas: positive when reclaimable, negative when an increase is
	// needed, absent when a request is missing.
	CPUSavingCores  *float64 `json:"cpuSavingCores,omitempty"`
	MemorySavingGiB *float64 `json:"memorySavingGiB,omitempty"`
}

// TopGainsReport is the data returned by GetTopGains.
type TopGainsReport struct {
	Limit      int             `json:"limit"`
	ByCPU      []ContainerGain `json:"byCPU"`
	ByMemory   []ContainerGain `json:"byMemory"`
	Tailorings SavingsCounts   `json:"tailorings"`
}

// containerSaving is the request change of one container of an included
// Tailoring. cpuMilli (millicores) and memBytes are (current - recommended)
// x replicas, nil when either request is missing or invalid.
type containerSaving struct {
	tailoring *autoscalingv1alpha1.Tailoring
	replicas  int32
	rec       *autoscalingv1alpha1.ContainerRecommendation
	cpuMilli  *float64
	memBytes  *float64
}

// tailoringSavings groups the container savings of one included Tailoring.
// requestedMilli and requestedBytes sum the current requests x replicas of
// all its containers with a valid current request, recommended or not.
type tailoringSavings struct {
	tailoring                      *autoscalingv1alpha1.Tailoring
	containers                     []containerSaving
	requestedMilli, requestedBytes float64
}

// collectSavings computes the container savings of every Tailoring that has
// a last analysis, a replica count and at least one container with both a
// current and a recommended request, and counts the others.
func collectSavings(items []autoscalingv1alpha1.Tailoring) ([]tailoringSavings, SavingsCounts) {
	counts := SavingsCounts{Total: len(items)}
	var out []tailoringSavings
	for i := range items {
		t := &items[i]
		analysis := t.Status.LastAnalysis
		switch {
		case analysis == nil:
			counts.SkippedNoAnalysis++
			continue
		case analysis.Replicas == nil || *analysis.Replicas < 0:
			counts.SkippedNoReplicas++
			continue
		}
		replicas := *analysis.Replicas
		ts := tailoringSavings{tailoring: t}
		for j := range analysis.Containers {
			c := &analysis.Containers[j]
			s := containerSaving{
				tailoring: t,
				replicas:  replicas,
				rec:       c,
				cpuMilli:  requestDelta(cpuRequest(c.Current), cpuRequest(c.Recommended), true, replicas),
				memBytes:  requestDelta(memoryRequest(c.Current), memoryRequest(c.Recommended), false, replicas),
			}
			if s.cpuMilli != nil || s.memBytes != nil {
				ts.containers = append(ts.containers, s)
			}
			if v, ok := requestValue(cpuRequest(c.Current), true); ok {
				ts.requestedMilli += float64(v) * float64(replicas)
			}
			if v, ok := requestValue(memoryRequest(c.Current), false); ok {
				ts.requestedBytes += float64(v) * float64(replicas)
			}
		}
		if len(ts.containers) == 0 {
			counts.SkippedNoCurrentRequest++
			continue
		}
		counts.Included++
		out = append(out, ts)
	}
	return out, counts
}

func cpuRequest(r autoscalingv1alpha1.ResourceRequirements) *resource.Quantity {
	if r.Requests == nil {
		return nil
	}
	return r.Requests.CPU
}

func memoryRequest(r autoscalingv1alpha1.ResourceRequirements) *resource.Quantity {
	if r.Requests == nil {
		return nil
	}
	return r.Requests.Memory
}

// requestDelta returns (current - recommended) x replicas in millicores
// (milli) or bytes, or nil when either request is missing, negative or
// implausibly large.
func requestDelta(current, recommended *resource.Quantity, milli bool, replicas int32) *float64 {
	cur, ok := requestValue(current, milli)
	if !ok {
		return nil
	}
	rec, ok := requestValue(recommended, milli)
	if !ok {
		return nil
	}
	d := float64(cur-rec) * float64(replicas)
	return &d
}

func requestValue(q *resource.Quantity, milli bool) (int64, bool) {
	if q == nil {
		return 0, false
	}
	limit := float64(maxRequestBytes)
	if milli {
		limit = maxRequestCores
	}
	if f := q.AsApproximateFloat64(); math.IsNaN(f) || f < 0 || f > limit {
		return 0, false
	}
	if milli {
		return q.MilliValue(), true
	}
	return q.Value(), true
}

// savingsAccumulator sums container savings in millicores and bytes.
type savingsAccumulator struct {
	tailorings                     int
	reclaimMilli, increaseMilli    float64
	reclaimBytes, increaseBytes    float64
	requestedMilli, requestedBytes float64
}

func (a *savingsAccumulator) add(ts tailoringSavings) {
	a.tailorings++
	a.requestedMilli += ts.requestedMilli
	a.requestedBytes += ts.requestedBytes
	for _, c := range ts.containers {
		if c.cpuMilli != nil {
			if *c.cpuMilli > 0 {
				a.reclaimMilli += *c.cpuMilli
			} else {
				a.increaseMilli -= *c.cpuMilli
			}
		}
		if c.memBytes != nil {
			if *c.memBytes > 0 {
				a.reclaimBytes += *c.memBytes
			} else {
				a.increaseBytes -= *c.memBytes
			}
		}
	}
}

func (a *savingsAccumulator) amounts(p SavingsPricing) SavingsAmounts {
	out := SavingsAmounts{
		CPU: CPUSavings{
			ReclaimableCores:    a.reclaimMilli / 1000,
			IncreaseCores:       a.increaseMilli / 1000,
			NetCores:            (a.reclaimMilli - a.increaseMilli) / 1000,
			TotalRequestedCores: a.requestedMilli / 1000,
			NetPercent:          percentOf(a.reclaimMilli-a.increaseMilli, a.requestedMilli),
		},
		Memory: MemorySavings{
			ReclaimableGiB:    a.reclaimBytes / bytesPerGiB,
			IncreaseGiB:       a.increaseBytes / bytesPerGiB,
			NetGiB:            (a.reclaimBytes - a.increaseBytes) / bytesPerGiB,
			TotalRequestedGiB: a.requestedBytes / bytesPerGiB,
			NetPercent:        percentOf(a.reclaimBytes-a.increaseBytes, a.requestedBytes),
		},
	}
	if p.CPUPricePerCoreHour == nil && p.MemPricePerGiBHour == nil {
		return out
	}
	price := func(cores, gib float64) float64 {
		var v float64
		if p.CPUPricePerCoreHour != nil {
			v += cores * *p.CPUPricePerCoreHour
		}
		if p.MemPricePerGiBHour != nil {
			v += gib * *p.MemPricePerGiBHour
		}
		return v
	}
	out.Cost = &CostSavings{
		ReclaimablePerHour: price(out.CPU.ReclaimableCores, out.Memory.ReclaimableGiB),
		IncreasePerHour:    price(out.CPU.IncreaseCores, out.Memory.IncreaseGiB),
	}
	out.Cost.NetPerHour = out.Cost.ReclaimablePerHour - out.Cost.IncreasePerHour
	return out
}

// percentOf returns part as a percentage of total, nil when total is zero.
func percentOf(part, total float64) *float64 {
	if total <= 0 {
		return nil
	}
	v := part / total * 100
	return &v
}

// buildSavingsReport aggregates the savings of all Tailorings in total, per
// cluster and per workload namespace.
func buildSavingsReport(items []autoscalingv1alpha1.Tailoring, pricing SavingsPricing) SavingsReport {
	included, counts := collectSavings(items)

	type nsKey struct{ cluster, namespace string }
	var total savingsAccumulator
	clusters := map[string]*savingsAccumulator{}
	namespaces := map[nsKey]*savingsAccumulator{}
	for _, ts := range included {
		cluster := ts.tailoring.Spec.Target.Cluster
		key := nsKey{cluster, ts.tailoring.TargetNamespace()}
		if clusters[cluster] == nil {
			clusters[cluster] = &savingsAccumulator{}
		}
		if namespaces[key] == nil {
			namespaces[key] = &savingsAccumulator{}
		}
		total.add(ts)
		clusters[cluster].add(ts)
		namespaces[key].add(ts)
	}

	report := SavingsReport{
		Totals:      total.amounts(pricing),
		ByCluster:   make([]SavingsGroup, 0, len(clusters)),
		ByNamespace: make([]SavingsGroup, 0, len(namespaces)),
		Tailorings:  counts,
	}
	if pricing.CPUPricePerCoreHour != nil || pricing.MemPricePerGiBHour != nil {
		report.Pricing = &pricing
	}
	for cluster, acc := range clusters {
		report.ByCluster = append(report.ByCluster, SavingsGroup{
			Cluster: cluster, Tailorings: acc.tailorings, SavingsAmounts: acc.amounts(pricing),
		})
	}
	for key, acc := range namespaces {
		report.ByNamespace = append(report.ByNamespace, SavingsGroup{
			Cluster: key.cluster, Namespace: key.namespace, Tailorings: acc.tailorings, SavingsAmounts: acc.amounts(pricing),
		})
	}
	sort.Slice(report.ByCluster, func(i, j int) bool { return report.ByCluster[i].Cluster < report.ByCluster[j].Cluster })
	sort.Slice(report.ByNamespace, func(i, j int) bool {
		a, b := report.ByNamespace[i], report.ByNamespace[j]
		if a.Cluster != b.Cluster {
			return a.Cluster < b.Cluster
		}
		return a.Namespace < b.Namespace
	})
	return report
}

// buildTopGainsReport ranks the containers by reclaimable CPU and by
// reclaimable memory; containers without a reclaimable amount are left out.
func buildTopGainsReport(items []autoscalingv1alpha1.Tailoring, limit int) TopGainsReport {
	included, counts := collectSavings(items)
	var byCPU, byMemory []containerSaving
	for _, ts := range included {
		for _, c := range ts.containers {
			if c.cpuMilli != nil && *c.cpuMilli > 0 {
				byCPU = append(byCPU, c)
			}
			if c.memBytes != nil && *c.memBytes > 0 {
				byMemory = append(byMemory, c)
			}
		}
	}
	return TopGainsReport{
		Limit:      limit,
		ByCPU:      rankGains(byCPU, func(c containerSaving) float64 { return *c.cpuMilli }, limit),
		ByMemory:   rankGains(byMemory, func(c containerSaving) float64 { return *c.memBytes }, limit),
		Tailorings: counts,
	}
}

// rankGains sorts savings by value, largest first, with ties ordered by
// cluster, namespace, Tailoring and container, and returns the first limit.
func rankGains(savings []containerSaving, value func(containerSaving) float64, limit int) []ContainerGain {
	sort.SliceStable(savings, func(i, j int) bool {
		a, b := savings[i], savings[j]
		if va, vb := value(a), value(b); va != vb {
			return va > vb
		}
		ka := [4]string{a.tailoring.Spec.Target.Cluster, a.tailoring.Namespace, a.tailoring.Name, a.rec.Name}
		kb := [4]string{b.tailoring.Spec.Target.Cluster, b.tailoring.Namespace, b.tailoring.Name, b.rec.Name}
		for k := range ka {
			if ka[k] != kb[k] {
				return ka[k] < kb[k]
			}
		}
		return false
	})
	if len(savings) > limit {
		savings = savings[:limit]
	}
	out := make([]ContainerGain, 0, len(savings))
	for _, c := range savings {
		t := c.tailoring
		g := ContainerGain{
			Cluster:             t.Spec.Target.Cluster,
			Namespace:           t.Namespace,
			Tailoring:           t.Name,
			WorkloadKind:        t.Spec.Target.Kind,
			WorkloadName:        t.Spec.Target.Name,
			WorkloadNamespace:   t.TargetNamespace(),
			Container:           c.rec.Name,
			Replicas:            c.replicas,
			CurrentRequests:     c.rec.Current.Requests,
			RecommendedRequests: c.rec.Recommended.Requests,
		}
		if c.cpuMilli != nil {
			v := *c.cpuMilli / 1000
			g.CPUSavingCores = &v
		}
		if c.memBytes != nil {
			v := *c.memBytes / bytesPerGiB
			g.MemorySavingGiB = &v
		}
		out = append(out, g)
	}
	return out
}

// parsePrice reads an optional price parameter from 0 to maxPrice.
func parsePrice(query url.Values, name string) (*float64, error) {
	if !query.Has(name) {
		return nil, nil
	}
	raw := query.Get(name)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > maxPrice {
		return nil, fmt.Errorf("invalid %s %q: must be a non-negative number up to %g", name, raw, float64(maxPrice))
	}
	return &v, nil
}

// parseTopGainsLimit reads the limit parameter: an integer from 1 to
// maxTopGainsLimit, defaultTopGainsLimit when absent.
func parseTopGainsLimit(query url.Values) (int, error) {
	if !query.Has("limit") {
		return defaultTopGainsLimit, nil
	}
	raw := query.Get("limit")
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxTopGainsLimit {
		return 0, fmt.Errorf("invalid limit %q: must be an integer from 1 to %d", raw, maxTopGainsLimit)
	}
	return n, nil
}

// listTailoringsForAnalytics lists all Tailorings, writing the error response
// when the list fails.
func (h *Handler) listTailoringsForAnalytics(w http.ResponseWriter, r *http.Request) ([]autoscalingv1alpha1.Tailoring, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	list := &autoscalingv1alpha1.TailoringList{}
	if err := h.client.List(ctx, list); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return list.Items, true
}

// GetSavings returns the request change recommended by the last analysis of
// every Tailoring, multiplied by the workload's replica count, in total, per
// cluster and per workload namespace. The optional query parameters
// cpuPricePerCoreHour and memPricePerGiBHour add hourly cost figures.
func (h *Handler) GetSavings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query := r.URL.Query()
	var pricing SavingsPricing
	var err error
	if pricing.CPUPricePerCoreHour, err = parsePrice(query, "cpuPricePerCoreHour"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if pricing.MemPricePerGiBHour, err = parsePrice(query, "memPricePerGiBHour"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, ok := h.listTailoringsForAnalytics(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: buildSavingsReport(items, pricing)})
}

// GetTopGains returns the containers with the largest reclaimable CPU and
// memory requests (multiplied by replicas). The optional limit query
// parameter (1 to 200, default 20) bounds each list.
func (h *Handler) GetTopGains(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, err := parseTopGainsLimit(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, ok := h.listTailoringsForAnalytics(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: buildTopGainsReport(items, limit)})
}

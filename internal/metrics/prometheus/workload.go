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

package prometheus

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Labels added by the snapshot query to tell its sub-results apart. They are
// prefixed so they cannot collide with labels of the source series.
const (
	sourceLabel = "sartor_source"
	aggLabel    = "sartor_agg"
)

// Sub-results of the snapshot query.
const (
	srcExists    = "exists"
	srcPods      = "pods"
	srcInfo      = "info"
	srcRequests  = "requests"
	srcLimits    = "limits"
	srcOOM       = "oom"
	srcQuota     = "quota"
	srcHPA       = "hpa"
	srcHPATarget = "hpa_target"
	srcKEDA      = "keda"
	srcVPA       = "vpa"
	srcVPAMode   = "vpa_mode"
)

// ResourceValues holds CPU and memory quantities; nil means not set.
type ResourceValues struct {
	CPU    *resource.Quantity
	Memory *resource.Quantity
}

// ContainerResources holds a container's requests and limits.
type ContainerResources struct {
	Requests ResourceValues
	Limits   ResourceValues
}

// HPAInfo is a HorizontalPodAutoscaler that targets the workload.
type HPAInfo struct {
	Name string
	// UtilizationResources are the resources (cpu, memory) with a Utilization target.
	UtilizationResources []string
	// ScaledObject is the KEDA ScaledObject that owns the HPA, when the
	// scaledobject.keda.sh/name label is exposed by kube-state-metrics.
	ScaledObject string
}

// VPAInfo is a VerticalPodAutoscaler that targets the workload.
type VPAInfo struct {
	Name       string
	UpdateMode string
}

// Quota is the first ResourceQuota (by name) of the namespace.
type Quota struct {
	Name        string
	CPULimit    *resource.Quantity
	MemoryLimit *resource.Quantity
}

// WorkloadState is what kube-state-metrics reports about a workload.
type WorkloadState struct {
	// Exists is true when the workload object exists (even with no pods).
	Exists bool
	// Pods is the number of Pending or Running pods owned by the workload.
	Pods int
	// Containers holds the requests and limits reported by those pods.
	Containers map[string]ContainerResources
	// Disagreeing lists containers whose pods report different requests,
	// limits or container sets, e.g. during a rollout.
	Disagreeing []string
	// OOMKilled lists containers whose last termination was an OOM kill.
	OOMKilled []string
	// Quota is the namespace's first ResourceQuota, if any.
	Quota *Quota
	HPAs  []HPAInfo
	VPAs  []VPAInfo
}

// workloadLabel returns the kube-state-metrics label naming a workload of kind.
func workloadLabel(kind string) (string, error) {
	switch kind {
	case KindDeployment:
		return "deployment", nil
	case KindStatefulSet:
		return "statefulset", nil
	case KindDaemonSet:
		return "daemonset", nil
	}
	return "", fmt.Errorf("unsupported workload kind %q", kind)
}

func tag(expr, label, value string) string {
	return fmt.Sprintf(`label_replace(%s, %q, %q, "", "")`, expr, label, value)
}

// snapshotQuery returns one instant query that reads the workload's state,
// OOM kills, namespace quota and autoscalers from kube-state-metrics. Every
// sub-result is tagged with sartor_source (and sartor_agg) so that one round
// trip replaces what the Kubernetes API path reads with several calls.
func snapshotQuery(sel podSelector) (string, error) {
	wl, err := workloadLabel(sel.workload.Kind)
	if err != nil {
		return "", err
	}
	scope := sel.scopeMatchers()
	name := promString(sel.workload.Name)
	kind := promString(sel.workload.Kind)

	// Terminated pods (e.g. evicted pods of an old ReplicaSet) must not count.
	active := fmt.Sprintf(`(%s * on (namespace, pod) group_left() max by (namespace, pod) (kube_pod_status_phase{%s, phase=~"Pending|Running"} == 1))`,
		sel.ownedPods(), scope)
	perPod := fmt.Sprintf(`((%s or %s or %s) * on (namespace, pod) group_left() %s)`,
		tag(fmt.Sprintf(`kube_pod_container_info{%s}`, scope), sourceLabel, srcInfo),
		tag(fmt.Sprintf(`kube_pod_container_resource_requests{%s, resource=~"cpu|memory"}`, scope), sourceLabel, srcRequests),
		tag(fmt.Sprintf(`kube_pod_container_resource_limits{%s, resource=~"cpu|memory"}`, scope), sourceLabel, srcLimits),
		active)
	by := "container, resource, " + sourceLabel
	hpaInfo := fmt.Sprintf(`kube_horizontalpodautoscaler_info{%s, scaletargetref_kind=%s, scaletargetref_name=%s}`, scope, kind, name)
	hpaJoin := fmt.Sprintf(`on (namespace, horizontalpodautoscaler) group_left() max by (namespace, horizontalpodautoscaler) (%s)`, hpaInfo)

	parts := []string{
		tag(fmt.Sprintf(`max(kube_%s_created{%s, %s=%s})`, wl, scope, wl, name), sourceLabel, srcExists),
		tag(tag(fmt.Sprintf(`count(%s)`, active), sourceLabel, srcPods), aggLabel, "count"),
		tag(fmt.Sprintf(`max by (%s) (%s)`, by, perPod), aggLabel, "max"),
		tag(fmt.Sprintf(`min by (%s) (%s)`, by, perPod), aggLabel, "min"),
		tag(fmt.Sprintf(`count by (%s) (count by (%s, namespace, pod) (%s))`, by, by, perPod), aggLabel, "count"),
		tag(fmt.Sprintf(`max by (container) (kube_pod_container_status_last_terminated_reason{%s, reason="OOMKilled"} * on (namespace, pod) group_left() %s)`,
			scope, sel.ownedPods()), sourceLabel, srcOOM),
		tag(fmt.Sprintf(`max by (resourcequota, resource) (kube_resourcequota{%s, type="hard"})`, scope), sourceLabel, srcQuota),
		tag(fmt.Sprintf(`max by (horizontalpodautoscaler) (%s)`, hpaInfo), sourceLabel, srcHPA),
		tag(fmt.Sprintf(`max by (horizontalpodautoscaler, metric_name) (kube_horizontalpodautoscaler_spec_target_metric{%s, metric_target_type="utilization", metric_name=~"cpu|memory"} * %s)`,
			scope, hpaJoin), sourceLabel, srcHPATarget),
		tag(fmt.Sprintf(`max by (horizontalpodautoscaler, label_scaledobject_keda_sh_name) (kube_horizontalpodautoscaler_labels{%s, label_scaledobject_keda_sh_name!=""} * %s)`,
			scope, hpaJoin), sourceLabel, srcKEDA),
		tag(fmt.Sprintf(`max by (verticalpodautoscaler) (kube_verticalpodautoscaler_labels{%s, target_kind=%s, target_name=%s})`, scope, kind, name), sourceLabel, srcVPA),
		tag(fmt.Sprintf(`max by (verticalpodautoscaler, update_mode) (kube_verticalpodautoscaler_spec_updatepolicy_updatemode{%s, target_kind=%s, target_name=%s} == 1)`,
			scope, kind, name), sourceLabel, srcVPAMode),
	}
	return strings.Join(parts, " or "), nil
}

// GetWorkloadState reads the workload's state from kube-state-metrics with a
// single instant query. Values are taken from the Pending and Running pods the
// workload owns, so they reflect admission-time changes (injected sidecars,
// LimitRange defaults, VPA-set requests) rather than the pod template.
func (c *Client) GetWorkloadState(ctx context.Context, namespace string, workload Workload) (*WorkloadState, error) {
	sel, err := newPodSelector(namespace, workload, c.config.ClusterLabel)
	if err != nil {
		return nil, err
	}
	query, err := snapshotQuery(sel)
	if err != nil {
		return nil, err
	}
	result, _, _, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to query workload state: %w", err)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}
	return parseSnapshot(vector), nil
}

type stateKey struct {
	container, source, resource string
}

// snapshotParser accumulates the tagged sub-results of a snapshot query.
type snapshotParser struct {
	st                *WorkloadState
	maxV, minV, count map[stateKey]float64
	quotas            map[string]*Quota
	hpas              map[string]*HPAInfo
	vpas              map[string]*VPAInfo
	oom               map[string]bool
}

func parseSnapshot(vector model.Vector) *WorkloadState {
	p := &snapshotParser{
		st:     &WorkloadState{Containers: map[string]ContainerResources{}},
		maxV:   map[stateKey]float64{},
		minV:   map[stateKey]float64{},
		count:  map[stateKey]float64{},
		quotas: map[string]*Quota{},
		hpas:   map[string]*HPAInfo{},
		vpas:   map[string]*VPAInfo{},
		oom:    map[string]bool{},
	}
	for _, s := range vector {
		v := float64(s.Value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		p.add(s.Metric, v)
	}
	return p.finish()
}

func (p *snapshotParser) add(m model.Metric, v float64) {
	switch source := string(m[sourceLabel]); source {
	case srcExists:
		p.st.Exists = true
	case srcPods:
		p.st.Pods = int(v)
	case srcInfo, srcRequests, srcLimits:
		p.addContainerValue(m, source, v)
	case srcOOM:
		if c := string(m["container"]); c != "" && v > 0 {
			p.oom[c] = true
		}
	case srcQuota:
		p.addQuota(m, v)
	case srcHPA, srcHPATarget, srcKEDA:
		p.addHPA(m, source)
	case srcVPA, srcVPAMode:
		p.addVPA(m)
	}
}

func (p *snapshotParser) addContainerValue(m model.Metric, source string, v float64) {
	k := stateKey{string(m["container"]), source, string(m["resource"])}
	if k.container == "" {
		return
	}
	switch string(m[aggLabel]) {
	case "max":
		p.maxV[k] = v
	case "min":
		p.minV[k] = v
	case "count":
		p.count[k] = v
	}
}

func (p *snapshotParser) addQuota(m model.Metric, v float64) {
	name := string(m["resourcequota"])
	q := p.quotas[name]
	if q == nil {
		q = &Quota{Name: name}
		p.quotas[name] = q
	}
	switch string(m["resource"]) {
	case "limits.cpu":
		q.CPULimit = resource.NewMilliQuantity(int64(math.Round(v*1000)), resource.DecimalSI)
	case "limits.memory":
		q.MemoryLimit = resource.NewQuantity(int64(v), resource.BinarySI)
	}
}

func (p *snapshotParser) addHPA(m model.Metric, source string) {
	name := string(m["horizontalpodautoscaler"])
	if name == "" {
		return
	}
	h := p.hpas[name]
	if h == nil {
		h = &HPAInfo{Name: name}
		p.hpas[name] = h
	}
	switch source {
	case srcHPATarget:
		h.UtilizationResources = appendUnique(h.UtilizationResources, string(m["metric_name"]))
	case srcKEDA:
		h.ScaledObject = string(m["label_scaledobject_keda_sh_name"])
	}
}

func (p *snapshotParser) addVPA(m model.Metric) {
	name := string(m["verticalpodautoscaler"])
	if name == "" {
		return
	}
	vpa := p.vpas[name]
	if vpa == nil {
		vpa = &VPAInfo{Name: name}
		p.vpas[name] = vpa
	}
	if mode := string(m["update_mode"]); mode != "" {
		vpa.UpdateMode = mode
	}
}

// finish builds the containers and flags those whose pods disagree: a
// container missing on some pods, a value that differs between pods
// (max != min), or a value set on some pods only.
func (p *snapshotParser) finish() *WorkloadState {
	st := p.st
	disagree := map[string]bool{}
	for k, n := range p.count {
		if k.source != srcInfo {
			continue
		}
		st.Containers[k.container] = ContainerResources{}
		if int(n) != st.Pods {
			disagree[k.container] = true
		}
	}
	for k, mx := range p.maxV {
		cr, present := st.Containers[k.container]
		if !present || k.source == srcInfo {
			continue
		}
		if mn, ok := p.minV[k]; !ok || mn != mx {
			disagree[k.container] = true
		}
		if p.count[k] != p.count[stateKey{k.container, srcInfo, ""}] {
			disagree[k.container] = true
		}
		if setValue(&cr, k, mx) {
			st.Containers[k.container] = cr
		}
	}
	st.Disagreeing = sortedKeys(disagree)
	st.OOMKilled = sortedKeys(p.oom)

	if names := sortedKeys(p.quotas); len(names) > 0 {
		st.Quota = p.quotas[names[0]]
	}
	for _, name := range sortedKeys(p.hpas) {
		h := p.hpas[name]
		sort.Strings(h.UtilizationResources)
		st.HPAs = append(st.HPAs, *h)
	}
	for _, name := range sortedKeys(p.vpas) {
		st.VPAs = append(st.VPAs, *p.vpas[name])
	}
	return st
}

// setValue stores a request or limit value (cores or bytes) in cr.
func setValue(cr *ContainerResources, k stateKey, v float64) bool {
	target := &cr.Requests
	if k.source == srcLimits {
		target = &cr.Limits
	}
	switch k.resource {
	case "cpu":
		target.CPU = resource.NewMilliQuantity(int64(math.Round(v*1000)), resource.DecimalSI)
	case "memory":
		target.Memory = resource.NewQuantity(int64(math.Round(v)), resource.BinarySI)
	default:
		return false
	}
	return true
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

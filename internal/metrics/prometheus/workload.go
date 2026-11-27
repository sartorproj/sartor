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
	srcVPABound  = "vpa_bound"
	srcReplicas  = "replicas"
	srcHPAMetric = "hpa_metric"
	srcHPAMin    = "hpa_min"
	srcHPAMax    = "hpa_max"
)

// boundLabel carries the VPA bound (minallowed_cpu, maxallowed_memory, ...)
// extracted from the metric name.
const boundLabel = "sartor_bound"

const vpaBoundMetrics = "kube_verticalpodautoscaler_spec_resourcepolicy_container_policies_(min|max)allowed_(cpu|memory)"

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
	// UtilizationTargets maps those resources to their target in percent.
	UtilizationTargets map[string]int32
	// OtherMetrics lists the other target metrics as "name type"
	// (for example "queue_length value").
	OtherMetrics []string
	// MinReplicas and MaxReplicas are nil when not reported.
	MinReplicas, MaxReplicas *int32
	// ScaledObject is the KEDA ScaledObject that owns the HPA, when the
	// scaledobject.keda.sh/name label is exposed by kube-state-metrics.
	ScaledObject string
}

// VPAInfo is a VerticalPodAutoscaler that targets the workload.
type VPAInfo struct {
	Name       string
	UpdateMode string
	// MinAllowed and MaxAllowed hold the container policy bounds by
	// container name ("*" for the default policy), when kube-state-metrics
	// exposes them.
	MinAllowed, MaxAllowed map[string]ResourceValues
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
	// Replicas is the desired replica count; nil when not reported.
	Replicas *int32
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

// replicaMetrics names the kube-state-metrics series holding the desired
// replica count of each workload kind: spec.replicas for Deployments and
// StatefulSets, the desired number of scheduled pods for DaemonSets.
var replicaMetrics = map[string]string{
	KindDeployment:  "kube_deployment_spec_replicas",
	KindStatefulSet: "kube_statefulset_replicas",
	KindDaemonSet:   "kube_daemonset_status_desired_number_scheduled",
}

// replicasExpr returns an expression for the workload's desired replica count.
func replicasExpr(sel podSelector) (string, error) {
	wl, err := workloadLabel(sel.workload.Kind)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`max(%s{%s, %s=%s})`,
		replicaMetrics[sel.workload.Kind], sel.scopeMatchers(), wl, promString(sel.workload.Name)), nil
}

// replicaCount converts a replica sample to a count; NaN, infinite, negative
// or out-of-range values are not a count.
func replicaCount(v float64) *int32 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > math.MaxInt32 {
		return nil
	}
	n := int32(math.Round(v))
	return &n
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
	replicas, err := replicasExpr(sel)
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
		tag(fmt.Sprintf(`max by (horizontalpodautoscaler, metric_name, metric_target_type) (kube_horizontalpodautoscaler_spec_target_metric{%s} * %s)`,
			scope, hpaJoin), sourceLabel, srcHPAMetric),
		tag(fmt.Sprintf(`max by (horizontalpodautoscaler) (kube_horizontalpodautoscaler_spec_min_replicas{%s} * %s)`, scope, hpaJoin), sourceLabel, srcHPAMin),
		tag(fmt.Sprintf(`max by (horizontalpodautoscaler) (kube_horizontalpodautoscaler_spec_max_replicas{%s} * %s)`, scope, hpaJoin), sourceLabel, srcHPAMax),
		tag(fmt.Sprintf(`max by (verticalpodautoscaler) (kube_verticalpodautoscaler_labels{%s, target_kind=%s, target_name=%s})`, scope, kind, name), sourceLabel, srcVPA),
		tag(fmt.Sprintf(`max by (verticalpodautoscaler, update_mode) (kube_verticalpodautoscaler_spec_updatepolicy_updatemode{%s, target_kind=%s, target_name=%s} == 1)`,
			scope, kind, name), sourceLabel, srcVPAMode),
		tag(fmt.Sprintf(`max by (verticalpodautoscaler, container, %s) (label_replace({%s, __name__=~%q}, %q, "$1_$2", "__name__", %q) `+
			`* on (namespace, verticalpodautoscaler) group_left() max by (namespace, verticalpodautoscaler) (kube_verticalpodautoscaler_labels{%s, target_kind=%s, target_name=%s}))`,
			boundLabel, scope, vpaBoundMetrics, boundLabel, vpaBoundMetrics, scope, kind, name), sourceLabel, srcVPABound),
		tag(replicas, sourceLabel, srcReplicas),
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

// GetReplicas reads the workload's desired replica count from
// kube-state-metrics. It returns nil without an error when no series exists.
func (c *Client) GetReplicas(ctx context.Context, namespace string, workload Workload) (*int32, error) {
	sel, err := newPodSelector(namespace, workload, c.config.ClusterLabel)
	if err != nil {
		return nil, err
	}
	query, err := replicasExpr(sel)
	if err != nil {
		return nil, err
	}
	result, _, _, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to query replicas: %w", err)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}
	if len(vector) == 0 {
		return nil, nil
	}
	return replicaCount(float64(vector[0].Value)), nil
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
	case srcHPA, srcHPATarget, srcKEDA, srcHPAMetric, srcHPAMin, srcHPAMax:
		p.addHPA(m, source, v)
	case srcVPA, srcVPAMode:
		p.addVPA(m)
	case srcVPABound:
		p.addVPABound(m, v)
	case srcReplicas:
		p.st.Replicas = replicaCount(v)
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

func (p *snapshotParser) addHPA(m model.Metric, source string, v float64) {
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
		res := string(m["metric_name"])
		h.UtilizationResources = appendUnique(h.UtilizationResources, res)
		if pct := replicaCount(v); pct != nil && *pct > 0 {
			if h.UtilizationTargets == nil {
				h.UtilizationTargets = map[string]int32{}
			}
			h.UtilizationTargets[res] = *pct
		}
	case srcHPAMetric:
		metric, typ := string(m["metric_name"]), string(m["metric_target_type"])
		if typ == "utilization" && (metric == ResourceTypeCPU || metric == ResourceTypeMemory) {
			return
		}
		h.OtherMetrics = appendUnique(h.OtherMetrics, strings.TrimSpace(metric+" "+typ))
	case srcHPAMin:
		h.MinReplicas = replicaCount(v)
	case srcHPAMax:
		h.MaxReplicas = replicaCount(v)
	case srcKEDA:
		h.ScaledObject = string(m["label_scaledobject_keda_sh_name"])
	}
}

func (p *snapshotParser) vpa(name string) *VPAInfo {
	vpa := p.vpas[name]
	if vpa == nil {
		vpa = &VPAInfo{Name: name}
		p.vpas[name] = vpa
	}
	return vpa
}

func (p *snapshotParser) addVPA(m model.Metric) {
	name := string(m["verticalpodautoscaler"])
	if name == "" {
		return
	}
	vpa := p.vpa(name)
	if mode := string(m["update_mode"]); mode != "" {
		vpa.UpdateMode = mode
	}
}

// addVPABound records a container policy bound; CPU is in cores, memory in bytes.
func (p *snapshotParser) addVPABound(m model.Metric, v float64) {
	name, container := string(m["verticalpodautoscaler"]), string(m["container"])
	if name == "" || container == "" || v < 0 || v > maxBoundValue {
		return
	}
	vpa := p.vpa(name)
	bound, res, ok := strings.Cut(string(m[boundLabel]), "_")
	if !ok {
		return
	}
	target := &vpa.MinAllowed
	if bound == "maxallowed" {
		target = &vpa.MaxAllowed
	} else if bound != "minallowed" {
		return
	}
	if *target == nil {
		*target = map[string]ResourceValues{}
	}
	rv := (*target)[container]
	switch res {
	case "cpu":
		rv.CPU = resource.NewMilliQuantity(int64(math.Round(v*1000)), resource.DecimalSI)
	case "memory":
		rv.Memory = resource.NewQuantity(int64(math.Round(v)), resource.BinarySI)
	default:
		return
	}
	(*target)[container] = rv
}

// maxBoundValue bounds VPA bound samples so the conversions cannot overflow.
const maxBoundValue = 1 << 50

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
		sort.Strings(h.OtherMetrics)
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

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/writeback"
	"github.com/sartorproj/sartor/internal/gitops/writer"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender"
	"github.com/sartorproj/sartor/internal/recommender/scaling"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

// WorkloadSource selects where the controller reads workload state from.
type WorkloadSource string

const (
	// WorkloadSourceKubernetes reads workloads, quotas and autoscalers from the
	// Kubernetes API of the cluster the controller runs in.
	WorkloadSourceKubernetes WorkloadSource = "kubernetes"
	// WorkloadSourceMetrics reads them from kube-state-metrics series in the
	// metrics backend (metrics-only mode); the target cluster's API is not used.
	WorkloadSourceMetrics WorkloadSource = "metrics"
)

// ParseWorkloadSource validates the --workload-source flag value.
func ParseWorkloadSource(s string) (WorkloadSource, error) {
	switch WorkloadSource(s) {
	case WorkloadSourceKubernetes, WorkloadSourceMetrics:
		return WorkloadSource(s), nil
	}
	return "", fmt.Errorf("invalid workload source %q: must be %q or %q", s, WorkloadSourceKubernetes, WorkloadSourceMetrics)
}

const (
	// ConditionTypeOOMKilled reports OOM kills read from metrics.
	ConditionTypeOOMKilled = "OOMKilled"
	// ConditionTypeScalingPointShifted reports that new requests move the
	// scaling point of an HPA with a Utilization target.
	ConditionTypeScalingPointShifted = "ScalingPointShifted"

	// ReasonPodsDisagree is set when a workload's pods report different
	// requests or limits; write-back is skipped until the next analysis.
	ReasonPodsDisagree = "PodsDisagree"

	labelKEDAScaledObject = "scaledobject.keda.sh/name"
)

var (
	gvkVPAList          = schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscalerList"}
	gvkScaledObjectList = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObjectList"}
)

// errTargetNotFound marks a missing target workload.
var errTargetNotFound = errors.New("target workload not found")

// workloadView is the workload state an analysis works from.
type workloadView struct {
	current     map[string]autoscalingv1alpha1.ResourceRequirements
	quota       *recommender.ResourceQuota
	autoscalers []autoscalingv1alpha1.DetectedAutoscaler
	// replicas is the desired replica count from kube-state-metrics, nil
	// when it is not reported.
	replicas *int32
	// disagreeing and oomKilled are only known in metrics-only mode.
	disagreeing []string
	oomKilled   []string
	// vpaPolicies holds the container policies of each detected VPA by name.
	vpaPolicies map[string][]scaling.VPAPolicy
}

func (r *TailoringReconciler) metricsOnly() bool {
	return r.WorkloadSource == WorkloadSourceMetrics
}

func (r *TailoringReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func targetWorkload(t *autoscalingv1alpha1.Tailoring) prometheus.Workload {
	return prometheus.Workload{Kind: t.Spec.Target.Kind, Name: t.Spec.Target.Name, Cluster: t.Spec.Target.Cluster}
}

// readWorkload reads the target's current resources, quota and autoscalers
// from the configured source. Errors wrapping errTargetNotFound mean the
// target is gone; others are read failures.
func (r *TailoringReconciler) readWorkload(
	ctx context.Context,
	t *autoscalingv1alpha1.Tailoring,
	promClient *prometheus.Client,
	logger logr.Logger,
) (*workloadView, error) {
	ns := t.TargetNamespace()
	if r.metricsOnly() {
		return readWorkloadFromMetrics(ctx, promClient, ns, targetWorkload(t))
	}

	current, err := r.getTargetResources(ctx, t)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("%w: %v", errTargetNotFound, err)
		}
		return nil, err
	}
	view := &workloadView{current: current, vpaPolicies: map[string][]scaling.VPAPolicy{}}
	if view.quota, err = r.getResourceQuota(ctx, ns); err != nil {
		logger.Error(err, "Failed to get ResourceQuota")
	}
	view.autoscalers = r.detectAutoscalersWithPolicies(ctx, ns, t.Spec.Target, logger, view.vpaPolicies)
	// The replica count only feeds reporting, so a failed query leaves it unset.
	if promClient != nil {
		if view.replicas, err = promClient.GetReplicas(ctx, ns, targetWorkload(t)); err != nil {
			logger.Error(err, "Failed to get replica count")
		}
	}
	return view, nil
}

func readWorkloadFromMetrics(ctx context.Context, promClient *prometheus.Client, ns string, wl prometheus.Workload) (*workloadView, error) {
	st, err := promClient.GetWorkloadState(ctx, ns, wl)
	if err != nil {
		return nil, err
	}
	if !st.Exists {
		return nil, fmt.Errorf("%w: no kube-state-metrics series for %s %s/%s", errTargetNotFound, wl.Kind, ns, wl.Name)
	}
	if st.Pods == 0 || len(st.Containers) == 0 {
		return nil, fmt.Errorf("%w: %s %s/%s has no Pending or Running pods", errTargetNotFound, wl.Kind, ns, wl.Name)
	}

	view := &workloadView{
		current:     map[string]autoscalingv1alpha1.ResourceRequirements{},
		replicas:    st.Replicas,
		disagreeing: st.Disagreeing,
		oomKilled:   st.OOMKilled,
		vpaPolicies: map[string][]scaling.VPAPolicy{},
	}
	for name, c := range st.Containers {
		view.current[name] = autoscalingv1alpha1.ResourceRequirements{
			Requests: metricValues(c.Requests),
			Limits:   metricValues(c.Limits),
		}
	}
	if st.Quota != nil {
		view.quota = &recommender.ResourceQuota{CPULimit: st.Quota.CPULimit, MemoryLimit: st.Quota.MemoryLimit}
	}
	seenSO := map[string]bool{}
	for _, h := range st.HPAs {
		var targets []autoscalingv1alpha1.UtilizationTarget
		for _, res := range h.UtilizationResources {
			if pct, ok := h.UtilizationTargets[res]; ok {
				targets = append(targets, autoscalingv1alpha1.UtilizationTarget{Resource: res, AverageUtilization: pct})
			}
		}
		view.autoscalers = append(view.autoscalers, autoscalingv1alpha1.DetectedAutoscaler{
			Kind:                 autoscalingv1alpha1.AutoscalerKindHPA,
			Name:                 h.Name,
			UtilizationResources: h.UtilizationResources,
			ManagedBy:            h.ScaledObject,
			MinReplicas:          h.MinReplicas,
			MaxReplicas:          h.MaxReplicas,
			UtilizationTargets:   targets,
			OtherMetrics:         h.OtherMetrics,
		})
		if h.ScaledObject != "" && !seenSO[h.ScaledObject] {
			seenSO[h.ScaledObject] = true
			view.autoscalers = append(view.autoscalers, autoscalingv1alpha1.DetectedAutoscaler{
				Kind: autoscalingv1alpha1.AutoscalerKindScaledObject,
				Name: h.ScaledObject,
			})
		}
	}
	for _, v := range st.VPAs {
		view.autoscalers = append(view.autoscalers, autoscalingv1alpha1.DetectedAutoscaler{
			Kind:       autoscalingv1alpha1.AutoscalerKindVPA,
			Name:       v.Name,
			UpdateMode: v.UpdateMode,
		})
		view.vpaPolicies[v.Name] = metricsVPAPolicies(v)
	}
	sortAutoscalers(view.autoscalers)
	return view, nil
}

func metricValues(v prometheus.ResourceValues) *autoscalingv1alpha1.ResourceValues {
	if v.CPU == nil && v.Memory == nil {
		return nil
	}
	return &autoscalingv1alpha1.ResourceValues{CPU: v.CPU, Memory: v.Memory}
}

func sortAutoscalers(a []autoscalingv1alpha1.DetectedAutoscaler) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Kind != a[j].Kind {
			return a[i].Kind < a[j].Kind
		}
		return a[i].Name < a[j].Name
	})
}

// detectAutoscalers lists the HPAs, VPAs and KEDA ScaledObjects that target
// the workload. Missing CRDs or permissions only mean nothing is detected.
func (r *TailoringReconciler) detectAutoscalers(ctx context.Context, ns string, target autoscalingv1alpha1.TargetRef, logger logr.Logger) []autoscalingv1alpha1.DetectedAutoscaler {
	return r.detectAutoscalersWithPolicies(ctx, ns, target, logger, nil)
}

// detectAutoscalersWithPolicies is detectAutoscalers that also records the
// container policies of each VPA in policies, when not nil.
func (r *TailoringReconciler) detectAutoscalersWithPolicies(
	ctx context.Context,
	ns string,
	target autoscalingv1alpha1.TargetRef,
	logger logr.Logger,
	policies map[string][]scaling.VPAPolicy,
) []autoscalingv1alpha1.DetectedAutoscaler {
	reader := r.apiReader()
	var out []autoscalingv1alpha1.DetectedAutoscaler
	ignore := func(what string, err error) {
		if !meta.IsNoMatchError(err) {
			logger.V(1).Info("Autoscaler detection skipped", "kind", what, "error", err.Error())
		}
	}

	hpas := &autoscalingv2.HorizontalPodAutoscalerList{}
	if err := reader.List(ctx, hpas, client.InNamespace(ns)); err != nil {
		ignore("HorizontalPodAutoscaler", err)
	}
	for _, h := range hpas.Items {
		ref := h.Spec.ScaleTargetRef
		if ref.Kind != target.Kind || ref.Name != target.Name {
			continue
		}
		targets, other := hpaMetricDetails(h.Spec.Metrics)
		maxReplicas := h.Spec.MaxReplicas
		out = append(out, autoscalingv1alpha1.DetectedAutoscaler{
			Kind:                 autoscalingv1alpha1.AutoscalerKindHPA,
			Name:                 h.Name,
			UtilizationResources: utilizationResources(h.Spec.Metrics),
			ManagedBy:            h.Labels[labelKEDAScaledObject],
			MinReplicas:          h.Spec.MinReplicas,
			MaxReplicas:          &maxReplicas,
			UtilizationTargets:   targets,
			OtherMetrics:         other,
		})
	}

	vpas := &unstructured.UnstructuredList{}
	vpas.SetGroupVersionKind(gvkVPAList)
	if err := reader.List(ctx, vpas, client.InNamespace(ns)); err != nil {
		ignore("VerticalPodAutoscaler", err)
	}
	for _, v := range vpas.Items {
		kind, _, _ := unstructured.NestedString(v.Object, "spec", "targetRef", "kind")
		name, _, _ := unstructured.NestedString(v.Object, "spec", "targetRef", "name")
		if kind != target.Kind || name != target.Name {
			continue
		}
		mode, _, _ := unstructured.NestedString(v.Object, "spec", "updatePolicy", "updateMode")
		if mode == "" {
			mode = "Auto"
		}
		out = append(out, autoscalingv1alpha1.DetectedAutoscaler{
			Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: v.GetName(), UpdateMode: mode,
		})
		if policies != nil {
			policies[v.GetName()] = vpaPolicies(v.Object)
		}
	}

	scaledObjects := &unstructured.UnstructuredList{}
	scaledObjects.SetGroupVersionKind(gvkScaledObjectList)
	if err := reader.List(ctx, scaledObjects, client.InNamespace(ns)); err != nil {
		ignore("ScaledObject", err)
	}
	for _, so := range scaledObjects.Items {
		kind, _, _ := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "kind")
		name, _, _ := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "name")
		if kind == "" {
			kind = "Deployment"
		}
		if kind != target.Kind || name != target.Name {
			continue
		}
		out = append(out, scaledObjectDetails(so.GetName(), so.Object))
	}
	sortAutoscalers(out)
	return out
}

// hpaMetricDetails returns an HPA's cpu/memory Utilization targets with a
// value, and a description of its other metrics.
func hpaMetricDetails(metrics []autoscalingv2.MetricSpec) ([]autoscalingv1alpha1.UtilizationTarget, []string) {
	var targets []autoscalingv1alpha1.UtilizationTarget
	var other []string
	resourceMetric := func(name corev1.ResourceName, container string, t autoscalingv2.MetricTarget) {
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
			other = append(other, strings.TrimSpace(fmt.Sprintf("%s %s", name, t.Type)))
			return
		}
		if t.Type != autoscalingv2.UtilizationMetricType {
			other = append(other, fmt.Sprintf("%s %s", name, t.Type))
			return
		}
		if t.AverageUtilization != nil && *t.AverageUtilization > 0 {
			targets = append(targets, autoscalingv1alpha1.UtilizationTarget{
				Resource: string(name), Container: container, AverageUtilization: *t.AverageUtilization,
			})
		}
	}
	for _, m := range metrics {
		switch {
		case m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil:
			resourceMetric(m.Resource.Name, "", m.Resource.Target)
		case m.Type == autoscalingv2.ContainerResourceMetricSourceType && m.ContainerResource != nil:
			resourceMetric(m.ContainerResource.Name, m.ContainerResource.Container, m.ContainerResource.Target)
		case m.Type == autoscalingv2.PodsMetricSourceType && m.Pods != nil:
			other = append(other, "Pods "+m.Pods.Metric.Name)
		case m.Type == autoscalingv2.ObjectMetricSourceType && m.Object != nil:
			other = append(other, "Object "+m.Object.Metric.Name)
		case m.Type == autoscalingv2.ExternalMetricSourceType && m.External != nil:
			other = append(other, "External "+m.External.Metric.Name)
		default:
			other = append(other, string(m.Type))
		}
	}
	sortTargets(targets)
	sort.Strings(other)
	return targets, other
}

func sortTargets(t []autoscalingv1alpha1.UtilizationTarget) {
	sort.SliceStable(t, func(i, j int) bool {
		if t[i].Resource != t[j].Resource {
			return t[i].Resource < t[j].Resource
		}
		return t[i].Container < t[j].Container
	})
}

// KEDA defaults for a ScaledObject without replica counts.
const (
	kedaDefaultMinReplicas = 0
	kedaDefaultMaxReplicas = 100
)

// scaledObjectDetails reads a KEDA ScaledObject's replica bounds and
// triggers: cpu/memory triggers with metricType Utilization become targets,
// the others are listed by type.
func scaledObjectDetails(name string, obj map[string]interface{}) autoscalingv1alpha1.DetectedAutoscaler {
	d := autoscalingv1alpha1.DetectedAutoscaler{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: name}
	replicas := func(field string, def int32) *int32 {
		n := def
		if v, found, _ := unstructured.NestedFieldNoCopy(obj, "spec", field); found {
			if f, ok := toFloat(v); ok && f >= 0 && f <= math.MaxInt32 {
				n = int32(f)
			}
		}
		return &n
	}
	d.MinReplicas = replicas("minReplicaCount", kedaDefaultMinReplicas)
	d.MaxReplicas = replicas("maxReplicaCount", kedaDefaultMaxReplicas)

	triggers, _, _ := unstructured.NestedSlice(obj, "spec", "triggers")
	for _, raw := range triggers {
		t, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(t, "type")
		metricType, _, _ := unstructured.NestedString(t, "metricType")
		if metricType == "" {
			metricType, _, _ = unstructured.NestedString(t, "metadata", "type")
		}
		if typ != "cpu" && typ != "memory" {
			d.OtherMetrics = append(d.OtherMetrics, typ)
			continue
		}
		value, _, _ := unstructured.NestedString(t, "metadata", "value")
		pct, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if metricType != "Utilization" || err != nil || pct < 1 || pct > math.MaxInt32 || math.IsNaN(pct) {
			d.OtherMetrics = append(d.OtherMetrics, strings.TrimSpace(typ+" "+metricType))
			continue
		}
		container, _, _ := unstructured.NestedString(t, "metadata", "containerName")
		d.UtilizationTargets = append(d.UtilizationTargets, autoscalingv1alpha1.UtilizationTarget{
			Resource: typ, Container: container, AverageUtilization: int32(math.Round(pct)),
		})
		if !contains(d.UtilizationResources, typ) {
			d.UtilizationResources = append(d.UtilizationResources, typ)
		}
	}
	sort.Strings(d.UtilizationResources)
	sortTargets(d.UtilizationTargets)
	sort.Strings(d.OtherMetrics)
	return d
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, !math.IsNaN(n)
	}
	return 0, false
}

// vpaPolicies reads the container policies of a VPA object.
func vpaPolicies(obj map[string]interface{}) []scaling.VPAPolicy {
	raw, _, _ := unstructured.NestedSlice(obj, "spec", "resourcePolicy", "containerPolicies")
	var out []scaling.VPAPolicy
	for _, r := range raw {
		p, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		pol := scaling.VPAPolicy{}
		pol.ContainerName, _, _ = unstructured.NestedString(p, "containerName")
		pol.Mode, _, _ = unstructured.NestedString(p, "mode")
		pol.ControlledResources, _, _ = unstructured.NestedStringSlice(p, "controlledResources")
		pol.MinAllowed = boundValues(p, "minAllowed")
		pol.MaxAllowed = boundValues(p, "maxAllowed")
		if pol.ContainerName != "" {
			out = append(out, pol)
		}
	}
	return out
}

func boundValues(policy map[string]interface{}, key string) *autoscalingv1alpha1.ResourceValues {
	m, _, _ := unstructured.NestedMap(policy, key)
	parse := func(res string) *resource.Quantity {
		switch v := m[res].(type) {
		case string:
			if q, err := resource.ParseQuantity(v); err == nil && q.Sign() >= 0 {
				return &q
			}
		case int64, float64, int32, int:
			if f, ok := toFloat(v); ok && f >= 0 {
				if q, err := resource.ParseQuantity(strconv.FormatFloat(f, 'f', -1, 64)); err == nil {
					return &q
				}
			}
		}
		return nil
	}
	rv := &autoscalingv1alpha1.ResourceValues{CPU: parse("cpu"), Memory: parse("memory")}
	if rv.CPU == nil && rv.Memory == nil {
		return nil
	}
	return rv
}

// utilizationResources returns the resources an HPA scales on relative to requests.
func utilizationResources(metrics []autoscalingv2.MetricSpec) []string {
	var out []string
	add := func(name corev1.ResourceName, target autoscalingv2.MetricTarget) {
		if target.Type != autoscalingv2.UtilizationMetricType {
			return
		}
		if (name == corev1.ResourceCPU || name == corev1.ResourceMemory) && !contains(out, string(name)) {
			out = append(out, string(name))
		}
	}
	for _, m := range metrics {
		switch {
		case m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil:
			add(m.Resource.Name, m.Resource.Target)
		case m.Type == autoscalingv2.ContainerResourceMetricSourceType && m.ContainerResource != nil:
			add(m.ContainerResource.Name, m.ContainerResource.Target)
		}
	}
	sort.Strings(out)
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// applyOOMFloor keeps the memory request and limit of OOM-killed containers
// at least at their current values: a container that was OOM-killed must not
// get less memory.
func applyOOMFloor(recs []strategy.Recommendation, current map[string]autoscalingv1alpha1.ResourceRequirements, oomKilled []string) {
	for i := range recs {
		if !contains(oomKilled, recs[i].ContainerName) {
			continue
		}
		cur := current[recs[i].ContainerName]
		if cur.Requests != nil {
			recs[i].Requests.Memory = maxQuantity(recs[i].Requests.Memory, cur.Requests.Memory)
		}
		if cur.Limits != nil {
			recs[i].Limits.Memory = maxQuantity(recs[i].Limits.Memory, cur.Limits.Memory)
		}
		strategy.EnsureLimitsCoverRequests(&recs[i])
	}
}

func maxQuantity(rec, floor *resource.Quantity) *resource.Quantity {
	if floor == nil || (rec != nil && rec.Cmp(*floor) >= 0) {
		return rec
	}
	q := floor.DeepCopy()
	return &q
}

// setWorkloadStatus records the detected autoscalers and OOM kills.
func (r *TailoringReconciler) setWorkloadStatus(t *autoscalingv1alpha1.Tailoring, view *workloadView) {
	t.Status.Autoscalers = view.autoscalers
	t.Status.VPADetected = false
	for _, a := range view.autoscalers {
		if a.Kind == autoscalingv1alpha1.AutoscalerKindVPA {
			t.Status.VPADetected = true
		}
	}
	if !r.metricsOnly() {
		return
	}
	t.Status.OOMKilledContainers = view.oomKilled
	if len(view.oomKilled) > 0 {
		r.setCondition(t, ConditionTypeOOMKilled, metav1.ConditionTrue, "LastTerminationOOMKilled",
			fmt.Sprintf("Last termination of %s was an OOM kill; memory is not lowered below the current values",
				strings.Join(view.oomKilled, ", ")))
	} else {
		r.setCondition(t, ConditionTypeOOMKilled, metav1.ConditionFalse, "NoOOMKills", "No OOM kill reported for the target's pods")
	}
}

// assumedKeptBy returns the ScaledObjects whose triggers a raw write-back
// rescales with the requests, for analyses without a write-back result.
func assumedKeptBy(t *autoscalingv1alpha1.Tailoring) map[string]bool {
	if t.Spec.WriteBack.Type != autoscalingv1alpha1.WriteBackTypeRaw {
		return nil
	}
	kept := map[string]bool{}
	for _, a := range t.Status.Autoscalers {
		if a.Kind == autoscalingv1alpha1.AutoscalerKindScaledObject {
			kept[a.Name] = true
		}
	}
	return kept
}

// setScalingPointCondition flags HPAs whose Utilization targets are relative
// to requests the recommendation changes. keptBy names ScaledObjects whose
// triggers were rescaled in Git, so their HPAs keep the scaling point.
func (r *TailoringReconciler) setScalingPointCondition(
	t *autoscalingv1alpha1.Tailoring,
	recs []strategy.Recommendation,
	current map[string]autoscalingv1alpha1.ResourceRequirements,
	keptBy map[string]bool,
) {
	hasHPA := false
	for _, a := range t.Status.Autoscalers {
		hasHPA = hasHPA || a.Kind == autoscalingv1alpha1.AutoscalerKindHPA
	}
	if !hasHPA {
		meta.RemoveStatusCondition(&t.Status.Conditions, ConditionTypeScalingPointShifted)
		return
	}

	updates := make([]writer.ResourceUpdate, 0, len(recs))
	cur := map[string]*writer.ResourceValues{}
	for _, rec := range recs {
		updates = append(updates, writer.ResourceUpdate{
			ContainerName: rec.ContainerName,
			Requests:      &writer.ResourceValues{CPU: rec.Requests.CPU, Memory: rec.Requests.Memory},
		})
		if c := current[rec.ContainerName]; c.Requests != nil {
			cur[rec.ContainerName] = &writer.ResourceValues{CPU: c.Requests.CPU, Memory: c.Requests.Memory}
		}
	}
	aligned := alignedResources(t)
	changed := writeback.WithoutResources(writeback.ChangedRequests(updates, cur), aligned)
	shifted := writeback.ShiftedHPAs(t.Status.Autoscalers, changed, keptBy)
	if len(shifted) == 0 {
		if len(aligned) > 0 {
			r.setCondition(t, ConditionTypeScalingPointShifted, metav1.ConditionFalse, "SizedForUtilizationTarget",
				"The "+strings.Join(aligned, "/")+" requests are sized so the HPA Utilization target matches the typical per-pod usage at the typical replica count")
			return
		}
		r.setCondition(t, ConditionTypeScalingPointShifted, metav1.ConditionFalse, "ScalingPointKept",
			"No HPA Utilization target depends on a changed request")
		return
	}
	names := make([]string, 0, len(shifted))
	for name, res := range shifted {
		names = append(names, fmt.Sprintf("%s (%s)", name, strings.Join(res, "/")))
	}
	sort.Strings(names)
	r.setCondition(t, ConditionTypeScalingPointShifted, metav1.ConditionTrue, "UtilizationRelativeToRequests",
		"HPA "+strings.Join(names, ", ")+": Utilization targets are relative to requests; the recommended requests shift the scaling point")
}

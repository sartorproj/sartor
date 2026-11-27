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
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

const (
	mmTailoringNS = "apps"
	mmPath        = "deploy/web.yaml"
	mmManifest    = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              cpu: 250m
              memory: 512Mi
            limits:
              memory: 1Gi
---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: web
spec:
  targetRef:
    kind: Deployment
    name: web
  updatePolicy:
    updateMode: Initial
  resourcePolicy:
    containerPolicies:
      - containerName: app
        maxAllowed:
          cpu: 300m
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: web
spec:
  scaleTargetRef:
    name: web
  triggers:
    - type: cpu
      metricType: Utilization
      metadata:
        value: "60"
    - type: rabbitmq
      metadata:
        queueName: jobs
        value: "20"
`
)

type promSample struct {
	labels map[string]string
	value  float64
}

// workloadSnapshot describes what kube-state-metrics reports for one namespace.
type workloadSnapshot struct {
	missing   bool
	pods      int
	cpu       float64 // request, cores
	minCPU    float64 // lowest request across pods; 0 means cpu
	memory    float64 // request, bytes
	memLimit  float64
	oomKilled bool
	extra     []promSample
}

func (s workloadSnapshot) samples() []promSample {
	if s.missing {
		return nil
	}
	tag := func(source, agg string, v float64, kv ...string) promSample {
		l := map[string]string{"sartor_source": source}
		if agg != "" {
			l["sartor_agg"] = agg
		}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		return promSample{labels: l, value: v}
	}
	minCPU := s.minCPU
	if minCPU == 0 {
		minCPU = s.cpu
	}
	n := float64(s.pods)
	out := []promSample{tag("exists", "", 1), tag("pods", "count", n)}
	if s.pods == 0 {
		return out
	}
	value := func(source, res string, maxV, minV float64) {
		kv := []string{"container", "app"}
		if res != "" {
			kv = append(kv, "resource", res)
		}
		out = append(out, tag(source, "max", maxV, kv...), tag(source, "min", minV, kv...), tag(source, "count", n, kv...))
	}
	value("info", "", 1, 1)
	value("requests", "cpu", s.cpu, minCPU)
	value("requests", "memory", s.memory, s.memory)
	if s.memLimit > 0 {
		value("limits", "memory", s.memLimit, s.memLimit)
	}
	if s.oomKilled {
		out = append(out, tag("oom", "", 1, "container", "app"))
	}
	return append(out, s.extra...)
}

func hpaSamples(name string, utilization []string, scaledObject string) []promSample {
	out := []promSample{{labels: map[string]string{"sartor_source": "hpa", "horizontalpodautoscaler": name}, value: 1}}
	for _, r := range utilization {
		out = append(out, promSample{labels: map[string]string{"sartor_source": "hpa_target", "horizontalpodautoscaler": name, "metric_name": r}, value: 60})
	}
	if scaledObject != "" {
		out = append(out, promSample{labels: map[string]string{
			"sartor_source": "keda", "horizontalpodautoscaler": name, "label_scaledobject_keda_sh_name": scaledObject,
		}, value: 1})
	}
	return out
}

func vpaSamples(name, mode string) []promSample {
	return []promSample{
		{labels: map[string]string{"sartor_source": "vpa", "verticalpodautoscaler": name}, value: 1},
		{labels: map[string]string{"sartor_source": "vpa_mode", "verticalpodautoscaler": name, "update_mode": mode}, value: 1},
	}
}

// metricsBackend fakes a metrics backend holding kube-state-metrics and
// cAdvisor series for several namespaces.
type metricsBackend struct {
	mu        sync.Mutex
	queries   []string
	snapshots map[string]workloadSnapshot
	cpuUsage  float64
	memUsage  float64
	noUsage   bool
	fail      bool
	// history, when set, answers range queries with one "app" series whose
	// value at a timestamp it returns; otherwise range queries get a vector.
	history func(ts float64, memory bool) float64
}

func (b *metricsBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.FormValue("query")
	b.mu.Lock()
	b.queries = append(b.queries, q)
	fail, snapshots := b.fail, b.snapshots
	cpuUsage, memUsage, noUsage := b.cpuUsage, b.memUsage, b.noUsage
	history := b.history
	b.mu.Unlock()
	if fail {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
		return
	}
	if history != nil && strings.HasSuffix(r.URL.Path, "/query_range") {
		start, _ := strconv.ParseFloat(r.FormValue("start"), 64)
		end, _ := strconv.ParseFloat(r.FormValue("end"), 64)
		step, _ := strconv.ParseFloat(r.FormValue("step"), 64)
		memory := strings.Contains(q, "container_memory_working_set_bytes")
		var values [][]interface{}
		for ts := start; ts <= end; ts += step {
			values = append(values, []interface{}{ts, strconv.FormatFloat(history(ts, memory), 'f', -1, 64)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{"resultType": "matrix", "result": []map[string]interface{}{
				{"metric": map[string]string{"container": "app"}, "values": values},
			}},
		})
		return
	}

	var samples []promSample
	switch {
	case q == "vector(1)":
		samples = []promSample{{labels: map[string]string{}, value: 1}}
	case strings.Contains(q, "sartor_source"):
		for ns, s := range snapshots {
			if strings.Contains(q, `namespace="`+ns+`"`) {
				samples = s.samples()
			}
		}
	case noUsage:
	case strings.Contains(q, "container_cpu_usage_seconds_total"):
		samples = []promSample{{labels: map[string]string{"container": "app"}, value: cpuUsage}}
	case strings.Contains(q, "container_memory_working_set_bytes"):
		samples = []promSample{{labels: map[string]string{"container": "app"}, value: memUsage}}
	}

	result := make([]map[string]interface{}, 0, len(samples))
	for _, s := range samples {
		result = append(result, map[string]interface{}{
			"metric": s.labels,
			"value":  []interface{}{1234567890.0, strconv.FormatFloat(s.value, 'f', -1, 64)},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   map[string]interface{}{"resultType": "vector", "result": result},
	})
}

func (b *metricsBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queries)
}

func (b *metricsBackend) all() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.queries...)
}

func (b *metricsBackend) set(f func(*metricsBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(b)
}

const (
	mi = 1024 * 1024
)

func agreeing() workloadSnapshot {
	return workloadSnapshot{pods: 3, cpu: 0.25, memory: 512 * mi, memLimit: 1024 * mi}
}

type mmFixture struct {
	*wbFixture
	backend *metricsBackend
	now     time.Time
	ctx     context.Context
}

// newMetricsModeFixture runs the reconciler in metrics-only mode. The fake
// cluster holds no workloads, so every workload read must come from metrics.
func newMetricsModeFixture(t *testing.T, tailorings ...*autoscalingv1alpha1.Tailoring) *mmFixture {
	t.Helper()
	backend := &metricsBackend{
		snapshots: map[string]workloadSnapshot{mmTailoringNS: agreeing()},
		cpuUsage:  1, memUsage: 600 * mi,
	}
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)

	f := newWBFixture(t, true, wbAtelier(server.URL, wbControllerNS), mmFitProfile())
	for _, tl := range tailorings {
		require.NoError(t, f.client.Create(context.Background(), tl))
	}
	f.git.SetFile("main", mmPath, mmManifest)

	fx := &mmFixture{wbFixture: f, backend: backend, now: time.Now().Truncate(time.Second), ctx: context.Background()}
	f.reconciler.WorkloadSource = WorkloadSourceMetrics
	f.reconciler.AnalysisInterval = 6 * time.Hour
	f.reconciler.now = func() time.Time { return fx.now }
	f.reconciler.jitter = func(*autoscalingv1alpha1.Tailoring) float64 { return 0 }
	return fx
}

func mmFitProfile() *autoscalingv1alpha1.FitProfile {
	return &autoscalingv1alpha1.FitProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "balanced", Namespace: mmTailoringNS},
		Spec: autoscalingv1alpha1.FitProfileSpec{
			Strategy:   "percentile",
			Parameters: &runtime.RawExtension{Raw: []byte(`{"maxChangePercent": 50}`)},
		},
	}
}

func mmTailoring(name string, mutate ...func(*autoscalingv1alpha1.Tailoring)) *autoscalingv1alpha1.Tailoring {
	tl := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mmTailoringNS, UID: types.UID("uid-" + name)},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target:        autoscalingv1alpha1.TargetRef{Kind: "Deployment", Name: "web"},
			FitProfileRef: autoscalingv1alpha1.FitProfileRef{Name: "balanced"},
			WriteBack: autoscalingv1alpha1.WriteBackConfig{
				Repository: "https://github.com/acme/gitops.git",
				Path:       mmPath,
				Type:       autoscalingv1alpha1.WriteBackTypeRaw,
			},
		},
	}
	for _, m := range mutate {
		m(tl)
	}
	return tl
}

func (fx *mmFixture) reconcile(t *testing.T, name string) (ctrl.Result, *autoscalingv1alpha1.Tailoring) {
	t.Helper()
	key := types.NamespacedName{Namespace: mmTailoringNS, Name: name}
	res, err := fx.reconciler.Reconcile(fx.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	tl := &autoscalingv1alpha1.Tailoring{}
	require.NoError(t, fx.client.Get(fx.ctx, key, tl))
	return res, tl
}

func condition(tl *autoscalingv1alpha1.Tailoring, condType string) *metav1.Condition {
	return meta.FindStatusCondition(tl.Status.Conditions, condType)
}

// TestMetricsMode_WriteBackWithAutoscalers: a workload with a VPA and a KEDA
// ScaledObject (whose HPA targets cpu utilization) is read entirely from
// metrics; the write-back adjusts the request, the VPA bounds and the cpu
// trigger, leaves the non-cpu trigger alone, and keeps the HPA scaling point.
func TestMetricsMode_WriteBackWithAutoscalers(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	snap := agreeing()
	snap.extra = append(hpaSamples("keda-hpa-web", []string{"cpu"}, "web"), vpaSamples("web", "Initial")...)
	fx.backend.snapshots[mmTailoringNS] = snap

	_, tl := fx.reconcile(t, "web")

	require.True(t, tl.Status.TargetFound)
	assert.Equal(t, "Ready", reason(tl, ConditionTypeReady), tl.Status.Conditions)
	assert.Equal(t, 5, fx.backend.count(), "1 snapshot + 4 usage queries")
	assert.Equal(t, []autoscalingv1alpha1.DetectedAutoscaler{
		{Kind: autoscalingv1alpha1.AutoscalerKindHPA, Name: "keda-hpa-web", UtilizationResources: []string{"cpu"}, ManagedBy: "web"},
		{Kind: autoscalingv1alpha1.AutoscalerKindScaledObject, Name: "web"},
		{Kind: autoscalingv1alpha1.AutoscalerKindVPA, Name: "web", UpdateMode: "Initial"},
	}, tl.Status.Autoscalers)
	assert.True(t, tl.Status.VPADetected)

	file, ok := fx.git.File("sartor/apps/web", mmPath)
	require.True(t, ok)
	assert.Contains(t, file, "cpu: 375m", "request raised, capped by maxChangePercent")
	assert.Contains(t, file, "maxAllowed:\n          cpu: 375m", "VPA bound follows the request")
	assert.Contains(t, file, `value: "40"`, "cpu trigger: 60% of 250m = 40% of 375m")
	assert.Contains(t, file, "queueName: jobs\n        value: \"20\"", "non-cpu/memory trigger untouched")

	cond := condition(tl, ConditionTypeScalingPointShifted)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "the rescaled ScaledObject keeps the scaling point: %s", cond.Message)

	body := fx.git.Body(tl.Status.PRNumber)
	assert.Contains(t, body, "ScaledObject `web` (in `deploy/web.yaml`): adjusted")
	assert.Contains(t, body, "has updateMode Initial: the current values were read from pods")
	assert.NotContains(t, body, "scaling point shifts")

	// Next cycle with the PR still open: the branch already holds consistent
	// values, nothing is rewritten and the scaling point is still kept.
	commits := fx.git.Commits()
	fx.now = fx.now.Add(7 * time.Hour)
	_, tl = fx.reconcile(t, "web")
	assert.Equal(t, commits, fx.git.Commits())
	cond = condition(tl, ConditionTypeScalingPointShifted)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, cond.Message)
	again, _ := fx.git.File("sartor/apps/web", mmPath)
	assert.Equal(t, file, again, "the trigger is not rescaled twice")
}

// TestMetricsMode_HPAScalingPointShift: a plain HPA on cpu utilization cannot
// be adjusted, so the shift is flagged in the condition and the PR.
func TestMetricsMode_HPAScalingPointShift(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	snap := agreeing()
	snap.extra = append(hpaSamples("web", []string{"cpu"}, ""), vpaSamples("web", "Off")...)
	fx.backend.snapshots[mmTailoringNS] = snap
	fx.git.SetFile("main", mmPath, strings.Replace(mmManifest, "updateMode: Initial", "updateMode: Off", 1))

	_, tl := fx.reconcile(t, "web")

	cond := condition(tl, ConditionTypeScalingPointShifted)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "HPA web (cpu)")
	body := fx.git.Body(tl.Status.PRNumber)
	assert.Contains(t, body, "**HorizontalPodAutoscaler `web`: scaling point shifts.**")
	assert.Contains(t, body, "VerticalPodAutoscaler `web` (in `deploy/web.yaml`): left unchanged: updateMode is Off")
	file, _ := fx.git.File("sartor/apps/web", mmPath)
	assert.Contains(t, file, "maxAllowed:\n          cpu: 300m", "a VPA with updateMode Off keeps its bounds")
}

// TestMetricsMode_DisagreeingPodsSkipWriteBack: during a rollout pods report
// different requests; nothing is written and the next cycle retries.
func TestMetricsMode_DisagreeingPodsSkipWriteBack(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	rollout := agreeing()
	rollout.minCPU = 0.1
	fx.backend.snapshots[mmTailoringNS] = rollout

	res, tl := fx.reconcile(t, "web")

	assert.Equal(t, ReasonPodsDisagree, reason(tl, ConditionTypeReady))
	assert.Contains(t, condition(tl, ConditionTypeReady).Message, "app")
	assert.Empty(t, fx.git.Calls, "no Git call while pods disagree")
	assert.NotNil(t, tl.Status.LastAnalysis, "the analysis itself is recorded")
	assert.Zero(t, tl.Status.ConsecutiveFailures, "not a failure: retried on the regular cycle")
	assert.Equal(t, 6*time.Hour, res.RequeueAfter)

	// Within the interval nothing is retried.
	fx.now = fx.now.Add(time.Hour)
	before := fx.backend.count()
	fx.reconcile(t, "web")
	assert.Equal(t, before, fx.backend.count())

	// Next cycle: the rollout is done.
	fx.backend.set(func(b *metricsBackend) { b.snapshots[mmTailoringNS] = agreeing() })
	fx.now = fx.now.Add(5 * time.Hour)
	_, tl = fx.reconcile(t, "web")
	assert.Equal(t, "Ready", reason(tl, ConditionTypeReady))
	assert.Equal(t, 1, tl.Status.PRNumber)
}

// TestMetricsMode_TargetDisappears covers a target that vanishes between
// reads: nothing is written and the analysis backs off.
func TestMetricsMode_TargetDisappears(t *testing.T) {
	tests := []struct {
		name        string
		snapshot    workloadSnapshot
		noUsage     bool
		wantCond    string
		wantReason  string
		wantQueries int
	}{
		{name: "workload deleted", snapshot: workloadSnapshot{missing: true}, wantCond: ConditionTypeTargetFound, wantReason: "TargetNotFound", wantQueries: 1},
		{name: "workload scaled to zero / pods gone", snapshot: workloadSnapshot{pods: 0}, wantCond: ConditionTypeTargetFound, wantReason: "TargetNotFound", wantQueries: 1},
		{name: "deleted after the snapshot, before the usage queries", snapshot: agreeing(), noUsage: true,
			wantCond: ConditionTypeAnalyzed, wantReason: "NoMetrics", wantQueries: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newMetricsModeFixture(t, mmTailoring("web"))
			fx.backend.snapshots[mmTailoringNS] = tt.snapshot
			fx.backend.noUsage = tt.noUsage

			res, tl := fx.reconcile(t, "web")

			assert.Equal(t, tt.wantReason, reason(tl, tt.wantCond))
			assert.Equal(t, tt.wantQueries, fx.backend.count())
			assert.Empty(t, fx.git.Calls)
			assert.Equal(t, int32(1), tl.Status.ConsecutiveFailures)
			assert.Equal(t, DefaultRequeueInterval, res.RequeueAfter)
		})
	}
}

// TestMetricsMode_OOMKilledKeepsMemory: low usage would lower memory, but the
// container was OOM-killed, so memory is not reduced and status shows it.
func TestMetricsMode_OOMKilledKeepsMemory(t *testing.T) {
	for _, oom := range []bool{false, true} {
		t.Run(fmt.Sprintf("oomKilled=%v", oom), func(t *testing.T) {
			fx := newMetricsModeFixture(t, mmTailoring("web"))
			snap := agreeing()
			snap.oomKilled = oom
			fx.backend.snapshots[mmTailoringNS] = snap
			fx.backend.memUsage = 100 * mi

			_, tl := fx.reconcile(t, "web")

			require.NotNil(t, tl.Status.LastAnalysis)
			rec := tl.Status.LastAnalysis.Containers[0].Recommended
			cond := condition(tl, ConditionTypeOOMKilled)
			require.NotNil(t, cond)
			if oom {
				assert.Equal(t, []string{"app"}, tl.Status.OOMKilledContainers)
				assert.Equal(t, metav1.ConditionTrue, cond.Status)
				assert.GreaterOrEqual(t, rec.Requests.Memory.Cmp(resource.MustParse("512Mi")), 0, "request %s", rec.Requests.Memory)
				assert.GreaterOrEqual(t, rec.Limits.Memory.Cmp(resource.MustParse("1Gi")), 0, "limit %s", rec.Limits.Memory)
			} else {
				assert.Empty(t, tl.Status.OOMKilledContainers)
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Negative(t, rec.Requests.Memory.Cmp(resource.MustParse("512Mi")), "without OOM kills memory may go down")
			}
		})
	}
}

// TestMetricsMode_TargetNamespace covers target.namespace set and unset, the
// same workload name in two namespaces and a namespace without series.
func TestMetricsMode_TargetNamespace(t *testing.T) {
	inNS := func(ns string) func(*autoscalingv1alpha1.Tailoring) {
		return func(tl *autoscalingv1alpha1.Tailoring) { tl.Spec.Target.Namespace = ns }
	}
	fx := newMetricsModeFixture(t,
		mmTailoring("own"),
		mmTailoring("blue", inNS("blue")),
		mmTailoring("green", inNS("green")),
		mmTailoring("nothing", inNS("empty")),
	)
	fx.reconciler.EnableGitWriteBack = false
	blue := agreeing()
	blue.cpu = 0.5
	green := agreeing()
	green.cpu = 2
	fx.backend.snapshots = map[string]workloadSnapshot{mmTailoringNS: agreeing(), "blue": blue, "green": green}

	scoped := func(name, ns string) {
		t.Helper()
		before := len(fx.backend.all())
		fx.reconcile(t, name)
		queries := fx.backend.all()[before:]
		require.NotEmpty(t, queries)
		for _, q := range queries {
			assert.Contains(t, q, `namespace="`+ns+`"`)
			for _, other := range []string{mmTailoringNS, "blue", "green", "empty"} {
				if other != ns {
					assert.NotContains(t, q, `namespace="`+other+`"`)
				}
			}
		}
	}

	scoped("own", mmTailoringNS)
	scoped("blue", "blue")
	scoped("green", "green")
	scoped("nothing", "empty")

	current := func(name string) string {
		tl := &autoscalingv1alpha1.Tailoring{}
		require.NoError(t, fx.client.Get(fx.ctx, types.NamespacedName{Namespace: mmTailoringNS, Name: name}, tl))
		require.NotNil(t, tl.Status.LastAnalysis, name)
		return tl.Status.LastAnalysis.Containers[0].Current.Requests.CPU.String()
	}
	assert.Equal(t, "250m", current("own"))
	assert.Equal(t, "500m", current("blue"))
	assert.Equal(t, "2", current("green"))

	_, nothing := fx.reconcile(t, "nothing")
	assert.Equal(t, "TargetNotFound", reason(nothing, ConditionTypeTargetFound))
	assert.Contains(t, condition(nothing, ConditionTypeTargetFound).Message, "empty/web")
}

// TestMetricsMode_WriteBackUsesTargetNamespace: the PR names the target
// namespace; the branch stays unique per Tailoring.
func TestMetricsMode_WriteBackUsesTargetNamespace(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web", func(tl *autoscalingv1alpha1.Tailoring) { tl.Spec.Target.Namespace = "blue" }))
	fx.backend.snapshots = map[string]workloadSnapshot{"blue": agreeing()}

	_, tl := fx.reconcile(t, "web")

	require.Equal(t, 1, tl.Status.PRNumber, tl.Status.Conditions)
	pr := fx.git.PullRequests()[0]
	assert.Equal(t, "chore(sartor): right-size Deployment blue/web", pr.Title)
	assert.Equal(t, "sartor/apps/web", tl.Status.BranchName)
}

// TestMetricsMode_ClusterLabel: with a cluster label every query is scoped to
// the Tailoring's cluster; without spec.target.cluster nothing is queried.
func TestMetricsMode_ClusterLabel(t *testing.T) {
	fx := newMetricsModeFixture(t,
		mmTailoring("east", func(tl *autoscalingv1alpha1.Tailoring) { tl.Spec.Target.Cluster = "east" }),
		mmTailoring("unscoped"),
	)
	fx.reconciler.EnableGitWriteBack = false
	atelier := &autoscalingv1alpha1.Atelier{}
	require.NoError(t, fx.client.Get(fx.ctx, types.NamespacedName{Name: "default"}, atelier))
	atelier.Spec.Prometheus.ClusterLabel = "cluster"
	require.NoError(t, fx.client.Update(fx.ctx, atelier))

	_, tl := fx.reconcile(t, "east")
	require.NotNil(t, tl.Status.LastAnalysis, tl.Status.Conditions)
	for _, q := range fx.backend.all() {
		assert.Equal(t, strings.Count(q, `namespace="`), strings.Count(q, `cluster="east", namespace="`), q)
	}

	before := fx.backend.count()
	_, tl = fx.reconcile(t, "unscoped")
	assert.Equal(t, before, fx.backend.count(), "no query without a cluster")
	assert.Contains(t, condition(tl, ConditionTypeAnalyzed).Message, "a target cluster is required")
}

// useARIMA switches the fixture's FitProfile to the arima strategy without
// a change limit, so the forecast is visible in the recommendation.
func (fx *mmFixture) useARIMA(t *testing.T) {
	t.Helper()
	fp := &autoscalingv1alpha1.FitProfile{}
	require.NoError(t, fx.client.Get(fx.ctx, types.NamespacedName{Namespace: mmTailoringNS, Name: "balanced"}, fp))
	fp.Spec.Strategy = "arima"
	fp.Spec.Parameters = &runtime.RawExtension{Raw: []byte(`{"maxChangePercent": 1000}`)}
	require.NoError(t, fx.client.Update(fx.ctx, fp))
}

// TestMetricsMode_ARIMAForecastsFromHistory: the arima strategy gets the
// usage history from two extra range queries and sizes from the forecast
// instead of the P95 (1 core) the percentile strategy would use.
func TestMetricsMode_ARIMAForecastsFromHistory(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	fx.reconciler.EnableGitWriteBack = false
	fx.useARIMA(t)
	fx.backend.set(func(b *metricsBackend) {
		b.history = func(ts float64, memory bool) float64 {
			wave := math.Sin(2 * math.Pi * ts / 86400)
			if memory {
				return 600*mi + 100*mi*wave
			}
			return 0.5 + 0.3*wave
		}
	})

	_, tl := fx.reconcile(t, "web")

	require.NotNil(t, tl.Status.LastAnalysis, tl.Status.Conditions)
	assert.Equal(t, 7, fx.backend.count(), "1 snapshot + 4 usage queries + 2 history queries")
	rec := tl.Status.LastAnalysis.Containers[0].Recommended
	cpu := rec.Requests.CPU.AsApproximateFloat64()
	assert.True(t, cpu >= 0.99*0.8*1.2 && cpu <= 1.07*0.8*1.2, "requests.cpu %s sized from the daily peak of 0.8 cores", rec.Requests.CPU)
	mem := rec.Requests.Memory.AsApproximateFloat64()
	assert.True(t, mem >= 0.99*700*mi*1.2 && mem <= 1.07*700*mi*1.2, "requests.memory %s sized from the daily peak of 700Mi", rec.Requests.Memory)
}

// TestMetricsMode_ARIMAWithoutHistory: when the history query fails the
// analysis still succeeds with the percentile values.
func TestMetricsMode_ARIMAWithoutHistory(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	fx.reconciler.EnableGitWriteBack = false
	fx.useARIMA(t)

	_, tl := fx.reconcile(t, "web")

	require.NotNil(t, tl.Status.LastAnalysis, tl.Status.Conditions)
	assert.Equal(t, 6, fx.backend.count(), "the failed CPU history query ends the history fetch")
	rec := tl.Status.LastAnalysis.Containers[0].Recommended
	assert.Equal(t, "1200m", rec.Requests.CPU.String(), "P95 of 1 core plus the 20% buffer")
}

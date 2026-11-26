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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

// TestSchedule_ManyTailoringsCreatedAtOnceAreSpread: Tailorings created in the
// same second get their first analyses spread over the startup window, and
// later analyses stay spread even if they all ran at the same moment.
func TestSchedule_ManyTailoringsCreatedAtOnceAreSpread(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &TailoringReconciler{AnalysisInterval: 6 * time.Hour, now: func() time.Time { return now }}

	const n = 500
	window := r.startupSpread()
	require.Equal(t, maxStartupSpread, window)
	buckets := make([]int, 20)
	periodic := make([]int, 20)
	for i := 0; i < n; i++ {
		tl := &autoscalingv1alpha1.Tailoring{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("app-%d", i), Namespace: "apps",
			UID:               types.UID(fmt.Sprintf("0b6c%04d-1f2e-4c3d-9a8b-%012d", i, i*7919)),
			CreationTimestamp: metav1.NewTime(now),
		}}
		due := r.analysisDueIn(tl)
		require.GreaterOrEqual(t, due, time.Duration(0))
		require.Less(t, due, window)
		buckets[int(due*time.Duration(len(buckets))/window)]++

		// All analyzed at the same moment: next analyses spread over interval/10.
		tl.Status.LastAnalysis = &autoscalingv1alpha1.AnalysisResult{Timestamp: metav1.NewTime(now)}
		next := r.analysisDueIn(tl) - r.analysisInterval()
		require.GreaterOrEqual(t, next, time.Duration(0))
		require.Less(t, next, r.analysisInterval()/10)
		periodic[int(next*time.Duration(len(periodic))/(r.analysisInterval()/10))]++
	}
	for i := range buckets {
		// Uniform would be 25 per bucket.
		assert.LessOrEqual(t, buckets[i], 50, "first analyses bunch up in bucket %d: %v", i, buckets)
		assert.Positive(t, buckets[i], "first analyses: empty bucket %d: %v", i, buckets)
		assert.LessOrEqual(t, periodic[i], 50, "periodic analyses bunch up in bucket %d: %v", i, periodic)
	}
}

// TestSchedule_ControllerRestart: after a restart no Tailoring with a recent
// analysis is queried, and overdue ones are spread instead of all running at
// once.
func TestSchedule_ControllerRestart(t *testing.T) {
	const n = 20
	var tailorings []*autoscalingv1alpha1.Tailoring
	for i := 0; i < n; i++ {
		tailorings = append(tailorings, mmTailoring(fmt.Sprintf("app-%d", i)))
	}
	fx := newMetricsModeFixture(t, tailorings...)
	fx.reconciler.EnableGitWriteBack = false
	offsets := map[string]float64{}
	for i, tl := range tailorings {
		offsets[tl.Name] = float64(i+1) / float64(n+1)
	}
	fx.reconciler.jitter = func(tl *autoscalingv1alpha1.Tailoring) float64 { return offsets[tl.Name] }

	// Before the restart: even Tailorings were analyzed 1h ago, odd ones 7h ago
	// (the controller was down when they fell due).
	for i, tl := range tailorings {
		cur := &autoscalingv1alpha1.Tailoring{}
		require.NoError(t, fx.client.Get(fx.ctx, types.NamespacedName{Namespace: mmTailoringNS, Name: tl.Name}, cur))
		age := time.Hour
		if i%2 == 1 {
			age = 7 * time.Hour
		}
		cur.Status.LastAnalysis = &autoscalingv1alpha1.AnalysisResult{Timestamp: metav1.NewTime(fx.now.Add(-age))}
		require.NoError(t, fx.client.Status().Update(fx.ctx, cur))
	}

	// Restart: the manager replays a create event for every Tailoring.
	fx.reconciler.startedAt = fx.now
	waits := map[string]time.Duration{}
	for _, tl := range tailorings {
		res, _ := fx.reconcile(t, tl.Name)
		waits[tl.Name] = res.RequeueAfter
	}
	assert.Zero(t, fx.backend.count(), "no query right after the restart")
	for i, tl := range tailorings {
		if i%2 == 0 {
			assert.Greater(t, waits[tl.Name], 4*time.Hour, "%s: recent analysis respected", tl.Name)
		} else {
			assert.Equal(t, scale(maxStartupSpread, offsets[tl.Name]), waits[tl.Name], "%s: overdue analysis spread", tl.Name)
		}
	}

	// Overdue analyses run one after another as their slot comes up.
	start := fx.now
	for i := 1; i < n; i += 2 {
		name := tailorings[i].Name
		fx.now = start.Add(waits[name])
		before := fx.backend.count()
		for j := 1; j < n; j += 2 {
			fx.reconcile(t, tailorings[j].Name)
		}
		assert.Equal(t, before+5, fx.backend.count(), "only %s is due at %v", name, waits[name])
	}
}

// TestReconcile_FailedQueryBacksOff: a failing backend is retried with
// exponential backoff that survives requeues, spec edits and status writes,
// capped at the analysis interval, and reset by a success.
func TestReconcile_FailedQueryBacksOff(t *testing.T) {
	fx := newMetricsModeFixture(t, mmTailoring("web"))
	fx.reconciler.AnalysisInterval = time.Hour
	fx.backend.fail = true

	wantBackoff := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	for i, want := range wantBackoff {
		before := fx.backend.count()
		res, tl := fx.reconcile(t, "web")
		require.Equal(t, before+1, fx.backend.count(), "attempt %d: exactly one query", i+1)
		assert.Equal(t, want, res.RequeueAfter, "attempt %d", i+1)
		assert.Equal(t, int32(i+1), tl.Status.ConsecutiveFailures)
		assert.Equal(t, "MetricsQueryError", reason(tl, ConditionTypeAnalyzed))

		// Events before the backoff elapsed (spec edit, other requeues) do not query.
		tl.Spec.ExcludeContainers = []string{fmt.Sprintf("sidecar-%d", i)}
		require.NoError(t, fx.client.Update(fx.ctx, tl))
		for k := 0; k < 3; k++ {
			res, _ := fx.reconcile(t, "web")
			assert.Equal(t, want, res.RequeueAfter)
		}
		require.Equal(t, before+1, fx.backend.count(), "attempt %d: no hot loop", i+1)
		fx.now = fx.now.Add(want)
	}

	fx.backend.set(func(b *metricsBackend) { b.fail = false })
	res, tl := fx.reconcile(t, "web")
	assert.Zero(t, tl.Status.ConsecutiveFailures)
	assert.NotNil(t, tl.Status.LastAnalysis)
	assert.Equal(t, time.Hour, res.RequeueAfter)
}

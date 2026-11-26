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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

func unitScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, autoscalingv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestAnalysisDueIn(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	analysis := func(age time.Duration) *autoscalingv1alpha1.AnalysisResult {
		return &autoscalingv1alpha1.AnalysisResult{Timestamp: metav1.NewTime(now.Add(-age))}
	}

	tests := []struct {
		name       string
		interval   time.Duration
		generation int64
		observed   int64
		last       *autoscalingv1alpha1.AnalysisResult
		want       time.Duration
	}{
		{name: "never analyzed", generation: 1, want: 0},
		{name: "zero timestamp", generation: 1, observed: 1, last: &autoscalingv1alpha1.AnalysisResult{}, want: 0},
		{name: "fresh, spec unchanged", generation: 1, observed: 1, last: analysis(time.Hour), want: 5 * time.Hour},
		{name: "fresh, spec changed", generation: 2, observed: 1, last: analysis(time.Hour), want: 0},
		{name: "fresh, status written by older controller (observed 0)", generation: 3, observed: 0, last: analysis(time.Minute), want: 0},
		{name: "stale", generation: 1, observed: 1, last: analysis(7 * time.Hour), want: 0},
		{name: "exactly at interval", generation: 1, observed: 1, last: analysis(6 * time.Hour), want: 0},
		{name: "custom interval", interval: 30 * time.Minute, generation: 1, observed: 1, last: analysis(10 * time.Minute), want: 20 * time.Minute},
		{name: "future timestamp capped at one interval", generation: 1, observed: 1, last: analysis(-48 * time.Hour), want: 6 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &TailoringReconciler{AnalysisInterval: tt.interval, now: func() time.Time { return now }}
			tl := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{Generation: tt.generation},
				Status:     autoscalingv1alpha1.TailoringStatus{LastAnalysis: tt.last, ObservedGeneration: tt.observed},
			}
			if got := r.analysisDueIn(tl); got != tt.want {
				t.Fatalf("analysisDueIn = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTailoringPredicates(t *testing.T) {
	base := func() *autoscalingv1alpha1.Tailoring {
		return &autoscalingv1alpha1.Tailoring{
			ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns", Generation: 1, ResourceVersion: "1"},
			Spec:       autoscalingv1alpha1.TailoringSpec{Target: autoscalingv1alpha1.TargetRef{Kind: "Deployment", Name: "web"}},
		}
	}

	statusOnly := base()
	statusOnly.ResourceVersion = "2"
	statusOnly.Status.LastAnalysis = &autoscalingv1alpha1.AnalysisResult{Timestamp: metav1.Now()}
	statusOnly.Status.NextAnalysisTime = &metav1.Time{Time: time.Now().Add(time.Hour)}

	labelsOnly := base()
	labelsOnly.ResourceVersion = "2"
	labelsOnly.Labels = map[string]string{"team": "a"}

	specChange := base()
	specChange.ResourceVersion = "2"
	specChange.Generation = 2
	specChange.Spec.Paused = true

	tests := []struct {
		name   string
		update *autoscalingv1alpha1.Tailoring
		want   bool
	}{
		{"status-only update does not requeue", statusOnly, false},
		{"metadata-only update does not requeue", labelsOnly, false},
		{"spec change requeues", specChange, true},
	}
	p := tailoringPredicates()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{ObjectOld: base(), ObjectNew: tt.update})
			if got != tt.want {
				t.Fatalf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
	if !p.Create(event.CreateEvent{Object: base()}) {
		t.Fatal("create events must be processed")
	}
	if !p.Delete(event.DeleteEvent{Object: base()}) {
		t.Fatal("delete events must be processed")
	}
}

// fakePrometheus answers every query with one container sample and counts
// requests. delay, if set, slows down responses per query.
func fakePrometheus(t *testing.T, delay ...func(query string) time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		for _, d := range delay {
			time.Sleep(d(r.FormValue("query")))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{
				"resultType": "vector",
				"result": []map[string]interface{}{{
					"metric": map[string]string{"container": "app"},
					"value":  []interface{}{1234567890.0, "0.25"},
				}},
			},
		})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func reconcileFixtures(promURL string) []client.Object {
	return []client.Object{
		&autoscalingv1alpha1.Atelier{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: autoscalingv1alpha1.AtelierSpec{
				Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: promURL},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name:  "app",
						Image: "example.com/web:1",
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						}},
					}}},
				},
			},
		},
		&autoscalingv1alpha1.Tailoring{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", Generation: 1},
			Spec: autoscalingv1alpha1.TailoringSpec{
				Target:        autoscalingv1alpha1.TargetRef{Kind: "Deployment", Name: "web"},
				FitProfileRef: autoscalingv1alpha1.FitProfileRef{Name: "missing"},
				WriteBack: autoscalingv1alpha1.WriteBackConfig{
					Repository: "github.com/example/deploy", Path: "apps/web.yaml", Type: autoscalingv1alpha1.WriteBackTypeRaw,
				},
				Paused: true,
			},
		},
	}
}

// TestTailoringReconcile_NoAnalysisLoop checks that the status write after an
// analysis does not cause another analysis, while a spec change or an expired
// interval does.
func TestTailoringReconcile_NoAnalysisLoop(t *testing.T) {
	server, calls := fakePrometheus(t)
	c := fake.NewClientBuilder().
		WithScheme(unitScheme(t)).
		WithObjects(reconcileFixtures(server.URL)...).
		WithStatusSubresource(&autoscalingv1alpha1.Tailoring{}).
		Build()

	// metav1.Time is persisted with second precision.
	now := time.Now().Truncate(time.Second)
	r := &TailoringReconciler{Client: c, Scheme: c.Scheme(), AnalysisInterval: 6 * time.Hour, now: func() time.Time { return now }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "apps", Name: "web"}}
	ctx := context.Background()

	get := func() *autoscalingv1alpha1.Tailoring {
		tl := &autoscalingv1alpha1.Tailoring{}
		if err := c.Get(ctx, req.NamespacedName, tl); err != nil {
			t.Fatal(err)
		}
		return tl
	}

	steps := []struct {
		name        string
		before      func()
		wantQueries bool
		wantRequeue time.Duration
	}{
		{name: "first reconcile analyzes", wantQueries: true, wantRequeue: 6 * time.Hour},
		{name: "reconcile right after the status write is skipped", wantRequeue: 6 * time.Hour},
		{
			name:        "later reconcile within the interval is skipped",
			before:      func() { now = now.Add(2 * time.Hour) },
			wantRequeue: 4 * time.Hour,
		},
		{
			name: "spec change re-analyzes",
			before: func() {
				tl := get()
				tl.Spec.ExcludeContainers = []string{"sidecar"}
				tl.Generation++ // the API server bumps generation on spec changes
				if err := c.Update(ctx, tl); err != nil {
					t.Fatal(err)
				}
			},
			wantQueries: true,
			wantRequeue: 6 * time.Hour,
		},
		{name: "unchanged after spec re-analysis is skipped", wantRequeue: 6 * time.Hour},
		{
			name:        "interval elapsed re-analyzes",
			before:      func() { now = now.Add(6*time.Hour + time.Second) },
			wantQueries: true,
			wantRequeue: 6 * time.Hour,
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if step.before != nil {
				step.before()
			}
			before := calls.Load()
			res, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if res.RequeueAfter != step.wantRequeue {
				t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, step.wantRequeue)
			}
			queried := calls.Load() > before
			if queried != step.wantQueries {
				t.Fatalf("queried Prometheus = %v, want %v", queried, step.wantQueries)
			}
			tl := get()
			if tl.Status.LastAnalysis == nil || tl.Status.ObservedGeneration != tl.Generation {
				t.Fatalf("status not recorded: lastAnalysis=%v observedGeneration=%d generation=%d",
					tl.Status.LastAnalysis, tl.Status.ObservedGeneration, tl.Generation)
			}
			cond := meta.FindStatusCondition(tl.Status.Conditions, ConditionTypeAnalyzed)
			if cond == nil || cond.Status != metav1.ConditionTrue {
				t.Fatalf("Analyzed condition = %+v", cond)
			}
		})
	}
}

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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/recommender"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

// fitProfile returns the FitProfile my-profile in namespace.
func fitProfile(namespace, strategyName, params string) *autoscalingv1alpha1.FitProfile {
	fp := &autoscalingv1alpha1.FitProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "my-profile", Namespace: namespace},
		Spec:       autoscalingv1alpha1.FitProfileSpec{Strategy: strategyName},
	}
	if params != "" {
		fp.Spec.Parameters = &runtime.RawExtension{Raw: []byte(params)}
	}
	return fp
}

func TestResolveFitProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile *autoscalingv1alpha1.FitProfile
		// rawOverride replaces the stored parameters on read, for bytes the
		// fake client cannot store (it round-trips objects through JSON).
		rawOverride  string
		wantStrategy string
		wantParams   map[string]interface{}
		wantPeak     bool
		wantFallback []string
	}{
		{
			name:         "missing FitProfile",
			wantStrategy: strategy.StrategyNamePercentile,
			wantParams:   map[string]interface{}{},
			wantFallback: []string{"FitProfile example-namespace/my-profile not found", "percentile strategy with default parameters"},
		},
		{
			name:         "parameters are not a JSON object",
			profile:      fitProfile("example-namespace", "dsp", `[1, 2]`),
			wantStrategy: "dsp",
			wantParams:   map[string]interface{}{},
			wantFallback: []string{"FitProfile example-namespace/my-profile has invalid parameters", "using default parameters"},
		},
		{
			name:         "malformed JSON parameters",
			profile:      fitProfile("example-namespace", "dsp", `{}`),
			rawOverride:  `{"enablePeakDetection": tru`,
			wantStrategy: "dsp",
			wantParams:   map[string]interface{}{},
			wantFallback: []string{"has invalid parameters"},
		},
		{
			name:         "unknown strategy keeps its parameters",
			profile:      fitProfile("example-namespace", "does-not-exist", `{"maxChangePercent": 50}`),
			wantStrategy: strategy.StrategyNamePercentile,
			wantParams:   map[string]interface{}{"maxChangePercent": 50.0},
			wantFallback: []string{`references unknown strategy "does-not-exist"`, "using the percentile strategy"},
		},
		{
			name:         "unknown strategy and invalid parameters report both",
			profile:      fitProfile("example-namespace", "does-not-exist", `"text"`),
			wantStrategy: strategy.StrategyNamePercentile,
			wantParams:   map[string]interface{}{},
			wantFallback: []string{"has invalid parameters", "; ", "references unknown strategy"},
		},
		{
			name:         "null parameters",
			profile:      fitProfile("example-namespace", "percentile", `null`),
			wantStrategy: strategy.StrategyNamePercentile,
			wantParams:   map[string]interface{}{},
		},
		{
			name:         "valid FitProfile",
			profile:      fitProfile("example-namespace", "dsp", `{"enablePeakDetection": true}`),
			wantStrategy: "dsp",
			wantParams:   map[string]interface{}{"enablePeakDetection": true},
			wantPeak:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(unitScheme(t))
			if tt.profile != nil {
				b = b.WithObjects(tt.profile)
			}
			if tt.rawOverride != "" {
				b = b.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if err := c.Get(ctx, key, obj, opts...); err != nil {
							return err
						}
						if fp, ok := obj.(*autoscalingv1alpha1.FitProfile); ok {
							fp.Spec.Parameters = &runtime.RawExtension{Raw: []byte(tt.rawOverride)}
						}
						return nil
					},
				})
			}
			r := &TailoringReconciler{Client: b.Build()}
			tl := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "example-namespace"},
				Spec:       autoscalingv1alpha1.TailoringSpec{FitProfileRef: autoscalingv1alpha1.FitProfileRef{Name: "my-profile"}},
			}

			got := r.resolveFitProfile(context.Background(), tl, recommender.NewCalculator().GetRegistry(), logr.Discard())

			assert.Equal(t, "example-namespace/my-profile", got.ref)
			assert.Equal(t, tt.wantStrategy, got.strategy)
			assert.Equal(t, tt.wantParams, got.params)
			assert.Equal(t, tt.wantPeak, got.enablePeakDetection)
			if len(tt.wantFallback) == 0 {
				assert.Empty(t, got.fallback)
			}
			for _, part := range tt.wantFallback {
				assert.Contains(t, got.fallback, part)
			}

			r.setFitProfileCondition(tl, got)
			cond := condition(tl, ConditionTypeFitProfileResolved)
			require.NotNil(t, cond)
			if len(tt.wantFallback) > 0 {
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, ReasonFitProfileFallback, cond.Reason)
				assert.Equal(t, got.fallback, cond.Message)
			} else {
				assert.Equal(t, metav1.ConditionTrue, cond.Status)
				assert.Equal(t, "FitProfileResolved", cond.Reason)
			}
		})
	}
}

// TestReconcile_FitProfileFallbackCondition checks that each fallback is
// reported on the FitProfileResolved and Analyzed conditions while the
// analysis still completes, and that fixing the FitProfile clears it.
func TestReconcile_FitProfileFallbackCondition(t *testing.T) {
	tests := []struct {
		name        string
		profile     *autoscalingv1alpha1.FitProfile
		wantMessage string
	}{
		{name: "missing FitProfile", wantMessage: "FitProfile example/my-profile not found"},
		{name: "invalid parameters", profile: fitProfile("example", "percentile", `[1]`), wantMessage: "has invalid parameters"},
		{name: "unknown strategy", profile: fitProfile("example", "does-not-exist", `{}`), wantMessage: `unknown strategy "does-not-exist"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := replicaPrometheus(t, "3")
			objs := reconcileFixtures(server.URL)
			for _, o := range objs {
				if tl, ok := o.(*autoscalingv1alpha1.Tailoring); ok {
					tl.Spec.FitProfileRef.Name = "my-profile"
				}
			}
			if tt.profile != nil {
				objs = append(objs, tt.profile)
			}
			c := fake.NewClientBuilder().WithScheme(unitScheme(t)).WithObjects(objs...).
				WithStatusSubresource(&autoscalingv1alpha1.Tailoring{}).Build()
			now := time.Now().Truncate(time.Second)
			r := &TailoringReconciler{
				Client: c, Scheme: c.Scheme(), AnalysisInterval: time.Hour,
				now:    func() time.Time { return now },
				jitter: func(*autoscalingv1alpha1.Tailoring) float64 { return 0 },
			}
			ctx := context.Background()
			key := types.NamespacedName{Namespace: "example", Name: "web"}
			reconcile := func() *autoscalingv1alpha1.Tailoring {
				t.Helper()
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				require.NoError(t, err)
				tl := &autoscalingv1alpha1.Tailoring{}
				require.NoError(t, c.Get(ctx, key, tl))
				return tl
			}

			tl := reconcile()
			require.NotNil(t, tl.Status.LastAnalysis, "the fallback still analyzes")
			cond := condition(tl, ConditionTypeFitProfileResolved)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, ReasonFitProfileFallback, cond.Reason)
			assert.Contains(t, cond.Message, tt.wantMessage)
			analyzed := condition(tl, ConditionTypeAnalyzed)
			require.NotNil(t, analyzed)
			assert.Equal(t, metav1.ConditionTrue, analyzed.Status)
			assert.Equal(t, ReasonFitProfileFallback, analyzed.Reason)
			assert.Contains(t, analyzed.Message, tt.wantMessage)

			valid := fitProfile("example", "percentile", `{}`)
			if tt.profile != nil {
				existing := &autoscalingv1alpha1.FitProfile{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(valid), existing))
				existing.Spec = valid.Spec
				require.NoError(t, c.Update(ctx, existing))
			} else {
				require.NoError(t, c.Create(ctx, valid))
			}
			now = now.Add(time.Hour + time.Second)

			tl = reconcile()
			cond = condition(tl, ConditionTypeFitProfileResolved)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status, cond.Message)
			assert.Equal(t, "FitProfileResolved", cond.Reason)
			analyzed = condition(tl, ConditionTypeAnalyzed)
			require.NotNil(t, analyzed)
			assert.Equal(t, "AnalysisComplete", analyzed.Reason)
		})
	}
}

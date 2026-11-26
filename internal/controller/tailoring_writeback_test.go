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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	fakegit "github.com/sartorproj/sartor/internal/gitops/provider/fake"
	"github.com/sartorproj/sartor/internal/gitops/provider/github"
)

const (
	wbControllerNS = "sartor-system"
	wbAppNS        = "payments"
	wbValuesPath   = "apps/api/values.yaml"
	wbValues       = "api:\n  resources:\n    requests:\n      cpu: 100m # tuned\n      memory: 128Mi\n"
)

// countingReader records every Secret read so tests can assert none happened.
type countingReader struct {
	client.Reader
	mu      sync.Mutex
	secrets []string
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		c.mu.Lock()
		c.secrets = append(c.secrets, key.String())
		c.mu.Unlock()
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("list is not allowed on the secret reader")
}

type wbFixture struct {
	reconciler *TailoringReconciler
	reader     *countingReader
	git        *fakegit.Provider
	factoryN   int
	client     client.Client
}

func q(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

func wbScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, autoscalingv1alpha1.AddToScheme(s))
	return s
}

func wbAtelier(promURL, secretNS string) *autoscalingv1alpha1.Atelier {
	return &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: promURL},
			GitProvider: autoscalingv1alpha1.GitProviderConfig{
				Type:      autoscalingv1alpha1.GitProviderGitHub,
				SecretRef: corev1.SecretReference{Name: "github-token", Namespace: secretNS},
			},
			PRSettings: &autoscalingv1alpha1.PRSettingsConfig{Labels: []string{"sartor"}},
		},
	}
}

func wbTailoring(recommendedCPU string) *autoscalingv1alpha1.Tailoring {
	return &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: wbAppNS},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target:        autoscalingv1alpha1.TargetRef{Kind: "Deployment", Name: "api"},
			FitProfileRef: autoscalingv1alpha1.FitProfileRef{Name: "balanced"},
			WriteBack: autoscalingv1alpha1.WriteBackConfig{
				Repository:    "https://github.com/acme/gitops.git",
				Path:          wbValuesPath,
				Type:          autoscalingv1alpha1.WriteBackTypeHelm,
				HelmValuePath: "api.resources",
			},
		},
		Status: autoscalingv1alpha1.TailoringStatus{LastAnalysis: &autoscalingv1alpha1.AnalysisResult{
			AnalysisWindow: metav1.Duration{Duration: 168 * time.Hour},
			Containers: []autoscalingv1alpha1.ContainerRecommendation{{
				Name:        "api",
				Recommended: autoscalingv1alpha1.ResourceRequirements{Requests: &autoscalingv1alpha1.ResourceValues{CPU: q(recommendedCPU)}},
			}},
		}},
	}
}

func wbLive() map[string]autoscalingv1alpha1.ResourceRequirements {
	return map[string]autoscalingv1alpha1.ResourceRequirements{
		"api": {Requests: &autoscalingv1alpha1.ResourceValues{CPU: q("100m"), Memory: q("128Mi")}},
	}
}

func newWBFixture(t *testing.T, enabled bool, atelier *autoscalingv1alpha1.Atelier, extra ...client.Object) *wbFixture {
	t.Helper()
	objs := make([]client.Object, 0, 4+len(extra))
	objs = append(objs,
		atelier,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: wbControllerNS},
			Data:       map[string][]byte{"token": []byte("ghp_test")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "kube-system"},
			Data:       map[string][]byte{"token": []byte("ghp_other_namespace")},
		},
		&autoscalingv1alpha1.FitProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "balanced", Namespace: wbAppNS},
			Spec: autoscalingv1alpha1.FitProfileSpec{
				Strategy:   "percentile",
				Parameters: &runtime.RawExtension{Raw: []byte(`{"maxChangePercent": 50}`)},
			},
		},
	)
	objs = append(objs, extra...)
	c := fake.NewClientBuilder().WithScheme(wbScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&autoscalingv1alpha1.Tailoring{}).Build()

	f := &wbFixture{
		reader: &countingReader{Reader: c},
		git:    fakegit.New("main", map[string]string{wbValuesPath: wbValues}),
		client: c,
	}
	f.reconciler = &TailoringReconciler{
		Client:              c,
		Scheme:              c.Scheme(),
		APIReader:           f.reader,
		ControllerNamespace: wbControllerNS,
		EnableGitWriteBack:  enabled,
		NewGitProvider: func(cfg github.Config) (provider.Provider, error) {
			f.factoryN++
			if cfg.Token != "ghp_test" {
				return nil, fmt.Errorf("unexpected credentials")
			}
			return f.git, nil
		},
	}
	return f
}

func reason(t *autoscalingv1alpha1.Tailoring, condType string) string {
	if c := meta.FindStatusCondition(t.Status.Conditions, condType); c != nil {
		return c.Reason
	}
	return ""
}

// TestWriteBackNeverTouchesGit covers every guard that must keep the
// controller away from Git, including bypass attempts.
func TestWriteBackNeverTouchesGit(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		secretNS      string
		controllerNS  *string
		mutate        func(*autoscalingv1alpha1.Tailoring)
		wantReady     string
		wantPRCreated string
	}{
		{name: "flag off", enabled: false, wantReady: "Ready", wantPRCreated: "WriteBackDisabled"},
		{name: "flag off and paused", enabled: false, mutate: func(tl *autoscalingv1alpha1.Tailoring) { tl.Spec.Paused = true }, wantReady: "Paused"},
		{name: "paused with flag on", enabled: true, mutate: func(tl *autoscalingv1alpha1.Tailoring) { tl.Spec.Paused = true }, wantReady: "Paused"},
		{name: "paused with an open PR in status", enabled: true, mutate: func(tl *autoscalingv1alpha1.Tailoring) {
			tl.Spec.Paused = true
			tl.Status.PRNumber, tl.Status.PRState = 7, autoscalingv1alpha1.PRStateOpen
		}, wantReady: "Paused"},
		{name: "ignored via sartor-ignore", enabled: true, mutate: func(tl *autoscalingv1alpha1.Tailoring) { tl.Status.Ignored = true }, wantReady: "Ignored"},
		{name: "missing helmValuePath", enabled: true, mutate: func(tl *autoscalingv1alpha1.Tailoring) { tl.Spec.WriteBack.HelmValuePath = "" },
			wantReady: "InvalidWriteBack", wantPRCreated: "InvalidWriteBack"},
		{name: "path escapes the repository", enabled: true, mutate: func(tl *autoscalingv1alpha1.Tailoring) { tl.Spec.WriteBack.Path = "../x.yaml" },
			wantReady: "InvalidWriteBack", wantPRCreated: "InvalidWriteBack"},
		{name: "secretRef in another namespace is not read", enabled: true, secretNS: "kube-system", wantPRCreated: "GitCredentialsError"},
		{name: "controller namespace unknown", enabled: true, controllerNS: ptrTo(""), wantPRCreated: "GitCredentialsError"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newWBFixture(t, tt.enabled, wbAtelier("http://unused", tt.secretNS))
			if tt.controllerNS != nil {
				f.reconciler.ControllerNamespace = *tt.controllerNS
			}
			tl := wbTailoring("150m")
			if tt.mutate != nil {
				tt.mutate(tl)
			}
			atelier := wbAtelier("http://unused", tt.secretNS)

			f.reconciler.reconcileGitOps(context.Background(), tl, atelier, wbLive(), logr.Discard())

			assert.Empty(t, f.reader.secrets, "no Secret may be read")
			assert.Zero(t, f.factoryN, "no Git provider may be created")
			assert.Empty(t, f.git.Calls, "no Git call may happen")
			if tt.wantReady != "" {
				assert.Equal(t, tt.wantReady, reason(tl, ConditionTypeReady))
			}
			if tt.wantPRCreated != "" {
				assert.Equal(t, tt.wantPRCreated, reason(tl, ConditionTypePRCreated))
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestWriteBackOpensOnePRAndUpdatesIt(t *testing.T) {
	for _, secretNS := range []string{wbControllerNS, ""} {
		t.Run("secretRef namespace "+secretNS, func(t *testing.T) {
			ctx := context.Background()
			atelier := wbAtelier("http://unused", secretNS)
			f := newWBFixture(t, true, atelier)
			tl := wbTailoring("130m")

			f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
			require.Equal(t, "Created", reason(tl, ConditionTypePRCreated), tl.Status.PRMessage)
			assert.Equal(t, []string{wbControllerNS + "/github-token"}, f.reader.secrets)
			assert.Equal(t, 1, tl.Status.PRNumber)
			assert.Equal(t, autoscalingv1alpha1.PRStateOpen, tl.Status.PRState)
			assert.Equal(t, "sartor/payments/api", tl.Status.BranchName)
			assert.NotNil(t, tl.Status.LastPRUpdateTime)
			content, _ := f.git.File("sartor/payments/api", wbValuesPath)
			assert.Equal(t, strings.Replace(wbValues, "cpu: 100m", "cpu: 130m", 1), content)

			// Rerun many times (e.g. after a lost status update): never a second PR.
			for i := 0; i < 3; i++ {
				f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
				assert.Equal(t, "Unchanged", reason(tl, ConditionTypePRCreated))
			}
			lost := wbTailoring("130m")
			f.reconciler.reconcileGitOps(ctx, lost, atelier, wbLive(), logr.Discard())
			assert.Equal(t, 1, lost.Status.PRNumber, "status lost: the open PR is found by branch")

			tl.Status.LastAnalysis.Containers[0].Recommended.Requests.CPU = q("140m")
			f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
			assert.Equal(t, "Updated", reason(tl, ConditionTypePRCreated))
			assert.Len(t, f.git.PullRequests(), 1)
			assert.Equal(t, 2, f.git.Commits())
			base, _ := f.git.File("main", wbValuesPath)
			assert.Equal(t, wbValues, base)
		})
	}
}

func TestWriteBackGuards(t *testing.T) {
	tests := []struct {
		name        string
		recommended string
		atelier     func(*autoscalingv1alpha1.Atelier)
		wantReady   string
		wantCPU     string
	}{
		{name: "maxChangePercent caps a 10x recommendation", recommended: "1", wantReady: "PROpen", wantCPU: "cpu: 150m"},
		{name: "safety rail max applies after the cap", recommended: "1", wantReady: "PROpen", wantCPU: "cpu: 120m",
			atelier: func(a *autoscalingv1alpha1.Atelier) {
				a.Spec.SafetyRails = &autoscalingv1alpha1.SafetyRailsConfig{MaxCPU: q("120m")}
			}},
		{name: "below minChangePercent", recommended: "105m", wantReady: "BelowThreshold"},
		{name: "cooldown blocks a new PR", recommended: "150m", wantReady: "CooldownActive",
			atelier: func(a *autoscalingv1alpha1.Atelier) {
				a.Spec.PRSettings.CooldownPeriod = &metav1.Duration{Duration: time.Hour}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atelier := wbAtelier("http://unused", wbControllerNS)
			if tt.atelier != nil {
				tt.atelier(atelier)
			}
			f := newWBFixture(t, true, atelier)
			tl := wbTailoring(tt.recommended)
			if tt.wantReady == "CooldownActive" {
				tl.Status.LastPRUpdateTime = &metav1.Time{Time: time.Now().Add(-time.Minute)}
				tl.Status.PRState = autoscalingv1alpha1.PRStateMerged
			}

			f.reconciler.reconcileGitOps(context.Background(), tl, atelier, wbLive(), logr.Discard())
			assert.Equal(t, tt.wantReady, reason(tl, ConditionTypeReady))
			content, ok := f.git.File("sartor/payments/api", wbValuesPath)
			if tt.wantCPU == "" {
				assert.False(t, ok, "no branch may be written")
				assert.Empty(t, f.git.PullRequests())
				return
			}
			assert.Contains(t, content, tt.wantCPU)
		})
	}
}

func TestWriteBackStopsWhenPRClosedWithIgnoreLabel(t *testing.T) {
	ctx := context.Background()
	atelier := wbAtelier("http://unused", wbControllerNS)
	f := newWBFixture(t, true, atelier)
	tl := wbTailoring("150m")

	f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
	require.Equal(t, 1, tl.Status.PRNumber)

	f.git.SetPRState(1, "closed", false, SartorIgnoreLabel)
	tl.Status.LastAnalysis.Containers[0].Recommended.Requests.CPU = q("140m")
	f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
	assert.True(t, tl.Status.Ignored)
	assert.Equal(t, autoscalingv1alpha1.PRStateClosed, tl.Status.PRState)

	calls := len(f.git.Calls)
	f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
	assert.Equal(t, calls, len(f.git.Calls), "ignored Tailorings make no Git calls")
	assert.Len(t, f.git.PullRequests(), 1)
}

func TestWriteBackMarksMergedPR(t *testing.T) {
	ctx := context.Background()
	atelier := wbAtelier("http://unused", wbControllerNS)
	f := newWBFixture(t, true, atelier)
	tl := wbTailoring("150m")
	f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
	f.git.SetPRState(1, "closed", true)

	f.reconciler.reconcileGitOps(ctx, tl, atelier, wbLive(), logr.Discard())
	assert.Equal(t, autoscalingv1alpha1.PRStateMerged, tl.Status.PRState)
	assert.Equal(t, "Merged", reason(tl, ConditionTypePRMerged))
	assert.Equal(t, "CooldownActive", reason(tl, ConditionTypeReady))
}

// fakePrometheus answers every instant query with one sample per container.
func fakePrometheus() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		value := "0.15"
		if strings.Contains(r.Form.Get("query"), "memory") {
			value = "134217728"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[`+
			`{"metric":{"container":"api","pod":"api-7d9f8b6c5-abcde"},"value":[%d,"%s"]}]}}`, time.Now().Unix(), value)
	}))
}

// TestReconcilePausedNeverReachesGit runs the full reconcile loop with Git
// write-back enabled and checks a paused Tailoring never reads the Git Secret
// or calls the Git provider, while an identical unpaused one does.
func TestReconcilePausedNeverReachesGit(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			prom := fakePrometheus()
			defer prom.Close()

			tl := wbTailoring("150m")
			tl.Status = autoscalingv1alpha1.TailoringStatus{}
			tl.Spec.Paused = paused
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: wbAppNS},
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "api",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					}},
				}}}}},
			}
			f := newWBFixture(t, true, wbAtelier(prom.URL, wbControllerNS), tl, deploy)

			_, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tl)})
			require.NoError(t, err)

			got := &autoscalingv1alpha1.Tailoring{}
			require.NoError(t, f.client.Get(context.Background(), client.ObjectKeyFromObject(tl), got))
			require.NotNil(t, got.Status.LastAnalysis, "analysis still runs")

			if paused {
				assert.Equal(t, "Paused", reason(got, ConditionTypeReady))
				assert.Empty(t, f.reader.secrets)
				assert.Zero(t, f.factoryN)
				assert.Empty(t, f.git.Calls)
				return
			}
			assert.Equal(t, "Created", reason(got, ConditionTypePRCreated), got.Status.PRMessage)
			assert.Len(t, f.git.PullRequests(), 1)
		})
	}
}

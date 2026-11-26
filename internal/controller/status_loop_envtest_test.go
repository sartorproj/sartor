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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

// TestEnvtest_StatusWritesDoNotRetrigger runs the real Tailoring and Atelier
// controllers against an API server and checks that their own status writes
// do not cause further analyses or connectivity checks, while a spec change
// does trigger a new analysis.
func TestEnvtest_StatusWritesDoNotRetrigger(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; envtest binaries unavailable")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	s := unitScheme(t)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Client:  client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		// The Ginkgo suite in this package registers controllers with the same names.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Status timestamps have second precision and identical status writes are
	// no-ops, so a loop only shows up when each write differs: advance the
	// Tailoring clock on every call and make Atelier checks take over a second,
	// as real 7-day queries and remote backends do.
	var clockMu sync.Mutex
	clock := time.Now().Truncate(time.Second)
	advancingClock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(time.Minute)
		return clock
	}
	if err := (&TailoringReconciler{Client: mgr.GetClient(), Scheme: s, AnalysisInterval: 24 * time.Hour, now: advancingClock}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if err := (&AtelierReconciler{Client: mgr.GetClient(), Scheme: s, ControllerNamespace: "sartor"}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	server, calls := fakePrometheus(t, func(query string) time.Duration {
		if query == "vector(1)" {
			return 1100 * time.Millisecond
		}
		return 0
	})
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	objs := reconcileFixtures(server.URL)
	atelier := objs[0].(*autoscalingv1alpha1.Atelier)
	atelier.Spec.GitProvider = autoscalingv1alpha1.GitProviderConfig{
		Type:      autoscalingv1alpha1.GitProviderGitHub,
		SecretRef: corev1.SecretReference{Name: "git", Namespace: "sartor"},
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}}); err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	key := types.NamespacedName{Namespace: "apps", Name: "web"}
	waitFor := func(desc string, cond func(*autoscalingv1alpha1.Tailoring) bool) *autoscalingv1alpha1.Tailoring {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			tl := &autoscalingv1alpha1.Tailoring{}
			if err := c.Get(ctx, key, tl); err == nil && cond(tl) {
				return tl
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
		return nil
	}

	first := waitFor("first analysis", func(tl *autoscalingv1alpha1.Tailoring) bool {
		return tl.Status.LastAnalysis != nil && tl.Status.ObservedGeneration == tl.Generation
	})
	// Let the initial Atelier check finish.
	time.Sleep(2 * time.Second)
	settled := calls.Load()

	time.Sleep(5 * time.Second)
	if got := calls.Load(); got != settled {
		t.Fatalf("status writes retriggered work: %d backend requests after settling, want %d", got, settled)
	}
	again := &autoscalingv1alpha1.Tailoring{}
	if err := c.Get(ctx, key, again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != first.ResourceVersion {
		t.Fatalf("Tailoring kept being rewritten (resourceVersion %s -> %s)", first.ResourceVersion, again.ResourceVersion)
	}

	again.Spec.ExcludeContainers = []string{"sidecar"}
	if err := c.Update(ctx, again); err != nil {
		t.Fatal(err)
	}
	waitFor("re-analysis after spec change", func(tl *autoscalingv1alpha1.Tailoring) bool {
		return tl.Generation == first.Generation+1 && tl.Status.ObservedGeneration == tl.Generation
	})
	if calls.Load() <= settled {
		t.Fatal("spec change did not query the backend")
	}
}

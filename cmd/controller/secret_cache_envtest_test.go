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

package main

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestManagerClient_SecretGetNeedsOnlyNamespacedGet runs the manager as a user
// that may only `get` Secrets in its own namespace (what the Helm chart
// grants) and checks that reading a credentials Secret works with the
// production client options, while a cached client would need list/watch.
func TestManagerClient_SecretGetNeedsOnlyNamespacedGet(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; envtest binaries unavailable")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	ctx := context.Background()
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const ns, user = "sartor", "sartor-controller"
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns},
			Data:       map[string][]byte{"token": []byte("t")},
		},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "secret-get", Namespace: ns},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "secret-get", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "secret-get"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
		},
	} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	restricted, err := env.AddUser(envtest.User{Name: user}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		opts    client.Options
		wantErr bool
	}{
		{name: "production options read Secrets directly", opts: managerClientOptions()},
		{name: "cached Secret reads would need list/watch", opts: client.Options{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := ctrl.NewManager(restricted.Config(), ctrl.Options{
				Scheme:  scheme,
				Metrics: metricsserver.Options{BindAddress: "0"},
				Client:  tt.opts,
			})
			if err != nil {
				t.Fatal(err)
			}
			mgrCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() { _ = mgr.Start(mgrCtx) }()
			if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
				t.Fatal("cache did not sync")
			}

			getCtx, getCancel := context.WithTimeout(ctx, 5*time.Second)
			defer getCancel()
			err = mgr.GetClient().Get(getCtx, types.NamespacedName{Namespace: ns, Name: "creds"}, &corev1.Secret{})
			if tt.wantErr != (err != nil) {
				t.Fatalf("Get error = %v, wantErr %v", err, tt.wantErr)
			}

			other := mgr.GetClient().Get(getCtx, types.NamespacedName{Namespace: "default", Name: "creds"}, &corev1.Secret{})
			if other == nil {
				t.Fatal("reading a Secret outside the granted namespace must fail")
			}
		})
	}
}

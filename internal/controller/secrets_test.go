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
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

func TestResolveControllerNamespace(t *testing.T) {
	tests := []struct {
		name string
		env  string
		file string
		err  error
		want string
	}{
		{name: "POD_NAMESPACE wins", env: "sartor", file: "other", want: "sartor"},
		{name: "POD_NAMESPACE is trimmed", env: " sartor \n", want: "sartor"},
		{name: "falls back to service account file", file: "sartor-system\n", want: "sartor-system"},
		{name: "unknown outside the cluster", err: os.ErrNotExist, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveControllerNamespace(
				func(string) string { return tt.env },
				func(string) ([]byte, error) {
					if tt.err != nil {
						return nil, tt.err
					}
					return []byte(tt.file), nil
				})
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSecretKeyFor(t *testing.T) {
	tests := []struct {
		name         string
		controllerNS string
		ref          corev1.SecretReference
		want         types.NamespacedName
		wantErr      bool
	}{
		{"same namespace", "sartor", corev1.SecretReference{Name: "s", Namespace: "sartor"}, types.NamespacedName{Namespace: "sartor", Name: "s"}, false},
		{"empty namespace defaults to controller namespace", "sartor", corev1.SecretReference{Name: "s"}, types.NamespacedName{Namespace: "sartor", Name: "s"}, false},
		{"other namespace rejected", "sartor", corev1.SecretReference{Name: "s", Namespace: "kube-system"}, types.NamespacedName{}, true},
		{"case differences are different namespaces", "sartor", corev1.SecretReference{Name: "s", Namespace: "Sartor"}, types.NamespacedName{}, true},
		{"unknown controller namespace fails closed", "", corev1.SecretReference{Name: "s", Namespace: "sartor"}, types.NamespacedName{}, true},
		{"unknown controller namespace, empty ref namespace", "", corev1.SecretReference{Name: "s"}, types.NamespacedName{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := secretKeyFor(tt.controllerNS, tt.ref)
			if tt.wantErr {
				if !IsSecretNamespaceError(err) {
					t.Fatalf("want SecretNamespaceError, got %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// secretGetRecorder builds a fake client that records every Secret Get.
func secretGetRecorder(t *testing.T, objs ...client.Object) (client.Client, *[]types.NamespacedName) {
	t.Helper()
	var gets []types.NamespacedName
	c := fake.NewClientBuilder().
		WithScheme(unitScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&autoscalingv1alpha1.Atelier{}, &autoscalingv1alpha1.Tailoring{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					gets = append(gets, key)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	return c, &gets
}

func TestAtelierReconcile_SecretNamespaceRestriction(t *testing.T) {
	secret := func(ns string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns},
			Data:       map[string][]byte{"token": []byte("t")},
		}
	}
	atelier := func(promNS, gitNS string) *autoscalingv1alpha1.Atelier {
		return &autoscalingv1alpha1.Atelier{
			ObjectMeta: metav1.ObjectMeta{Name: "default", Generation: 1},
			Spec: autoscalingv1alpha1.AtelierSpec{
				Prometheus: autoscalingv1alpha1.PrometheusConfig{
					URL:       "http://127.0.0.1:1",
					SecretRef: &corev1.SecretReference{Name: "creds", Namespace: promNS},
				},
				GitProvider: autoscalingv1alpha1.GitProviderConfig{
					Type:      autoscalingv1alpha1.GitProviderGitHub,
					SecretRef: corev1.SecretReference{Name: "creds", Namespace: gitNS},
				},
			},
		}
	}

	tests := []struct {
		name           string
		atelier        *autoscalingv1alpha1.Atelier
		wantValid      metav1.ConditionStatus
		wantPromReason string
		wantGets       []types.NamespacedName
	}{
		{
			name:           "refs in controller namespace are read",
			atelier:        atelier("sartor", ""),
			wantValid:      metav1.ConditionTrue,
			wantPromReason: "ConnectionFailed",
			wantGets:       []types.NamespacedName{{Namespace: "sartor", Name: "creds"}, {Namespace: "sartor", Name: "creds"}},
		},
		{
			name:           "Prometheus ref in another namespace is rejected without reading it",
			atelier:        atelier("kube-system", "sartor"),
			wantValid:      metav1.ConditionFalse,
			wantPromReason: ReasonSecretNamespaceNotAllowed,
			wantGets:       []types.NamespacedName{{Namespace: "sartor", Name: "creds"}},
		},
		{
			name:           "both refs elsewhere: no Secret is read",
			atelier:        atelier("kube-system", "default"),
			wantValid:      metav1.ConditionFalse,
			wantPromReason: ReasonSecretNamespaceNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, gets := secretGetRecorder(t, tt.atelier, secret("sartor"), secret("kube-system"), secret("default"))
			r := &AtelierReconciler{Client: c, Scheme: c.Scheme(), ControllerNamespace: "sartor"}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			got := &autoscalingv1alpha1.Atelier{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: "default"}, got); err != nil {
				t.Fatal(err)
			}
			valid := meta.FindStatusCondition(got.Status.Conditions, ConditionTypeSecretRefsValid)
			if valid == nil || valid.Status != tt.wantValid {
				t.Fatalf("SecretRefsValid = %+v, want %s", valid, tt.wantValid)
			}
			if tt.wantValid == metav1.ConditionFalse && valid.Reason != ReasonSecretNamespaceNotAllowed {
				t.Fatalf("SecretRefsValid reason = %s", valid.Reason)
			}
			prom := meta.FindStatusCondition(got.Status.Conditions, ConditionTypePrometheusConnected)
			if prom == nil || prom.Reason != tt.wantPromReason {
				t.Fatalf("PrometheusConnected = %+v, want reason %s", prom, tt.wantPromReason)
			}
			if len(*gets) != len(tt.wantGets) {
				t.Fatalf("Secret reads = %v, want %v", *gets, tt.wantGets)
			}
			for i := range tt.wantGets {
				if (*gets)[i] != tt.wantGets[i] {
					t.Fatalf("Secret reads = %v, want %v", *gets, tt.wantGets)
				}
			}
		})
	}
}

func TestTailoringReconcile_SecretNamespaceRestriction(t *testing.T) {
	server, calls := fakePrometheus(t)
	objs := reconcileFixtures(server.URL)
	atelier := objs[0].(*autoscalingv1alpha1.Atelier)
	atelier.Spec.Prometheus.SecretRef = &corev1.SecretReference{Name: "creds", Namespace: "kube-system"}
	objs = append(objs, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "kube-system"}})

	c, gets := secretGetRecorder(t, objs...)
	r := &TailoringReconciler{Client: c, Scheme: c.Scheme(), ControllerNamespace: "sartor"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "example", Name: "web"}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != DefaultRequeueInterval {
		t.Fatalf("RequeueAfter = %v", res.RequeueAfter)
	}
	if len(*gets) != 0 || calls.Load() != 0 {
		t.Fatalf("secret reads %v, Prometheus calls %d; want none", *gets, calls.Load())
	}
	tl := &autoscalingv1alpha1.Tailoring{}
	if err := c.Get(context.Background(), req.NamespacedName, tl); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(tl.Status.Conditions, ConditionTypeAnalyzed)
	if cond == nil || cond.Reason != ReasonSecretNamespaceNotAllowed {
		t.Fatalf("Analyzed condition = %+v", cond)
	}
}

func TestSecretNamespaceError_Message(t *testing.T) {
	err := error(&SecretNamespaceError{Ref: corev1.SecretReference{Name: "s", Namespace: "x"}, ControllerNamespace: "sartor"})
	wrapped := errors.Join(errors.New("outer"), err)
	if !IsSecretNamespaceError(wrapped) {
		t.Fatal("wrapped error not detected")
	}
	unknown := &SecretNamespaceError{Ref: corev1.SecretReference{Name: "s"}}
	if got := unknown.Error(); got == "" || !strings.Contains(got, PodNamespaceEnv) {
		t.Fatalf("message %q should mention %s", got, PodNamespaceEnv)
	}
}

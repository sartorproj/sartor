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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	fakegit "github.com/sartorproj/sartor/internal/gitops/provider/fake"
	"github.com/sartorproj/sartor/internal/gitops/provider/github"
)

func TestAtelierGitConnectivityCheck(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		secretNS      string
		wantReason    string
		wantConnected bool
		wantReads     int
	}{
		{name: "flag off: no secret read, no Git call", enabled: false, secretNS: wbControllerNS, wantReason: "WriteBackDisabled"},
		{name: "secret in another namespace is rejected unread", enabled: true, secretNS: "kube-system", wantReason: ReasonSecretNamespaceNotAllowed},
		{name: "secret in controller namespace", enabled: true, secretNS: wbControllerNS, wantReason: "Connected", wantConnected: true, wantReads: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atelier := wbAtelier("http://127.0.0.1:1", tt.secretNS)
			c := fake.NewClientBuilder().WithScheme(wbScheme(t)).
				WithObjects(atelier,
					&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: wbControllerNS}, Data: map[string][]byte{"token": []byte("ghp_test")}},
					&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "kube-system"}, Data: map[string][]byte{"token": []byte("x")}},
				).
				WithStatusSubresource(&autoscalingv1alpha1.Atelier{}).Build()
			reader := &countingClient{Client: c}
			git := fakegit.New("main", nil)
			r := &AtelierReconciler{
				Client: reader, Scheme: c.Scheme(),
				ControllerNamespace: wbControllerNS, EnableGitWriteBack: tt.enabled,
				NewGitProvider: func(github.Config) (provider.Provider, error) { return git, nil },
			}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(atelier)})
			require.NoError(t, err)

			got := &autoscalingv1alpha1.Atelier{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(atelier), got))
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionTypeGitProviderConnected)
			require.NotNil(t, cond)
			assert.Equal(t, tt.wantReason, cond.Reason)
			assert.Equal(t, tt.wantConnected, got.Status.GitProviderConnected)
			assert.Len(t, reader.secrets, tt.wantReads)
			if !tt.wantConnected {
				assert.Empty(t, git.Calls)
			}
		})
	}
}

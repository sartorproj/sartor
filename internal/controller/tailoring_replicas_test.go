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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

// replicaPrometheus answers the replica query with replicas (no series when
// empty) and every other query with one container sample.
func replicaPrometheus(t *testing.T, replicas string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := []map[string]interface{}{}
		switch {
		case strings.Contains(r.FormValue("query"), "kube_deployment_spec_replicas"):
			if replicas != "" {
				result = append(result, map[string]interface{}{
					"metric": map[string]string{}, "value": []interface{}{1700000000.0, replicas},
				})
			}
		default:
			result = append(result, map[string]interface{}{
				"metric": map[string]string{"container": "app"}, "value": []interface{}{1700000000.0, "0.25"},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"data":   map[string]interface{}{"resultType": "vector", "result": result},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestReconcile_RecordsReplicas checks the replica count from
// kube-state-metrics is recorded with the analysis, and left unset (not 0)
// when the metrics backend does not report it.
func TestReconcile_RecordsReplicas(t *testing.T) {
	tests := []struct {
		name     string
		replicas string
		want     *int32
	}{
		{name: "reported", replicas: "3", want: ptrInt32(3)},
		{name: "scaled to zero", replicas: "0", want: ptrInt32(0)},
		{name: "not reported", replicas: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := replicaPrometheus(t, tt.replicas)
			c := fake.NewClientBuilder().WithScheme(unitScheme(t)).WithObjects(reconcileFixtures(server.URL)...).
				WithStatusSubresource(&autoscalingv1alpha1.Tailoring{}).Build()
			r := &TailoringReconciler{
				Client: c, Scheme: c.Scheme(),
				jitter: func(*autoscalingv1alpha1.Tailoring) float64 { return 0 },
			}
			key := types.NamespacedName{Namespace: "example", Name: "web"}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			tl := &autoscalingv1alpha1.Tailoring{}
			require.NoError(t, c.Get(context.Background(), key, tl))
			require.NotNil(t, tl.Status.LastAnalysis)
			assert.Equal(t, tt.want, tl.Status.LastAnalysis.Replicas)
		})
	}
}

func ptrInt32(v int32) *int32 { return &v }

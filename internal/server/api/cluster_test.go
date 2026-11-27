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

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/argocd"
)

func TestHandler_GetPrometheusMetrics_Cluster(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.FormValue("query"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"data":   map[string]interface{}{"resultType": "matrix", "result": []interface{}{}},
		})
	}))
	defer prom.Close()

	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       autoscalingv1alpha1.AtelierSpec{Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: prom.URL, ClusterLabel: "cluster"}},
	}
	handler := NewHandler(newFakeClient(atelier))
	base := "/api/v1/prometheus/metrics?namespace=blue&name=web&containerName=app&resourceType=cpu"

	rec := httptest.NewRecorder()
	handler.GetPrometheusMetrics(rec, httptest.NewRequest(http.MethodGet, base+"&cluster=east", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotEmpty(t, queries)
	for _, q := range queries {
		assert.Equal(t, strings.Count(q, `namespace="`), strings.Count(q, `cluster="east", namespace="blue"`), q)
	}

	queries = nil
	rec = httptest.NewRecorder()
	handler.GetPrometheusMetrics(rec, httptest.NewRequest(http.MethodGet, base, nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "a target cluster is required")
	assert.Empty(t, queries, "no unscoped query")
}

func TestHandler_DetectArgoCDApp_Cluster(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, autoscalingv1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: "http://prom:9090"},
			ArgoCD:     &autoscalingv1alpha1.ArgoCDConfig{Enabled: true},
		},
	}
	local := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "blue", Name: "web", Labels: map[string]string{argocd.ArgoCDAppLabel: "local-app"},
	}}
	handler := NewHandler(fake.NewClientBuilder().WithScheme(scheme).WithObjects(atelier, local).Build())

	detect := func(query string) Response {
		rec := httptest.NewRecorder()
		handler.DetectArgoCDApp(rec, httptest.NewRequest(http.MethodGet, "/api/v1/argocd/detect?namespace=blue&workload=web"+query, nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var resp Response
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	assert.NotNil(t, detect("").Data, "without cluster the local workload labels are used")
	resp := detect("&cluster=east")
	assert.Nil(t, resp.Data, "a workload in another cluster is not looked up in this cluster")
	assert.Equal(t, "No ArgoCD application found", resp.Message)
}

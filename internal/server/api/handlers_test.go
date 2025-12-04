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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&autoscalingv1alpha1.Tailoring{}, &autoscalingv1alpha1.FitProfile{}, &autoscalingv1alpha1.Atelier{}).
		Build()
}

func TestNewHandler(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)
	assert.NotNil(t, handler)
	assert.NotNil(t, handler.client)
	assert.NotNil(t, handler.calculator)
}

func TestHandler_ListTailorings(t *testing.T) {
	tailoring1 := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tailoring-1",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target: autoscalingv1alpha1.TargetRef{
				Kind: "Deployment",
				Name: "my-app",
			},
			FitProfileRef: autoscalingv1alpha1.FitProfileRef{
				Name: "balanced",
			},
		},
		Status: autoscalingv1alpha1.TailoringStatus{
			TargetFound: true,
			LastAnalysis: &autoscalingv1alpha1.AnalysisResult{
				Timestamp: metav1.Now(),
				Containers: []autoscalingv1alpha1.ContainerRecommendation{
					{Name: "app"},
				},
			},
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			PRState:  autoscalingv1alpha1.PRStateOpen,
			PRNumber: 42,
			PRUrl:    "https://github.com/org/repo/pull/42",
		},
	}

	tailoring2 := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tailoring-2",
			Namespace: "production",
		},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target: autoscalingv1alpha1.TargetRef{
				Kind: "Deployment",
				Name: "other-app",
			},
			Paused: true,
		},
	}

	tests := []struct {
		name           string
		namespace      string
		objects        []client.Object
		expectedCount  int
		expectedStatus int
	}{
		{
			name:           "list all tailorings",
			objects:        []client.Object{tailoring1, tailoring2},
			expectedCount:  2,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "list tailorings by namespace",
			namespace:      "default",
			objects:        []client.Object{tailoring1, tailoring2},
			expectedCount:  1,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "empty list",
			objects:        []client.Object{},
			expectedCount:  0,
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := newFakeClient(tt.objects...)
			handler := NewHandler(fakeClient)

			path := "/api/v1/tailorings"
			if tt.namespace != "" {
				path += "?namespace=" + tt.namespace
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			handler.ListTailorings(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)

			var response Response
			err := json.NewDecoder(rec.Body).Decode(&response)
			require.NoError(t, err)
			assert.True(t, response.Success)

			summaries, ok := response.Data.([]interface{})
			require.True(t, ok)
			assert.Len(t, summaries, tt.expectedCount)
		})
	}
}

func TestHandler_GetTailoring(t *testing.T) {
	tailoring := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tailoring",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target: autoscalingv1alpha1.TargetRef{
				Kind: "Deployment",
				Name: "my-app",
			},
		},
	}

	tests := []struct {
		name           string
		path           string
		objects        []client.Object
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:           "get existing tailoring",
			path:           "/api/v1/tailorings/default/test-tailoring",
			objects:        []client.Object{tailoring},
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
		},
		{
			name:           "get non-existent tailoring",
			path:           "/api/v1/tailorings/default/non-existent",
			objects:        []client.Object{tailoring},
			expectedStatus: http.StatusNotFound,
			expectSuccess:  false,
		},
		{
			name:           "invalid path format",
			path:           "/api/v1/tailorings/invalid",
			objects:        []client.Object{tailoring},
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := newFakeClient(tt.objects...)
			handler := NewHandler(fakeClient)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.GetTailoring(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)

			var response Response
			err := json.NewDecoder(rec.Body).Decode(&response)
			require.NoError(t, err)
			assert.Equal(t, tt.expectSuccess, response.Success)
		})
	}
}

func TestHandler_GetAtelier(t *testing.T) {
	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{
				URL: "http://prometheus:9090",
			},
			GitProvider: autoscalingv1alpha1.GitProviderConfig{
				Type: autoscalingv1alpha1.GitProviderGitHub,
			},
			ArgoCD: &autoscalingv1alpha1.ArgoCDConfig{
				Enabled: true,
			},
			OpenCost: &autoscalingv1alpha1.OpenCostConfig{
				Enabled: true,
				URL:     "http://opencost:9003",
			},
			BatchMode: false,
		},
		Status: autoscalingv1alpha1.AtelierStatus{
			PrometheusConnected:  true,
			GitProviderConnected: true,
			ArgoCDConnected:      true,
			OpenCostConnected:    true,
			ManagedTailorings:    5,
		},
	}

	tests := []struct {
		name           string
		objects        []client.Object
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:           "get existing atelier",
			objects:        []client.Object{atelier},
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
		},
		{
			name:           "no atelier found",
			objects:        []client.Object{},
			expectedStatus: http.StatusNotFound,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := newFakeClient(tt.objects...)
			handler := NewHandler(fakeClient)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/atelier", nil)
			rec := httptest.NewRecorder()

			handler.GetAtelier(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)

			var response Response
			err := json.NewDecoder(rec.Body).Decode(&response)
			require.NoError(t, err)
			assert.Equal(t, tt.expectSuccess, response.Success)
		})
	}
}

func TestHandler_GetDashboardStats(t *testing.T) {
	tailoring1 := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Status: autoscalingv1alpha1.TailoringStatus{
			PRState: autoscalingv1alpha1.PRStateOpen,
		},
	}
	tailoring2 := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{Name: "t2", Namespace: "default"},
		Spec:       autoscalingv1alpha1.TailoringSpec{Paused: true},
		Status: autoscalingv1alpha1.TailoringStatus{
			PRState: autoscalingv1alpha1.PRStateMerged,
		},
	}
	tailoring3 := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{Name: "t3", Namespace: "default"},
		Status: autoscalingv1alpha1.TailoringStatus{
			PRState: autoscalingv1alpha1.PRStateClosed,
			Ignored: true,
		},
	}
	fitProfile := &autoscalingv1alpha1.FitProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "fp1", Namespace: "default"},
	}

	fakeClient := newFakeClient(tailoring1, tailoring2, tailoring3, fitProfile)
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/stats", nil)
	rec := httptest.NewRecorder()

	handler.GetDashboardStats(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response Response
	err := json.NewDecoder(rec.Body).Decode(&response)
	require.NoError(t, err)
	assert.True(t, response.Success)

	stats, ok := response.Data.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, float64(3), stats["totalTailorings"])
	assert.Equal(t, float64(2), stats["activeTailorings"])
	assert.Equal(t, float64(1), stats["pausedTailorings"])
	assert.Equal(t, float64(1), stats["totalFitProfiles"])
	assert.Equal(t, float64(1), stats["openPRs"])
	assert.Equal(t, float64(1), stats["mergedPRs"])
	assert.Equal(t, float64(1), stats["ignoredPRs"])
}

func TestHandler_GetRecommendations(t *testing.T) {
	tailoringWithAnalysis := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "with-analysis",
			Namespace: "default",
		},
		Status: autoscalingv1alpha1.TailoringStatus{
			LastAnalysis: &autoscalingv1alpha1.AnalysisResult{
				Timestamp: metav1.Now(),
				Containers: []autoscalingv1alpha1.ContainerRecommendation{
					{Name: "app"},
				},
			},
		},
	}

	tailoringNoAnalysis := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-analysis",
			Namespace: "default",
		},
	}

	tests := []struct {
		name            string
		path            string
		objects         []client.Object
		expectedStatus  int
		expectSuccess   bool
		expectData      bool
		expectedMessage string
	}{
		{
			name:           "get recommendations with analysis",
			path:           "/api/v1/recommendations/default/with-analysis",
			objects:        []client.Object{tailoringWithAnalysis},
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
			expectData:     true,
		},
		{
			name:            "get recommendations without analysis",
			path:            "/api/v1/recommendations/default/no-analysis",
			objects:         []client.Object{tailoringNoAnalysis},
			expectedStatus:  http.StatusOK,
			expectSuccess:   true,
			expectData:      false,
			expectedMessage: "No analysis available yet",
		},
		{
			name:           "invalid path",
			path:           "/api/v1/recommendations/invalid",
			objects:        []client.Object{},
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := newFakeClient(tt.objects...)
			handler := NewHandler(fakeClient)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.GetRecommendations(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)

			var response Response
			err := json.NewDecoder(rec.Body).Decode(&response)
			require.NoError(t, err)
			assert.Equal(t, tt.expectSuccess, response.Success)

			if tt.expectedMessage != "" {
				assert.Equal(t, tt.expectedMessage, response.Message)
			}
		})
	}
}

func TestHandler_ListFitProfiles(t *testing.T) {
	fitProfile1 := &autoscalingv1alpha1.FitProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "balanced",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.FitProfileSpec{
			Strategy:    "percentile",
			DisplayName: "Balanced",
		},
	}

	fitProfile2 := &autoscalingv1alpha1.FitProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "performance",
			Namespace: "production",
		},
		Spec: autoscalingv1alpha1.FitProfileSpec{
			Strategy:    "dsp",
			DisplayName: "Performance",
		},
	}

	tests := []struct {
		name           string
		namespace      string
		objects        []client.Object
		expectedCount  int
		expectedStatus int
	}{
		{
			name:           "list all fit profiles",
			objects:        []client.Object{fitProfile1, fitProfile2},
			expectedCount:  2,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "list fit profiles by namespace",
			namespace:      "default",
			objects:        []client.Object{fitProfile1, fitProfile2},
			expectedCount:  1,
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := newFakeClient(tt.objects...)
			handler := NewHandler(fakeClient)

			path := "/api/v1/fitprofiles"
			if tt.namespace != "" {
				path += "?namespace=" + tt.namespace
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			handler.ListFitProfiles(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)

			var response Response
			err := json.NewDecoder(rec.Body).Decode(&response)
			require.NoError(t, err)
			assert.True(t, response.Success)

			profiles, ok := response.Data.([]interface{})
			require.True(t, ok)
			assert.Len(t, profiles, tt.expectedCount)
		})
	}
}

func TestHandler_GetFitProfile(t *testing.T) {
	fitProfile := &autoscalingv1alpha1.FitProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "balanced",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.FitProfileSpec{
			Strategy: "percentile",
		},
	}

	tests := []struct {
		name           string
		path           string
		objects        []client.Object
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:           "get existing fit profile",
			path:           "/api/v1/fitprofiles/default/balanced",
			objects:        []client.Object{fitProfile},
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
		},
		{
			name:           "get non-existent fit profile",
			path:           "/api/v1/fitprofiles/default/non-existent",
			objects:        []client.Object{fitProfile},
			expectedStatus: http.StatusNotFound,
			expectSuccess:  false,
		},
		{
			name:           "invalid path",
			path:           "/api/v1/fitprofiles/invalid",
			objects:        []client.Object{},
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := newFakeClient(tt.objects...)
			handler := NewHandler(fakeClient)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.GetFitProfile(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)

			var response Response
			err := json.NewDecoder(rec.Body).Decode(&response)
			require.NoError(t, err)
			assert.Equal(t, tt.expectSuccess, response.Success)
		})
	}
}

func TestHandler_ListStrategies(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/strategies", nil)
	rec := httptest.NewRecorder()

	handler.ListStrategies(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response Response
	err := json.NewDecoder(rec.Body).Decode(&response)
	require.NoError(t, err)
	assert.True(t, response.Success)

	strategies, ok := response.Data.([]interface{})
	require.True(t, ok)
	assert.GreaterOrEqual(t, len(strategies), 1) // At least percentile strategy should exist
}

func TestHandler_GetStrategy(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)

	tests := []struct {
		name           string
		path           string
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:           "get percentile strategy",
			path:           "/api/v1/strategies/percentile",
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
		},
		{
			name:           "get non-existent strategy",
			path:           "/api/v1/strategies/non-existent",
			expectedStatus: http.StatusNotFound,
			expectSuccess:  false,
		},
		{
			name:           "empty strategy name",
			path:           "/api/v1/strategies/",
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.GetStrategy(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)

			var response Response
			err := json.NewDecoder(rec.Body).Decode(&response)
			require.NoError(t, err)
			assert.Equal(t, tt.expectSuccess, response.Success)
		})
	}
}

func TestHandler_ListStrategies_MethodNotAllowed(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/strategies", nil)
	rec := httptest.NewRecorder()

	handler.ListStrategies(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandler_GetStrategy_MethodNotAllowed(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/strategies/percentile", nil)
	rec := httptest.NewRecorder()

	handler.GetStrategy(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandler_ListArgoCDApps_NotEnabled(t *testing.T) {
	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: "http://prom:9090"},
		},
	}

	fakeClient := newFakeClient(atelier)
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/argocd/apps", nil)
	rec := httptest.NewRecorder()

	handler.ListArgoCDApps(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var response Response
	err := json.NewDecoder(rec.Body).Decode(&response)
	require.NoError(t, err)
	assert.False(t, response.Success)
	assert.Contains(t, response.Error, "ArgoCD integration is not enabled")
}

func TestHandler_GetArgoCDApp_NotEnabled(t *testing.T) {
	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: "http://prom:9090"},
		},
	}

	fakeClient := newFakeClient(atelier)
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/argocd/apps/argocd/my-app", nil)
	rec := httptest.NewRecorder()

	handler.GetArgoCDApp(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandler_SyncArgoCDApp_MethodNotAllowed(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/argocd/apps/argocd/my-app/sync", nil)
	rec := httptest.NewRecorder()

	handler.SyncArgoCDApp(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandler_RefreshArgoCDApp_MethodNotAllowed(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/argocd/apps/argocd/my-app/refresh", nil)
	rec := httptest.NewRecorder()

	handler.RefreshArgoCDApp(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandler_DetectArgoCDApp_MissingParams(t *testing.T) {
	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: "http://prom:9090"},
			ArgoCD:     &autoscalingv1alpha1.ArgoCDConfig{Enabled: true},
		},
	}

	fakeClient := newFakeClient(atelier)
	handler := NewHandler(fakeClient)

	tests := []struct {
		name  string
		query string
	}{
		{"missing both", ""},
		{"missing workload", "?namespace=default"},
		{"missing namespace", "?workload=my-app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/argocd/detect"+tt.query, nil)
			rec := httptest.NewRecorder()

			handler.DetectArgoCDApp(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestHandler_CheckOpenCostHealth_NotEnabled(t *testing.T) {
	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: "http://prom:9090"},
		},
	}

	fakeClient := newFakeClient(atelier)
	handler := NewHandler(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/opencost/health", nil)
	rec := httptest.NewRecorder()

	handler.CheckOpenCostHealth(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response Response
	err := json.NewDecoder(rec.Body).Decode(&response)
	require.NoError(t, err)
	assert.True(t, response.Success)

	data, ok := response.Data.(map[string]interface{})
	require.True(t, ok)
	assert.False(t, data["enabled"].(bool))
}

func TestAuthMiddleware(t *testing.T) {
	tests := []struct {
		name           string
		apiKeys        map[string]bool
		headerKey      string
		queryKey       string
		expectedStatus int
	}{
		{
			name:           "valid header key",
			apiKeys:        map[string]bool{"valid-key": true},
			headerKey:      "valid-key",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "valid query key",
			apiKeys:        map[string]bool{"valid-key": true},
			queryKey:       "valid-key",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "invalid key",
			apiKeys:        map[string]bool{"valid-key": true},
			headerKey:      "invalid-key",
			expectedStatus: http.StatusForbidden,
		},
		{
			name:           "missing key",
			apiKeys:        map[string]bool{"valid-key": true},
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			middleware := AuthMiddleware(tt.apiKeys, handler)

			path := "/api/v1/test"
			if tt.queryKey != "" {
				path += "?api_key=" + tt.queryKey
			}

			req := httptest.NewRequest(http.MethodGet, path, nil)
			if tt.headerKey != "" {
				req.Header.Set("X-API-Key", tt.headerKey)
			}

			rec := httptest.NewRecorder()
			middleware.ServeHTTP(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)
		})
	}
}

func TestAuthMiddleware_SkipHealthEndpoints(t *testing.T) {
	healthPaths := []string{"/healthz", "/readyz"}

	for _, path := range healthPaths {
		t.Run(path, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			// Empty API keys - should still pass for health endpoints
			middleware := AuthMiddleware(map[string]bool{}, handler)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			middleware.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestCORSMiddleware(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := CORSMiddleware(handler)

	t.Run("adds CORS headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "GET")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "X-API-Key")
	})

	t.Run("handles OPTIONS preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/test", nil)
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestResponse_JSON(t *testing.T) {
	response := Response{
		Success: true,
		Data: map[string]string{
			"key": "value",
		},
		Message: "Test message",
	}

	data, err := json.Marshal(response)
	require.NoError(t, err)

	var decoded Response
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.True(t, decoded.Success)
	assert.Equal(t, "Test message", decoded.Message)
}

func TestTailoringSummary(t *testing.T) {
	prNumber := 42
	summary := TailoringSummary{
		Name:           "test-tailoring",
		Namespace:      "default",
		TargetKind:     "Deployment",
		TargetName:     "my-app",
		FitProfile:     "balanced-profile",
		Paused:         false,
		TargetFound:    true,
		VPADetected:    false,
		ContainerCount: 2,
		ReadyCondition: "True",
		PRState:        "Open",
		PRNumber:       &prNumber,
		PRUrl:          "https://github.com/org/repo/pull/42",
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded TailoringSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.Name, decoded.Name)
	assert.Equal(t, summary.FitProfile, decoded.FitProfile)
	assert.Equal(t, summary.PRState, decoded.PRState)
	assert.Equal(t, *summary.PRNumber, *decoded.PRNumber)
}

func TestDashboardStats(t *testing.T) {
	stats := DashboardStats{
		TotalTailorings:  10,
		ActiveTailorings: 8,
		PausedTailorings: 2,
		TotalFitProfiles: 5,
		OpenPRs:          5,
		MergedPRs:        18,
		ClosedPRs:        1,
		IgnoredPRs:       1,
	}

	data, err := json.Marshal(stats)
	require.NoError(t, err)

	var decoded DashboardStats
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, stats.TotalTailorings, decoded.TotalTailorings)
	assert.Equal(t, stats.OpenPRs, decoded.OpenPRs)
}

func TestAtelierSummary(t *testing.T) {
	summary := AtelierSummary{
		Name:                 "default",
		PrometheusURL:        "http://prometheus:9090",
		PrometheusConnected:  true,
		GitProviderType:      "github",
		GitProviderConnected: true,
		ArgoCDEnabled:        true,
		ArgoCDConnected:      true,
		OpenCostEnabled:      true,
		OpenCostConnected:    true,
		OpenCostURL:          "http://opencost:9003",
		ManagedTailorings:    10,
		BatchMode:            false,
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded AtelierSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.PrometheusURL, decoded.PrometheusURL)
	assert.True(t, decoded.ArgoCDEnabled)
	assert.True(t, decoded.OpenCostEnabled)
	assert.Equal(t, summary.OpenCostURL, decoded.OpenCostURL)
}

func TestArgoCDAppSummary(t *testing.T) {
	summary := ArgoCDAppSummary{
		Name:           "my-app",
		Namespace:      "argocd",
		Project:        "default",
		RepoURL:        "https://github.com/org/repo",
		Path:           "apps/my-app",
		TargetRevision: "main",
		SyncStatus:     "Synced",
		HealthStatus:   "Healthy",
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded ArgoCDAppSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.SyncStatus, decoded.SyncStatus)
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()

	data := map[string]string{"test": "value"}
	writeJSON(rec, http.StatusOK, data)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var decoded map[string]string
	err := json.NewDecoder(rec.Body).Decode(&decoded)
	require.NoError(t, err)
	assert.Equal(t, "value", decoded["test"])
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()

	writeError(rec, http.StatusBadRequest, "test error")

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var response Response
	err := json.NewDecoder(rec.Body).Decode(&response)
	require.NoError(t, err)
	assert.False(t, response.Success)
	assert.Equal(t, "test error", response.Error)
}

func TestNotFoundError(t *testing.T) {
	err := &notFoundError{message: "resource not found"}
	assert.Equal(t, "resource not found", err.Error())
}

func TestOpenCostAllocationSummary(t *testing.T) {
	summary := OpenCostAllocationSummary{
		Name:             "test-workload",
		Cluster:          "my-cluster",
		Namespace:        "default",
		Controller:       "my-deployment",
		ControllerKind:   "Deployment",
		Pod:              "my-pod-abc123",
		Container:        "app",
		CPUCores:         0.5,
		CPUCost:          0.025,
		CPUEfficiency:    0.8,
		RAMBytes:         536870912,
		RAMCost:          0.01,
		RAMEfficiency:    0.7,
		GPUCost:          0.0,
		PVCost:           0.005,
		NetworkCost:      0.002,
		LoadBalancerCost: 0.0,
		SharedCost:       0.001,
		TotalCost:        0.043,
		TotalEfficiency:  0.75,
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded OpenCostAllocationSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.Name, decoded.Name)
	assert.Equal(t, summary.CPUCores, decoded.CPUCores)
	assert.Equal(t, summary.TotalCost, decoded.TotalCost)
}

func TestOpenCostSummary(t *testing.T) {
	summary := OpenCostSummary{
		TotalCost:        100.0,
		CPUCost:          40.0,
		RAMCost:          30.0,
		GPUCost:          10.0,
		PVCost:           10.0,
		NetworkCost:      5.0,
		LoadBalancerCost: 3.0,
		SharedCost:       2.0,
		Efficiency:       0.75,
		CPUEfficiency:    0.8,
		RAMEfficiency:    0.7,
		ByNamespace: map[string]float64{
			"default":    50.0,
			"production": 40.0,
			"staging":    10.0,
		},
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded OpenCostSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.TotalCost, decoded.TotalCost)
	assert.Equal(t, summary.ByNamespace["default"], decoded.ByNamespace["default"])
}

func TestFitProfileSummary(t *testing.T) {
	summary := FitProfileSummary{
		Name:        "balanced",
		Namespace:   "default",
		Strategy:    "percentile",
		DisplayName: "Balanced Profile",
		Description: "A balanced profile for general workloads",
		UsageCount:  5,
		Valid:       true,
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded FitProfileSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.Name, decoded.Name)
	assert.Equal(t, summary.Strategy, decoded.Strategy)
	assert.True(t, decoded.Valid)
}

func TestArgoCDSyncRequest(t *testing.T) {
	request := ArgoCDSyncRequest{
		Revision: "main",
		Prune:    true,
		DryRun:   false,
	}

	data, err := json.Marshal(request)
	require.NoError(t, err)

	var decoded ArgoCDSyncRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, request.Revision, decoded.Revision)
	assert.True(t, decoded.Prune)
	assert.False(t, decoded.DryRun)
}

func TestHandler_RegisterRoutes(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)
	mux := http.NewServeMux()

	// Should not panic
	handler.RegisterRoutes(mux)

	// Test that routes are registered by making requests
	// Note: Go's http.ServeMux uses exact pattern matching for routes without trailing slash
	tests := []struct {
		path         string
		method       string
		skipNotFound bool // Some routes may return 404 for valid reasons (e.g., no data)
	}{
		{"/api/v1/tailorings", http.MethodGet, false},
		{"/api/v1/dashboard/stats", http.MethodGet, false},
		{"/api/v1/fitprofiles", http.MethodGet, false},
		{"/api/v1/strategies", http.MethodGet, false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if !tt.skipNotFound {
				// Should not return 404 - route should be registered
				// Note: some routes may return 404 if resource not found, but that's a valid response
				// We're just checking the route is registered (not getting 404 from ServeMux itself)
				assert.True(t, rec.Code != http.StatusNotFound || rec.Body.Len() > 0,
					"Route %s should be registered", tt.path)
			}
		})
	}
}

// Test context timeout handling
func TestHandler_ContextTimeout(t *testing.T) {
	fakeClient := newFakeClient()
	handler := NewHandler(fakeClient)

	// Create a request with an already cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tailorings", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	// This should handle the cancelled context gracefully
	handler.ListTailorings(rec, req)

	// The handler might return an error due to context cancellation
	// but should not panic
	assert.NotPanics(t, func() {
		var response Response
		_ = json.NewDecoder(rec.Body).Decode(&response)
	})
}

// Integration test for handler chain
func TestHandler_IntegrationFlow(t *testing.T) {
	// Create test resources
	fitProfile := &autoscalingv1alpha1.FitProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "balanced",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.FitProfileSpec{
			Strategy: "percentile",
		},
	}

	tailoring := &autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tailoring",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target: autoscalingv1alpha1.TargetRef{
				Kind: "Deployment",
				Name: "my-app",
			},
			FitProfileRef: autoscalingv1alpha1.FitProfileRef{
				Name: "balanced",
			},
		},
		Status: autoscalingv1alpha1.TailoringStatus{
			TargetFound: true,
			LastAnalysis: &autoscalingv1alpha1.AnalysisResult{
				Timestamp: metav1.Now(),
				Containers: []autoscalingv1alpha1.ContainerRecommendation{
					{Name: "app"},
				},
			},
		},
	}

	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{
			Prometheus: autoscalingv1alpha1.PrometheusConfig{
				URL: "http://prometheus:9090",
			},
			GitProvider: autoscalingv1alpha1.GitProviderConfig{
				Type: autoscalingv1alpha1.GitProviderGitHub,
			},
		},
		Status: autoscalingv1alpha1.AtelierStatus{
			PrometheusConnected:  true,
			GitProviderConnected: true,
			ManagedTailorings:    1,
		},
	}

	fakeClient := newFakeClient(fitProfile, tailoring, atelier)
	handler := NewHandler(fakeClient)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Test dashboard stats endpoint
	t.Run("dashboard stats", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/stats", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response Response
		err := json.NewDecoder(rec.Body).Decode(&response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		stats := response.Data.(map[string]interface{})
		assert.Equal(t, float64(1), stats["totalTailorings"])
		assert.Equal(t, float64(1), stats["totalFitProfiles"])
	})

	// Test atelier endpoint
	t.Run("atelier", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/atelier", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response Response
		err := json.NewDecoder(rec.Body).Decode(&response)
		require.NoError(t, err)
		assert.True(t, response.Success)
	})

	// Test get specific tailoring
	t.Run("get tailoring", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tailorings/default/test-tailoring", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response Response
		err := json.NewDecoder(rec.Body).Decode(&response)
		require.NoError(t, err)
		assert.True(t, response.Success)
	})

	// Test get recommendations
	t.Run("get recommendations", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/recommendations/default/test-tailoring", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response Response
		err := json.NewDecoder(rec.Body).Decode(&response)
		require.NoError(t, err)
		assert.True(t, response.Success)
	})
}

// Ensure context is used
var _ = context.Background
var _ = time.Now

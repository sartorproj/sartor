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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded TailoringSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.Name, decoded.Name)
	assert.Equal(t, summary.FitProfile, decoded.FitProfile)
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

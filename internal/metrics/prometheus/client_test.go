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

package prometheus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "basic config",
			config: Config{
				URL: "http://prometheus:9090",
			},
			wantErr: false,
		},
		{
			name: "config with insecure",
			config: Config{
				URL:                "https://prometheus:9090",
				InsecureSkipVerify: true,
			},
			wantErr: false,
		},
		{
			name: "config with auth",
			config: Config{
				URL:      "http://prometheus:9090",
				Username: "admin",
				Password: "secret",
			},
			wantErr: false,
		},
		{
			name: "config with token",
			config: Config{
				URL:   "http://prometheus:9090",
				Token: "bearer-token",
			},
			wantErr: false,
		},
		{
			name: "empty URL",
			config: Config{
				URL: "",
			},
			wantErr: false, // Client creation succeeds, but connection will fail
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.config)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, client)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, client)
			}
		})
	}
}

func TestClient_CheckConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prometheus /api/v1/status/config endpoint
		if r.URL.Path == "/api/v1/status/config" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Return valid Prometheus config response
			config := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"yaml": "global:\n  scrape_interval: 15s\n",
				},
			}
			_ = json.NewEncoder(w).Encode(config)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	err = client.CheckConnection(ctx)
	assert.NoError(t, err)
}

func TestClient_GetContainerMetrics(t *testing.T) {
	// This test requires a real Prometheus instance or more complex mocking
	// For now, we'll just test that the function signature is correct
	// and that it handles errors properly

	// Test with invalid URL (should fail connection)
	client, err := NewClient(Config{URL: "http://invalid-prometheus:9090"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	metrics, err := client.GetContainerMetrics(ctx, "default", "test-pod", time.Hour)

	// Should fail due to connection error, not due to API signature
	assert.Error(t, err)
	assert.Nil(t, metrics)
}

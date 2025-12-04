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
			name: "config with basic auth",
			config: Config{
				URL:      "http://prometheus:9090",
				Username: "admin",
				Password: "secret",
			},
			wantErr: false,
		},
		{
			name: "config with bearer token",
			config: Config{
				URL:   "http://prometheus:9090",
				Token: "bearer-token-12345",
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
	t.Run("successful connection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Prometheus /api/v1/status/config endpoint
			if r.URL.Path == "/api/v1/status/config" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
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
	})

	t.Run("failed connection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		client, err := NewClient(Config{URL: server.URL})
		require.NoError(t, err)

		ctx := context.Background()
		err = client.CheckConnection(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to connect to Prometheus")
	})
}

func TestClient_GetContainerMetrics(t *testing.T) {
	// Create a mock Prometheus server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			// Return mock metrics data
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result": []map[string]interface{}{
						{
							"metric": map[string]string{
								"container": "app",
								"namespace": "default",
								"pod":       "test-pod",
							},
							"value": []interface{}{1234567890.123, "0.5"},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	metrics, err := client.GetContainerMetrics(ctx, "default", "test-pod", time.Hour)

	require.NoError(t, err)
	assert.NotEmpty(t, metrics)
}

func TestClient_GetContainerMetrics_ConnectionError(t *testing.T) {
	// Test with invalid URL (should fail connection)
	client, err := NewClient(Config{URL: "http://invalid-prometheus:9090"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	metrics, err := client.GetContainerMetrics(ctx, "default", "test-pod", time.Hour)

	// Should fail due to connection error
	assert.Error(t, err)
	assert.Nil(t, metrics)
}

func TestClient_GetTimeSeriesMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			// Return mock time series data
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "matrix",
					"result": []map[string]interface{}{
						{
							"metric": map[string]string{
								"container": "app",
								"namespace": "default",
								"pod":       "test-pod",
							},
							"values": [][]interface{}{
								{1234567890.0, "0.5"},
								{1234567900.0, "0.6"},
								{1234567910.0, "0.55"},
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("cpu metrics", func(t *testing.T) {
		points, err := client.GetTimeSeriesMetrics(ctx, "default", "test-pod", "app", "cpu", time.Hour)
		require.NoError(t, err)
		assert.NotEmpty(t, points)
	})

	t.Run("memory metrics", func(t *testing.T) {
		points, err := client.GetTimeSeriesMetrics(ctx, "default", "test-pod", "app", "memory", time.Hour)
		require.NoError(t, err)
		assert.NotEmpty(t, points)
	})

	t.Run("unsupported resource type", func(t *testing.T) {
		_, err := client.GetTimeSeriesMetrics(ctx, "default", "test-pod", "app", "unsupported", time.Hour)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported resource type")
	})
}

func TestClient_GetTimeSeriesMetrics_ShortWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "matrix",
					"result":     []map[string]interface{}{},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	// Use very short window to test step calculation
	points, err := client.GetTimeSeriesMetrics(ctx, "default", "test-pod", "app", "cpu", time.Second*10)
	require.NoError(t, err)
	assert.Empty(t, points)
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		expected string
	}{
		{30 * time.Second, "30s"},
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{time.Hour, "1h"},
		{2 * time.Hour, "2h"},
		{24 * time.Hour, "1d"},
		{48 * time.Hour, "2d"},
		{7 * 24 * time.Hour, "7d"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatDuration(tt.duration)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestContainerMetrics_Structure(t *testing.T) {
	metrics := ContainerMetrics{
		ContainerName: "app",
	}

	assert.Equal(t, "app", metrics.ContainerName)
	assert.Nil(t, metrics.P95CPU)
	assert.Nil(t, metrics.P99CPU)
	assert.Nil(t, metrics.P95Memory)
	assert.Nil(t, metrics.P99Memory)
}

func TestTimeSeriesPoint_Structure(t *testing.T) {
	point := TimeSeriesPoint{
		Timestamp: 1234567890,
		Value:     0.5,
	}

	data, err := json.Marshal(point)
	require.NoError(t, err)

	var decoded TimeSeriesPoint
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, point.Timestamp, decoded.Timestamp)
	assert.Equal(t, point.Value, decoded.Value)
}

func TestTimeSeriesMetrics_Structure(t *testing.T) {
	metrics := TimeSeriesMetrics{
		ContainerName: "app",
		ResourceType:  "cpu",
		Data: []TimeSeriesPoint{
			{Timestamp: 1234567890, Value: 0.5},
			{Timestamp: 1234567900, Value: 0.6},
		},
	}

	data, err := json.Marshal(metrics)
	require.NoError(t, err)

	var decoded TimeSeriesMetrics
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, metrics.ContainerName, decoded.ContainerName)
	assert.Equal(t, metrics.ResourceType, decoded.ResourceType)
	assert.Len(t, decoded.Data, 2)
}

func TestConfig_Structure(t *testing.T) {
	config := Config{
		URL:                "http://prometheus:9090",
		Username:           "admin",
		Password:           "secret",
		Token:              "",
		InsecureSkipVerify: true,
	}

	assert.Equal(t, "http://prometheus:9090", config.URL)
	assert.Equal(t, "admin", config.Username)
	assert.Equal(t, "secret", config.Password)
	assert.True(t, config.InsecureSkipVerify)
}

func TestBasicAuthRoundTripper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "admin" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := &basicAuthRoundTripper{
		username: "admin",
		password: "secret",
		rt:       http.DefaultTransport,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestBearerAuthRoundTripper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := &bearerAuthRoundTripper{
		token: "test-token",
		rt:    http.DefaultTransport,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestClient_QueryAndExtract_UnexpectedResultType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			// Return scalar result instead of vector
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "scalar",
					"result":     []interface{}{1234567890.123, "0.5"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	metrics, err := client.GetContainerMetrics(ctx, "default", "test-pod", time.Hour)

	// Should return error due to unexpected result type
	assert.Error(t, err)
	assert.Nil(t, metrics)
}

func TestClient_GetTimeSeriesMetrics_UnexpectedResultType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			// Return vector result instead of matrix
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result":     []map[string]interface{}{},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	points, err := client.GetTimeSeriesMetrics(ctx, "default", "test-pod", "app", "cpu", time.Hour)

	// Should return error due to unexpected result type
	assert.Error(t, err)
	assert.Nil(t, points)
}

func TestClient_GetContainerMetrics_EmptyResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result":     []map[string]interface{}{},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	metrics, err := client.GetContainerMetrics(ctx, "default", "test-pod", time.Hour)

	require.NoError(t, err)
	assert.Empty(t, metrics)
}

func TestClient_GetContainerMetrics_EmptyContainerName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result": []map[string]interface{}{
						{
							"metric": map[string]string{
								"container": "", // Empty container name
								"namespace": "default",
								"pod":       "test-pod",
							},
							"value": []interface{}{1234567890.123, "0.5"},
						},
						{
							"metric": map[string]string{
								"container": "app",
								"namespace": "default",
								"pod":       "test-pod",
							},
							"value": []interface{}{1234567890.123, "0.6"},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	metrics, err := client.GetContainerMetrics(ctx, "default", "test-pod", time.Hour)

	require.NoError(t, err)
	// Should only contain the container with non-empty name
	assert.Len(t, metrics, 1)
	assert.Equal(t, "app", metrics[0].ContainerName)
}

func TestClient_GetContainerMetrics_WithWarnings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			response := map[string]interface{}{
				"status":   "success",
				"warnings": []string{"Some warning"},
				"data": map[string]interface{}{
					"resultType": "vector",
					"result": []map[string]interface{}{
						{
							"metric": map[string]string{
								"container": "app",
							},
							"value": []interface{}{1234567890.123, "0.5"},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	metrics, err := client.GetContainerMetrics(ctx, "default", "test-pod", time.Hour)

	// Should succeed despite warnings
	require.NoError(t, err)
	assert.NotEmpty(t, metrics)
}

func TestClient_GetTimeSeriesMetrics_MemoryConversion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "matrix",
					"result": []map[string]interface{}{
						{
							"metric": map[string]string{
								"container": "app",
							},
							"values": [][]interface{}{
								{1234567890.0, "1048576"}, // 1 MiB in bytes
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	points, err := client.GetTimeSeriesMetrics(ctx, "default", "test-pod", "app", "memory", time.Hour)

	require.NoError(t, err)
	assert.Len(t, points, 1)
	// Value should be converted from bytes to MiB
	assert.InDelta(t, 1.0, points[0].Value, 0.01)
}

func TestClient_GetTimeSeriesMetrics_ConnectionError(t *testing.T) {
	client, err := NewClient(Config{URL: "http://invalid-prometheus:9090"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	points, err := client.GetTimeSeriesMetrics(ctx, "default", "test-pod", "app", "cpu", time.Hour)

	assert.Error(t, err)
	assert.Nil(t, points)
}

func TestClient_WithAllAuthMethods(t *testing.T) {
	// Test client creation with all auth methods
	t.Run("basic auth", func(t *testing.T) {
		client, err := NewClient(Config{
			URL:      "http://prometheus:9090",
			Username: "user",
			Password: "pass",
		})
		require.NoError(t, err)
		assert.NotNil(t, client)
		assert.Equal(t, "user", client.config.Username)
		assert.Equal(t, "pass", client.config.Password)
	})

	t.Run("bearer auth", func(t *testing.T) {
		client, err := NewClient(Config{
			URL:   "http://prometheus:9090",
			Token: "my-bearer-token",
		})
		require.NoError(t, err)
		assert.NotNil(t, client)
		assert.Equal(t, "my-bearer-token", client.config.Token)
	})

	t.Run("insecure skip verify", func(t *testing.T) {
		client, err := NewClient(Config{
			URL:                "https://prometheus:9090",
			InsecureSkipVerify: true,
		})
		require.NoError(t, err)
		assert.NotNil(t, client)
		assert.True(t, client.config.InsecureSkipVerify)
	})
}

// Integration test placeholder
func TestClient_Integration(t *testing.T) {
	t.Skip("Skipping integration test - requires real Prometheus server")

	client, err := NewClient(Config{URL: "http://localhost:9090"})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("CheckConnection", func(t *testing.T) {
		err := client.CheckConnection(ctx)
		require.NoError(t, err)
	})

	t.Run("GetContainerMetrics", func(t *testing.T) {
		metrics, err := client.GetContainerMetrics(ctx, "default", "test", 24*time.Hour)
		require.NoError(t, err)
		t.Logf("Found %d containers", len(metrics))
	})

	t.Run("GetTimeSeriesMetrics", func(t *testing.T) {
		points, err := client.GetTimeSeriesMetrics(ctx, "default", "test", "app", "cpu", time.Hour)
		require.NoError(t, err)
		t.Logf("Found %d data points", len(points))
	})
}

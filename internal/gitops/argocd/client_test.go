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

package argocd

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
		name   string
		config Config
	}{
		{
			name: "basic config",
			config: Config{
				ServerURL: "https://argocd.example.com",
				Token:     "test-token",
			},
		},
		{
			name: "config with insecure",
			config: Config{
				ServerURL:          "https://argocd.example.com",
				Token:              "test-token",
				InsecureSkipVerify: true,
			},
		},
		{
			name: "config with timeout",
			config: Config{
				ServerURL: "https://argocd.example.com",
				Token:     "test-token",
				Timeout:   60 * time.Second,
			},
		},
		{
			name:   "cluster-only mode",
			config: Config{
				// No ServerURL, will use k8s client
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.config, nil)
			assert.NotNil(t, client)
		})
	}
}

func TestClient_ListApplications(t *testing.T) {
	mockResponse := `{
		"items": [
			{
				"metadata": {
					"name": "app1",
					"namespace": "argocd"
				},
				"spec": {
					"project": "default",
					"source": {
						"repoURL": "https://github.com/org/repo",
						"path": "apps/app1",
						"targetRevision": "main"
					}
				},
				"status": {
					"sync": {"status": "Synced"},
					"health": {"status": "Healthy"}
				}
			},
			{
				"metadata": {
					"name": "app2",
					"namespace": "argocd"
				},
				"spec": {
					"project": "default",
					"source": {
						"repoURL": "https://github.com/org/repo",
						"path": "apps/app2",
						"targetRevision": "main"
					}
				},
				"status": {
					"sync": {"status": "OutOfSync"},
					"health": {"status": "Degraded"}
				}
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/applications", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResponse))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	apps, err := client.ListApplications(context.Background())
	require.NoError(t, err)
	assert.Len(t, apps, 2)

	assert.Equal(t, "app1", apps[0].Name)
	assert.Equal(t, "Synced", apps[0].SyncStatus)
	assert.Equal(t, "Healthy", apps[0].HealthStatus)

	assert.Equal(t, "app2", apps[1].Name)
	assert.Equal(t, "OutOfSync", apps[1].SyncStatus)
}

func TestClient_GetApplication(t *testing.T) {
	mockResponse := `{
		"metadata": {
			"name": "my-app",
			"namespace": "argocd",
			"labels": {"team": "platform"}
		},
		"spec": {
			"project": "default",
			"source": {
				"repoURL": "https://github.com/org/repo",
				"path": "apps/my-app",
				"targetRevision": "main"
			}
		},
		"status": {
			"sync": {"status": "Synced"},
			"health": {"status": "Healthy"}
		}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/applications/my-app", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResponse))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	app, err := client.GetApplication(context.Background(), "my-app", "argocd")
	require.NoError(t, err)

	assert.Equal(t, "my-app", app.Name)
	assert.Equal(t, "argocd", app.Namespace)
	assert.Equal(t, "default", app.Project)
	assert.Equal(t, "Synced", app.SyncStatus)
	assert.Equal(t, "Healthy", app.HealthStatus)
	assert.Equal(t, "platform", app.Labels["team"])
}

func TestClient_Sync(t *testing.T) {
	tests := []struct {
		name       string
		request    SyncRequest
		handler    http.HandlerFunc
		wantErr    bool
		wantStatus string
	}{
		{
			name:    "successful sync",
			request: SyncRequest{},
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "POST", r.Method)
				assert.Contains(t, r.URL.Path, "/sync")

				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "Syncing"})
			},
			wantErr:    false,
			wantStatus: "Syncing",
		},
		{
			name: "sync with revision",
			request: SyncRequest{
				Revision: "abc123",
				Prune:    true,
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				var req SyncRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				assert.Equal(t, "abc123", req.Revision)
				assert.True(t, req.Prune)

				w.WriteHeader(http.StatusOK)
			},
			wantErr: false,
		},
		{
			name:    "sync failure",
			request: SyncRequest{},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("sync failed"))
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			client := NewClient(Config{
				ServerURL: server.URL,
				Token:     "test-token",
			}, nil)

			result, err := client.Sync(context.Background(), "my-app", "argocd", tt.request)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, result)
			}
		})
	}
}

func TestClient_Refresh(t *testing.T) {
	tests := []struct {
		name    string
		hard    bool
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name: "normal refresh",
			hard: false,
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "normal", r.URL.Query().Get("refresh"))
				w.WriteHeader(http.StatusOK)
			},
			wantErr: false,
		},
		{
			name: "hard refresh",
			hard: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "hard", r.URL.Query().Get("refresh"))
				w.WriteHeader(http.StatusOK)
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			client := NewClient(Config{
				ServerURL: server.URL,
				Token:     "test-token",
			}, nil)

			result, err := client.Refresh(context.Background(), "my-app", "argocd", tt.hard)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.True(t, result.Success)
			}
		})
	}
}

func TestClient_IsArgoCDManaged(t *testing.T) {
	// This test would require mocking the k8s client
	// For now, test that the method exists and handles nil client gracefully
	client := NewClient(Config{}, nil)

	// Without k8s client, should return false
	result := client.IsArgoCDManaged(context.Background(), "default", "my-app")
	assert.False(t, result)
}

func TestApplication_JSON(t *testing.T) {
	app := Application{
		Name:           "test-app",
		Namespace:      "argocd",
		Project:        "default",
		RepoURL:        "https://github.com/org/repo",
		Path:           "apps/test",
		TargetRevision: "main",
		SyncStatus:     "Synced",
		HealthStatus:   "Healthy",
		Labels: map[string]string{
			"team": "platform",
		},
	}

	data, err := json.Marshal(app)
	require.NoError(t, err)

	var decoded Application
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, app.Name, decoded.Name)
	assert.Equal(t, app.SyncStatus, decoded.SyncStatus)
	assert.Equal(t, app.Labels["team"], decoded.Labels["team"])
}

func TestClient_ListApplications_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	apps, err := client.ListApplications(context.Background())
	assert.Error(t, err)
	assert.Nil(t, apps)
	assert.Contains(t, err.Error(), "500")
}

func TestClient_ListApplications_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("invalid json"))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	apps, err := client.ListApplications(context.Background())
	assert.Error(t, err)
	assert.Nil(t, apps)
	assert.Contains(t, err.Error(), "decode")
}

func TestClient_GetApplication_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	app, err := client.GetApplication(context.Background(), "nonexistent", "argocd")
	assert.Error(t, err)
	assert.Nil(t, app)
	assert.Contains(t, err.Error(), "404")
}

func TestClient_GetApplication_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("invalid json"))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	app, err := client.GetApplication(context.Background(), "my-app", "argocd")
	assert.Error(t, err)
	assert.Nil(t, app)
	assert.Contains(t, err.Error(), "decode")
}

func TestClient_Sync_DryRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req SyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		assert.True(t, req.DryRun)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	result, err := client.Sync(context.Background(), "my-app", "argocd", SyncRequest{DryRun: true})
	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestClient_Sync_NetworkError(t *testing.T) {
	client := NewClient(Config{
		ServerURL: "http://localhost:99999",
		Token:     "test-token",
		Timeout:   100 * time.Millisecond,
	}, nil)

	result, err := client.Sync(context.Background(), "my-app", "argocd", SyncRequest{})
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestClient_Refresh_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized"))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "bad-token",
	}, nil)

	result, err := client.Refresh(context.Background(), "my-app", "argocd", false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "401")
}

func TestClient_DetectApplication_NoAPINoK8s(t *testing.T) {
	// Client without API credentials or k8s client
	client := NewClient(Config{}, nil)

	app, err := client.DetectApplication(context.Background(), "default", "my-workload")
	assert.Error(t, err)
	assert.Nil(t, app)
	assert.Contains(t, err.Error(), "could not detect")
}

func TestClient_ListApplications_NoServer(t *testing.T) {
	// Client without server URL and no k8s client
	client := NewClient(Config{}, nil)

	apps, err := client.ListApplications(context.Background())
	assert.Error(t, err)
	assert.Nil(t, apps)
	assert.Contains(t, err.Error(), "no Kubernetes client")
}

func TestSyncRequest_JSON(t *testing.T) {
	req := SyncRequest{
		Revision: "abc123",
		Prune:    true,
		DryRun:   false,
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded SyncRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, req.Revision, decoded.Revision)
	assert.Equal(t, req.Prune, decoded.Prune)
	assert.Equal(t, req.DryRun, decoded.DryRun)
}

func TestSyncResult_JSON(t *testing.T) {
	result := SyncResult{
		Status:   "Synced",
		Message:  "Sync completed",
		Revision: "abc123",
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	var decoded SyncResult
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, result.Status, decoded.Status)
	assert.Equal(t, result.Message, decoded.Message)
	assert.Equal(t, result.Revision, decoded.Revision)
}

func TestRefreshResult_JSON(t *testing.T) {
	result := RefreshResult{
		Success: true,
		Message: "Refresh completed",
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	var decoded RefreshResult
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, result.Success, decoded.Success)
	assert.Equal(t, result.Message, decoded.Message)
}

func TestConfig_Fields(t *testing.T) {
	cfg := Config{
		ServerURL:          "https://argocd.example.com",
		Token:              "secret-token",
		InsecureSkipVerify: true,
		Timeout:            60 * time.Second,
	}

	assert.Equal(t, "https://argocd.example.com", cfg.ServerURL)
	assert.Equal(t, "secret-token", cfg.Token)
	assert.True(t, cfg.InsecureSkipVerify)
	assert.Equal(t, 60*time.Second, cfg.Timeout)
}

func TestClient_DefaultTimeout(t *testing.T) {
	cfg := Config{
		ServerURL: "https://argocd.example.com",
		Token:     "test-token",
		// Timeout not set - should default to 30s
	}

	client := NewClient(cfg, nil)
	assert.NotNil(t, client)
}

func TestClient_findApplicationByResource(t *testing.T) {
	mockResponse := `{
		"items": [
			{
				"metadata": {
					"name": "my-app",
					"namespace": "argocd"
				},
				"spec": {
					"project": "default",
					"source": {
						"repoURL": "https://github.com/org/repo",
						"path": "apps/my-namespace",
						"targetRevision": "main"
					}
				},
				"status": {
					"sync": {"status": "Synced"},
					"health": {"status": "Healthy"}
				}
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResponse))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	// Test finding application by path containing namespace
	app, err := client.findApplicationByResource(context.Background(), "my-namespace", "my-workload")
	assert.NoError(t, err)
	assert.NotNil(t, app)
	assert.Equal(t, "my-app", app.Name)
}

func TestClient_findApplicationByResource_NotFound(t *testing.T) {
	mockResponse := `{
		"items": [
			{
				"metadata": {
					"name": "other-app",
					"namespace": "argocd"
				},
				"spec": {
					"project": "default",
					"source": {
						"repoURL": "https://github.com/org/repo",
						"path": "apps/other-namespace",
						"targetRevision": "main"
					}
				},
				"status": {
					"sync": {"status": "Synced"},
					"health": {"status": "Healthy"}
				}
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResponse))
	}))
	defer server.Close()

	client := NewClient(Config{
		ServerURL: server.URL,
		Token:     "test-token",
	}, nil)

	// Should not find application for unrelated namespace
	app, err := client.findApplicationByResource(context.Background(), "my-namespace", "my-workload")
	assert.Error(t, err)
	assert.Nil(t, app)
	assert.Contains(t, err.Error(), "no ArgoCD application found")
}

func TestConstants(t *testing.T) {
	assert.Equal(t, "argocd.argoproj.io/instance", ArgoCDAppLabel)
	assert.Equal(t, "argocd.argoproj.io/tracking-id", ArgoCDAppAnnotation)
	assert.Equal(t, "argocd.argoproj.io/sync-wave", ArgoCDSyncAnnotation)
	assert.Equal(t, "argoproj.io", ApplicationGroup)
	assert.Equal(t, "v1alpha1", ApplicationVersion)
	assert.Equal(t, "Application", ApplicationKind)
}

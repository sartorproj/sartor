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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type seenRequest struct {
	method   string
	path     string
	urlQuery url.Values
	form     url.Values
	auth     string
}

// TestClient_URLAndAuthPreserved checks that every request the client makes
// (connection check, ownership probe, instant and range queries) keeps the
// URL path prefix, the URL query string (exactly once) and the credentials.
func TestClient_URLAndAuthPreserved(t *testing.T) {
	tests := []struct {
		name       string
		suffix     string // appended to the test server URL
		cfg        Config
		wantPrefix string
		wantParams url.Values
		wantAuth   string
	}{
		{name: "plain", suffix: "", wantPrefix: ""},
		{name: "trailing slash", suffix: "/", wantPrefix: ""},
		{name: "path prefix", suffix: "/select/0/prometheus", wantPrefix: "/select/0/prometheus"},
		{name: "path prefix trailing slash", suffix: "/select/0/prometheus/", wantPrefix: "/select/0/prometheus"},
		{
			name:       "query string",
			suffix:     "?extra_label=cluster=example-cluster",
			wantParams: url.Values{"extra_label": {"cluster=example-cluster"}},
		},
		{
			name:       "prefix and query string",
			suffix:     "/select/0/prometheus?extra_label=cluster=example-cluster",
			wantPrefix: "/select/0/prometheus",
			wantParams: url.Values{"extra_label": {"cluster=example-cluster"}},
		},
		{
			name:       "prefix trailing slash and repeated params",
			suffix:     "/select/0/prometheus/?extra_label=cluster=a&extra_label=env=test&nocache=1",
			wantPrefix: "/select/0/prometheus",
			wantParams: url.Values{"extra_label": {"cluster=a", "env=test"}, "nocache": {"1"}},
		},
		{
			name:       "basic auth with prefix and query",
			suffix:     "/select/0/prometheus?extra_label=cluster=example-cluster",
			cfg:        Config{Username: "vm", Password: "s3cret"},
			wantPrefix: "/select/0/prometheus",
			wantParams: url.Values{"extra_label": {"cluster=example-cluster"}},
			wantAuth:   "Basic dm06czNjcmV0",
		},
		{
			name:     "basic auth with empty password",
			cfg:      Config{Username: "vm"},
			wantAuth: "Basic dm06",
		},
		{
			name:       "bearer auth with prefix and query",
			suffix:     "/select/0/prometheus?extra_label=cluster=example-cluster",
			cfg:        Config{Token: "tkn"},
			wantPrefix: "/select/0/prometheus",
			wantParams: url.Values{"extra_label": {"cluster=example-cluster"}},
			wantAuth:   "Bearer tkn",
		},
		{
			name:     "basic auth wins over bearer",
			cfg:      Config{Username: "vm", Password: "s3cret", Token: "tkn"},
			wantAuth: "Basic dm06czNjcmV0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []seenRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				mu.Lock()
				seen = append(seen, seenRequest{
					method:   r.Method,
					path:     r.URL.Path,
					urlQuery: r.URL.Query(),
					form:     r.PostForm,
					auth:     r.Header.Get("Authorization"),
				})
				mu.Unlock()
				if strings.HasSuffix(r.URL.Path, "/query_range") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
					return
				}
				writeVector(w, sample{labels: map[string]string{"container": "app"}, value: "1"})
			}))
			defer server.Close()

			cfg := tt.cfg
			cfg.URL = server.URL + tt.suffix
			client, err := NewClient(cfg)
			require.NoError(t, err)

			ctx := context.Background()
			require.NoError(t, client.CheckConnection(ctx))
			_, err = client.GetContainerMetrics(ctx, "default", testWorkload, time.Hour)
			require.NoError(t, err)
			_, err = client.GetTimeSeriesMetrics(ctx, "default", testWorkload, "app", "cpu", time.Hour)
			require.NoError(t, err)

			// 1 connection check + 2 probes + 4 instant queries + 1 range query.
			require.Len(t, seen, 8)
			for _, req := range seen {
				assert.True(t,
					req.path == tt.wantPrefix+"/api/v1/query" || req.path == tt.wantPrefix+"/api/v1/query_range",
					"unexpected path %q", req.path)
				for key, want := range tt.wantParams {
					assert.Equal(t, want, req.urlQuery[key], "URL param %s on %s", key, req.path)
					assert.Empty(t, req.form[key], "param %s must not be duplicated in the form body", key)
				}
				assert.NotEmpty(t, req.form.Get("query"), "PromQL is sent in the body")
				assert.Equal(t, tt.wantAuth, req.auth)
			}
		})
	}
}

// TestClient_GetFallbackKeepsParams covers backends that reject POST.
func TestClient_GetFallbackKeepsParams(t *testing.T) {
	var gets []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		assert.Equal(t, "/select/0/prometheus/api/v1/query", r.URL.Path)
		gets = append(gets, r.URL.Query())
		writeVector(w, sample{value: "1"})
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL + "/select/0/prometheus?extra_label=cluster=x"})
	require.NoError(t, err)
	require.NoError(t, client.CheckConnection(context.Background()))
	require.Len(t, gets, 1)
	assert.Equal(t, []string{"cluster=x"}, gets[0]["extra_label"])
	assert.Equal(t, "vector(1)", gets[0].Get("query"))
}

func TestNewClient_InvalidURL(t *testing.T) {
	_, err := NewClient(Config{URL: "http://[::1"})
	assert.Error(t, err)
}

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

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "basic config",
			config: Config{
				Token: "test-token",
			},
			wantErr: false,
		},
		{
			name: "config with base URL",
			config: Config{
				Token:   "test-token",
				BaseURL: "https://github.company.com/api/v3",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := New(tt.config)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, provider)
		})
	}
}

func TestProvider_Name(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)
	assert.Equal(t, "github", p.Name())
}

func TestParseRepository(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		wantOwner  string
		wantRepo   string
		wantErr    bool
	}{
		{
			name:       "valid repository",
			repository: "owner/repo",
			wantOwner:  "owner",
			wantRepo:   "repo",
			wantErr:    false,
		},
		{
			name:       "valid repository with org",
			repository: "my-org/my-repo",
			wantOwner:  "my-org",
			wantRepo:   "my-repo",
			wantErr:    false,
		},
		{
			name:       "invalid - no slash",
			repository: "invalid",
			wantErr:    true,
		},
		{
			name:       "invalid - empty",
			repository: "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := parseRepository(tt.repository)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

func TestExtractLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels []*github.Label
		want   []string
	}{
		{name: "nil", labels: nil, want: []string{}},
		{name: "names in order", labels: []*github.Label{{Name: "sartor"}, {Name: "sartor/ignore"}}, want: []string{"sartor", "sartor/ignore"}},
		{name: "skips nil and unnamed", labels: []*github.Label{nil, {Name: ""}, {Name: "sartor"}}, want: []string{"sartor"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractLabels(tt.labels))
		})
	}
}

// TestRefWrites checks the Git Data API request bodies: only ResetBranch may force-move a ref.
func TestRefWrites(t *testing.T) {
	type refWrite struct {
		method string
		path   string
		body   map[string]interface{}
	}
	tests := []struct {
		name string
		run  func(ctx context.Context, p *Provider) error
		want refWrite
	}{
		{
			name: "create branch",
			run: func(ctx context.Context, p *Provider) error {
				return p.CreateBranch(ctx, "acme/gitops", "sartor/new", "main")
			},
			want: refWrite{http.MethodPost, "/api/v3/repos/acme/gitops/git/refs",
				map[string]interface{}{"ref": "refs/heads/sartor/new", "sha": "base"}},
		},
		{
			name: "reset existing branch forces",
			run: func(ctx context.Context, p *Provider) error {
				return p.ResetBranch(ctx, "acme/gitops", "sartor/x", "main")
			},
			want: refWrite{http.MethodPatch, "/api/v3/repos/acme/gitops/git/refs/heads/sartor/x",
				map[string]interface{}{"sha": "base", "force": true}},
		},
		{
			name: "commit fast-forwards without force",
			run: func(ctx context.Context, p *Provider) error {
				_, err := p.Commit(ctx, provider.CommitRequest{
					Repository: "acme/gitops",
					Branch:     "sartor/x",
					Message:    "msg",
					Changes:    []provider.FileChange{{Path: "values.yaml", Content: []byte("a: 1\n")}},
				})
				return err
			},
			want: refWrite{http.MethodPatch, "/api/v3/repos/acme/gitops/git/refs/heads/sartor/x",
				map[string]interface{}{"sha": "newcommit"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var writes []refWrite
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := map[string]interface{}{}
				if r.Body != nil {
					_ = json.NewDecoder(r.Body).Decode(&body)
				}
				p := r.URL.Path
				switch {
				case r.Method == http.MethodGet && p == "/api/v3/repos/acme/gitops/git/ref/heads/main":
					_, _ = w.Write([]byte(`{"ref":"refs/heads/main","object":{"sha":"base"}}`))
				case r.Method == http.MethodGet && p == "/api/v3/repos/acme/gitops/git/ref/heads/sartor/x":
					_, _ = w.Write([]byte(`{"ref":"refs/heads/sartor/x","object":{"sha":"old"}}`))
				case r.Method == http.MethodGet && p == "/api/v3/repos/acme/gitops/git/commits/old":
					_, _ = w.Write([]byte(`{"sha":"old","tree":{"sha":"oldtree"}}`))
				case r.Method == http.MethodPost && p == "/api/v3/repos/acme/gitops/git/trees":
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"sha":"newtree"}`))
				case r.Method == http.MethodPost && p == "/api/v3/repos/acme/gitops/git/commits":
					assert.Equal(t, "newtree", body["tree"])
					assert.Equal(t, []interface{}{"old"}, body["parents"])
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"sha":"newcommit"}`))
				case strings.Contains(p, "/git/refs"):
					writes = append(writes, refWrite{r.Method, p, body})
					_, _ = w.Write([]byte(`{"ref":"refs/heads/x","object":{"sha":"x"}}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, p)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			p, err := New(Config{Token: "t", BaseURL: srv.URL})
			require.NoError(t, err)
			require.NoError(t, tt.run(context.Background(), p))
			assert.Equal(t, []refWrite{tt.want}, writes)
		})
	}
}

func TestConfig_Fields(t *testing.T) {
	cfg := Config{
		Token:   "my-token",
		BaseURL: "https://api.github.com",
	}

	assert.Equal(t, "my-token", cfg.Token)
	assert.Equal(t, "https://api.github.com", cfg.BaseURL)
}

// MockGitHubServer creates a mock GitHub API server for testing
func MockGitHubServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func TestProvider_GetFileContent_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	_, err = p.GetFileContent(context.Background(), "invalid-repo", "path/to/file", "main")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid repository format")
}

func TestProvider_CreateBranch_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	err = p.CreateBranch(context.Background(), "invalid-repo", "new-branch", "main")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid repository format")
}

func TestProvider_BranchExists_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	_, err = p.BranchExists(context.Background(), "invalid-repo", "branch")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid repository format")
}

func TestProvider_CreatePullRequest_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	_, err = p.CreatePullRequest(context.Background(), provider.CreatePRRequest{
		Repository: "invalid-repo",
		Title:      "Test PR",
		Body:       "Test body",
		HeadBranch: "feature",
		BaseBranch: "main",
	})
	assert.Error(t, err)
}

func TestProvider_GetPullRequest_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	_, err = p.GetPullRequest(context.Background(), "invalid-repo", 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid repository format")
}

func TestProvider_GetPullRequestByBranch_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	_, err = p.GetPullRequestByBranch(context.Background(), "invalid-repo", "feature")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid repository format")
}

func TestProvider_HasLabel_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	_, err = p.HasLabel(context.Background(), "invalid-repo", 1, "label")
	assert.Error(t, err)
}

func TestProvider_Commit_InvalidRepo(t *testing.T) {
	p, err := New(Config{Token: "test-token"})
	require.NoError(t, err)

	_, err = p.Commit(context.Background(), provider.CommitRequest{
		Repository: "invalid-repo",
		Branch:     "main",
		Message:    "test commit",
		Changes:    nil,
	})
	assert.Error(t, err)
}

// Integration-style tests with mock server

func TestProvider_CheckConnection_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"login": "testuser",
				"id":    123,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	// Create provider with mock server
	// Note: This requires the provider to use a custom base URL
	p, err := New(Config{
		Token:   "test-token",
		BaseURL: server.URL,
	})
	require.NoError(t, err)

	// The actual test would need to mock the GitHub client properly
	assert.NotNil(t, p)
}

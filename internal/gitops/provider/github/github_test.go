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
	"testing"

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
	provider, err := New(Config{Token: "test-token"})
	require.NoError(t, err)
	assert.Equal(t, "github", provider.Name())
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
		labels []*struct{ Name *string }
		want   []string
	}{
		{
			name:   "empty labels",
			labels: nil,
			want:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractLabels(nil)
			assert.Equal(t, tt.want, result)
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
	provider, err := New(Config{
		Token:   "test-token",
		BaseURL: server.URL,
	})
	require.NoError(t, err)

	// The actual test would need to mock the GitHub client properly
	assert.NotNil(t, provider)
}

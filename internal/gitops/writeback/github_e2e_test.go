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

package writeback

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	"github.com/sartorproj/sartor/internal/gitops/provider/github"
)

// fakeGitHub is a minimal GitHub REST API (contents, git data, pulls, labels)
// backed by an in-memory object store, used to exercise the real provider.
type fakeGitHub struct {
	mu       sync.Mutex
	t        *testing.T
	refs     map[string]string            // branch -> commit sha
	commits  map[string]map[string]string // commit sha -> files
	trees    map[string]map[string]string // tree sha -> files
	pulls    []map[string]interface{}
	seq      int
	requests []string
}

func newFakeGitHub(t *testing.T, files map[string]string) *fakeGitHub {
	f := &fakeGitHub{t: t, refs: map[string]string{}, commits: map[string]map[string]string{}, trees: map[string]map[string]string{}}
	f.refs["main"] = f.storeCommit(files)
	return f
}

func (f *fakeGitHub) nextSHA() string {
	f.seq++
	return fmt.Sprintf("%040d", f.seq)
}

func (f *fakeGitHub) storeCommit(files map[string]string) string {
	sha := f.nextSHA()
	f.commits[sha] = files
	f.trees["t"+sha] = files
	return sha
}

func (f *fakeGitHub) writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	require.NoError(f.t, json.NewEncoder(w).Encode(v))
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/acme/gitops/")
	f.requests = append(f.requests, r.Method+" "+path)
	var body map[string]interface{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	if strings.HasPrefix(path, "git/") {
		f.serveGitData(w, r, path, body)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "contents/"):
		files := f.commits[f.refs[r.URL.Query().Get("ref")]]
		content, ok := files[strings.TrimPrefix(path, "contents/")]
		if !ok {
			f.writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		f.writeJSON(w, http.StatusOK, map[string]interface{}{
			"type": "file", "encoding": "base64", "sha": "blob",
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
		})

	case r.Method == http.MethodGet && path == "pulls":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "acme:")
		out := []map[string]interface{}{}
		for _, pr := range f.pulls {
			if pr["head"].(map[string]interface{})["ref"] == head &&
				(r.URL.Query().Get("state") == "all" || pr["state"] == r.URL.Query().Get("state")) {
				out = append(out, pr)
			}
		}
		f.writeJSON(w, http.StatusOK, out)

	case r.Method == http.MethodPost && path == "pulls":
		for _, pr := range f.pulls {
			if pr["state"] == "open" && pr["head"].(map[string]interface{})["ref"] == body["head"] {
				f.writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
					"message": "Validation Failed",
					"errors":  []map[string]string{{"message": "A pull request already exists for acme:" + body["head"].(string) + "."}},
				})
				return
			}
		}
		n := len(f.pulls) + 1
		pr := map[string]interface{}{
			"number": n, "state": "open", "title": body["title"], "body": body["body"],
			"html_url": fmt.Sprintf("https://github.com/acme/gitops/pull/%d", n),
			"head":     map[string]interface{}{"ref": body["head"]}, "base": map[string]interface{}{"ref": body["base"]},
		}
		f.pulls = append(f.pulls, pr)
		f.writeJSON(w, http.StatusCreated, pr)

	case r.Method == http.MethodPatch && strings.HasPrefix(path, "pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(path, "pulls/"))
		pr := f.pulls[n-1]
		pr["title"], pr["body"] = body["title"], body["body"]
		f.writeJSON(w, http.StatusOK, pr)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "issues/") && strings.HasSuffix(path, "/labels"):
		f.writeJSON(w, http.StatusOK, []map[string]string{{"name": "sartor"}})

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// serveGitData implements the Git Data API (refs, commits, trees).
func (f *fakeGitHub) serveGitData(w http.ResponseWriter, r *http.Request, path string, body map[string]interface{}) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "git/ref/heads/"):
		sha, ok := f.refs[strings.TrimPrefix(path, "git/ref/heads/")]
		if !ok {
			f.writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		f.writeJSON(w, http.StatusOK, map[string]interface{}{"ref": "refs/" + strings.TrimPrefix(path, "git/ref/"), "object": map[string]string{"sha": sha}})

	case r.Method == http.MethodPost && path == "git/refs":
		branch := strings.TrimPrefix(body["ref"].(string), "refs/heads/")
		if _, ok := f.refs[branch]; ok {
			f.writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists"})
			return
		}
		f.refs[branch] = body["sha"].(string)
		f.writeJSON(w, http.StatusCreated, map[string]interface{}{"ref": body["ref"], "object": map[string]string{"sha": body["sha"].(string)}})

	case r.Method == http.MethodPatch && strings.HasPrefix(path, "git/refs/heads/"):
		branch := strings.TrimPrefix(path, "git/refs/heads/")
		f.refs[branch] = body["sha"].(string)
		f.writeJSON(w, http.StatusOK, map[string]interface{}{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": body["sha"].(string)}})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "git/commits/"):
		sha := strings.TrimPrefix(path, "git/commits/")
		f.writeJSON(w, http.StatusOK, map[string]interface{}{"sha": sha, "tree": map[string]string{"sha": "t" + sha}})

	case r.Method == http.MethodPost && path == "git/trees":
		files := map[string]string{}
		for k, v := range f.trees[body["base_tree"].(string)] {
			files[k] = v
		}
		for _, e := range body["tree"].([]interface{}) {
			entry := e.(map[string]interface{})
			files[entry["path"].(string)] = entry["content"].(string)
		}
		sha := "t" + f.nextSHA()
		f.trees[sha] = files
		f.writeJSON(w, http.StatusCreated, map[string]interface{}{"sha": sha})

	case r.Method == http.MethodPost && path == "git/commits":
		sha := f.nextSHA()
		f.commits[sha] = f.trees[body["tree"].(string)]
		f.trees["t"+sha] = f.commits[sha]
		f.writeJSON(w, http.StatusCreated, map[string]interface{}{"sha": sha})

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fakeGitHub) countRequests(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// TestApplyAgainstGitHubAPI runs the write-back several times against a fake
// GitHub API through the real provider and checks only one PR is ever opened.
func TestApplyAgainstGitHubAPI(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{"apps/api/values.yaml": helmValues})
	srv := httptest.NewServer(gh)
	defer srv.Close()

	p, err := github.New(github.Config{Token: "t", BaseURL: srv.URL})
	require.NoError(t, err)

	req := baseRequest(autoscalingv1alpha1.WriteBackTypeHelm, "apps/api/values.yaml", "api.resources")
	ctx := context.Background()

	steps := []struct {
		cpu     string
		outcome Outcome
	}{
		{cpu: "150m", outcome: OutcomeCreated},
		{cpu: "150m", outcome: OutcomeUnchanged},
		{cpu: "150m", outcome: OutcomeUnchanged},
		{cpu: "175m", outcome: OutcomeUpdated},
	}
	for i, step := range steps {
		req.Updates = cpuUpdate(step.cpu)
		res, err := Apply(ctx, p, req)
		require.NoError(t, err, "step %d", i)
		assert.Equal(t, step.outcome, res.Outcome, "step %d", i)
		require.NotNil(t, res.PR)
		assert.Equal(t, 1, res.PR.Number)
	}

	assert.Equal(t, 1, gh.countRequests("POST pulls"), "only one PR create call")
	assert.Equal(t, 2, gh.countRequests("POST git/commits"), "one commit to open, one to update")
	assert.Equal(t, 1, gh.countRequests("PATCH pulls/1"))

	files := gh.commits[gh.refs[req.Branch]]
	assert.Equal(t, strings.Replace(helmValues, "cpu: 100m", "cpu: 175m", 1), files["apps/api/values.yaml"])
	assert.Equal(t, helmValues, gh.commits[gh.refs["main"]]["apps/api/values.yaml"], "main is untouched")
}

// TestCreatePullRequestDuplicateIsDetected checks GitHub's 422 "already exists"
// is mapped to provider.ErrAlreadyExists, which Apply uses to reuse the open PR.
func TestCreatePullRequestDuplicateIsDetected(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{"apps/api/values.yaml": helmValues})
	srv := httptest.NewServer(gh)
	defer srv.Close()
	p, err := github.New(github.Config{Token: "t", BaseURL: srv.URL})
	require.NoError(t, err)

	pr := provider.CreatePRRequest{Repository: "acme/gitops", Title: "x", HeadBranch: "sartor/default/api", BaseBranch: "main"}
	_, err = p.CreatePullRequest(context.Background(), pr)
	require.NoError(t, err)
	_, err = p.CreatePullRequest(context.Background(), pr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrAlreadyExists), "got %v", err)
	assert.Len(t, gh.pulls, 1)
}

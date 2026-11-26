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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	"github.com/sartorproj/sartor/internal/gitops/provider/fake"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

const helmValues = `# api chart values
api:
  resources:
    requests:
      cpu: 100m # keep me
      memory: 128Mi
`

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
      - name: api
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
`

func cpuUpdate(cpu string) []writer.ResourceUpdate {
	return []writer.ResourceUpdate{{ContainerName: "api", Requests: &writer.ResourceValues{CPU: q(cpu)}}}
}

func baseRequest(t autoscalingv1alpha1.WriteBackType, path, helmPath string) Request {
	return Request{
		Repository:    "acme/gitops",
		BaseBranch:    "main",
		Branch:        "sartor/default/api",
		Path:          path,
		Type:          t,
		HelmValuePath: helmPath,
		Target:        Target{Kind: "Deployment", Name: "api", Namespace: "default"},
		Title:         "right-size api",
		Body:          "body",
		CommitMessage: "right-size api",
		Labels:        []string{"sartor"},
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		req   Request
		file  string
	}{
		{name: "helm", files: map[string]string{"apps/api/values.yaml": helmValues},
			req: baseRequest(autoscalingv1alpha1.WriteBackTypeHelm, "apps/api/values.yaml", "api.resources"), file: "apps/api/values.yaml"},
		{name: "raw", files: map[string]string{"apps/api/deploy.yaml": deployment},
			req: baseRequest(autoscalingv1alpha1.WriteBackTypeRaw, "apps/api/deploy.yaml", ""), file: "apps/api/deploy.yaml"},
		{name: "kustomize existing patch", files: map[string]string{"overlays/stg/api-patch.yaml": deployment},
			req: baseRequest(autoscalingv1alpha1.WriteBackTypeKustomize, "overlays/stg/api-patch.yaml", ""), file: "overlays/stg/api-patch.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			p := fake.New("main", tt.files)

			tt.req.Updates = cpuUpdate("150m")
			first, err := Apply(ctx, p, tt.req)
			require.NoError(t, err)
			assert.Equal(t, OutcomeCreated, first.Outcome)
			content, _ := p.File(tt.req.Branch, tt.file)
			assert.Contains(t, content, "cpu: 150m")

			// Same recommendation again: no commit, no PR update, no second PR.
			commits := p.Commits()
			again, err := Apply(ctx, p, tt.req)
			require.NoError(t, err)
			assert.Equal(t, OutcomeUnchanged, again.Outcome)
			assert.Equal(t, first.PR.Number, again.PR.Number)
			assert.Equal(t, commits, p.Commits())

			// New recommendation: the same PR is updated.
			tt.req.Updates = cpuUpdate("180m")
			updated, err := Apply(ctx, p, tt.req)
			require.NoError(t, err)
			assert.Equal(t, OutcomeUpdated, updated.Outcome)
			assert.Equal(t, first.PR.Number, updated.PR.Number)
			content, _ = p.File(tt.req.Branch, tt.file)
			assert.Contains(t, content, "cpu: 180m")

			assert.Len(t, p.PullRequests(), 1, "exactly one PR per Tailoring")
			base, _ := p.File("main", tt.file)
			assert.Equal(t, tt.files[tt.file], base, "base branch is never written")
		})
	}
}

func TestApplyReusesPRWhenCreateRaces(t *testing.T) {
	p := fake.New("main", map[string]string{"values.yaml": helmValues})
	p.CreatePRErr = &wrapErr{provider.ErrAlreadyExists}

	req := baseRequest(autoscalingv1alpha1.WriteBackTypeHelm, "values.yaml", "api.resources")
	req.Updates = cpuUpdate("150m")
	res, err := Apply(context.Background(), p, req)
	require.NoError(t, err)
	assert.Equal(t, OutcomeUpdated, res.Outcome)
	assert.Len(t, p.PullRequests(), 1)
}

type wrapErr struct{ err error }

func (w *wrapErr) Error() string { return "create PR: " + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }

func TestApplyAfterMergeOpensNewPRFromFreshBase(t *testing.T) {
	ctx := context.Background()
	p := fake.New("main", map[string]string{"values.yaml": helmValues})
	req := baseRequest(autoscalingv1alpha1.WriteBackTypeHelm, "values.yaml", "api.resources")
	req.Updates = cpuUpdate("150m")
	first, err := Apply(ctx, p, req)
	require.NoError(t, err)

	merged, _ := p.File(req.Branch, "values.yaml")
	p.SetFile("main", "values.yaml", merged)
	p.SetPRState(first.PR.Number, "closed", true)

	// The merged values are already on main: nothing to do.
	res, err := Apply(ctx, p, req)
	require.NoError(t, err)
	assert.Equal(t, OutcomeUnchanged, res.Outcome)
	assert.Nil(t, res.PR)

	req.Updates = cpuUpdate("200m")
	res, err = Apply(ctx, p, req)
	require.NoError(t, err)
	assert.Equal(t, OutcomeCreated, res.Outcome)
	assert.Equal(t, 2, res.PR.Number)
	open := 0
	for _, pr := range p.PullRequests() {
		if pr.State == "open" {
			open++
		}
	}
	assert.Equal(t, 1, open)
}

func TestApplyRejectsInvalidRequestsBeforeTouchingGit(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr string
	}{
		{name: "missing helmValuePath", mutate: func(r *Request) { r.Type = autoscalingv1alpha1.WriteBackTypeHelm; r.HelmValuePath = "" },
			wantErr: "helmValuePath is required"},
		{name: "path escapes repository", mutate: func(r *Request) { r.Path = "../secrets.yaml" }, wantErr: "clean path"},
		{name: "absolute path", mutate: func(r *Request) { r.Path = "/etc/passwd" }, wantErr: "clean path"},
		{name: "head equals base", mutate: func(r *Request) { r.Branch = "main" }, wantErr: "must differ"},
		{name: "unknown type", mutate: func(r *Request) { r.Type = "jsonnet" }, wantErr: "unsupported"},
		{name: "bad repository", mutate: func(r *Request) { r.Repository = "acme" }, wantErr: "invalid repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fake.New("main", map[string]string{"deploy.yaml": deployment})
			req := baseRequest(autoscalingv1alpha1.WriteBackTypeRaw, "deploy.yaml", "")
			req.Updates = cpuUpdate("150m")
			tt.mutate(&req)
			_, err := Apply(context.Background(), p, req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Empty(t, p.Calls, "no Git call may happen for an invalid request")
		})
	}
}

func TestApplyErrorsWithoutWriting(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		req     Request
		updates []writer.ResourceUpdate
		wantErr string
	}{
		{
			name:    "helm path does not exist in values.yaml",
			files:   map[string]string{"values.yaml": helmValues},
			req:     baseRequest(autoscalingv1alpha1.WriteBackTypeHelm, "values.yaml", "app.resources"),
			wantErr: "path not found",
		},
		{
			name:    "file missing",
			files:   map[string]string{},
			req:     baseRequest(autoscalingv1alpha1.WriteBackTypeRaw, "deploy.yaml", ""),
			wantErr: "not found",
		},
		{
			name:    "raw manifest for another workload",
			files:   map[string]string{"deploy.yaml": strings.Replace(deployment, "name: api\nspec", "name: web\nspec", 1)},
			req:     baseRequest(autoscalingv1alpha1.WriteBackTypeRaw, "deploy.yaml", ""),
			wantErr: `Deployment "api" not found`,
		},
		{
			name:  "single helm path with several containers",
			files: map[string]string{"values.yaml": helmValues},
			req:   baseRequest(autoscalingv1alpha1.WriteBackTypeHelm, "values.yaml", "api.resources"),
			updates: []writer.ResourceUpdate{
				{ContainerName: "a", Requests: &writer.ResourceValues{CPU: q("1")}},
				{ContainerName: "b", Requests: &writer.ResourceValues{CPU: q("1")}},
			},
			wantErr: "container=path",
		},
		{
			name:    "kustomize patch missing and no kustomization",
			files:   map[string]string{},
			req:     baseRequest(autoscalingv1alpha1.WriteBackTypeKustomize, "overlays/stg/api-patch.yaml", ""),
			wantErr: "no kustomization file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fake.New("main", tt.files)
			tt.req.Updates = tt.updates
			if tt.req.Updates == nil {
				tt.req.Updates = cpuUpdate("150m")
			}
			_, err := Apply(context.Background(), p, tt.req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Zero(t, p.Commits())
			assert.Empty(t, p.PullRequests())
		})
	}
}

func TestApplyKustomizeCreatesPatchAndRegistersIt(t *testing.T) {
	kustomization := "# staging overlay\nresources:\n- ../../base\n"
	p := fake.New("main", map[string]string{"overlays/stg/kustomization.yaml": kustomization})
	req := baseRequest(autoscalingv1alpha1.WriteBackTypeKustomize, "overlays/stg/sartor-api.yaml", "")
	req.Updates = cpuUpdate("150m")

	res, err := Apply(context.Background(), p, req)
	require.NoError(t, err)
	assert.Equal(t, OutcomeCreated, res.Outcome)

	patch, ok := p.File(req.Branch, "overlays/stg/sartor-api.yaml")
	require.True(t, ok)
	assert.Contains(t, patch, "kind: Deployment")
	assert.Contains(t, patch, "cpu: 150m")
	k, _ := p.File(req.Branch, "overlays/stg/kustomization.yaml")
	assert.Equal(t, "# staging overlay\nresources:\n- ../../base\npatches:\n- path: sartor-api.yaml\n  target:\n    kind: Deployment\n    name: api\n", k)

	again, err := Apply(context.Background(), p, req)
	require.NoError(t, err)
	assert.Equal(t, OutcomeUnchanged, again.Outcome)
}

func TestNormalizeRepository(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "acme/gitops", want: "acme/gitops"},
		{in: "github.com/acme/gitops", want: "acme/gitops"},
		{in: "https://github.com/acme/gitops.git", want: "acme/gitops"},
		{in: "https://github.com/acme/gitops/", want: "acme/gitops"},
		{in: "git@github.com:acme/gitops.git", want: "acme/gitops"},
		{in: "ssh://git@github.example.com/acme/gitops.git", want: "acme/gitops"},
		{in: "https://x-access-token:abc@github.com/acme/gitops", want: "acme/gitops"},
		{in: "acme", wantErr: true},
		{in: "https://github.com/acme/gitops/tree/main", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := NormalizeRepository(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

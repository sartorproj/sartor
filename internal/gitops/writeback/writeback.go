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
	"errors"
	"fmt"
	"path"
	"strings"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

// Target identifies the workload being written back.
type Target struct {
	Kind      string
	Name      string
	Namespace string
}

// Request describes one write-back for one Tailoring.
type Request struct {
	// Repository is "owner/repo".
	Repository string
	// BaseBranch is the branch PRs target.
	BaseBranch string
	// Branch is the Sartor-owned head branch; one per Tailoring.
	Branch        string
	Path          string
	Type          autoscalingv1alpha1.WriteBackType
	HelmValuePath string
	Target        Target
	Updates       []writer.ResourceUpdate
	Title         string
	Body          string
	CommitMessage string
	Labels        []string
}

// Outcome describes what Apply did.
type Outcome string

const (
	// OutcomeCreated means a new pull request was opened.
	OutcomeCreated Outcome = "Created"
	// OutcomeUpdated means the existing open pull request received a new commit.
	OutcomeUpdated Outcome = "Updated"
	// OutcomeUnchanged means Git already contains the recommended values; nothing was written.
	OutcomeUnchanged Outcome = "Unchanged"
)

// Result is the result of Apply.
type Result struct {
	Outcome Outcome
	// PR is the open pull request, if any.
	PR        *provider.PullRequest
	CommitSHA string
}

// Validate checks a request without contacting Git.
func (r Request) Validate() error {
	if err := checkRepository(r.Repository); err != nil {
		return err
	}
	if r.BaseBranch == "" || r.Branch == "" {
		return fmt.Errorf("base and head branches are required")
	}
	if r.BaseBranch == r.Branch {
		return fmt.Errorf("head branch %q must differ from base branch", r.Branch)
	}
	if err := ValidatePath(r.Path); err != nil {
		return err
	}
	switch r.Type {
	case autoscalingv1alpha1.WriteBackTypeRaw, autoscalingv1alpha1.WriteBackTypeKustomize:
	case autoscalingv1alpha1.WriteBackTypeHelm:
		if _, _, err := writer.ParseHelmValuePath(r.HelmValuePath); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported writeBack type %q", r.Type)
	}
	return nil
}

// ValidatePath rejects absolute paths and paths escaping the repository.
func ValidatePath(p string) error {
	if p == "" {
		return fmt.Errorf("writeBack.path is required")
	}
	if strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") || p == ".." {
		return fmt.Errorf("writeBack.path %q must be a clean path relative to the repository root", p)
	}
	return nil
}

// Apply writes the request to Git idempotently:
//   - if an open PR exists for the Tailoring's branch, the file is re-rendered
//     from that branch and a commit is added only if the content changed;
//   - otherwise, if the base branch already has the values, nothing is written;
//   - otherwise the branch is reset to the base branch, one commit is added and
//     one PR is opened. If GitHub reports the PR already exists, it is reused.
func Apply(ctx context.Context, p provider.Provider, req Request) (*Result, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	open, err := p.FindOpenPullRequest(ctx, req.Repository, req.Branch, req.BaseBranch)
	if err != nil {
		return nil, err
	}

	if open != nil {
		return updateOpen(ctx, p, req, open)
	}

	changes, err := renderChanges(ctx, p, req, req.BaseBranch)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return &Result{Outcome: OutcomeUnchanged}, nil
	}

	if err := p.ResetBranch(ctx, req.Repository, req.Branch, req.BaseBranch); err != nil {
		return nil, err
	}
	sha, err := p.Commit(ctx, provider.CommitRequest{
		Repository: req.Repository,
		Branch:     req.Branch,
		Message:    req.CommitMessage,
		Changes:    changes,
	})
	if err != nil {
		return nil, err
	}

	pr, err := p.CreatePullRequest(ctx, provider.CreatePRRequest{
		Repository: req.Repository,
		Title:      req.Title,
		Body:       req.Body,
		HeadBranch: req.Branch,
		BaseBranch: req.BaseBranch,
		Labels:     req.Labels,
	})
	if errors.Is(err, provider.ErrAlreadyExists) {
		existing, findErr := p.FindOpenPullRequest(ctx, req.Repository, req.Branch, req.BaseBranch)
		if findErr != nil {
			return nil, findErr
		}
		if existing == nil {
			return nil, err
		}
		return &Result{Outcome: OutcomeUpdated, PR: existing, CommitSHA: sha}, nil
	}
	if err != nil {
		return nil, err
	}
	return &Result{Outcome: OutcomeCreated, PR: pr, CommitSHA: sha}, nil
}

func updateOpen(ctx context.Context, p provider.Provider, req Request, open *provider.PullRequest) (*Result, error) {
	changes, err := renderChanges(ctx, p, req, req.Branch)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return &Result{Outcome: OutcomeUnchanged, PR: open}, nil
	}
	sha, err := p.Commit(ctx, provider.CommitRequest{
		Repository: req.Repository,
		Branch:     req.Branch,
		Message:    req.CommitMessage,
		Changes:    changes,
	})
	if err != nil {
		return nil, err
	}
	updated, err := p.UpdatePullRequest(ctx, req.Repository, open.Number, req.Title, req.Body)
	if err != nil {
		return nil, err
	}
	return &Result{Outcome: OutcomeUpdated, PR: updated, CommitSHA: sha}, nil
}

// renderChanges returns the file changes needed at ref, omitting files whose content is unchanged.
func renderChanges(ctx context.Context, p provider.Provider, req Request, ref string) ([]provider.FileChange, error) {
	switch req.Type {
	case autoscalingv1alpha1.WriteBackTypeRaw:
		content, err := p.GetFileContent(ctx, req.Repository, req.Path, ref)
		if err != nil {
			return nil, err
		}
		updated, err := writer.NewRawWriter().UpdateManifestFor(content,
			writer.ManifestTarget{Kind: req.Target.Kind, Name: req.Target.Name}, req.Updates)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Path, err)
		}
		return changed(req.Path, content, updated), nil

	case autoscalingv1alpha1.WriteBackTypeHelm:
		content, err := p.GetFileContent(ctx, req.Repository, req.Path, ref)
		if err != nil {
			return nil, err
		}
		updated, err := renderHelm(content, req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Path, err)
		}
		return changed(req.Path, content, updated), nil

	case autoscalingv1alpha1.WriteBackTypeKustomize:
		return renderKustomize(ctx, p, req, ref)
	}
	return nil, fmt.Errorf("unsupported writeBack type %q", req.Type)
}

func renderHelm(content []byte, req Request) ([]byte, error) {
	single, perContainer, err := writer.ParseHelmValuePath(req.HelmValuePath)
	if err != nil {
		return nil, err
	}
	w := &writer.HelmWriter{RequireExistingPath: true}
	if perContainer != nil {
		var unmapped []string
		for _, u := range req.Updates {
			if _, ok := perContainer[u.ContainerName]; !ok {
				unmapped = append(unmapped, u.ContainerName)
			}
		}
		if len(unmapped) > 0 {
			return nil, fmt.Errorf("helmValuePath has no entry for container(s) %s", strings.Join(unmapped, ", "))
		}
		return w.UpdateValuesMultiContainer(content, req.Updates, perContainer)
	}

	update, err := singleHelmUpdate(req.Updates, req.Target.Name)
	if err != nil {
		return nil, err
	}
	return w.UpdateValues(content, []writer.ResourceUpdate{update}, single)
}

// singleHelmUpdate picks the container a single helmValuePath applies to.
func singleHelmUpdate(updates []writer.ResourceUpdate, targetName string) (writer.ResourceUpdate, error) {
	if len(updates) == 1 {
		return updates[0], nil
	}
	for _, u := range updates {
		if u.ContainerName == targetName {
			return u, nil
		}
	}
	names := make([]string, 0, len(updates))
	for _, u := range updates {
		names = append(names, u.ContainerName)
	}
	return writer.ResourceUpdate{}, fmt.Errorf(
		"helmValuePath names one resources block but containers %s changed; "+
			"use \"container=path,...\" or excludeContainers", strings.Join(names, ", "))
}

var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// renderKustomize updates the strategic merge patch at req.Path. If the patch
// does not exist yet it is created and registered in the kustomization file of
// the same directory.
func renderKustomize(ctx context.Context, p provider.Provider, req Request, ref string) ([]provider.FileChange, error) {
	kw := writer.NewKustomizeWriter()
	target := writer.ManifestTarget{Kind: req.Target.Kind, Name: req.Target.Name}

	content, err := p.GetFileContent(ctx, req.Repository, req.Path, ref)
	if err == nil {
		updated, err := kw.UpdatePatchFor(content, target, req.Updates)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Path, err)
		}
		return changed(req.Path, content, updated), nil
	}
	if !errors.Is(err, provider.ErrNotFound) {
		return nil, err
	}

	patch, err := kw.GeneratePatch(writer.PatchTarget{
		APIVersion: "apps/v1",
		Kind:       req.Target.Kind,
		Name:       req.Target.Name,
	}, req.Updates)
	if err != nil {
		return nil, err
	}

	dir := path.Dir(req.Path)
	for _, name := range kustomizationNames {
		kpath := path.Join(dir, name)
		kcontent, err := p.GetFileContent(ctx, req.Repository, kpath, ref)
		if errors.Is(err, provider.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		updatedK, err := kw.UpdateKustomization(kcontent, writer.KustomizationEntry{
			PatchPath: path.Base(req.Path),
			Target:    writer.PatchTarget{Kind: req.Target.Kind, Name: req.Target.Name},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kpath, err)
		}
		changes := []provider.FileChange{{Path: req.Path, Content: patch}}
		return append(changes, changed(kpath, kcontent, updatedK)...), nil
	}
	return nil, fmt.Errorf("patch %s does not exist and no kustomization file was found in %s", req.Path, dir)
}

func changed(p string, before, after []byte) []provider.FileChange {
	if string(before) == string(after) {
		return nil
	}
	return []provider.FileChange{{Path: p, Content: after}}
}

// NormalizeRepository converts "org/repo", "github.com/org/repo",
// "https://github.com/org/repo.git" or "git@github.com:org/repo.git" to "org/repo".
func NormalizeRepository(repo string) (string, error) {
	s := strings.TrimSpace(repo)
	for _, prefix := range []string{"https://", "http://", "ssh://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	if at := strings.Index(s, "@"); at >= 0 && at < strings.IndexAny(s+"/", "/:") {
		s = s[at+1:]
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	// "host:org/repo" (scp-like SSH syntax)
	if colon := strings.Index(s, ":"); colon >= 0 && !strings.Contains(s[:colon], "/") {
		s = s[colon+1:]
	}
	parts := strings.Split(s, "/")
	if len(parts) == 3 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	normalized := strings.Join(parts, "/")
	if err := checkRepository(normalized); err != nil {
		return "", fmt.Errorf("invalid writeBack.repository %q (expected owner/repo or a GitHub URL)", repo)
	}
	return normalized, nil
}

func checkRepository(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid repository %q (expected owner/repo)", repo)
	}
	return nil
}

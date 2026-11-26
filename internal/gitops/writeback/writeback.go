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

	// Current holds the live requests of the updated containers; it decides
	// which resources' requests change (for HPA scaling-point notes).
	Current map[string]*writer.ResourceValues
	// Autoscalers are the autoscalers detected for the workload.
	Autoscalers []autoscalingv1alpha1.DetectedAutoscaler
	// LiveFromPods means the live values were read from running pods
	// (metrics-only mode): containers missing from Git are skipped instead of
	// added or failing, since pods can carry injected sidecars.
	LiveFromPods bool
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
	// Autoscalers reports the autoscalers edited (or left alone) in Git.
	Autoscalers []writer.AutoscalerNote
	// Skipped lists containers not written because they are not in Git.
	Skipped []string
}

// rendered is the outcome of rendering the request at one ref.
type rendered struct {
	changes     []provider.FileChange
	autoscalers []writer.AutoscalerNote
	skipped     []string
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
	if r.Path == "" {
		return fmt.Errorf("writeBack.path is required")
	}
	switch r.Type {
	case autoscalingv1alpha1.WriteBackTypeRaw, autoscalingv1alpha1.WriteBackTypeKustomize:
	case autoscalingv1alpha1.WriteBackTypeHelm:
		if r.HelmValuePath == "" {
			return fmt.Errorf("helmValuePath is required for helm write-back")
		}
	default:
		return fmt.Errorf("unsupported writeBack type %q", r.Type)
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

	r, err := renderChanges(ctx, p, req, req.BaseBranch)
	if err != nil {
		return nil, err
	}
	result := &Result{Outcome: OutcomeUnchanged, Autoscalers: r.autoscalers, Skipped: r.skipped}
	if len(r.changes) == 0 {
		return result, nil
	}

	if err := p.ResetBranch(ctx, req.Repository, req.Branch, req.BaseBranch); err != nil {
		return nil, err
	}
	sha, err := p.Commit(ctx, provider.CommitRequest{
		Repository: req.Repository,
		Branch:     req.Branch,
		Message:    req.CommitMessage,
		Changes:    r.changes,
	})
	if err != nil {
		return nil, err
	}
	result.CommitSHA = sha

	pr, err := p.CreatePullRequest(ctx, provider.CreatePRRequest{
		Repository: req.Repository,
		Title:      req.Title,
		Body:       req.Body + autoscalingSection(req, r),
		HeadBranch: req.Branch,
		BaseBranch: req.BaseBranch,
	})
	if errors.Is(err, provider.ErrAlreadyExists) {
		existing, findErr := p.FindOpenPullRequest(ctx, req.Repository, req.Branch, req.BaseBranch)
		if findErr != nil {
			return nil, findErr
		}
		if existing == nil {
			return nil, err
		}
		result.Outcome, result.PR = OutcomeUpdated, existing
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	result.Outcome, result.PR = OutcomeCreated, pr
	return result, nil
}

func updateOpen(ctx context.Context, p provider.Provider, req Request, open *provider.PullRequest) (*Result, error) {
	r, err := renderChanges(ctx, p, req, req.Branch)
	if err != nil {
		return nil, err
	}
	result := &Result{Outcome: OutcomeUnchanged, PR: open, Autoscalers: r.autoscalers, Skipped: r.skipped}
	if len(r.changes) == 0 {
		return result, nil
	}
	sha, err := p.Commit(ctx, provider.CommitRequest{
		Repository: req.Repository,
		Branch:     req.Branch,
		Message:    req.CommitMessage,
		Changes:    r.changes,
	})
	if err != nil {
		return nil, err
	}
	updated, err := p.UpdatePullRequest(ctx, req.Repository, open.Number, req.Title, req.Body+autoscalingSection(req, r))
	if err != nil {
		return nil, err
	}
	result.Outcome, result.PR, result.CommitSHA = OutcomeUpdated, updated, sha
	return result, nil
}

// renderChanges returns the file changes needed at ref, omitting files whose content is unchanged.
func renderChanges(ctx context.Context, p provider.Provider, req Request, ref string) (*rendered, error) {
	content, err := p.GetFileContent(ctx, req.Repository, req.Path, ref)
	if err != nil {
		return nil, err
	}
	target := writer.ManifestTarget{Kind: req.Target.Kind, Name: req.Target.Name}
	r := &rendered{}
	var updated []byte
	switch req.Type {
	case autoscalingv1alpha1.WriteBackTypeRaw:
		res, err := writer.NewRawWriter().Update(content, target, req.Updates,
			writer.RawOptions{SkipMissingContainers: req.LiveFromPods})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Path, err)
		}
		updated, r.autoscalers, r.skipped = res.Content, res.Autoscalers, res.Skipped

	case autoscalingv1alpha1.WriteBackTypeHelm:
		if req.LiveFromPods && len(req.Updates) > 1 {
			// helmValuePath holds one container's values; with pod-level values
			// an injected sidecar could otherwise be written there.
			names := make([]string, 0, len(req.Updates))
			for _, u := range req.Updates {
				names = append(names, u.ContainerName)
			}
			return nil, fmt.Errorf("%s: helm write-back writes one container's values but the pods run %s; "+
				"list the other containers in spec.excludeContainers", req.Path, strings.Join(names, ", "))
		}
		updated, err = writer.NewHelmWriter().UpdateValues(content, req.Updates, req.HelmValuePath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Path, err)
		}

	case autoscalingv1alpha1.WriteBackTypeKustomize:
		updated, r.skipped, err = writer.NewKustomizeWriter().UpdateExistingPatchFor(content, target, req.Updates, req.LiveFromPods)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Path, err)
		}

	default:
		return nil, fmt.Errorf("unsupported writeBack type %q", req.Type)
	}
	r.changes = changed(req.Path, content, updated)
	return r, nil
}

func changed(p string, before, after []byte) []provider.FileChange {
	if string(before) == string(after) {
		return nil
	}
	return []provider.FileChange{{Path: p, Content: after}}
}

// NormalizeRepository converts "org/repo", "github.com/org/repo" or
// "https://github.com/org/repo.git" to "org/repo".
func NormalizeRepository(repo string) (string, error) {
	s := strings.TrimSpace(repo)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
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

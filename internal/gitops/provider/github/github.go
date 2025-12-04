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
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"

	"github.com/sartorproj/sartor/internal/gitops/provider"
)

// Config holds the configuration for the GitHub provider.
type Config struct {
	// Token is the GitHub personal access token.
	Token string
	// BaseURL is the GitHub API base URL (for GitHub Enterprise).
	BaseURL string
}

// Provider implements the provider.Provider interface for GitHub.
type Provider struct {
	client *github.Client
	config Config
}

// New creates a new GitHub provider.
func New(cfg Config) (*Provider, error) {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: cfg.Token},
	)
	tc := oauth2.NewClient(ctx, ts)

	var client *github.Client
	var err error

	if cfg.BaseURL != "" {
		client, err = github.NewClient(tc).WithEnterpriseURLs(cfg.BaseURL, cfg.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("failed to create GitHub Enterprise client: %w", err)
		}
	} else {
		client = github.NewClient(tc)
	}

	return &Provider{
		client: client,
		config: cfg,
	}, nil
}

// Name returns the provider name.
func (p *Provider) Name() string {
	return "github"
}

// CheckConnection verifies the connection to GitHub.
func (p *Provider) CheckConnection(ctx context.Context) error {
	_, _, err := p.client.Users.Get(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to connect to GitHub: %w", err)
	}
	return nil
}

// parseRepository splits "owner/repo" into owner and repo.
func parseRepository(repository string) (owner, repo string, err error) {
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid repository format: %s (expected owner/repo)", repository)
	}
	return parts[0], parts[1], nil
}

// GetFileContent retrieves the content of a file from a repository.
func (p *Provider) GetFileContent(ctx context.Context, repository, path, ref string) ([]byte, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}

	opts := &github.RepositoryContentGetOptions{Ref: ref}
	fileContent, _, _, err := p.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get file content: %w", err)
	}

	if fileContent == nil {
		return nil, fmt.Errorf("file not found: %s", path)
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return nil, fmt.Errorf("failed to decode file content: %w", err)
	}

	return []byte(content), nil
}

// CreateBranch creates a new branch from the given base.
func (p *Provider) CreateBranch(ctx context.Context, repository, branchName, baseBranch string) error {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return err
	}

	// Get the SHA of the base branch
	baseRef, _, err := p.client.Git.GetRef(ctx, owner, repo, "refs/heads/"+baseBranch)
	if err != nil {
		return fmt.Errorf("failed to get base branch ref: %w", err)
	}

	// Create the new branch
	newRef := &github.Reference{
		Ref:    github.String("refs/heads/" + branchName),
		Object: &github.GitObject{SHA: baseRef.Object.SHA},
	}

	_, _, err = p.client.Git.CreateRef(ctx, owner, repo, newRef)
	if err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}

	return nil
}

// BranchExists checks if a branch exists.
func (p *Provider) BranchExists(ctx context.Context, repository, branchName string) (bool, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return false, err
	}

	_, _, err = p.client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branchName)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check branch: %w", err)
	}

	return true, nil
}

// Commit creates a commit with the given changes.
func (p *Provider) Commit(ctx context.Context, req provider.CommitRequest) (string, error) {
	owner, repo, err := parseRepository(req.Repository)
	if err != nil {
		return "", err
	}

	// Get the current commit on the branch
	ref, _, err := p.client.Git.GetRef(ctx, owner, repo, "refs/heads/"+req.Branch)
	if err != nil {
		return "", fmt.Errorf("failed to get branch ref: %w", err)
	}

	// Get the tree of the current commit
	commit, _, err := p.client.Git.GetCommit(ctx, owner, repo, *ref.Object.SHA)
	if err != nil {
		return "", fmt.Errorf("failed to get commit: %w", err)
	}

	// Create tree entries for each file change
	entries := make([]*github.TreeEntry, 0, len(req.Changes))
	for _, change := range req.Changes {
		content := base64.StdEncoding.EncodeToString(change.Content)
		entries = append(entries, &github.TreeEntry{
			Path:    github.String(change.Path),
			Mode:    github.String("100644"),
			Type:    github.String("blob"),
			Content: github.String(string(change.Content)),
		})
		_ = content // silence unused warning
	}

	// Create a new tree
	tree, _, err := p.client.Git.CreateTree(ctx, owner, repo, *commit.Tree.SHA, entries)
	if err != nil {
		return "", fmt.Errorf("failed to create tree: %w", err)
	}

	// Create the commit
	newCommit, _, err := p.client.Git.CreateCommit(ctx, owner, repo, &github.Commit{
		Message: github.String(req.Message),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: ref.Object.SHA}},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create commit: %w", err)
	}

	// Update the branch reference
	ref.Object.SHA = newCommit.SHA
	_, _, err = p.client.Git.UpdateRef(ctx, owner, repo, ref, false)
	if err != nil {
		return "", fmt.Errorf("failed to update branch ref: %w", err)
	}

	return *newCommit.SHA, nil
}

// CreatePullRequest creates a new pull request.
func (p *Provider) CreatePullRequest(ctx context.Context, req provider.CreatePRRequest) (*provider.PullRequest, error) {
	owner, repo, err := parseRepository(req.Repository)
	if err != nil {
		return nil, err
	}

	pr, _, err := p.client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.String(req.Title),
		Body:  github.String(req.Body),
		Head:  github.String(req.HeadBranch),
		Base:  github.String(req.BaseBranch),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create pull request: %w", err)
	}

	// Add labels if specified
	if len(req.Labels) > 0 {
		_, _, err = p.client.Issues.AddLabelsToIssue(ctx, owner, repo, *pr.Number, req.Labels)
		if err != nil {
			// Log but don't fail if labels can't be added
			fmt.Printf("Warning: failed to add labels to PR: %v\n", err)
		}
	}

	return &provider.PullRequest{
		Number:   *pr.Number,
		URL:      *pr.HTMLURL,
		State:    *pr.State,
		Title:    *pr.Title,
		Labels:   extractLabels(pr.Labels),
		IsMerged: pr.GetMerged(),
	}, nil
}

// GetPullRequest retrieves a pull request by number.
func (p *Provider) GetPullRequest(ctx context.Context, repository string, number int) (*provider.PullRequest, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}

	pr, _, err := p.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get pull request: %w", err)
	}

	return &provider.PullRequest{
		Number:   *pr.Number,
		URL:      *pr.HTMLURL,
		State:    *pr.State,
		Title:    *pr.Title,
		Labels:   extractLabels(pr.Labels),
		IsMerged: pr.GetMerged(),
	}, nil
}

// GetPullRequestByBranch retrieves a pull request by head branch.
func (p *Provider) GetPullRequestByBranch(ctx context.Context, repository, headBranch string) (*provider.PullRequest, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}

	prs, _, err := p.client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:  owner + ":" + headBranch,
		State: "all",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pull requests: %w", err)
	}

	if len(prs) == 0 {
		return nil, nil
	}

	pr := prs[0]
	return &provider.PullRequest{
		Number:   *pr.Number,
		URL:      *pr.HTMLURL,
		State:    *pr.State,
		Title:    *pr.Title,
		Labels:   extractLabels(pr.Labels),
		IsMerged: pr.GetMerged(),
	}, nil
}

// HasLabel checks if a PR has a specific label.
func (p *Provider) HasLabel(ctx context.Context, repository string, number int, label string) (bool, error) {
	pr, err := p.GetPullRequest(ctx, repository, number)
	if err != nil {
		return false, err
	}

	for _, l := range pr.Labels {
		if l == label {
			return true, nil
		}
	}

	return false, nil
}

// extractLabels extracts label names from GitHub label objects.
func extractLabels(labels []*github.Label) []string {
	result := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != nil {
			result = append(result, *l.Name)
		}
	}
	return result
}

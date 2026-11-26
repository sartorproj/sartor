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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v93/github"
	"golang.org/x/oauth2"

	"github.com/sartorproj/sartor/internal/gitops/provider"
)

// Config holds the configuration for the GitHub provider.
// Either Token or the GitHub App fields (AppID, InstallationID, PrivateKey) must be set.
type Config struct {
	// Token is a GitHub personal access token (classic or fine-grained).
	Token string
	// BaseURL is the GitHub API base URL (for GitHub Enterprise).
	BaseURL string

	// AppID is the GitHub App ID.
	AppID int64
	// InstallationID is the GitHub App installation ID for the target organization.
	InstallationID int64
	// PrivateKey is the PEM-encoded GitHub App private key.
	PrivateKey []byte
}

// IsApp reports whether the config uses GitHub App authentication.
func (c Config) IsApp() bool {
	return c.Token == ""
}

// Validate checks that a token or complete GitHub App credentials are configured.
func (c Config) Validate() error {
	if c.IsApp() && (c.AppID == 0 || c.InstallationID == 0 || len(c.PrivateKey) == 0) {
		return fmt.Errorf("GitHub authentication requires a token, or appId, installationId and privateKey")
	}
	return nil
}

// Provider implements the provider.Provider interface for GitHub.
type Provider struct {
	client *github.Client
	config Config
}

// New creates a new GitHub provider.
func New(cfg Config) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	ctx := context.Background()

	var ts oauth2.TokenSource
	if cfg.IsApp() {
		src, err := newAppTokenSource(cfg, http.DefaultClient)
		if err != nil {
			return nil, err
		}
		ts = oauth2.ReuseTokenSource(nil, src)
	} else {
		ts = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Token})
	}

	client, err := newGitHubClient(oauth2.NewClient(ctx, ts), cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	return &Provider{
		client: client,
		config: cfg,
	}, nil
}

func newGitHubClient(httpClient *http.Client, baseURL string, opts ...github.ClientOptionsFunc) (*github.Client, error) {
	opts = append([]github.ClientOptionsFunc{github.WithHTTPClient(httpClient)}, opts...)
	if baseURL != "" {
		opts = append(opts, github.WithEnterpriseURLs(baseURL, baseURL))
	}
	client, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub Enterprise client: %w", err)
	}
	return client, nil
}

// Name returns the provider name.
func (p *Provider) Name() string {
	return "github"
}

// CheckConnection verifies the connection to GitHub.
func (p *Provider) CheckConnection(ctx context.Context) error {
	var err error
	if p.config.IsApp() {
		// Installation tokens cannot call /user.
		_, _, err = p.client.Apps.ListRepos(ctx, &github.ListOptions{PerPage: 1})
	} else {
		_, _, err = p.client.Users.Get(ctx, "")
	}
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

func isStatus(err error, code int) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == code
}

// GetFileContent retrieves the content of a file from a repository.
// It returns an error wrapping provider.ErrNotFound if the file does not exist.
func (p *Provider) GetFileContent(ctx context.Context, repository, path, ref string) ([]byte, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}

	opts := &github.RepositoryContentGetOptions{Ref: ref}
	fileContent, _, _, err := p.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("%s@%s: %w", path, ref, provider.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get file content: %w", err)
	}

	if fileContent == nil {
		return nil, fmt.Errorf("%s is not a file: %w", path, provider.ErrNotFound)
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return nil, fmt.Errorf("failed to decode file content: %w", err)
	}

	return []byte(content), nil
}

// GetBranchSHA returns the commit SHA the branch points to.
func (p *Provider) GetBranchSHA(ctx context.Context, repository, branchName string) (string, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return "", err
	}
	ref, _, err := p.client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branchName)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("branch %s: %w", branchName, provider.ErrNotFound)
		}
		return "", fmt.Errorf("failed to get branch ref: %w", err)
	}
	return ref.GetObject().GetSHA(), nil
}

// CreateBranch creates a new branch from the given base.
func (p *Provider) CreateBranch(ctx context.Context, repository, branchName, baseBranch string) error {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return err
	}

	baseSHA, err := p.GetBranchSHA(ctx, repository, baseBranch)
	if err != nil {
		return fmt.Errorf("failed to get base branch ref: %w", err)
	}

	newRef := github.CreateRef{Ref: "refs/heads/" + branchName, SHA: baseSHA}

	_, _, err = p.client.Git.CreateRef(ctx, owner, repo, newRef)
	if err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}

	return nil
}

// ResetBranch points branchName at the head of baseBranch, creating it if needed.
func (p *Provider) ResetBranch(ctx context.Context, repository, branchName, baseBranch string) error {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return err
	}

	baseSHA, err := p.GetBranchSHA(ctx, repository, baseBranch)
	if err != nil {
		return fmt.Errorf("failed to get base branch ref: %w", err)
	}

	exists, err := p.BranchExists(ctx, repository, branchName)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + branchName
	if !exists {
		if _, _, err := p.client.Git.CreateRef(ctx, owner, repo, github.CreateRef{Ref: ref, SHA: baseSHA}); err != nil {
			return fmt.Errorf("failed to create branch: %w", err)
		}
		return nil
	}
	update := github.UpdateRef{SHA: baseSHA, Force: new(true)}
	if _, _, err := p.client.Git.UpdateRef(ctx, owner, repo, ref, update); err != nil {
		return fmt.Errorf("failed to reset branch: %w", err)
	}
	return nil
}

// BranchExists checks if a branch exists.
func (p *Provider) BranchExists(ctx context.Context, repository, branchName string) (bool, error) {
	_, err := p.GetBranchSHA(ctx, repository, branchName)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, provider.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("failed to check branch: %w", err)
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
	commit, _, err := p.client.Git.GetCommit(ctx, owner, repo, ref.GetObject().GetSHA())
	if err != nil {
		return "", fmt.Errorf("failed to get commit: %w", err)
	}

	// Create tree entries for each file change
	entries := make([]*github.TreeEntry, 0, len(req.Changes))
	for _, change := range req.Changes {
		entries = append(entries, &github.TreeEntry{
			Path:    new(change.Path),
			Mode:    new("100644"),
			Type:    new("blob"),
			Content: new(string(change.Content)),
		})
	}

	// Create a new tree
	tree, _, err := p.client.Git.CreateTree(ctx, owner, repo, commit.GetTree().GetSHA(), entries)
	if err != nil {
		return "", fmt.Errorf("failed to create tree: %w", err)
	}

	// Create the commit
	newCommit, _, err := p.client.Git.CreateCommit(ctx, owner, repo, github.Commit{
		Message: new(req.Message),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: ref.Object.SHA}},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create commit: %w", err)
	}

	// Fast-forward the branch reference
	_, _, err = p.client.Git.UpdateRef(ctx, owner, repo, ref.GetRef(), github.UpdateRef{SHA: newCommit.GetSHA()})
	if err != nil {
		return "", fmt.Errorf("failed to update branch ref: %w", err)
	}

	return newCommit.GetSHA(), nil
}

// CreatePullRequest creates a new pull request.
func (p *Provider) CreatePullRequest(ctx context.Context, req provider.CreatePRRequest) (*provider.PullRequest, error) {
	owner, repo, err := parseRepository(req.Repository)
	if err != nil {
		return nil, err
	}

	pr, _, err := p.client.PullRequests.Create(ctx, owner, repo, github.CreatePullRequest{
		Title: new(req.Title),
		Body:  new(req.Body),
		Head:  req.HeadBranch,
		Base:  req.BaseBranch,
	})
	if err != nil {
		if isStatus(err, http.StatusUnprocessableEntity) && strings.Contains(err.Error(), "already exists") {
			return nil, fmt.Errorf("failed to create pull request: %w", provider.ErrAlreadyExists)
		}
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

	return convertPullRequest(pr), nil
}

// UpdatePullRequest updates the title and body of an existing pull request.
func (p *Provider) UpdatePullRequest(ctx context.Context, repository string, number int, title, body string) (*provider.PullRequest, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}
	pr, _, err := p.client.PullRequests.Edit(ctx, owner, repo, number, &github.PullRequest{
		Title: new(title),
		Body:  new(body),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update pull request: %w", err)
	}
	return convertPullRequest(pr), nil
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

	return convertPullRequest(pr), nil
}

// GetPullRequestByBranch retrieves a pull request by head branch.
func (p *Provider) GetPullRequestByBranch(ctx context.Context, repository, headBranch string) (*provider.PullRequest, error) {
	return p.findPullRequest(ctx, repository, headBranch, "", "all")
}

// FindOpenPullRequest returns the open pull request from headBranch into baseBranch, or nil.
func (p *Provider) FindOpenPullRequest(ctx context.Context, repository, headBranch, baseBranch string) (*provider.PullRequest, error) {
	return p.findPullRequest(ctx, repository, headBranch, baseBranch, "open")
}

func (p *Provider) findPullRequest(ctx context.Context, repository, headBranch, baseBranch, state string) (*provider.PullRequest, error) {
	owner, repo, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}

	prs, _, err := p.client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:  owner + ":" + headBranch,
		Base:  baseBranch,
		State: state,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pull requests: %w", err)
	}

	if len(prs) == 0 {
		return nil, nil
	}

	return convertPullRequest(prs[0]), nil
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

func convertPullRequest(pr *github.PullRequest) *provider.PullRequest {
	return &provider.PullRequest{
		Number:   pr.GetNumber(),
		URL:      pr.GetHTMLURL(),
		State:    pr.GetState(),
		Title:    pr.GetTitle(),
		Labels:   extractLabels(pr.Labels),
		IsMerged: pr.GetMerged(),
	}
}

// extractLabels extracts label names from GitHub label objects.
func extractLabels(labels []*github.Label) []string {
	result := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			result = append(result, l.Name)
		}
	}
	return result
}

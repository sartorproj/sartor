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

package provider

import (
	"context"
)

// PullRequest represents a Pull Request/Merge Request.
type PullRequest struct {
	// Number is the PR number.
	Number int
	// URL is the web URL of the PR.
	URL string
	// State is the PR state (open, closed, merged).
	State string
	// Title is the PR title.
	Title string
	// Labels are the labels on the PR.
	Labels []string
	// IsMerged indicates if the PR has been merged.
	IsMerged bool
}

// CreatePRRequest contains the parameters for creating a PR.
type CreatePRRequest struct {
	// Repository is the repository path (e.g., "owner/repo").
	Repository string
	// Title is the PR title.
	Title string
	// Body is the PR description.
	Body string
	// HeadBranch is the source branch.
	HeadBranch string
	// BaseBranch is the target branch.
	BaseBranch string
	// Labels are labels to add to the PR.
	Labels []string
}

// FileChange represents a file change to commit.
type FileChange struct {
	// Path is the file path within the repository.
	Path string
	// Content is the new file content.
	Content []byte
}

// CommitRequest contains the parameters for creating a commit.
type CommitRequest struct {
	// Repository is the repository path (e.g., "owner/repo").
	Repository string
	// Branch is the branch to commit to.
	Branch string
	// Message is the commit message.
	Message string
	// Changes are the file changes to commit.
	Changes []FileChange
}

// Provider defines the interface for Git providers (GitHub, GitLab, etc.).
type Provider interface {
	// Name returns the provider name.
	Name() string

	// CheckConnection verifies the connection to the Git provider.
	CheckConnection(ctx context.Context) error

	// GetFileContent retrieves the content of a file from a repository.
	GetFileContent(ctx context.Context, repository, path, ref string) ([]byte, error)

	// CreateBranch creates a new branch from the given base.
	CreateBranch(ctx context.Context, repository, branchName, baseBranch string) error

	// BranchExists checks if a branch exists.
	BranchExists(ctx context.Context, repository, branchName string) (bool, error)

	// Commit creates a commit with the given changes.
	Commit(ctx context.Context, req CommitRequest) (sha string, err error)

	// CreatePullRequest creates a new pull request.
	CreatePullRequest(ctx context.Context, req CreatePRRequest) (*PullRequest, error)

	// GetPullRequest retrieves a pull request by number.
	GetPullRequest(ctx context.Context, repository string, number int) (*PullRequest, error)

	// GetPullRequestByBranch retrieves a pull request by head branch.
	GetPullRequestByBranch(ctx context.Context, repository, headBranch string) (*PullRequest, error)

	// HasLabel checks if a PR has a specific label.
	HasLabel(ctx context.Context, repository string, number int, label string) (bool, error)
}

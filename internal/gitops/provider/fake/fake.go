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

// Package fake provides an in-memory Git provider for tests.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/sartorproj/sartor/internal/gitops/provider"
)

// Provider is an in-memory provider.Provider that records every call.
type Provider struct {
	mu       sync.Mutex
	branches map[string]map[string][]byte
	prs      []*provider.PullRequest
	commits  int

	// Calls lists every method invoked, in order.
	Calls []string
	// CreatePRErr, if set, is returned by CreatePullRequest after the PR is
	// stored, simulating a lost response or a concurrent creator.
	CreatePRErr error
}

var _ provider.Provider = (*Provider)(nil)

// New returns a provider whose base branch holds files.
func New(baseBranch string, files map[string]string) *Provider {
	p := &Provider{branches: map[string]map[string][]byte{baseBranch: {}}}
	for k, v := range files {
		p.branches[baseBranch][k] = []byte(v)
	}
	return p
}

func (p *Provider) record(call string) {
	p.Calls = append(p.Calls, call)
}

// File returns a file's content on a branch.
func (p *Provider) File(branch, path string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.branches[branch][path]
	return string(c), ok
}

// SetFile writes a file directly to a branch.
func (p *Provider) SetFile(branch, path, content string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.branches[branch] == nil {
		p.branches[branch] = map[string][]byte{}
	}
	p.branches[branch][path] = []byte(content)
}

// PullRequests returns all pull requests ever created.
func (p *Provider) PullRequests() []provider.PullRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.PullRequest, 0, len(p.prs))
	for _, pr := range p.prs {
		out = append(out, *pr)
	}
	return out
}

// SetPRState changes a PR's state ("open", "closed") and merged flag.
func (p *Provider) SetPRState(number int, state string, merged bool, labels ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.prs {
		if pr.Number == number {
			pr.State = state
			pr.IsMerged = merged
			pr.Labels = append(pr.Labels, labels...)
		}
	}
}

// Commits returns the number of commits created.
func (p *Provider) Commits() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commits
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "fake" }

// CheckConnection implements provider.Provider.
func (p *Provider) CheckConnection(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("CheckConnection")
	return nil
}

// GetFileContent implements provider.Provider.
func (p *Provider) GetFileContent(_ context.Context, _, path, ref string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("GetFileContent " + ref + ":" + path)
	files, ok := p.branches[ref]
	if !ok {
		return nil, fmt.Errorf("branch %s: %w", ref, provider.ErrNotFound)
	}
	c, ok := files[path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, provider.ErrNotFound)
	}
	return append([]byte(nil), c...), nil
}

// CreateBranch implements provider.Provider.
func (p *Provider) CreateBranch(_ context.Context, _, branch, base string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("CreateBranch " + branch)
	if _, ok := p.branches[branch]; ok {
		return fmt.Errorf("branch %s already exists", branch)
	}
	return p.copyBranch(branch, base)
}

// ResetBranch implements provider.Provider.
func (p *Provider) ResetBranch(_ context.Context, _, branch, base string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("ResetBranch " + branch)
	return p.copyBranch(branch, base)
}

func (p *Provider) copyBranch(branch, base string) error {
	src, ok := p.branches[base]
	if !ok {
		return fmt.Errorf("branch %s: %w", base, provider.ErrNotFound)
	}
	dst := make(map[string][]byte, len(src))
	for k, v := range src {
		dst[k] = append([]byte(nil), v...)
	}
	p.branches[branch] = dst
	return nil
}

// BranchExists implements provider.Provider.
func (p *Provider) BranchExists(_ context.Context, _, branch string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("BranchExists " + branch)
	_, ok := p.branches[branch]
	return ok, nil
}

// Commit implements provider.Provider.
func (p *Provider) Commit(_ context.Context, req provider.CommitRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("Commit " + req.Branch)
	files, ok := p.branches[req.Branch]
	if !ok {
		return "", fmt.Errorf("branch %s: %w", req.Branch, provider.ErrNotFound)
	}
	for _, c := range req.Changes {
		files[c.Path] = append([]byte(nil), c.Content...)
	}
	p.commits++
	return fmt.Sprintf("sha-%d", p.commits), nil
}

// CreatePullRequest implements provider.Provider. Like GitHub, it refuses a
// second open PR for the same head and base.
func (p *Provider) CreatePullRequest(_ context.Context, req provider.CreatePRRequest) (*provider.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("CreatePullRequest " + req.HeadBranch)
	for _, pr := range p.prs {
		if pr.State == "open" && pr.HeadBranch == req.HeadBranch && pr.BaseBranch == req.BaseBranch {
			return nil, fmt.Errorf("pull request for %s: %w", req.HeadBranch, provider.ErrAlreadyExists)
		}
	}
	pr := &provider.PullRequest{
		Number:     len(p.prs) + 1,
		URL:        fmt.Sprintf("https://github.example/%s/pull/%d", req.Repository, len(p.prs)+1),
		State:      "open",
		Title:      req.Title,
		Labels:     append([]string(nil), req.Labels...),
		HeadBranch: req.HeadBranch,
		BaseBranch: req.BaseBranch,
	}
	p.prs = append(p.prs, pr)
	if p.CreatePRErr != nil {
		return nil, p.CreatePRErr
	}
	c := *pr
	return &c, nil
}

// UpdatePullRequest implements provider.Provider.
func (p *Provider) UpdatePullRequest(_ context.Context, _ string, number int, title, _ string) (*provider.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record(fmt.Sprintf("UpdatePullRequest %d", number))
	for _, pr := range p.prs {
		if pr.Number == number {
			pr.Title = title
			c := *pr
			return &c, nil
		}
	}
	return nil, fmt.Errorf("pull request %d: %w", number, provider.ErrNotFound)
}

// FindOpenPullRequest implements provider.Provider.
func (p *Provider) FindOpenPullRequest(_ context.Context, _, head, base string) (*provider.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("FindOpenPullRequest " + head)
	for _, pr := range p.prs {
		if pr.State == "open" && pr.HeadBranch == head && pr.BaseBranch == base {
			c := *pr
			return &c, nil
		}
	}
	return nil, nil
}

// GetPullRequest implements provider.Provider.
func (p *Provider) GetPullRequest(_ context.Context, _ string, number int) (*provider.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record(fmt.Sprintf("GetPullRequest %d", number))
	for _, pr := range p.prs {
		if pr.Number == number {
			c := *pr
			return &c, nil
		}
	}
	return nil, fmt.Errorf("pull request %d: %w", number, provider.ErrNotFound)
}

// GetPullRequestByBranch implements provider.Provider.
func (p *Provider) GetPullRequestByBranch(_ context.Context, _, head string) (*provider.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("GetPullRequestByBranch " + head)
	for i := len(p.prs) - 1; i >= 0; i-- {
		if p.prs[i].HeadBranch == head {
			c := *p.prs[i]
			return &c, nil
		}
	}
	return nil, nil
}

// HasLabel implements provider.Provider.
func (p *Provider) HasLabel(ctx context.Context, repo string, number int, label string) (bool, error) {
	pr, err := p.GetPullRequest(ctx, repo, number)
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

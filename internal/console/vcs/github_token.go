package vcs

import (
	"context"
	"fmt"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

// NewTokenProvider returns a VCSProvider backed by a GitHub Personal Access
// Token. The token requires `contents:write` and `pull_requests:write` scopes
// (fine-grained PAT) or equivalent classic-PAT repo scope.
//
// The GitHub App implementation is the intended long-term path (per-install,
// per-repo permissions); this PAT implementation is the first cut.
func NewTokenProvider(token string) VCSProvider {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return &tokenProvider{client: github.NewClient(tc)}
}

type tokenProvider struct {
	client *github.Client
}

func (p *tokenProvider) GetFileContent(ctx context.Context, repo RepoRef, path string) ([]byte, error) {
	fc, _, _, err := p.client.Repositories.GetContents(ctx, repo.Owner, repo.Repo, path,
		&github.RepositoryContentGetOptions{Ref: repo.Branch})
	if err != nil {
		return nil, fmt.Errorf("get %s/%s@%s: %w", repo.Owner+"/"+repo.Repo, path, repo.Branch, err)
	}
	content, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode content for %s: %w", path, err)
	}
	return []byte(content), nil
}

func (p *tokenProvider) OpenPR(ctx context.Context, repo RepoRef, branch string, files []FileChange, title, body string) (PRResult, error) {
	// 1. Resolve the base branch SHA.
	ref, _, err := p.client.Git.GetRef(ctx, repo.Owner, repo.Repo, "refs/heads/"+repo.Branch)
	if err != nil {
		return PRResult{}, fmt.Errorf("get ref %s: %w", repo.Branch, err)
	}
	baseSHA := ref.GetObject().GetSHA()

	// 2. Create a blob for each changed file.
	entries := make([]*github.TreeEntry, 0, len(files))
	for _, f := range files {
		content := string(f.Content)
		blob, _, err := p.client.Git.CreateBlob(ctx, repo.Owner, repo.Repo, &github.Blob{
			Content:  &content,
			Encoding: github.Ptr("utf-8"),
		})
		if err != nil {
			return PRResult{}, fmt.Errorf("create blob for %s: %w", f.Path, err)
		}
		path := f.Path
		mode := "100644"
		typ := "blob"
		entries = append(entries, &github.TreeEntry{
			Path: &path,
			Mode: &mode,
			Type: &typ,
			SHA:  blob.SHA,
		})
	}

	// 3. Create a tree on top of the base commit's tree.
	tree, _, err := p.client.Git.CreateTree(ctx, repo.Owner, repo.Repo, baseSHA, entries)
	if err != nil {
		return PRResult{}, fmt.Errorf("create tree: %w", err)
	}

	// 4. Create the commit.
	commit, _, err := p.client.Git.CreateCommit(ctx, repo.Owner, repo.Repo, &github.Commit{
		Message: &title,
		Tree:    tree,
		Parents: []*github.Commit{{SHA: &baseSHA}},
	}, nil)
	if err != nil {
		return PRResult{}, fmt.Errorf("create commit: %w", err)
	}

	// 5. Create the branch ref pointing at the new commit.
	branchRef := "refs/heads/" + branch
	commitSHA := commit.GetSHA()
	_, _, err = p.client.Git.CreateRef(ctx, repo.Owner, repo.Repo, &github.Reference{
		Ref:    &branchRef,
		Object: &github.GitObject{SHA: &commitSHA},
	})
	if err != nil {
		return PRResult{}, fmt.Errorf("create ref %s: %w", branch, err)
	}

	// 6. Open the pull request.
	pr, _, err := p.client.PullRequests.Create(ctx, repo.Owner, repo.Repo, &github.NewPullRequest{
		Title: &title,
		Body:  &body,
		Head:  &branch,
		Base:  &repo.Branch,
	})
	if err != nil {
		return PRResult{}, fmt.Errorf("create PR: %w", err)
	}
	return PRResult{URL: pr.GetHTMLURL(), Number: pr.GetNumber()}, nil
}

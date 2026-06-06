// Package vcs abstracts version-control operations the console needs:
// fetching a file from a repo and opening a pull request. The interface
// accepts a PAT-based implementation now; a GitHub App implementation can
// be swapped in later without changing callers.
package vcs

import "context"

// RepoRef identifies a GitHub repository and a base branch.
type RepoRef struct {
	Owner  string
	Repo   string
	Branch string // base branch (e.g. "main")
}

// FileChange is one file to write into a PR commit.
type FileChange struct {
	Path    string // repo-relative path
	Content []byte
}

// PRResult holds the URL and number of the opened pull request.
type PRResult struct {
	URL    string
	Number int
}

// VCSProvider abstracts the two operations the console's edit flow needs.
type VCSProvider interface {
	// GetFileContent returns the current content of a repo file at HEAD of
	// the given branch. Used to base the PR edit on the repo's actual HEAD,
	// not the server's potentially-stale cached copy.
	GetFileContent(ctx context.Context, repo RepoRef, path string) ([]byte, error)

	// OpenPR creates a new branch with the given file changes (one atomic
	// commit via the Git Data API) and opens a pull request against
	// repo.Branch. title is used for both the commit message and the PR
	// title; body is the PR description.
	OpenPR(ctx context.Context, repo RepoRef, branch string, files []FileChange, title, body string) (PRResult, error)
}

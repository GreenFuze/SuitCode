// Package vcs provides a VCSProvider implementation backed by the local git
// binary. All git operations are performed via os/exec; no CGO or external
// Go git library is required.
package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const id provider.ProviderID = "vcs"

// Provider implements provider.VCSProvider using the local git binary.
type Provider struct {
	repoPath string
	ready    bool
}

// New returns a new, unattached VCS Provider.
func New() *Provider {
	return &Provider{}
}

// Capabilities satisfies provider.Provider.
func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		ID:          id,
		DisplayName: "VCS Provider (git)",
		Roles:       []provider.ProviderRole{provider.RoleVCS},
	}
}

// Attach verifies that the path contains a git repository.
func (p *Provider) Attach(ctx context.Context, repoPath string) error {
	out, err := p.run(ctx, repoPath, "git", "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("vcs provider: %q does not appear to be a git repository: %w", repoPath, err)
	}
	_ = out
	p.repoPath = repoPath
	p.ready = true
	return nil
}

// Ready reports whether the provider is attached to a valid git repository.
func (p *Provider) Ready() bool { return p.ready }

// Close is a no-op; git subprocesses are not persistent.
func (p *Provider) Close() error { return nil }

// Status returns the current branch, HEAD hash, and working-tree state.
func (p *Provider) Status(ctx context.Context) (*provider.ProviderResult[provider.VCSStatus], error) {
	if !p.ready {
		return nil, fmt.Errorf("vcs provider: not attached — call Attach first")
	}

	start := time.Now()

	// Branch name.
	branch, err := p.run(ctx, p.repoPath, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("vcs provider: getting branch: %w", err)
	}

	// HEAD hash.
	head, err := p.run(ctx, p.repoPath, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("vcs provider: getting HEAD: %w", err)
	}

	// Porcelain status for modified/untracked files.
	statusOut, err := p.run(ctx, p.repoPath, "git", "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("vcs provider: getting status: %w", err)
	}

	var modified, untracked []string
	for _, line := range strings.Split(statusOut, "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		path := strings.TrimSpace(line[3:])
		if strings.Contains(xy, "?") {
			untracked = append(untracked, path)
		} else {
			modified = append(modified, path)
		}
	}

	status := provider.VCSStatus{
		Branch:    strings.TrimSpace(branch),
		HeadHash:  strings.TrimSpace(head),
		IsClean:   len(modified) == 0 && len(untracked) == 0,
		Modified:  modified,
		Untracked: untracked,
	}

	return &provider.ProviderResult[provider.VCSStatus]{
		Data: status,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindGit,
			SourceTool:      "git status",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("branch=%s head=%s", status.Branch, shortHash(status.HeadHash)),
			EvidencePaths:   []string{p.repoPath},
		}},
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// Diff returns a diff between fromRef and toRef (defaults to HEAD if toRef is
// empty).
func (p *Provider) Diff(ctx context.Context, fromRef, toRef string) (*provider.ProviderResult[provider.VCSDiff], error) {
	if !p.ready {
		return nil, fmt.Errorf("vcs provider: not attached — call Attach first")
	}

	start := time.Now()

	args := []string{"diff", "--stat", fromRef}
	if toRef != "" {
		args = append(args, toRef)
	}

	statOut, err := p.run(ctx, p.repoPath, "git", args...)
	if err != nil {
		return nil, fmt.Errorf("vcs provider: diff stat %s..%s: %w", fromRef, toRef, err)
	}

	additions, deletions := parseDiffStat(statOut)

	// Also get the list of changed files.
	nameArgs := []string{"diff", "--name-only", fromRef}
	if toRef != "" {
		nameArgs = append(nameArgs, toRef)
	}

	namesOut, err := p.run(ctx, p.repoPath, "git", nameArgs...)
	if err != nil {
		return nil, fmt.Errorf("vcs provider: diff names %s..%s: %w", fromRef, toRef, err)
	}

	changedFiles := splitLines(namesOut)

	diff := provider.VCSDiff{
		FromRef:      fromRef,
		ToRef:        toRef,
		ChangedFiles: changedFiles,
		Additions:    additions,
		Deletions:    deletions,
	}

	return &provider.ProviderResult[provider.VCSDiff]{
		Data: diff,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindGit,
			SourceTool:      "git diff",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("%d files changed from %s", len(changedFiles), fromRef),
			EvidencePaths:   []string{p.repoPath},
		}},
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// ChangedFiles returns the relative paths of files changed since fromRef.
func (p *Provider) ChangedFiles(ctx context.Context, fromRef string) (*provider.ProviderResult[[]string], error) {
	if !p.ready {
		return nil, fmt.Errorf("vcs provider: not attached — call Attach first")
	}

	start := time.Now()

	out, err := p.run(ctx, p.repoPath, "git", "diff", "--name-only", fromRef)
	if err != nil {
		return nil, fmt.Errorf("vcs provider: changed files since %s: %w", fromRef, err)
	}

	files := splitLines(out)

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindGit,
			SourceTool:      "git diff --name-only",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("%d files changed since %s", len(files), fromRef),
			EvidencePaths:   []string{p.repoPath},
		}},
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// RecentCommits returns up to limit recent commits from HEAD.
func (p *Provider) RecentCommits(ctx context.Context, limit int) (*provider.ProviderResult[[]provider.VCSCommit], error) {
	if !p.ready {
		return nil, fmt.Errorf("vcs provider: not attached — call Attach first")
	}

	start := time.Now()

	format := "%H%x1f%h%x1f%an%x1f%ad%x1f%s"
	out, err := p.run(ctx, p.repoPath, "git", "log",
		fmt.Sprintf("--max-count=%d", limit),
		"--date=short",
		fmt.Sprintf("--format=%s", format),
	)
	if err != nil {
		return nil, fmt.Errorf("vcs provider: recent commits: %w", err)
	}

	var commits []provider.VCSCommit
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x1f")
		if len(parts) < 5 {
			continue
		}
		commits = append(commits, provider.VCSCommit{
			Hash:      parts[0],
			ShortHash: parts[1],
			Author:    parts[2],
			Date:      parts[3],
			Message:   parts[4],
		})
	}

	return &provider.ProviderResult[[]provider.VCSCommit]{
		Data: commits,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindGit,
			SourceTool:      "git log",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("%d recent commits", len(commits)),
			EvidencePaths:   []string{p.repoPath},
		}},
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// run executes a git command in dir and returns trimmed stdout, or an error
// that includes the combined stderr output for diagnostics.
func (p *Provider) run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", strings.Join(append([]string{name}, args...), " "), errMsg)
	}

	return strings.TrimRight(stdout.String(), "\n\r"), nil
}

// splitLines splits a newline-separated string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// parseDiffStat extracts additions and deletions from `git diff --stat` output.
// The last line typically reads "N files changed, A insertions(+), D deletions(-)".
func parseDiffStat(s string) (additions, deletions int) {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "insertion") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if strings.HasPrefix(p, "insertion") && i > 0 {
					additions, _ = strconv.Atoi(parts[i-1])
				}
				if strings.HasPrefix(p, "deletion") && i > 0 {
					deletions, _ = strconv.Atoi(parts[i-1])
				}
			}
		}
	}
	return additions, deletions
}

// shortHash returns the first 7 characters of a full git hash.
func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

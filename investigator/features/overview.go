package features

import (
	"context"
	"path/filepath"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultOverviewBudget = 4_000

// RunRepoOverview produces a RepoOverviewResponse from a pre-fetched file listing.
func RunRepoOverview(
	_ context.Context,
	req cfeatures.RepoOverviewRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
) (*cfeatures.RepoOverviewResponse, error) {
	budget := budgetOrDefault(req.Budget, defaultOverviewBudget)
	runID := newRunID("repo-overview")
	metrics, start := startMetrics(runID, "repo-overview", req.RepoPath, budget)

	resp := &cfeatures.RepoOverviewResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
		Languages:           listing.Data.Languages,
		BuildSystems:        listing.Data.BuildSystems,
		TestSystems:         listing.Data.TestSystems,
		TotalFiles:          listing.Data.TotalFiles,
		IgnoredAreas:        summariseIgnoredPaths(listing.Data.IgnoredPaths),
	}

	// Build the top-level structure from root-level entries.
	topLevel := buildTopLevelStructure(listing)
	resp.TopLevelStructure = topLevel

	// Extract configuration files (root-level config-role files).
	for _, f := range listing.Data.Files {
		if f.Role == "config" && !strings.Contains(f.RelPath, "/") {
			resp.ConfigFiles = append(resp.ConfigFiles, fileToRef(f, fsProv("root config file", f.Path)))
		}
	}

	// Notable directories: top-level source directories.
	resp.NotableDirectories = buildNotableDirectories(listing)

	// Generated areas: directories that have generated files.
	resp.GeneratedAreas = findGeneratedAreas(listing)

	// Token estimate for the whole response (rough).
	responseTokens := estimator.Estimate(strings.Join(resp.Languages, " ") +
		strings.Join(resp.BuildSystems, " ")).Tokens
	metrics.Budget.Used = responseTokens

	// Context reduction: scanned all files, returned only metadata.
	scanned := 0
	for _, f := range listing.Data.Files {
		scanned += estimator.Estimate(f.RelPath).Tokens
	}
	computeContextReduction(&metrics, scanned, responseTokens,
		listing.Data.TotalFiles, len(resp.TopLevelStructure), listing.Data.TotalFiles-len(resp.TopLevelStructure))

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func buildTopLevelStructure(listing *provider.ProviderResult[provider.FilesystemListing]) []cfeatures.DirectoryEntry {
	seen := make(map[string]bool)
	var entries []cfeatures.DirectoryEntry

	for _, f := range listing.Data.Files {
		parts := strings.SplitN(f.RelPath, "/", 2)
		if len(parts) == 0 {
			continue
		}
		top := parts[0]
		if seen[top] {
			continue
		}
		seen[top] = true

		isDir := len(parts) > 1
		notes := topLevelNotes(top, f)
		entries = append(entries, cfeatures.DirectoryEntry{
			RelPath:  top,
			IsDir:    isDir,
			Notes:    notes,
			Language: f.Language,
		})
	}

	return entries
}

func topLevelNotes(name string, f provider.FilesystemFile) string {
	switch strings.ToLower(name) {
	case "cmd":
		return "command entry points"
	case "internal":
		return "internal packages"
	case "pkg":
		return "public packages"
	case "api":
		return "API definitions"
	case "web", "frontend", "ui":
		return "web/frontend"
	case "test", "tests", "spec", "specs":
		return "test suites"
	case "docs", "doc", "documentation":
		return "documentation"
	case "scripts", "script":
		return "build/automation scripts"
	case "deploy", "infra", "terraform", "k8s", "kubernetes":
		return "infrastructure/deployment"
	case "proto", "protobuf":
		return "protobuf definitions"
	default:
		if f.Role == "config" {
			return "configuration"
		}
		return ""
	}
}

func buildNotableDirectories(listing *provider.ProviderResult[provider.FilesystemListing]) []cfeatures.DirectoryEntry {
	dirCounts := make(map[string]int)
	for _, f := range listing.Data.Files {
		dir := filepath.ToSlash(filepath.Dir(f.RelPath))
		if dir != "." {
			dirCounts[dir]++
		}
	}

	var notable []cfeatures.DirectoryEntry
	seen := make(map[string]bool)

	for _, f := range listing.Data.Files {
		parts := strings.SplitN(f.RelPath, "/", 2)
		if len(parts) < 2 {
			continue
		}
		top := parts[0]
		if seen[top] || dirCounts[top] == 0 {
			continue
		}
		seen[top] = true
		notable = append(notable, cfeatures.DirectoryEntry{
			RelPath: top,
			IsDir:   true,
			Notes:   topLevelNotes(top, f),
		})
		if len(notable) >= 12 {
			break
		}
	}

	return notable
}

func findGeneratedAreas(listing *provider.ProviderResult[provider.FilesystemListing]) []string {
	seen := make(map[string]bool)
	var areas []string

	for _, f := range listing.Data.Files {
		if f.Role == "generated" {
			dir := filepath.ToSlash(filepath.Dir(f.RelPath))
			if !seen[dir] {
				seen[dir] = true
				areas = append(areas, dir)
			}
		}
	}

	return areas
}

func summariseIgnoredPaths(patterns []string) []string {
	// Return at most 20 patterns to keep the response compact.
	if len(patterns) > 20 {
		return patterns[:20]
	}
	return patterns
}

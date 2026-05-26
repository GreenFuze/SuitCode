package features

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultVerifyBudget = 4_000

// RunVerifyPlan generates a deterministic verification plan for a set of
// changed files, using detected build systems.
// langProv may be nil; when provided, test files are discovered via FileTests
// rather than naming conventions.
func RunVerifyPlan(
	ctx context.Context,
	req cfeatures.VerifyPlanRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	vcsResult *provider.ProviderResult[provider.VCSDiff],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
) (*cfeatures.VerifyPlanResponse, error) {
	if len(req.FilePaths) == 0 && req.GitRef == "" {
		return nil, fmt.Errorf("verify-plan: --files or --from is required")
	}

	budget := budgetOrDefault(req.Budget, defaultVerifyBudget)
	runID := newRunID("verify-plan")
	metrics, start := startMetrics(runID, "verify-plan", req.RepoPath, budget)

	resp := &cfeatures.VerifyPlanResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
	}

	// Resolve changed file list.
	changedRelPaths := req.FilePaths
	if vcsResult != nil && req.GitRef != "" {
		changedRelPaths = vcsResult.Data.ChangedFiles
	}

	// Build a path→file index for O(1) lookups when resolving abs paths.
	listingByPath := make(map[string]provider.FilesystemFile, len(listing.Data.Files))
	for _, f := range listing.Data.Files {
		listingByPath[f.Path] = f
	}

	// Collect affected packages and structural test files.
	affectedPkgs := make(map[string]bool)
	seenTests := make(map[string]bool)
	var relatedTestRefs []cfeatures.TestReference

	for _, relPath := range changedRelPaths {
		dir := filepath.ToSlash(filepath.Dir(relPath))
		if dir == "." {
			dir = ""
		}
		if dir != "" {
			affectedPkgs[dir] = true
		}

		// Find structural test files for each changed file via language provider.
		if langProv == nil {
			continue
		}
		fsFile, err := findFile(listing, relPath, req.RepoPath)
		if err != nil {
			continue
		}

		// FileTests: spec-backed test files for the changed file's compilation unit.
		testRes, testErr := langProv.FileTests(ctx, fsFile.Path)
		if testErr == nil && testRes != nil {
			resp.Limitations = append(resp.Limitations, testRes.Limitations...)
			for _, absPath := range testRes.Data {
				if seenTests[absPath] {
					continue
				}
				tf, ok := listingByPath[absPath]
				if !ok {
					continue
				}
				seenTests[absPath] = true
				prov := provider.Provenance{
					SourceKind:      provider.SourceKindSyntax,
					SourceTool:      "language-provider",
					Authority:       provider.AuthorityVerified,
					EvidenceSummary: fmt.Sprintf("test file for compilation unit containing changed file %s", fsFile.RelPath),
					EvidencePaths:   []string{absPath},
				}
				relatedTestRefs = append(relatedTestRefs, cfeatures.TestReference{
					Name:       filepath.Base(tf.RelPath),
					FilePath:   tf.Path,
					RelPath:    tf.RelPath,
					RunCommand: buildTestCommand(tf, listing),
					Provenance: prov,
				})
			}
		}
	}

	// Build verification commands based on detected build/test systems.
	isGo := containsAny(listing.Data.BuildSystems, "Go Modules")
	isNPM := containsAny(listing.Data.BuildSystems, "npm")
	isPython := containsAny(listing.Data.BuildSystems, "Poetry", "pip", "Pipenv")
	// MSBuild may be detected via root markers (global.json etc.) OR by the
	// presence of .csproj/.sln files anywhere in the tree.
	isDotNet := containsAny(listing.Data.BuildSystems, "MSBuild") || hasDotNetProject(listing.Data.Files)

	commandsConsidered := 0

	if isGo {
		commandsConsidered += 4

		// Build.
		resp.Commands = append(resp.Commands, cfeatures.VerificationCommand{
			Command:  "go",
			Args:     []string{"build", "./..."},
			Reason:   "verify the project compiles after changes",
			Kind:     "build",
			Required: true,
			Provenance: provider.Provenance{
				SourceKind:      provider.SourceKindManifest,
				SourceTool:      "go_modules",
				Authority:       provider.AuthorityVerified,
				EvidenceSummary: "go.mod detected",
			},
		})

		// Vet.
		resp.Commands = append(resp.Commands, cfeatures.VerificationCommand{
			Command:           "go",
			Args:              []string{"vet", "./..."},
			Reason:            "static analysis for common Go mistakes",
			Kind:              "vet",
			Required:          true,
			EstimatedCostHint: "fast",
			Provenance: provider.Provenance{
				SourceKind:      provider.SourceKindManifest,
				SourceTool:      "go_modules",
				Authority:       provider.AuthorityVerified,
				EvidenceSummary: "go.mod detected",
			},
		})

		// Targeted tests for affected packages.
		if len(affectedPkgs) > 0 {
			for pkg := range affectedPkgs {
				resp.Commands = append(resp.Commands, cfeatures.VerificationCommand{
					Command:           "go",
					Args:              []string{"test", fmt.Sprintf("./%s/...", pkg), "-race"},
					Reason:            fmt.Sprintf("run tests for affected package %s", pkg),
					Kind:              "test",
					Required:          true,
					EstimatedCostHint: "medium",
					Provenance: provider.Provenance{
						SourceKind:      provider.SourceKindGit,
						SourceTool:      "git",
						Authority:       provider.AuthorityVerified,
						EvidenceSummary: fmt.Sprintf("package %s contains changed files (from diff)", pkg),
					},
				})
			}
		} else {
			// Fall back to all tests.
			resp.Commands = append(resp.Commands, cfeatures.VerificationCommand{
				Command:           "go",
				Args:              []string{"test", "./...", "-race"},
				Reason:            "run all tests (no package-level targeting available)",
				Kind:              "test",
				Required:          true,
				EstimatedCostHint: "slow",
				Provenance:        fsProv("fallback: no specific packages identified"),
			})
		}
	}

	if isNPM {
		commandsConsidered += 3
		resp.Commands = append(resp.Commands,
			cfeatures.VerificationCommand{
				Command:  "npm",
				Args:     []string{"run", "build"},
				Reason:   "compile TypeScript / bundle assets",
				Kind:     "build",
				Required: true,
				Provenance: provider.Provenance{
					SourceKind: provider.SourceKindManifest,
					SourceTool: "package.json",
					Authority:  provider.AuthorityVerified,
				},
			},
			cfeatures.VerificationCommand{
				Command:  "npm",
				Args:     []string{"test"},
				Reason:   "run the test suite",
				Kind:     "test",
				Required: true,
			},
		)
	}

	if isPython {
		commandsConsidered += 2
		resp.Commands = append(resp.Commands,
			cfeatures.VerificationCommand{
				Command:  "pytest",
				Args:     []string{},
				Reason:   "run the Python test suite",
				Kind:     "test",
				Required: true,
				Provenance: provider.Provenance{
					SourceKind: provider.SourceKindManifest,
					SourceTool: "pyproject.toml/setup.cfg",
					Authority:  provider.AuthorityVerified,
				},
			},
		)
	}

	if isDotNet {
		commandsConsidered += 2

		// Find the solution file (prefer one at the repo root).
		slnFile := findDotNetSolution(listing.Data.Files)

		buildArgs := []string{"build"}
		if slnFile != "" {
			buildArgs = append(buildArgs, slnFile)
		}
		resp.Commands = append(resp.Commands, cfeatures.VerificationCommand{
			Command:  "dotnet",
			Args:     buildArgs,
			Reason:   "compile the .NET solution / project",
			Kind:     "build",
			Required: true,
			Provenance: provider.Provenance{
				SourceKind:      provider.SourceKindManifest,
				SourceTool:      "csproj",
				Authority:       provider.AuthorityVerified,
				EvidenceSummary: ".csproj / .sln files detected in repository",
			},
		})

		// Add dotnet test only when test projects are present.
		if hasDotNetTestProject(listing.Data.Files) {
			resp.Commands = append(resp.Commands, cfeatures.VerificationCommand{
				Command:           "dotnet",
				Args:              []string{"test"},
				Reason:            "run the .NET test suite",
				Kind:              "test",
				Required:          true,
				EstimatedCostHint: "medium",
				Provenance: provider.Provenance{
					SourceKind:      provider.SourceKindManifest,
					SourceTool:      "csproj",
					Authority:       provider.AuthorityVerified,
					EvidenceSummary: "test project (.Tests/.Test/.Specs) detected",
				},
			})
		}
	}

	// If no build system was detected, add a generic note.
	if !isGo && !isNPM && !isPython && !isDotNet {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "unknown_build_system",
			Message: "no known build system detected; cannot generate verification commands",
			Scope:   "verify-plan",
		})
	}

	// Populate RelatedTests from the structurally discovered test references.
	resp.RelatedTests = relatedTestRefs

	resp.CommandsConsidered = commandsConsidered
	resp.CommandsSelected = len(resp.Commands)

	outputTokens := estimator.Estimate(fmt.Sprintf("%v", resp.Commands)).Tokens
	metrics.Budget.Used = outputTokens

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

func containsAny(slice []string, values ...string) bool {
	for _, v := range values {
		for _, s := range slice {
			if s == v {
				return true
			}
		}
	}
	return false
}

// hasDotNetProject returns true when any file in the listing has a .NET project
// extension (.csproj, .fsproj, .vbproj, .sln). Used as a fallback when the
// MSBuild root-file markers (global.json etc.) are absent.
func hasDotNetProject(files []provider.FilesystemFile) bool {
	for _, f := range files {
		switch strings.ToLower(filepath.Ext(f.RelPath)) {
		case ".csproj", ".fsproj", ".vbproj", ".sln":
			return true
		}
	}
	return false
}

// hasDotNetTestProject returns true when any .csproj path contains a common
// test-project naming convention (Tests, Test, Specs, Spec).
func hasDotNetTestProject(files []provider.FilesystemFile) bool {
	for _, f := range files {
		if strings.ToLower(filepath.Ext(f.RelPath)) != ".csproj" {
			continue
		}
		base := strings.ToLower(filepath.Base(f.RelPath))
		if strings.Contains(base, "test") || strings.Contains(base, "spec") {
			return true
		}
	}
	return false
}

// findDotNetSolution returns the relative path of the first .sln file found,
// preferring one in the repository root (shortest path). Returns "" when none exists.
func findDotNetSolution(files []provider.FilesystemFile) string {
	best := ""
	for _, f := range files {
		if strings.ToLower(filepath.Ext(f.RelPath)) != ".sln" {
			continue
		}
		if best == "" || len(f.RelPath) < len(best) {
			best = f.RelPath
		}
	}
	return best
}


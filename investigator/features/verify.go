package features

import (
	"context"
	"fmt"
	"path/filepath"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultVerifyBudget = 4_000

// RunVerifyPlan generates a deterministic verification plan for a set of
// changed files, using detected build systems and naming conventions.
func RunVerifyPlan(
	_ context.Context,
	req cfeatures.VerifyPlanRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	vcsResult *provider.ProviderResult[provider.VCSDiff],
	estimator provider.TokenEstimator,
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

	// Collect affected packages and test commands.
	affectedPkgs := make(map[string]bool)
	var relatedTests []provider.FilesystemFile

	for _, relPath := range changedRelPaths {
		dir := filepath.ToSlash(filepath.Dir(relPath))
		if dir == "." {
			dir = ""
		}
		if dir != "" {
			affectedPkgs[dir] = true
		}

		// Find tests for each changed file.
		fsFile, err := findFile(listing, relPath, req.RepoPath)
		if err != nil {
			continue
		}
		testFiles := testFilesForSource(listing, fsFile.RelPath)
		relatedTests = append(relatedTests, testFiles...)
	}

	// Build verification commands based on detected build/test systems.
	isGo := containsAny(listing.Data.BuildSystems, "Go Modules")
	isNPM := containsAny(listing.Data.BuildSystems, "npm")
	isPython := containsAny(listing.Data.BuildSystems, "Poetry", "pip", "Pipenv")

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
					Provenance: heuristicProv("changed_files",
						fmt.Sprintf("package %s contains changed files", pkg)),
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

	// If no build system was detected, add a generic note.
	if !isGo && !isNPM && !isPython {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "unknown_build_system",
			Message: "no known build system detected; cannot generate verification commands",
			Scope:   "verify-plan",
		})
	}

	// Related tests.
	for _, tf := range relatedTests {
		prov := heuristicProv("naming_convention",
			fmt.Sprintf("test file for changed source: %s", tf.RelPath), tf.Path)
		resp.RelatedTests = append(resp.RelatedTests, cfeatures.TestReference{
			Name:       filepath.Base(tf.RelPath),
			FilePath:   tf.Path,
			RelPath:    tf.RelPath,
			RunCommand: buildTestCommand(tf, listing),
			Provenance: prov,
		})
	}

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


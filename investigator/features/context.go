package features

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

// ChangedFilesProvider can report which files have changed since a git ref.
// Implemented by *vcs.Provider; nil is accepted (changed-since is disabled).
type ChangedFilesProvider interface {
	ChangedFiles(ctx context.Context, fromRef string) (*provider.ProviderResult[[]string], error)
}

const defaultContextBudget = 8_000

// Scoring constants for context candidate ranking. Scores also determine tier
// membership — see tierCriticalMin below.
const (
	scoreImportedBy        = 0.90 // forward import: seed imports this package
	scoreImporterOf        = 0.80 // reverse import: production file that imports the seed
	scorePeer              = 0.75 // package peer: same compilation unit as seed
	scoreTest              = 0.70 // test file: tests for the seed or test-importer of seed
)

// tierCriticalMin is the minimum score for Tier-1 (critical path) candidates.
//
// Tier 1 (score ≥ 0.80 — seeds, direct imports, production importers):
//   These files have a direct structural relationship with the seed.
//   They are always included regardless of budget.
//
// Tier 2 (score < 0.80 — peers, test files, test-importers):
//   These files are coincident in the same compilation unit (peers), serve as
//   verification artifacts (tests), or are test files that import the seed.
//   Test-importers are demoted to scoreTest (0.70) even when discovered via
//   FileImporters, because they rarely represent the structural callers that
//   matter for understanding a production code change. Within Tier 2, peers
//   (0.75) rank above test files (0.70), so structural co-residents are
//   preferred when the remaining budget is limited.
//   When trimmed, a "contextual_trimmed" limitation reports the exact
//   --budget value needed to include all structurally related files.
//
// The boundary sits between scoreImporterOf (0.80) and scorePeer (0.75).
const tierCriticalMin = scoreImporterOf

// RunContext is the ContextCompiler: it gathers candidates, scores and ranks
// them, applies tiered budget logic, and returns a bounded ContextCapsule.
//
// langProv may be nil — when provided it enriches scoring with import-graph
// signals and provides symbol extraction for --depth signatures mode.
// vcsProv may be nil — when provided it enables --changed-since differential
// depth: changed files receive full content, unchanged files receive signatures.
func RunContext(
	ctx context.Context,
	req cfeatures.ContextRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
	vcsProv ChangedFilesProvider,
) (*cfeatures.ContextResponse, error) {
	if len(req.Files) == 0 && req.DiffRef == "" {
		return nil, fmt.Errorf("context: --files or --from is required")
	}

	budget := budgetOrDefault(req.Budget, defaultContextBudget)
	runID := newRunID("context")
	metrics, start := startMetrics(runID, "context", req.RepoPath, budget)

	resp := &cfeatures.ContextResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
	}

	// ── Seed resolution ───────────────────────────────────────────────────────
	//
	// Each entry in req.Files may be a file path OR a directory path.
	// Directories are expanded to every indexed file they contain (recursive)
	// so that "suitcode . context --files server" works as expected.

	var seedRelPaths []string
	seenSeeds := make(map[string]bool)

	for _, f := range req.Files {
		fsFiles, err := findFilesOrDir(listing, f, req.RepoPath)
		if err != nil {
			resp.Limitations = append(resp.Limitations, provider.Limitation{
				Kind:    "seed_file_not_found",
				Message: fmt.Sprintf("seed not found in index: %q — %v", f, err),
				Scope:   f,
			})
			continue
		}
		// Deduplicate: a directory seed might overlap with an explicit file seed.
		for _, fsFile := range fsFiles {
			if !seenSeeds[fsFile.RelPath] {
				seenSeeds[fsFile.RelPath] = true
				seedRelPaths = append(seedRelPaths, fsFile.RelPath)
			}
		}
	}

	if len(seedRelPaths) == 0 {
		return nil, fmt.Errorf("context: none of the specified files were found in the repository index")
	}

	// ── Changed-since resolution ──────────────────────────────────────────────
	//
	// Build a set of rel-paths that have changed since req.ChangedSince.
	// Files in this set receive full (or req.Depth) content; all others receive
	// signatures-only content to save tokens on stable background code.
	changedRelPaths := make(map[string]bool)
	if req.ChangedSince != "" && vcsProv != nil {
		if cfRes, err := vcsProv.ChangedFiles(ctx, req.ChangedSince); err == nil {
			for _, relPath := range cfRes.Data {
				changedRelPaths[filepath.ToSlash(relPath)] = true
			}
		} else {
			resp.Limitations = append(resp.Limitations, provider.Limitation{
				Kind:    "changed_since_failed",
				Message: fmt.Sprintf("could not resolve changed files since %q: %v — all files treated as changed", req.ChangedSince, err),
			})
		}
	}

	// ── Relations filter ─────────────────────────────────────────────────────
	//
	// req.Relations restricts which structural relationship types are included.
	// Valid values: "imports", "importers", "peers", "tests".
	// An empty slice (default) includes all types. Seeds always included.

	relationAllowed := func(rel string) bool {
		if len(req.Relations) == 0 {
			return true
		}
		for _, r := range req.Relations {
			if r == rel {
				return true
			}
		}
		return false
	}

	// ── Import-graph enrichment ───────────────────────────────────────────────
	//
	// All four sets are language-provider-backed — no naming heuristics.

	importedAbsPaths := make(map[string]bool)
	importerAbsPaths := make(map[string]bool)
	peerAbsPaths := make(map[string]bool)
	testAbsPaths := make(map[string]bool)
	importEdgesScanned := 0
	lspEnhanced := false

	if langProv != nil {
		for _, seedRel := range seedRelPaths {
			seedAbs := filepath.Join(req.RepoPath, filepath.FromSlash(seedRel))

			if relationAllowed("imports") {
				if res, err := langProv.FileImports(ctx, seedAbs); err == nil {
					for _, p := range res.Data {
						importedAbsPaths[p] = true
						importEdgesScanned++
					}
					if len(res.Data) > 0 {
						lspEnhanced = true
					}
				}
			}

			if relationAllowed("importers") {
				if res, err := langProv.FileImporters(ctx, seedAbs); err == nil {
					for _, p := range res.Data {
						importerAbsPaths[p] = true
						importEdgesScanned++
					}
					if len(res.Data) > 0 {
						lspEnhanced = true
					}
				}
			}

			if relationAllowed("peers") {
				if res, err := langProv.FilePeers(ctx, seedAbs); err == nil {
					for _, p := range res.Data {
						peerAbsPaths[p] = true
						importEdgesScanned++
					}
					if len(res.Data) > 0 {
						lspEnhanced = true
					}
				}
			}

			if relationAllowed("tests") {
				if res, err := langProv.FileTests(ctx, seedAbs); err == nil {
					for _, p := range res.Data {
						testAbsPaths[p] = true
						importEdgesScanned++
					}
					if len(res.Data) > 0 {
						lspEnhanced = true
					}
				}
			}
		}
	}

	// ── Candidate selection ───────────────────────────────────────────────────

	type candidate struct {
		file   provider.FilesystemFile
		score  float64
		reason string
		est    provider.TokenEstimate
		isTier2 bool
	}

	seedSet := make(map[string]bool, len(seedRelPaths))
	for _, s := range seedRelPaths {
		seedSet[s] = true
	}

	var candidates []candidate
	seenCandidates := make(map[string]bool)

	for _, f := range listing.Data.Files {
		if seenCandidates[f.RelPath] {
			continue
		}

		var score float64
		var reason string

		switch {
		case seedSet[f.RelPath]:
			score = 1.0
			reason = "seed file (explicitly requested)"
		case importedAbsPaths[f.Path]:
			score = scoreImportedBy
			reason = "file is in a package directly imported by a seed"
		case importerAbsPaths[f.Path]:
			// Test files that happen to import the seed (e.g. unit tests for the
			// seed's type) are contextual, not critical-path. Demoting them to
			// scoreTest keeps Tier 1 clean for production structural callers, and
			// lets peers (0.75) rank above test-importers (0.70) within Tier 2.
			if f.Role == "test" {
				score = scoreTest
				reason = "test file that imports a seed (contextual tier — production importers are Tier 1)"
			} else {
				score = scoreImporterOf
				reason = "file is in a package that directly imports a seed"
			}
		case peerAbsPaths[f.Path]:
			score = scorePeer
			reason = "file is in the same compilation unit as a seed"
		case testAbsPaths[f.Path]:
			score = scoreTest
			reason = "test file for the seed's package"
		default:
			continue
		}

		est, _ := estimator.EstimateFile(f.Path)
		seenCandidates[f.RelPath] = true
		candidates = append(candidates, candidate{
			file:    f,
			score:   score,
			reason:  reason,
			est:     est,
			isTier2: score < tierCriticalMin,
		})
	}

	// Sort by score descending, then by path for determinism.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].file.RelPath < candidates[j].file.RelPath
	})

	// ── Tiered budget gate ────────────────────────────────────────────────────
	//
	// Pre-compute Tier-1 total so we know how much budget Tier-2 can use.
	// Also compute seed-only tokens — the hard floor below which no budget
	// value can reduce response size (seeds are always included).

	tier1Tokens := 0
	seedOnlyTokens := 0
	for _, c := range candidates {
		if !c.isTier2 {
			tier1Tokens += c.est.Tokens
		}
		if c.score >= 1.0 {
			seedOnlyTokens += c.est.Tokens
		}
	}

	tier2Budget := budget - tier1Tokens
	if tier2Budget < 0 {
		tier2Budget = 0
	}

	// ── Build capsule ─────────────────────────────────────────────────────────

	capsule := cfeatures.ContextCapsule{BudgetRequested: budget}
	tokenUsed := 0
	tier2TokensUsed := 0
	tier2OmittedCount := 0
	tier2OmittedTokens := 0
	criticalPathCount := 0
	contextualIncluded := 0
	rank := 0

	for _, c := range candidates {
		// Tier-2 gate: include only if the file fits in the remaining tier-2 budget.
		if c.isTier2 {
			if tier2TokensUsed+c.est.Tokens > tier2Budget {
				tier2OmittedCount++
				tier2OmittedTokens += c.est.Tokens
				continue
			}
		}

		// Determine the effective depth for this file.
		//
		// Rules:
		//   1. If ChangedSince is set and the file is NOT in the changed set →
		//      force signatures mode regardless of req.Depth (unchanged background code).
		//   2. If ChangedSince is set and the file IS changed, OR ChangedSince is
		//      unset → use req.Depth ("full" or "signatures").
		//   3. Seed files always get full content (score 1.0) — they are the
		//      explicit focus of the session.
		effectiveDepth := req.Depth
		if effectiveDepth == "" {
			effectiveDepth = "full"
		}
		if req.ChangedSince != "" && len(changedRelPaths) > 0 {
			fileSlash := filepath.ToSlash(c.file.RelPath)
			if !changedRelPaths[fileSlash] && c.score < 1.0 {
				// Unchanged non-seed file → signatures to save tokens.
				effectiveDepth = "signatures"
			}
		}

		// Acquire file content using the effective depth.
		fileContent, contentMode, actualEst, readErr := readFileContent(
			ctx, c.file.Path, effectiveDepth, c.est, langProv,
		)
		if readErr != nil {
			capsule.Rejections = append(capsule.Rejections, cfeatures.ContextRejection{
				Candidate: cfeatures.ContextCandidate{
					File:  fileToRef(c.file, fsProv(c.reason, c.file.Path)),
					Score: c.score,
				},
				Reason: fmt.Sprintf("could not read file: %v", readErr),
			})
			continue
		}

		// Include this candidate.
		rank++
		tokenUsed += actualEst.Tokens
		if c.isTier2 {
			tier2TokensUsed += actualEst.Tokens
			contextualIncluded++
		} else {
			criticalPathCount++
		}

		factKind := "file_content"
		if contentMode == "signatures" {
			factKind = "file_signatures"
		}

		prov := fsProv(c.reason, c.file.Path)
		capsule.Selections = append(capsule.Selections, cfeatures.ContextSelection{
			Candidate: cfeatures.ContextCandidate{
				File:          fileToRef(c.file, prov),
				Score:         c.score,
				ScoreReasons:  []string{c.reason},
				TokenEstimate: actualEst,
			},
			Rank:   rank,
			Reason: c.reason,
		})
		capsule.Facts = append(capsule.Facts, cfeatures.ContextFact{
			Kind:          factKind,
			Content:       fileContent,
			Source:        fileToRef(c.file, prov),
			Provenance:    prov,
			TokenEstimate: actualEst,
		})
	}

	// ── Tier-aware limitations ────────────────────────────────────────────────

	// Tier-1 over budget: informational only (files are still included).
	// Include seed-only token count so callers know the hard floor — no budget
	// value below seedOnlyTokens will reduce the response size.
	if tier1Tokens > budget {
		overagePct := int(float64(tier1Tokens-budget) / float64(budget) * 100)
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind: "critical_path_over_budget",
			Message: fmt.Sprintf(
				"critical path (seeds, imports, importers) is %d%% over budget: %d tokens (%d requested) — all %d critical-path files included; seeds alone require %d tokens (hard floor)",
				overagePct, tier1Tokens, budget, criticalPathCount, seedOnlyTokens,
			),
		})
	}

	// Tier-2 trimmed: actionable — tells the agent the exact budget for everything.
	if tier2OmittedCount > 0 {
		budgetForAll := tier1Tokens + tier2TokensUsed + tier2OmittedTokens
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind: "contextual_trimmed",
			Message: fmt.Sprintf(
				"%d peer/test file(s) omitted (%d tokens) — use --budget %d to include all structurally related files",
				tier2OmittedCount, tier2OmittedTokens, budgetForAll,
			),
		})
		resp.BudgetForAll = budgetForAll
	}

	// ── Assemble response ─────────────────────────────────────────────────────

	totalCandidateTokens := tier1Tokens + tier2TokensUsed + tier2OmittedTokens

	capsule.BudgetUsed = tokenUsed
	capsule.TotalEstimate = provider.TokenEstimate{
		Tokens:     tokenUsed,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}
	if totalCandidateTokens > 0 {
		capsule.CompressionRatio = roundTo2(float64(tokenUsed) / float64(totalCandidateTokens))
	}

	resp.Capsule = capsule
	resp.FilesConsidered = len(candidates)
	resp.FilesIncluded = len(capsule.Selections)
	resp.FilesExcluded = len(capsule.Rejections) + tier2OmittedCount

	// Tier breakdown for agents and CLI.
	resp.CriticalPathFiles = criticalPathCount
	resp.ContextualIncluded = contextualIncluded
	resp.ContextualOmitted = tier2OmittedCount
	resp.ContextualOmittedTokens = tier2OmittedTokens
	resp.SeedOnlyTokens = seedOnlyTokens
	if resp.BudgetForAll == 0 {
		resp.BudgetForAll = totalCandidateTokens
	}

	// IncludedRelPaths for eval golden-files checks.
	for _, sel := range capsule.Selections {
		resp.IncludedRelPaths = append(resp.IncludedRelPaths, sel.Candidate.File.RelPath)
	}

	// Flat Files[] for agents.
	for i, fact := range capsule.Facts {
		sel := capsule.Selections[i]
		contentMode := ""
		if fact.Kind == "file_signatures" {
			contentMode = "signatures"
		}
		resp.Files = append(resp.Files, cfeatures.ContextFileEntry{
			Path:          fact.Source.Path,
			RelPath:       fact.Source.RelPath,
			Language:      fact.Source.Language,
			Role:          fact.Source.Role,
			TokenEstimate: fact.TokenEstimate.Tokens,
			Rank:          sel.Rank,
			Score:         sel.Candidate.Score,
			Reason:        sel.Reason,
			Content:       fact.Content,
			ContentMode:   contentMode,
		})
	}

	avoided := totalCandidateTokens - tokenUsed
	if avoided < 0 {
		avoided = 0
	}
	resp.EvidenceScanned = provider.TokenEstimate{
		Tokens:     totalCandidateTokens,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}
	resp.EstimatedContextAvoided = provider.TokenEstimate{
		Tokens:     avoided,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}
	resp.CompressionRatio = capsule.CompressionRatio

	metrics.Budget.Used = tokenUsed
	computeContextReduction(&metrics, totalCandidateTokens, tokenUsed,
		resp.FilesConsidered, resp.FilesIncluded, resp.FilesExcluded)
	metrics.ContextReduction.LspEnhanced = lspEnhanced
	metrics.ContextReduction.ImportEdgesScanned = importEdgesScanned

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

func roundTo2(f float64) float64 {
	return float64(int(f*100)) / 100
}

// ──────────────────────────────────────────────────────────────────────────────
// Content reading helpers
// ──────────────────────────────────────────────────────────────────────────────

// readFileContent reads file content according to the effective depth mode.
//
// depth "full"       → read complete file, return ("full", original estimate)
// depth "signatures" → ask langProv for symbols; format as compact outline.
//                      Falls back to the first 60 lines when no symbols are
//                      available (langProv nil, unsupported language, LSP not ready).
//
// Returns: text content, content mode ("full" or "signatures"), actual token
// estimate for the returned content, and any read error.
func readFileContent(
	ctx context.Context,
	absPath string,
	depth string,
	originalEst provider.TokenEstimate,
	langProv provider.ImportGraphProvider,
) (content string, contentMode string, actualEst provider.TokenEstimate, err error) {
	if depth != "signatures" {
		// Full mode: read the entire file.
		raw, readErr := os.ReadFile(absPath)
		if readErr != nil {
			return "", "", provider.TokenEstimate{}, readErr
		}
		return string(raw), "full", originalEst, nil
	}

	// Signatures mode: prefer LSP symbol outline; fall back to first 60 lines.
	sigText := ""
	gotSymbols := false

	if langProv != nil {
		if res, symErr := langProv.GetSymbols(ctx, absPath); symErr == nil && len(res.Data) > 0 {
			sigText = renderSymbolOutline(absPath, res.Data)
			gotSymbols = true
		}
	}

	if !gotSymbols {
		// Fall back to first 60 lines — still much cheaper than the full file.
		sigText, err = readFirstLines(absPath, 60)
		if err != nil {
			return "", "", provider.TokenEstimate{}, err
		}
	}

	// Recompute token estimate from actual content length.
	sigTokens := len(sigText) / 4
	if sigTokens < 1 {
		sigTokens = 1
	}
	sigEst := provider.TokenEstimate{
		Tokens:     sigTokens,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}

	return sigText, "signatures", sigEst, nil
}

// renderSymbolOutline formats a flat list of dotted symbol names (e.g.
// "MainViewModel", "MainViewModel.Title", "MainViewModel.LoadGames") into a
// compact, human-readable outline block that agents can read as a structural
// summary of the file.
//
// Example output:
//
//	// Symbol outline: MainViewModel.cs
//	MainViewModel
//	  .Title
//	  .IsLoading
//	  .LoadGames
func renderSymbolOutline(absPath string, names []string) string {
	var sb strings.Builder
	sb.WriteString("// Symbol outline: ")
	sb.WriteString(filepath.Base(absPath))
	sb.WriteByte('\n')

	for _, name := range names {
		// Indent child symbols (those with a "." separator) relative to their parent.
		if idx := strings.IndexByte(name, '.'); idx >= 0 {
			sb.WriteString("  .")
			sb.WriteString(name[idx+1:])
		} else {
			sb.WriteString(name)
		}
		sb.WriteByte('\n')
	}

	return sb.String()
}

// readFirstLines returns the first n lines of the file at absPath as a string.
// Used as a fallback when symbol extraction is not available.
func readFirstLines(absPath string, n int) (string, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	lineCount := 0
	for scanner.Scan() && lineCount < n {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
		lineCount++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return sb.String(), nil
}

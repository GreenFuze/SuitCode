package eval

const (
	// SuiteGoProviderSymbols verifies that GetSymbols returns expected symbol
	// names from known Go source files via the gopls integration.
	SuiteGoProviderSymbols SuiteID = "go-provider-symbols"
)

// GoProviderScenarios returns golden-files scenarios that verify the Go
// language provider's import-graph signals produce the correct capsule
// contents. They require the Go binary to be available at run time.
func GoProviderScenarios(_ string) []EvalScenario {
	return []EvalScenario{
		// ── Forward and reverse import edges ──────────────────────────────────────

		{
			ID:    "goprovider-golden-forward-import",
			Suite: SuiteGoProvider,
			Kind:  KindGoldenFiles,
			Name:  "go-provider: forward import inclusion",
			Description: "seed=investigator/features/context.go must pull in core/features/context.go " +
				"and core/provider/roles.go (packages directly imported by that file's package). " +
				"Budget is intentionally large — this scenario tests import scoring correctness, not compression.",
			Feature: "context",
			Expectation: EvalExpectation{
				BudgetLimit: 200_000,
				SeedFiles:   []string{"investigator/features/context.go"},
				ExpectedFiles: []string{
					"core/features/context.go",
					"core/provider/roles.go",
				},
			},
		},
		{
			ID:    "goprovider-golden-reverse-import",
			Suite: SuiteGoProvider,
			Kind:  KindGoldenFiles,
			Name:  "go-provider: reverse import inclusion",
			Description: "seed=core/provider/roles.go must pull in core/provider/filesystem/provider.go " +
				"(the filesystem package directly imports core/provider). " +
				"Budget is intentionally large — this scenario tests import scoring correctness, not compression.",
			Feature: "context",
			Expectation: EvalExpectation{
				BudgetLimit: 200_000,
				SeedFiles:   []string{"core/provider/roles.go"},
				ExpectedFiles: []string{
					"core/provider/filesystem/provider.go",
				},
			},
		},

		// ── Same-package / same-directory cohesion ────────────────────────────────

		{
			ID:    "goprovider-same-package-cohesion",
			Suite: SuiteGoProvider,
			Kind:  KindGoldenFiles,
			Name:  "go-provider: same-package file cohesion",
			Description: "seed=investigator/eval/runner.go must include all other files in the same " +
				"package (suites.go, eval.go). Files sharing a package are co-scored by the import " +
				"graph and should always be included when budget allows.",
			Feature: "context",
			Expectation: EvalExpectation{
				BudgetLimit: 40_000,
				SeedFiles:   []string{"investigator/eval/runner.go"},
				ExpectedFiles: []string{
					"investigator/eval/suites.go",
					"investigator/eval/eval.go",
				},
			},
		},
		{
			ID:    "goprovider-gopls-package-cohesion",
			Suite: SuiteGoProvider,
			Kind:  KindGoldenFiles,
			Name:  "go-provider: gopls package file cohesion",
			Description: "seed=core/provider/language/go/provider.go must include key sibling files " +
				"(gopls_client.go, lsp_transport.go). The gopls provider is a multi-file package " +
				"and its files should travel together in context.",
			Feature: "context",
			Expectation: EvalExpectation{
				BudgetLimit: 200_000,
				SeedFiles:   []string{"core/provider/language/go/provider.go"},
				ExpectedFiles: []string{
					"core/provider/language/go/gopls_client.go",
					"core/provider/language/go/lsp_transport.go",
					"core/provider/language/go/lsp_types.go",
					"core/provider/language/go/gopls_binary.go",
				},
			},
		},

		// ── Forbidden-file anti-regression ────────────────────────────────────────

		{
			ID:    "goprovider-forbidden-unrelated-file",
			Suite: SuiteGoProvider,
			Kind:  KindGoldenFiles,
			Name:  "go-provider: unrelated file excluded from capsule",
			Description: "seed=core/provider/language/go/lsp_types.go has no import relationship " +
				"with investigator/eval/eval.go — the eval package should never appear in its " +
				"capsule regardless of budget. Failures here indicate false-positive scoring.",
			Feature: "context",
			Expectation: EvalExpectation{
				BudgetLimit:    200_000,
				SeedFiles:      []string{"core/provider/language/go/lsp_types.go"},
				ForbiddenFiles: []string{"investigator/eval/eval.go"},
			},
		},
	}
}

// GoProviderSymbolScenarios returns scenarios that verify GetSymbols returns
// real symbol names for known Go source files via the gopls integration.
// These scenarios require gopls to be available and skip gracefully if it is not.
func GoProviderSymbolScenarios(_ string) []EvalScenario {
	return []EvalScenario{
		{
			ID:    "goprovider-symbols-provider-go",
			Suite: SuiteGoProviderSymbols,
			Kind:  KindGoldenSymbols,
			Name:  "go-provider: symbols in provider.go",
			Description: "GetSymbols for core/provider/language/go/provider.go must return " +
				"the top-level type and function names defined in that file.",
			Feature: "symbols",
			Expectation: EvalExpectation{
				SeedFiles: []string{"core/provider/language/go/provider.go"},
				ExpectedSymbols: []string{
					"GoLanguageProvider",
					"New",
					"Attach",
					"Ready",
					"GoplsReady",
					"Close",
					"GetImports",
					"GetSymbols",
					"FileImports",
					"FileImporters",
				},
			},
		},
		{
			ID:    "goprovider-symbols-lsp-types-go",
			Suite: SuiteGoProviderSymbols,
			Kind:  KindGoldenSymbols,
			Name:  "go-provider: symbols in lsp_types.go",
			Description: "GetSymbols for core/provider/language/go/lsp_types.go must return " +
				"key LSP type names and the pathToURI function.",
			Feature: "symbols",
			Expectation: EvalExpectation{
				SeedFiles: []string{"core/provider/language/go/lsp_types.go"},
				ExpectedSymbols: []string{
					"lspDocumentSymbol",
					"pathToURI",
				},
			},
		},
	}
}

// SmokeScenarios returns the smoke suite — a minimal set of checks that
// verify the investigator produces valid, deterministic, budget-compliant
// output. All scenarios run against the repository at repoPath.
func SmokeScenarios(repoPath string) []EvalScenario {
	return []EvalScenario{
		{
			ID:          "smoke-repo-overview-determinism",
			Suite:       SuiteSmoke,
			Kind:        KindDeterminism,
			Name:        "repo-overview determinism",
			Description: "Running repo-overview three times should produce the same deterministic hash",
			Feature:     "repo-overview",
			Expectation: EvalExpectation{
				RepeatCount: 3,
				BudgetLimit: 3000,
			},
		},
		{
			ID:          "smoke-repo-overview-budget",
			Suite:       SuiteSmoke,
			Kind:        KindBudgetCompliance,
			Name:        "repo-overview budget compliance",
			Description: "repo-overview must not exceed the requested token budget",
			Feature:     "repo-overview",
			Expectation: EvalExpectation{
				BudgetLimit: 3000,
			},
		},
		{
			ID:          "smoke-context-budget",
			Suite:       SuiteSmoke,
			Kind:        KindBudgetCompliance,
			Name:        "context budget compliance",
			Description: "context must not exceed the requested token budget",
			Feature:     "context",
			Expectation: EvalExpectation{
				BudgetLimit:   2000,
				ExpectedFiles: []string{"go.mod"},
			},
		},
	}
}

// ContextReductionScenarios returns scenarios that measure the core product
// metric: how effectively SuitCode reduces context for a caller.
func ContextReductionScenarios(repoPath string) []EvalScenario {
	return []EvalScenario{
		{
			ID:          "ctxred-context-compression",
			Suite:       SuiteContextReduction,
			Kind:        KindContextCompression,
			Name:        "context capsule compression",
			Description: "The context capsule for the investigator's own entry point should have a compression ratio below 0.9 (i.e. at least 10% of candidates excluded)",
			Feature:     "context",
			Expectation: EvalExpectation{
				BudgetLimit:         4000,
				ExpectedFiles:       []string{"investigator/investigator.go"},
				MaxCompressionRatio: 0.9,
			},
		},
		{
			ID:          "ctxred-context-budget-strict",
			Suite:       SuiteContextReduction,
			Kind:        KindBudgetCompliance,
			Name:        "context strict budget enforcement",
			Description: "A tight budget must be respected even when there are many candidate files",
			Feature:     "context",
			Expectation: EvalExpectation{
				BudgetLimit:   500,
				ExpectedFiles: []string{"investigator/investigator.go"},
			},
		},
		{
			ID:          "ctxred-context-determinism",
			Suite:       SuiteContextReduction,
			Kind:        KindDeterminism,
			Name:        "context determinism",
			Description: "Running context five times for the same seed must produce an identical hash " +
				"each time. Non-determinism here means the capsule contents vary unpredictably.",
			Feature: "context",
			Expectation: EvalExpectation{
				RepeatCount: 5,
				BudgetLimit: 4000,
				SeedFiles:   []string{"investigator/investigator.go"},
			},
		},
	}
}

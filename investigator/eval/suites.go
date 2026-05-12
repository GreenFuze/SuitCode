package eval

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
			ID:          "ctxred-determinism",
			Suite:       SuiteContextReduction,
			Kind:        KindDeterminism,
			Name:        "context determinism",
			Description: "Running context five times must produce the same hash",
			Feature:     "repo-overview",
			Expectation: EvalExpectation{
				RepeatCount: 5,
				BudgetLimit: 4000,
			},
		},
	}
}

package features

import "github.com/GreenFuze/SuitCode/core/provider"

// VerificationCommand is one command in a verification plan.
type VerificationCommand struct {
	// Command is the executable (e.g. "go").
	Command string `json:"command"`
	// Args are the command arguments (e.g. ["test", "./internal/...", "-run", "TestFoo"]).
	Args []string `json:"args"`
	// Reason explains why this command was chosen.
	Reason string `json:"reason"`
	// Kind categorises the command: "test", "build", "typecheck", "lint",
	// "format", "vet".
	Kind string `json:"kind"`
	// Required is true when skipping this command would leave the change
	// unverified.
	Required bool `json:"required"`
	// EstimatedCostHint is a rough timing hint if known (e.g. "fast", "slow").
	EstimatedCostHint string             `json:"estimated_cost_hint,omitempty"`
	Provenance        provider.Provenance `json:"provenance"`
}

// VerifyPlanRequest parameters for the verify-plan feature.
type VerifyPlanRequest struct {
	BaseFeatureRequest
	// FilePaths is an explicit list of changed files.
	FilePaths []string
	// GitRef, if set, derives the changed-file list from `git diff <GitRef>`.
	GitRef string
}

// VerifyPlanResponse is the structured result of a verify-plan run.
type VerifyPlanResponse struct {
	BaseFeatureResponse

	Commands           []VerificationCommand `json:"commands"`
	CommandsConsidered int                   `json:"commands_considered"`
	CommandsSelected   int                   `json:"commands_selected"`
	RelatedTests       []TestReference       `json:"related_tests,omitempty"`

	EstimatedContextAvoided provider.TokenEstimate `json:"estimated_context_avoided"`
}

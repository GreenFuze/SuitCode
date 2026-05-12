package features

import "github.com/GreenFuze/SuitCode/core/provider"

// VerificationCommand is one command in a verification plan.
type VerificationCommand struct {
	// Command is the executable (e.g. "go").
	Command string
	// Args are the command arguments (e.g. ["test", "./internal/...", "-run", "TestFoo"]).
	Args []string
	// Reason explains why this command was chosen.
	Reason string
	// Kind categorises the command: "test", "build", "typecheck", "lint",
	// "format", "vet".
	Kind string
	// Required is true when skipping this command would leave the change
	// unverified.
	Required bool
	// EstimatedCostHint is a rough timing hint if known (e.g. "fast", "slow").
	EstimatedCostHint string
	Provenance        provider.Provenance
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

	Commands           []VerificationCommand
	CommandsConsidered int
	CommandsSelected   int
	RelatedTests       []TestReference

	EstimatedContextAvoided provider.TokenEstimate
}

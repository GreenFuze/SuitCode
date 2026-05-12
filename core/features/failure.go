package features

import "github.com/GreenFuze/SuitCode/core/provider"

// FailureSignal is one piece of structured information extracted from a
// failure log.
type FailureSignal struct {
	// Kind categorises the signal: "file_path", "test_name", "error_message",
	// "stack_frame", "package", "line_ref".
	Kind       string
	Value      string
	Confidence float64
	Provenance provider.Provenance
}

// FailureContextRequest parameters for the failure-context feature.
type FailureContextRequest struct {
	BaseFeatureRequest
	// LogPath is a path to a file containing the failure output.
	LogPath string
	// LogText is inline failure text (used when LogPath is empty).
	LogText string
}

// FailureContextResponse is the structured result of a failure-context run.
type FailureContextResponse struct {
	BaseFeatureResponse

	// ParsedSignals lists structured signals extracted from the log.
	ParsedSignals []FailureSignal
	// SuspectedFiles are repository files mentioned or implied by the log.
	SuspectedFiles []provider.FileReference
	// SuspectedTests are test names extracted from the log.
	SuspectedTests []TestReference
	// SuspectedCommands are commands (build/test/lint) implied by the log.
	SuspectedCommands []string
	// RelatedContext is a bounded capsule of relevant context for the failure.
	RelatedContext ContextCapsule

	EstimatedContextAvoided provider.TokenEstimate
}

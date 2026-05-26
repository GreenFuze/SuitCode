package features

import "github.com/GreenFuze/SuitCode/core/provider"

// FailureSignal is one piece of structured information extracted from a
// failure log.
type FailureSignal struct {
	// Kind categorises the signal: "file_path", "test_name", "error_message",
	// "stack_frame", "package", "line_ref".
	Kind       string             `json:"kind"`
	Value      string             `json:"value"`
	Confidence float64            `json:"confidence"`
	Provenance provider.Provenance `json:"provenance"`
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
	ParsedSignals []FailureSignal `json:"parsed_signals,omitempty"`
	// SuspectedFiles are repository files mentioned or implied by the log.
	SuspectedFiles []provider.FileReference `json:"suspected_files,omitempty"`
	// SuspectedTests are test names extracted from the log.
	SuspectedTests []TestReference `json:"suspected_tests,omitempty"`
	// SuspectedCommands are commands (build/test/lint) implied by the log.
	SuspectedCommands []string `json:"suspected_commands,omitempty"`
	// RelatedContext is a bounded capsule of relevant context for the failure.
	RelatedContext ContextCapsule `json:"related_context"`

	EstimatedContextAvoided provider.TokenEstimate `json:"estimated_context_avoided"`
}

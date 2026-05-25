package features

import "github.com/GreenFuze/SuitCode/core/provider"

// RelevantTest pairs a TestReference with the reason it was selected.
type RelevantTest struct {
	Test       TestReference      `json:"test"`
	Reason     string             `json:"reason"`
	Provenance provider.Provenance `json:"provenance"`
	Confidence float64            `json:"confidence"`
}

// TestsRequest parameters for the tests feature.
type TestsRequest struct {
	BaseFeatureRequest
	// FilePath is the source file whose relevant tests we want.
	FilePath string
	// Symbol narrows the search to tests covering a specific symbol.
	Symbol string
	// DiffRef, if set, selects tests relevant to the changed files in that diff.
	DiffRef string
}

// TestsResponse is the structured result of a tests run.
type TestsResponse struct {
	BaseFeatureResponse

	TargetPath    string         `json:"target_path,omitempty"`
	RelevantTests []RelevantTest `json:"relevant_tests"`

	TestsConsidered int `json:"tests_considered"`
	TestsSelected   int `json:"tests_selected"`
	TestsExcluded   int `json:"tests_excluded"`

	EstimatedContextAvoided provider.TokenEstimate `json:"estimated_context_avoided"`
}

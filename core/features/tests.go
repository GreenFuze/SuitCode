package features

import "github.com/GreenFuze/SuitCode/core/provider"

// RelevantTest pairs a TestReference with the reason it was selected.
type RelevantTest struct {
	Test       TestReference
	Reason     string
	Provenance provider.Provenance
	Confidence float64
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

	TargetPath    string
	RelevantTests []RelevantTest

	TestsConsidered int
	TestsSelected   int
	TestsExcluded   int

	EstimatedContextAvoided provider.TokenEstimate
}

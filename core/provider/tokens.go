package provider

import (
	"fmt"
	"os"
)

// TokenEstimate holds an approximate token count together with enough metadata
// for a caller to judge how much to trust it.
//
// All estimates in SuitCode v1 use the heuristic_chars_div4 method and must
// always set IsEstimate = true. Do not claim actual backend tokens saved.
type TokenEstimate struct {
	Tokens int    `json:"tokens"`
	// Method names the estimation algorithm used (e.g. "heuristic_chars_div4").
	Method     string `json:"method,omitempty"`
	// IsEstimate is always true in v1; present to remind callers not to treat
	// this as a precise billing figure.
	IsEstimate bool `json:"is_estimate"`
}

// Zero returns a zero-valued TokenEstimate (no tokens, no method).
func (t TokenEstimate) Zero() bool { return t.Tokens == 0 }

// Add returns a new TokenEstimate whose Tokens is the sum of t and other.
// The method field is preserved from the receiver.
func (t TokenEstimate) Add(other TokenEstimate) TokenEstimate {
	return TokenEstimate{
		Tokens:     t.Tokens + other.Tokens,
		Method:     t.Method,
		IsEstimate: true,
	}
}

// TokenEstimator can estimate the token count of arbitrary text or files.
type TokenEstimator interface {
	Estimate(text string) TokenEstimate
	EstimateFile(path string) (TokenEstimate, error)
}

// HeuristicEstimator approximates token counts using the chars/4 rule.
// This is intentionally rough; every result is labelled IsEstimate = true.
type HeuristicEstimator struct{}

// NewHeuristicEstimator returns a HeuristicEstimator ready to use.
func NewHeuristicEstimator() *HeuristicEstimator {
	return &HeuristicEstimator{}
}

// Estimate returns max(1, len(text)/4) tokens.
func (e *HeuristicEstimator) Estimate(text string) TokenEstimate {
	tokens := len(text) / 4
	if tokens < 1 {
		tokens = 1
	}
	return TokenEstimate{
		Tokens:     tokens,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}
}

// EstimateFile reads path and calls Estimate on its contents.
func (e *HeuristicEstimator) EstimateFile(path string) (TokenEstimate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TokenEstimate{}, fmt.Errorf("reading file for token estimate: %w", err)
	}
	return e.Estimate(string(data)), nil
}

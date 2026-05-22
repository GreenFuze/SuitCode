// Package provider defines the base interfaces, result container, and capability
// types shared by every SuitCode provider. It is the lowest-level package in
// the dependency graph — nothing in core/provider imports from other SuitCode
// packages.
package provider


// ProviderID uniquely identifies a registered provider.
type ProviderID string

// ProviderRole describes a category of evidence a provider can supply.
type ProviderRole string

const (
	RoleFilesystem ProviderRole = "filesystem"
	RoleVCS        ProviderRole = "vcs"
	RoleLanguage   ProviderRole = "language"
	RoleTest       ProviderRole = "test"
	RoleBuild      ProviderRole = "build"
)

// ProviderCapabilities describes what a provider offers.
type ProviderCapabilities struct {
	ID          ProviderID
	DisplayName string
	Roles       []ProviderRole
	// Languages this provider understands. Empty means language-agnostic.
	Languages []string
}

// Provider is the base contract every provider must satisfy.
// Initialization (attaching to a repository) is a constructor concern handled
// by each provider's own New* function, not part of this behavioral interface.
type Provider interface {
	// Capabilities returns static metadata about this provider.
	Capabilities() ProviderCapabilities

	// Ready reports whether the provider has been successfully initialised and
	// can answer queries.
	Ready() bool

	// Close releases any resources held by the provider (e.g. sub-processes,
	// open file handles, DB connections).
	Close() error
}

// ProviderResult wraps typed provider output together with the evidence
// quality metadata that every caller needs to reason about trust level.
type ProviderResult[T any] struct {
	Data T

	// Provenance records where this data came from.
	Provenance []Provenance

	// Limitations lists anything the provider could not do or could not
	// determine. Callers must surface these rather than hiding them.
	Limitations []Limitation

	// DurationMs is wall-clock milliseconds spent producing this result.
	DurationMs int64

	// CacheHit is true when the result was served from a previous computation.
	CacheHit bool

	// TokensScanned is a token-equivalent estimate of all evidence the
	// provider examined to produce this result (not just what was returned).
	TokensScanned TokenEstimate
}

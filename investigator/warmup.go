package main

import (
	"context"
	"fmt"
	"time"
)

// Warm drives the investigator through readiness levels 1 and 2, which are
// sufficient for all v1 features. Higher levels (3, 4) require language
// providers not yet implemented.
//
// Warm is idempotent and safe to call concurrently — subsequent calls return
// immediately if the investigator is already at the target level.
func (inv *ProjectInvestigator) Warm(ctx context.Context) error {
	inv.mu.Lock()
	defer inv.mu.Unlock()

	if inv.readiness >= ReadinessLevel2 {
		return nil // Already warm.
	}

	start := time.Now()

	// ── Level 1: repo identity and VCS state ──────────────────────────────────
	if inv.vcsProvider != nil {
		status, err := inv.vcsProvider.Status(ctx)
		if err != nil {
			// VCS failure is non-fatal at level 1; record a limitation and
			// continue.
			fmt.Printf("SuitCode [warn]: vcs status failed: %v\n", err)
		} else {
			inv.vcsStatus = status
		}
	}

	inv.readiness = ReadinessLevel1

	// ── Level 2: full file index ──────────────────────────────────────────────
	listing, err := inv.fsProvider.ListFiles(ctx)
	if err != nil {
		return fmt.Errorf("warmup level 2: listing files: %w", err)
	}

	inv.fileListing = listing
	inv.readiness = ReadinessLevel2

	// Record warm timing.
	elapsed := time.Since(start)
	inv.warmDuration = elapsed
	now := time.Now()
	inv.lastWarmed = &now

	return nil
}

// Invalidate clears the cached state so the next feature call will re-warm.
// Use this when the repository is known to have changed significantly.
func (inv *ProjectInvestigator) Invalidate() {
	inv.mu.Lock()
	defer inv.mu.Unlock()

	inv.fileListing = nil
	inv.vcsStatus = nil
	inv.readiness = ReadinessUnknown
	inv.lastWarmed = nil
}

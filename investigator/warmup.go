package main

import (
	"context"
	"fmt"
	"time"
)

// Warm drives the investigator through readiness levels 1–3. Level 3 requires
// the Go language provider to be ready; if it is not, the investigator stays at
// level 2 (heuristic-only scoring).
//
// Warm is idempotent and safe to call concurrently — subsequent calls return
// immediately if the investigator is already at the target level.
func (inv *ProjectInvestigator) Warm(ctx context.Context) error {
	inv.mu.Lock()
	defer inv.mu.Unlock()

	if inv.readiness >= ReadinessLevel3 {
		return nil // Already fully warm.
	}

	start := time.Now()

	// ── Level 1: repo identity and VCS state ──────────────────────────────────
	if inv.vcsProvider != nil {
		status, err := inv.vcsProvider.Status(ctx)
		if err != nil {
			logf("warn: vcs status failed: %v", err)
		} else {
			inv.vcsStatus = status
		}
	}

	inv.readiness = ReadinessLevel1
	logf("readiness level 1 — repo identity ready")

	// ── Level 2: full file index ──────────────────────────────────────────────
	listing, err := inv.fsProvider.ListFiles(ctx)
	if err != nil {
		return fmt.Errorf("warmup level 2: listing files: %w", err)
	}

	inv.fileListing = listing
	inv.readiness = ReadinessLevel2
	logf("readiness level 2 — file index ready (%d files)", listing.Data.TotalFiles)

	// ── Level 3: import graph (any language provider ready) ──────────────────
	if inv.multiProvider.HasAnyLanguageProvider() {
		inv.readiness = ReadinessLevel3
		if inv.multiProvider.GoplsReady() {
			logf("readiness level 3 — package graph + gopls ready")
		} else {
			logf("readiness level 3 — package graph ready (gopls still starting)")
		}
	}

	// Record warm timing.
	elapsed := time.Since(start)
	inv.warmDuration = elapsed
	now := time.Now()
	inv.lastWarmed = &now

	logf("warmup complete in %dms — readiness: %s", elapsed.Milliseconds(), inv.readiness)
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

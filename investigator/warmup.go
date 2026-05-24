package main

import (
	"context"
	"fmt"
	"time"
)

// Warm drives the investigator through readiness levels 1–3.
//
// Level 3 requires both the import graph and gopls to be ready. The mutex is
// released before waiting for gopls so that concurrent feature calls can
// proceed with Phase-1 data (import graph) during the LSP handshake.
//
// Warm is idempotent — subsequent calls return immediately when already at
// Level 3. It is safe to call concurrently (the idempotent check is guarded
// by the mutex).
func (inv *ProjectInvestigator) Warm(ctx context.Context) error {
	inv.mu.Lock()

	if inv.readiness >= ReadinessLevel3 {
		inv.mu.Unlock()
		return nil
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
		inv.mu.Unlock()
		return fmt.Errorf("warmup level 2: listing files: %w", err)
	}

	inv.fileListing = listing
	inv.readiness = ReadinessLevel2
	logf("readiness level 2 — file index ready (%d files)", listing.Data.TotalFiles)

	// ── Level 3: import graph + gopls ─────────────────────────────────────────
	// Release the lock before waiting for gopls. Feature calls at Level 2 can
	// proceed with the file index and import graph while the LSP handshake
	// completes. gopls typically takes 10–30 s; we cap at 90 s.
	hasLang := inv.multiProvider.HasAnyLanguageProvider()
	inv.mu.Unlock()

	if hasLang {
		goplsCtx, goplsCancel := context.WithTimeout(ctx, 90*time.Second)
		goplsOK := inv.multiProvider.WaitForGopls(goplsCtx)
		goplsCancel()

		inv.mu.Lock()
		inv.readiness = ReadinessLevel3
		if goplsOK {
			logf("readiness level 3 — package graph + gopls ready")
		} else {
			logf("readiness level 3 — package graph ready (gopls not ready within 90s)")
		}
		inv.mu.Unlock()
	}

	// Record warm timing.
	elapsed := time.Since(start)
	inv.mu.Lock()
	inv.warmDuration = elapsed
	now := time.Now()
	inv.lastWarmed = &now
	readiness := inv.readiness
	inv.mu.Unlock()

	logf("warmup complete in %dms — readiness: %s", elapsed.Milliseconds(), readiness)
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

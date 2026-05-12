// Package output renders SuitCode feature responses to either compact Markdown
// or strict JSON. All rendering happens after structured data is produced;
// no business logic lives here.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

// ──────────────────────────────────────────────────────────────────────────────
// JSON renderer
// ──────────────────────────────────────────────────────────────────────────────

// WriteJSON encodes v as indented JSON and writes it to w.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// ──────────────────────────────────────────────────────────────────────────────
// Markdown renderers — one per feature response type
// ──────────────────────────────────────────────────────────────────────────────

// WriteRepoOverview renders a RepoOverviewResponse as Markdown.
func WriteRepoOverview(w io.Writer, r *cfeatures.RepoOverviewResponse) error {
	b := &strings.Builder{}

	b.WriteString("# Repository Overview\n\n")

	// Languages
	if len(r.Languages) > 0 {
		b.WriteString("## Languages\n\n")
		for _, l := range r.Languages {
			fmt.Fprintf(b, "- %s\n", l)
		}
		b.WriteString("\n")
	}

	// Build systems
	if len(r.BuildSystems) > 0 {
		b.WriteString("## Build Systems\n\n")
		for _, bs := range r.BuildSystems {
			fmt.Fprintf(b, "- %s\n", bs)
		}
		b.WriteString("\n")
	}

	// Test systems
	if len(r.TestSystems) > 0 {
		b.WriteString("## Test Systems\n\n")
		for _, ts := range r.TestSystems {
			fmt.Fprintf(b, "- %s\n", ts)
		}
		b.WriteString("\n")
	}

	// Top-level structure
	if len(r.TopLevelStructure) > 0 {
		b.WriteString("## Top-Level Structure\n\n")
		for _, e := range r.TopLevelStructure {
			icon := "📄"
			if e.IsDir {
				icon = "📁"
			}
			if e.Notes != "" {
				fmt.Fprintf(b, "- %s `%s` — %s\n", icon, e.RelPath, e.Notes)
			} else {
				fmt.Fprintf(b, "- %s `%s`\n", icon, e.RelPath)
			}
		}
		b.WriteString("\n")
	}

	// Config files
	if len(r.ConfigFiles) > 0 {
		b.WriteString("## Configuration Files\n\n")
		for _, f := range r.ConfigFiles {
			fmt.Fprintf(b, "- `%s`\n", f.RelPath)
		}
		b.WriteString("\n")
	}

	// Stats
	fmt.Fprintf(b, "## Stats\n\n- Total files: %d\n\n", r.TotalFiles)

	// Limitations
	writeMarkdownLimitations(b, r.Limitations)

	// Metrics footer
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteExplainFile renders an ExplainFileResponse as Markdown.
func WriteExplainFile(w io.Writer, r *cfeatures.ExplainFileResponse) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "# File: `%s`\n\n", r.RelPath)
	fmt.Fprintf(b, "**Language:** %s  \n**Role:** %s  \n**Estimated tokens:** %d (est.)\n\n",
		r.Language, r.FileRole, r.FileTokenEstimate.Tokens)

	// Symbols
	if len(r.Symbols) > 0 {
		b.WriteString("## Symbols\n\n")
		for _, s := range r.Symbols {
			if s.Signature != "" {
				fmt.Fprintf(b, "- `%s` (%s) — `%s`\n", s.Name, s.Kind, s.Signature)
			} else {
				fmt.Fprintf(b, "- `%s` (%s)\n", s.Name, s.Kind)
			}
		}
		b.WriteString("\n")
	}

	// Imports
	if len(r.Imports) > 0 {
		b.WriteString("## Imports\n\n")
		for _, f := range r.Imports {
			fmt.Fprintf(b, "- `%s`\n", f.RelPath)
		}
		b.WriteString("\n")
	}

	// Related tests
	if len(r.RelatedTests) > 0 {
		b.WriteString("## Related Tests\n\n")
		for _, t := range r.RelatedTests {
			fmt.Fprintf(b, "- `%s`", t.RelPath)
			if t.RunCommand != "" {
				fmt.Fprintf(b, " — `%s`", t.RunCommand)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Related files
	if len(r.RelatedFiles) > 0 {
		b.WriteString("## Related Files\n\n")
		for _, f := range r.RelatedFiles {
			fmt.Fprintf(b, "- `%s`\n", f.RelPath)
		}
		b.WriteString("\n")
	}

	// Risks
	if len(r.RisksAndBoundaries) > 0 {
		b.WriteString("## Risks & Boundaries\n\n")
		for _, risk := range r.RisksAndBoundaries {
			fmt.Fprintf(b, "- %s\n", risk)
		}
		b.WriteString("\n")
	}

	writeMarkdownLimitations(b, r.Limitations)
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteContext renders a ContextResponse as Markdown.
func WriteContext(w io.Writer, r *cfeatures.ContextResponse) error {
	b := &strings.Builder{}

	b.WriteString("# Context Capsule\n\n")

	fmt.Fprintf(b, "**Budget:** %d tokens requested, %d used  \n",
		r.Capsule.BudgetRequested, r.Capsule.BudgetUsed)
	fmt.Fprintf(b, "**Files:** %d included / %d considered  \n",
		r.FilesIncluded, r.FilesConsidered)
	if r.CompressionRatio > 0 {
		fmt.Fprintf(b, "**Compression:** %.0f%% (estimated context avoided: ~%d tokens)\n\n",
			(1-r.CompressionRatio)*100, r.EstimatedContextAvoided.Tokens)
	}
	b.WriteString("\n")

	// Included files
	if len(r.Capsule.Selections) > 0 {
		b.WriteString("## Included Files\n\n")
		for _, sel := range r.Capsule.Selections {
			fmt.Fprintf(b, "### `%s`\n\n", sel.Candidate.File.RelPath)
			fmt.Fprintf(b, "> Reason: %s (score: %.2f, ~%d tokens)\n\n",
				sel.Reason, sel.Candidate.Score, sel.Candidate.TokenEstimate.Tokens)
		}
	}

	// Facts (file content)
	if len(r.Capsule.Facts) > 0 {
		b.WriteString("## Content\n\n")
		for _, fact := range r.Capsule.Facts {
			fmt.Fprintf(b, "### `%s`\n\n", fact.Source.RelPath)
			b.WriteString("```")
			if fact.Source.Language != "" {
				b.WriteString(strings.ToLower(fact.Source.Language))
			}
			b.WriteString("\n")
			b.WriteString(fact.Content)
			if !strings.HasSuffix(fact.Content, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("```\n\n")
		}
	}

	// Excluded files
	if len(r.Capsule.Rejections) > 0 {
		b.WriteString("## Excluded Files\n\n")
		for _, rej := range r.Capsule.Rejections {
			fmt.Fprintf(b, "- `%s` — %s\n", rej.Candidate.File.RelPath, rej.Reason)
		}
		b.WriteString("\n")
	}

	writeMarkdownLimitations(b, r.Limitations)
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteRelated renders a RelatedResponse as Markdown.
func WriteRelated(w io.Writer, r *cfeatures.RelatedResponse) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "# Related Files: `%s`\n\n", r.TargetPath)
	fmt.Fprintf(b, "Found %d related files (%d considered, %d excluded)\n\n",
		r.FilesIncluded, r.FilesConsidered, r.FilesExcluded)

	if len(r.RelatedFiles) > 0 {
		b.WriteString("| File | Relation | Confidence | Reason |\n")
		b.WriteString("|------|----------|------------|--------|\n")
		for _, rf := range r.RelatedFiles {
			fmt.Fprintf(b, "| `%s` | %s | %.0f%% | %s |\n",
				rf.File.RelPath, rf.Relation, rf.Confidence*100, rf.Reason)
		}
		b.WriteString("\n")
	}

	writeMarkdownLimitations(b, r.Limitations)
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteTests renders a TestsResponse as Markdown.
func WriteTests(w io.Writer, r *cfeatures.TestsResponse) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "# Relevant Tests: `%s`\n\n", r.TargetPath)
	fmt.Fprintf(b, "Selected %d / %d tests considered\n\n",
		r.TestsSelected, r.TestsConsidered)

	for _, rt := range r.RelevantTests {
		fmt.Fprintf(b, "- `%s` — %s", rt.Test.RelPath, rt.Reason)
		if rt.Test.RunCommand != "" {
			fmt.Fprintf(b, "\n  ```\n  %s\n  ```", rt.Test.RunCommand)
		}
		b.WriteString("\n")
	}
	if len(r.RelevantTests) > 0 {
		b.WriteString("\n")
	}

	writeMarkdownLimitations(b, r.Limitations)
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteImpact renders an ImpactResponse as Markdown.
func WriteImpact(w io.Writer, r *cfeatures.ImpactResponse) error {
	b := &strings.Builder{}

	b.WriteString("# Impact Analysis\n\n")

	if len(r.ChangedFiles) > 0 {
		b.WriteString("## Changed Files\n\n")
		for _, f := range r.ChangedFiles {
			fmt.Fprintf(b, "- `%s`\n", f.RelPath)
		}
		b.WriteString("\n")
	}

	if len(r.ImpactedFiles) > 0 {
		b.WriteString("## Impacted Files\n\n")
		for _, f := range r.ImpactedFiles {
			fmt.Fprintf(b, "- `%s` — %s\n", f.File.RelPath, f.Reason)
		}
		b.WriteString("\n")
	}

	if len(r.ImpactedTests) > 0 {
		b.WriteString("## Impacted Tests\n\n")
		for _, t := range r.ImpactedTests {
			fmt.Fprintf(b, "- `%s` — %s\n", t.Test.RelPath, t.Reason)
		}
		b.WriteString("\n")
	}

	if len(r.GeneratedWarnings) > 0 {
		b.WriteString("## ⚠ Generated File Warnings\n\n")
		for _, w := range r.GeneratedWarnings {
			fmt.Fprintf(b, "- %s\n", w)
		}
		b.WriteString("\n")
	}

	writeMarkdownLimitations(b, r.Limitations)
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteVerifyPlan renders a VerifyPlanResponse as Markdown.
func WriteVerifyPlan(w io.Writer, r *cfeatures.VerifyPlanResponse) error {
	b := &strings.Builder{}

	b.WriteString("# Verification Plan\n\n")

	required := filterCommands(r.Commands, true)
	optional := filterCommands(r.Commands, false)

	if len(required) > 0 {
		b.WriteString("## Required\n\n")
		for _, cmd := range required {
			fmt.Fprintf(b, "```\n%s %s\n```\n_%s_\n\n",
				cmd.Command, strings.Join(cmd.Args, " "), cmd.Reason)
		}
	}

	if len(optional) > 0 {
		b.WriteString("## Optional / Hygiene\n\n")
		for _, cmd := range optional {
			fmt.Fprintf(b, "```\n%s %s\n```\n_%s_\n\n",
				cmd.Command, strings.Join(cmd.Args, " "), cmd.Reason)
		}
	}

	writeMarkdownLimitations(b, r.Limitations)
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteFailureContext renders a FailureContextResponse as Markdown.
func WriteFailureContext(w io.Writer, r *cfeatures.FailureContextResponse) error {
	b := &strings.Builder{}

	b.WriteString("# Failure Context\n\n")

	if len(r.ParsedSignals) > 0 {
		b.WriteString("## Signals Extracted\n\n")
		for _, sig := range r.ParsedSignals {
			fmt.Fprintf(b, "- **%s**: `%s` (confidence: %.0f%%)\n",
				sig.Kind, sig.Value, sig.Confidence*100)
		}
		b.WriteString("\n")
	}

	if len(r.SuspectedFiles) > 0 {
		b.WriteString("## Suspected Files\n\n")
		for _, f := range r.SuspectedFiles {
			fmt.Fprintf(b, "- `%s`\n", f.RelPath)
		}
		b.WriteString("\n")
	}

	if len(r.SuspectedTests) > 0 {
		b.WriteString("## Suspected Tests\n\n")
		for _, t := range r.SuspectedTests {
			fmt.Fprintf(b, "- `%s`\n", t.Name)
		}
		b.WriteString("\n")
	}

	writeMarkdownLimitations(b, r.Limitations)
	writeMarkdownMetricsFooter(b, r.Metrics)

	_, err := io.WriteString(w, b.String())
	return err
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func writeMarkdownLimitations(b *strings.Builder, limitations []provider.Limitation) {
	if len(limitations) == 0 {
		return
	}
	b.WriteString("## ⚠ Limitations\n\n")
	for _, l := range limitations {
		fmt.Fprintf(b, "- **%s**: %s", l.Kind, l.Message)
		if l.Scope != "" {
			fmt.Fprintf(b, " _(scope: %s)_", l.Scope)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func writeMarkdownMetricsFooter(b *strings.Builder, m cfeatures.FeatureMetrics) {
	fmt.Fprintf(b, "---\n_Run `%s` · %dms · budget %d/%d · hash `%s`_\n",
		m.RunID,
		m.Timing.DurationMs,
		m.Budget.Used, m.Budget.Requested,
		shortHash(m.DeterministicHash),
	)
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func filterCommands(cmds []cfeatures.VerificationCommand, required bool) []cfeatures.VerificationCommand {
	var out []cfeatures.VerificationCommand
	for _, c := range cmds {
		if c.Required == required {
			out = append(out, c)
		}
	}
	return out
}

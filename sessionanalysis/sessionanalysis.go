// Package sessionanalysis analyses Claude Code session files to produce
// heuristic quality signals and analysis packs for LLM review.
//
// Session files live at:
//
//	~/.claude/projects/<hash>/<session-id>.jsonl
//
// where <hash> is the project path with ':', '\', '/', '.' all replaced by '-'.
// Each line is a JSON object describing one conversation event.
package sessionanalysis

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GreenFuze/SuitCode/core/config"
)

const (
	analysisFilePrefix    = "analysis-"
	maxSessions           = 5
	maxContentPreviewLen  = 300
	// Tools that represent file mutations in a Claude Code session.
)

// editToolNames is the set of Claude Code tool names that constitute file edits.
var editToolNames = map[string]bool{
	"Edit":              true,
	"Write":             true,
	"str_replace_editor": true,
	"create_file":       true,
	"NotebookEdit":      true,
}

// ──────────────────────────────────────────────────────────────────────────────
// Public types
// ──────────────────────────────────────────────────────────────────────────────

// SessionFile represents a discovered Claude Code session file.
type SessionFile struct {
	// Path is the absolute path to the JSONL file.
	Path string `json:"path"`
	// ModTime is the file's modification time.
	ModTime time.Time `json:"mod_time"`
	// SessionID is extracted from the filename (UUID without .jsonl extension).
	SessionID string `json:"session_id"`
}

// DateRange holds start and end timestamps.
type DateRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// ConversationTurn is a single turn included in the conversation excerpt
// surrounding a suitcode call.
type ConversationTurn struct {
	// Role is "user" or "assistant".
	Role string `json:"role"`
	// ContentPreview is the truncated text content of the turn.
	ContentPreview string `json:"content_preview,omitempty"`
	// Tool is the tool name when this turn represents a tool call.
	Tool string `json:"tool,omitempty"`
	// File is the file path for Edit/Write tool calls.
	File string `json:"file,omitempty"`
}

// HeuristicSignals contains indicators of how useful a suitcode call was,
// derived purely from observable session structure (no LLM required).
type HeuristicSignals struct {
	// EditToolUsedAfter is true when an Edit/Write tool was invoked after this
	// suitcode call and before the next suitcode call (or end of session).
	// Strong positive signal: the agent likely used the returned context.
	EditToolUsedAfter bool `json:"edit_tool_used_after"`

	// FilesEditedAfterContext lists the paths of files edited/written after
	// this suitcode call. Empty when EditToolUsedAfter is false.
	FilesEditedAfterContext []string `json:"files_edited_after_context,omitempty"`

	// SuitcodeRetryAfter is true when a subsequent suitcode call within the
	// session uses overlapping seed files. Suggests the first response may
	// have been insufficient.
	SuitcodeRetryAfter bool `json:"suitcode_retry_after"`

	// TurnsUntilNextEdit is the number of assistant turns between this
	// suitcode call and the first Edit/Write. -1 means no edit followed.
	TurnsUntilNextEdit int `json:"turns_until_next_edit"`

	// TurnsUntilNextSuitcode is the number of assistant turns until the next
	// suitcode call. -1 means this is the last suitcode call in the session.
	TurnsUntilNextSuitcode int `json:"turns_until_next_suitcode"`
}

// CallEntry pairs one suitcode CLI invocation with its surrounding evidence.
type CallEntry struct {
	// Command is the full suitcode CLI invocation as it appeared in the session.
	Command string `json:"command"`
	// Timestamp is the ISO-8601 timestamp from the session file.
	Timestamp string `json:"timestamp,omitempty"`
	// HeuristicSignals contains computed quality indicators.
	HeuristicSignals HeuristicSignals `json:"heuristic_signals"`
	// ConversationExcerpt is the 3 turns before + suitcode turn + 3 turns after.
	ConversationExcerpt []ConversationTurn `json:"conversation_excerpt"`
}

// AnalysisPack is the full analysis output saved to .suitcode/analysis-<ts>.json.
// It is designed to be fed to an LLM for a second-opinion quality assessment —
// it contains raw evidence, not bottom-line calculations.
type AnalysisPack struct {
	GeneratedAt        string      `json:"generated_at"`
	ProjectPath        string      `json:"project_path"`
	SessionFile        string      `json:"session_file"`
	SessionDateRange   DateRange   `json:"session_date_range"`
	TotalTurns         int         `json:"total_turns"`
	SuitcodeCallsFound int         `json:"suitcode_calls_found"`
	Calls              []CallEntry `json:"calls"`
	InstructionsForLLM string      `json:"instructions_for_llm"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Public API
// ──────────────────────────────────────────────────────────────────────────────

// FindSessionFiles returns Claude Code session files for the given repository,
// ordered by modification time (most recent first). At most maxSessions files
// are returned. Returns nil when no sessions are found.
func FindSessionFiles(repoPath string) ([]SessionFile, error) {
	sessionsDir, err := claudeProjectSessionsDir(repoPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionanalysis: read dir %q: %w", sessionsDir, err)
	}

	var sessions []SessionFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		sessions = append(sessions, SessionFile{
			Path:      filepath.Join(sessionsDir, entry.Name()),
			ModTime:   info.ModTime(),
			SessionID: strings.TrimSuffix(entry.Name(), ".jsonl"),
		})
	}

	// Most recent first.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})

	if len(sessions) > maxSessions {
		sessions = sessions[:maxSessions]
	}
	return sessions, nil
}

// AnalyzeSession parses the given session file, extracts all suitcode calls with
// their heuristic signals and conversation excerpts, and returns an AnalysisPack
// ready for saving and/or LLM review.
func AnalyzeSession(sf SessionFile, repoPath string) (*AnalysisPack, error) {
	entries, err := parseSessionFile(sf.Path)
	if err != nil {
		return nil, fmt.Errorf("sessionanalysis: parse %q: %w", sf.Path, err)
	}

	// Count user+assistant turns for the summary.
	totalTurns := 0
	for _, e := range entries {
		if e.entryType == "user" || e.entryType == "assistant" {
			totalTurns++
		}
	}

	calls := extractCalls(entries)

	return &AnalysisPack{
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		ProjectPath:        repoPath,
		SessionFile:        sf.Path,
		SessionDateRange:   computeDateRange(entries),
		TotalTurns:         totalTurns,
		SuitcodeCallsFound: len(calls),
		Calls:              calls,
		InstructionsForLLM: buildInstructionsForLLM(),
	}, nil
}

// SaveAnalysisPack writes the pack to .suitcode/analysis-<timestamp>.json and
// returns the absolute path of the saved file.
func SaveAnalysisPack(pack *AnalysisPack, repoPath string) (string, error) {
	stateDir, err := config.StateDirForRepo(repoPath)
	if err != nil {
		return "", fmt.Errorf("sessionanalysis: %w", err)
	}

	ts := time.Now().UTC().Format("20060102-150405")
	outputPath := filepath.Join(stateDir, analysisFilePrefix+ts+".json")

	data, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return "", fmt.Errorf("sessionanalysis: marshal: %w", err)
	}

	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		return "", fmt.Errorf("sessionanalysis: write %q: %w", outputPath, err)
	}

	return outputPath, nil
}

// FindLatestAnalysisPack returns the path to the most recently saved analysis
// pack for the given project, or an empty string when none exists.
func FindLatestAnalysisPack(repoPath string) (string, error) {
	stateDir, err := config.StateDirForRepo(repoPath)
	if err != nil {
		return "", fmt.Errorf("sessionanalysis: %w", err)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return "", nil // directory missing → no packs
	}

	// Collect matching filenames (timestamp-prefixed, so lex order = chrono order).
	var packs []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, analysisFilePrefix) && strings.HasSuffix(n, ".json") {
			packs = append(packs, filepath.Join(stateDir, n))
		}
	}

	if len(packs) == 0 {
		return "", nil
	}

	sort.Strings(packs)
	return packs[len(packs)-1], nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal session-file parsing
// ──────────────────────────────────────────────────────────────────────────────

// rawSessionEntry is the minimal wire shape of a Claude Code session JSONL line.
type rawSessionEntry struct {
	Type      string  `json:"type"`
	UUID      string  `json:"uuid"`
	Timestamp string  `json:"timestamp"`
	SessionID string  `json:"sessionId"`
	Message   *rawMsg `json:"message"`
}

type rawMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock is a single item in an assistant or user message content array.
type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	Name  string         `json:"name,omitempty"`
	ID    string         `json:"id,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

// parsedEntry is a normalised view of a user or assistant session entry.
type parsedEntry struct {
	entryType string
	timestamp string
	blocks    []contentBlock
	rawText   string // when message.content is a plain string rather than an array
}

// parseSessionFile reads a Claude Code session JSONL file and returns a slice of
// normalised user/assistant entries. Malformed lines and other entry types are
// silently skipped.
func parseSessionFile(path string) ([]parsedEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Use a generous scanner buffer — session files can contain large tool results.
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 4*1024*1024) // 4 MiB
	scanner.Buffer(buf, len(buf))

	var entries []parsedEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var raw rawSessionEntry
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		// We only need user and assistant turns.
		if raw.Type != "user" && raw.Type != "assistant" {
			continue
		}

		entry := parsedEntry{
			entryType: raw.Type,
			timestamp: raw.Timestamp,
		}

		if raw.Message != nil {
			entry.blocks, entry.rawText = parseMessageContent(raw.Message.Content)
		}

		entries = append(entries, entry)
	}

	return entries, scanner.Err()
}

// parseMessageContent handles both plain-string and array-of-blocks message content.
func parseMessageContent(raw json.RawMessage) (blocks []contentBlock, text string) {
	if len(raw) == 0 {
		return nil, ""
	}

	// Try array of content blocks first (assistant messages).
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks, ""
	}

	// Fall back to plain string (simple user messages).
	if err := json.Unmarshal(raw, &text); err == nil {
		return nil, text
	}

	return nil, ""
}

// computeDateRange returns the earliest and latest timestamps across all entries.
func computeDateRange(entries []parsedEntry) DateRange {
	var first, last string
	for _, e := range entries {
		if e.timestamp == "" {
			continue
		}
		if first == "" || e.timestamp < first {
			first = e.timestamp
		}
		if last == "" || e.timestamp > last {
			last = e.timestamp
		}
	}
	return DateRange{Start: first, End: last}
}

// ──────────────────────────────────────────────────────────────────────────────
// Call extraction and signal computation
// ──────────────────────────────────────────────────────────────────────────────

// extractCalls scans all entries for suitcode Bash/PowerShell tool calls and
// returns a CallEntry for each one, enriched with heuristic signals and a
// surrounding conversation excerpt.
func extractCalls(entries []parsedEntry) []CallEntry {
	var calls []CallEntry

	for i, entry := range entries {
		if entry.entryType != "assistant" {
			continue
		}

		for _, block := range entry.blocks {
			if block.Type != "tool_use" {
				continue
			}
			if block.Name != "Bash" && block.Name != "PowerShell" {
				continue
			}

			cmd := extractCommand(block.Input)
			if !isSuitcodeInvocation(cmd) {
				continue
			}

			calls = append(calls, CallEntry{
				Command:             cmd,
				Timestamp:           entry.timestamp,
				HeuristicSignals:    computeSignals(entries, i, cmd),
				ConversationExcerpt: buildExcerpt(entries, i),
			})
		}
	}

	return calls
}

// computeSignals inspects the entries that follow the suitcode call at
// suitcodeIdx and derives heuristic quality indicators.
func computeSignals(entries []parsedEntry, suitcodeIdx int, originalCmd string) HeuristicSignals {
	signals := HeuristicSignals{
		TurnsUntilNextEdit:     -1,
		TurnsUntilNextSuitcode: -1,
	}

	assistantTurns := 0

	for i := suitcodeIdx + 1; i < len(entries); i++ {
		if entries[i].entryType != "assistant" {
			continue
		}
		assistantTurns++

		for _, block := range entries[i].blocks {
			if block.Type != "tool_use" {
				continue
			}

			switch {
			// ── File-edit tools ───────────────────────────────────────────────
			case editToolNames[block.Name]:
				if !signals.EditToolUsedAfter {
					signals.EditToolUsedAfter = true
					signals.TurnsUntilNextEdit = assistantTurns
				}
				if path := extractFilePath(block.Input); path != "" {
					signals.FilesEditedAfterContext = appendUnique(
						signals.FilesEditedAfterContext, path,
					)
				}

			// ── Next suitcode call ────────────────────────────────────────────
			case (block.Name == "Bash" || block.Name == "PowerShell") &&
				isSuitcodeInvocation(extractCommand(block.Input)):
				if signals.TurnsUntilNextSuitcode < 0 {
					signals.TurnsUntilNextSuitcode = assistantTurns
					nextCmd := extractCommand(block.Input)
					signals.SuitcodeRetryAfter = detectRetry(originalCmd, nextCmd)
				}
				// Stop — don't attribute edits past the next suitcode call.
				return signals
			}
		}
	}

	return signals
}

// buildExcerpt returns up to 3 turns before + the suitcode turn + up to 3 turns
// after (bounded by the next suitcode call or end of session).
func buildExcerpt(entries []parsedEntry, suitcodeIdx int) []ConversationTurn {
	// Pre-context: up to 3 turns before.
	start := max(0, suitcodeIdx-3)

	// Post-context: up to 3 turns after (stop before next suitcode call).
	end := suitcodeIdx + 1
	for end < len(entries) && (end-suitcodeIdx) <= 3 {
		if end > suitcodeIdx && entries[end].entryType == "assistant" {
			if hasNextSuitcodeCall(entries[end]) {
				break
			}
		}
		end++
	}

	var turns []ConversationTurn
	for i := start; i < end; i++ {
		turn := entryToTurn(entries[i], i == suitcodeIdx)
		turns = append(turns, turn)
	}
	return turns
}

// hasNextSuitcodeCall reports whether the given entry contains a suitcode
// Bash/PowerShell invocation (used to stop excerpt expansion).
func hasNextSuitcodeCall(entry parsedEntry) bool {
	for _, block := range entry.blocks {
		if block.Type != "tool_use" {
			continue
		}
		if (block.Name == "Bash" || block.Name == "PowerShell") &&
			strings.Contains(extractCommand(block.Input), "suitcode") {
			return true
		}
	}
	return false
}

// entryToTurn converts a parsed session entry into a ConversationTurn suitable
// for the excerpt. Content is truncated to maxContentPreviewLen characters.
func entryToTurn(entry parsedEntry, isSuitcodeEntry bool) ConversationTurn {
	turn := ConversationTurn{Role: entry.entryType}

	// Plain-string user messages.
	if entry.rawText != "" {
		turn.ContentPreview = truncateStr(entry.rawText, maxContentPreviewLen)
		return turn
	}

	// Walk content blocks and build a representative summary.
	var textParts []string
	for _, block := range entry.blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}

		case "tool_use":
			if isSuitcodeEntry && (block.Name == "Bash" || block.Name == "PowerShell") {
				cmd := extractCommand(block.Input)
				if isSuitcodeInvocation(cmd) {
					// Show the exact command for the suitcode turn.
					turn.Tool = block.Name
					turn.ContentPreview = cmd
					return turn
				}
			}

			if editToolNames[block.Name] {
				turn.Tool = block.Name
				if path := extractFilePath(block.Input); path != "" {
					turn.File = path
					textParts = append(textParts, fmt.Sprintf("[%s: %s]", block.Name, path))
				} else {
					textParts = append(textParts, fmt.Sprintf("[%s]", block.Name))
				}
			} else {
				textParts = append(textParts, fmt.Sprintf("[%s]", block.Name))
			}
		}
	}

	turn.ContentPreview = truncateStr(strings.Join(textParts, " "), maxContentPreviewLen)
	return turn
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// claudeProjectSessionsDir returns the directory where Claude Code stores session
// JSONL files for the given repository.
func claudeProjectSessionsDir(repoPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("sessionanalysis: home dir: %w", err)
	}
	hash := pathToClaudeHash(repoPath)
	return filepath.Join(home, ".claude", "projects", hash), nil
}

// pathToClaudeHash converts a repository absolute path to Claude Code's
// project folder name by replacing ':', '\', '/', '.' with '-'.
//
// Example:
//
//	C:\src\github.com\GreenFuze\SuitCode  →  C--src-github-com-GreenFuze-SuitCode
func pathToClaudeHash(path string) string {
	r := strings.NewReplacer(":", "-", "\\", "-", "/", "-", ".", "-")
	return r.Replace(path)
}

// isSuitcodeInvocation returns true when cmd contains an actual suitcode binary
// invocation — "suitcode" is the first word of a shell command segment, not a
// path component, argument, or occurrence inside a string/commit message.
//
// It splits cmd on common shell operators (&&, ||, ;, \n) and checks whether
// any segment starts with "suitcode " (command with an argument) or equals
// "suitcode" alone.
func isSuitcodeInvocation(cmd string) bool {
	const token = "suitcode"
	for _, seg := range splitShellSegments(cmd) {
		seg = strings.TrimSpace(seg)
		if seg == token || strings.HasPrefix(seg, token+" ") {
			return true
		}
	}
	return false
}

// splitShellSegments splits a shell command string on &&, ||, ;, and newlines,
// returning the individual command segments. Used to identify which segment
// actually invokes the suitcode binary.
func splitShellSegments(cmd string) []string {
	// Normalise compound operators and split. We do not split on | (pipe) because
	// suitcode output may be piped: "suitcode . context ... | jq .".
	cmd = strings.ReplaceAll(cmd, "&&", "\n")
	cmd = strings.ReplaceAll(cmd, "||", "\n")
	cmd = strings.ReplaceAll(cmd, ";", "\n")
	return strings.Split(cmd, "\n")
}

// extractCommand pulls the "command" string from a Bash/PowerShell tool_use input map.
func extractCommand(input map[string]any) string {
	if input == nil {
		return ""
	}
	cmd, _ := input["command"].(string)
	return cmd
}

// extractFilePath pulls the file path from an Edit/Write tool_use input map.
// It tries "file_path" (Edit/Write) then "path" (alternative conventions).
func extractFilePath(input map[string]any) string {
	if input == nil {
		return ""
	}
	if p, ok := input["file_path"].(string); ok && p != "" {
		return p
	}
	if p, ok := input["path"].(string); ok && p != "" {
		return p
	}
	return ""
}

// detectRetry returns true when cmd2 uses any of the same seed files as cmd1,
// suggesting the agent retried a suitcode call with the same (or overlapping)
// seeds — a signal that the first response may have been insufficient.
func detectRetry(cmd1, cmd2 string) bool {
	seeds1 := extractSeedsFromCommand(cmd1)
	seeds2 := extractSeedsFromCommand(cmd2)
	if len(seeds1) == 0 || len(seeds2) == 0 {
		return false
	}

	// Compare by basename to handle relative vs absolute path variation.
	set := make(map[string]bool, len(seeds1))
	for _, s := range seeds1 {
		set[filepath.Base(strings.TrimSpace(s))] = true
	}
	for _, s := range seeds2 {
		if set[filepath.Base(strings.TrimSpace(s))] {
			return true
		}
	}
	return false
}

// extractSeedsFromCommand parses the --files flag value from a suitcode command.
func extractSeedsFromCommand(cmd string) []string {
	for _, prefix := range []string{"--files=", "--files "} {
		_, after, found := strings.Cut(cmd, prefix)
		if !found {
			continue
		}

		value := strings.TrimSpace(after)

		// Truncate at the next flag or end of string.
		if before, _, found := strings.Cut(value, " --"); found {
			value = before
		}

		var seeds []string
		for part := range strings.SplitSeq(value, ",") {
			if p := strings.TrimSpace(part); p != "" {
				seeds = append(seeds, p)
			}
		}
		return seeds
	}
	return nil
}

// appendUnique appends s to slice only when it is not already present.
func appendUnique(slice []string, s string) []string {
	if slices.Contains(slice, s) {
		return slice
	}
	return append(slice, s)
}

// truncateStr shortens s to at most n runes, appending "..." when truncated.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

// buildInstructionsForLLM returns the instructions string embedded in every
// AnalysisPack. These guide an LLM that receives the pack for quality analysis.
func buildInstructionsForLLM() string {
	return `You are analysing a SuitCode session pack. SuitCode is a repository
intelligence CLI that provides coding agents with bounded context capsules
(import graphs, related files, test mappings) to help them edit code more
accurately.

For each call in "calls":

1. COMMAND — what suitcode feature was invoked and with which seeds.

2. HEURISTIC SIGNALS — observable evidence about usefulness:
   - edit_tool_used_after: true = agent edited a file after the suitcode call
     (positive signal: context was likely used).
   - files_edited_after_context: which files were edited.
   - suitcode_retry_after: true = agent called suitcode again with the same
     seeds shortly after (negative signal: response may have been insufficient).
   - turns_until_next_edit: lower = faster path from context to action.
   - turns_until_next_suitcode: -1 = last call in session.

3. CONVERSATION_EXCERPT — surrounding conversation turns. The suitcode tool_use
   turn is the one with "tool": "Bash" or "tool": "PowerShell".

Your task:
a) For each call, estimate usefulness on a 1–5 scale and give a one-sentence reason.
b) Identify any recurring quality issues (limitations that appear repeatedly).
c) Estimate the overall helpful rate: "X of Y calls appear to have been useful (Z%)".
d) Recommend one concrete improvement if the helpful rate is below 80%.

Keep your response under 300 words.`
}

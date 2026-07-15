package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxFileBytes = 96 << 10 // cap file/dir output fed back to the model

// read_file ---------------------------------------------------------------

type readFile struct{}

func (readFile) Name() string { return "read_file" }
func (readFile) Description() string {
	return "Read a UTF-8 text file. Every line is prefixed with its 1-based line number as a 'N\\t' gutter — use those numbers with edit_lines for precise edits. The gutter is display-only: edit_file's old_string is the raw code WITHOUT it. The output ends with the total line count. For large files, pass offset (1-based line) and limit to read just a range."
}
func (readFile) Mutating() bool { return false }
func (readFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file, relative to the working directory."},"offset":{"type":"integer","description":"1-based line to start reading from (optional)."},"limit":{"type":"integer","description":"Maximum number of lines to read from offset (optional)."}},"required":["path"]}`)
}

func (readFile) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s is not a UTF-8 text file", p.Path)
	}
	if len(b) == 0 {
		return "(empty file)", nil
	}
	if p.Offset > 0 || p.Limit > 0 {
		// readRange already reports the total line count ("… of Z").
		return readRange(string(b), p.Offset, p.Limit), nil
	}
	lines := countLines(b) // total line count of the whole file, for length comparisons
	numbered := numberLines(string(b), 1)
	if len(numbered) > maxFileBytes {
		return numbered[:maxFileBytes] + fmt.Sprintf("\n… [%s total; truncated — call read_file with offset/limit to read the rest]", plural(lines, "line")), nil
	}
	return numbered + "\n[" + plural(lines, "line") + "]", nil
}

// numberLines prefixes each line of content with its 1-based number (the first
// line is startLine), as a "N\t" gutter, so the model can navigate and edit by
// line number. A file's final newline isn't counted as an extra empty line.
func numberLines(content string, startLine int) string {
	content = strings.TrimSuffix(content, "\n")
	var b strings.Builder
	for i, ln := range strings.Split(content, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\t%s", startLine+i, ln)
	}
	return b.String()
}

// gutterRE matches the "N\t" line-number gutter that read_file prepends, so
// edit_file can strip it if the model copies a numbered line into old_string.
var gutterRE = regexp.MustCompile(`(?m)^\d+\t`)

// countLines returns the number of lines in b (a trailing newline doesn't add an
// empty final line, matching how editors count).
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++ // last line has no trailing newline
	}
	return n
}

// plural formats a count with a singular/plural unit, e.g. "1 line" / "120 lines".
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// readRange returns lines [offset, offset+limit) (1-based; offset<=0 ⇒ 1,
// limit<=0 ⇒ to end), byte-capped, with a trailing note when the read is partial
// so the model knows there's more and how the lines are numbered.
func readRange(content string, offset, limit int) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n") // final newline isn't a line
	total := len(lines)
	start := offset - 1
	if start < 0 {
		start = 0
	}
	if start >= total {
		return fmt.Sprintf("(offset %d is past end of file — it has %d lines)", offset, total)
	}
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	out := truncate(numberLines(strings.Join(lines[start:end], "\n"), start+1), maxFileBytes)
	if start > 0 || end < total {
		out += fmt.Sprintf("\n[showing lines %d-%d of %d]", start+1, end, total)
	}
	return out
}

// list_dir ----------------------------------------------------------------

type listDir struct{}

func (listDir) Name() string        { return "list_dir" }
func (listDir) Description() string { return "List the entries of a directory (defaults to '.')." }
func (listDir) Mutating() bool      { return false }
func (listDir) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory path; defaults to the working directory."}}}`)
}

func (listDir) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Path == "" {
		p.Path = "."
	}
	entries, err := os.ReadDir(p.Path)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
			continue
		}
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		fmt.Fprintf(&b, "%s (%d bytes)\n", e.Name(), size)
	}
	return truncate(b.String(), maxFileBytes), nil
}

// write_file --------------------------------------------------------------

type writeFile struct{}

func (writeFile) Name() string { return "write_file" }
func (writeFile) Description() string {
	return "Create or overwrite a whole file with the given content. The result says whether it CREATED a new file or OVERWROTE an existing one (and the new line count) — prefer edit_file for changing part of an existing file rather than rewriting it."
}
func (writeFile) Mutating() bool { return true }
func (writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)
}

func (writeFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if err := guardPath(ctx, p.Path); err != nil {
		return "", err
	}
	// Flag create vs overwrite so the model (and the user) notice an accidental
	// full rewrite of an existing file, and capture the prior content for a diff.
	verb, old := "created", ""
	if b, err := os.ReadFile(p.Path); err == nil {
		verb, old = "overwrote", string(b)
	}
	if dir := filepath.Dir(p.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	summary := fmt.Sprintf("%s %s (%s)", verb, p.Path, plural(countLines([]byte(p.Content)), "line"))
	// Overwrite diffs against the prior file (shows the real change); a fresh create
	// diffs against the model's own content, so the diff is empty UNLESS the
	// formatter changed something — no whole-file-as-added noise, but the model
	// still sees any reformatting the harness applied.
	base := old
	if verb == "created" {
		base = p.Content
	}
	return finishEdit(ctx, p.Path, base, p.Content, summary)
}

// edit_file ---------------------------------------------------------------

type editFile struct{}

func (editFile) Name() string { return "edit_file" }
func (editFile) Description() string {
	return "Replace old_string with new_string in a file. Copy old_string verbatim from read_file; by default it must match a unique spot. If an exact match fails only on leading/trailing whitespace (e.g. tabs vs spaces) but the text is unambiguous, the edit is applied to that line range anyway. Set replace_all to change every occurrence. On success it returns a unified diff so you can confirm the edit landed as intended."
}
func (editFile) Mutating() bool { return true }
func (editFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string","description":"Exact text to replace, copied verbatim from the file."},"new_string":{"type":"string"},"replace_all":{"type":"boolean","description":"Replace every occurrence instead of requiring a unique match (default false)."}},"required":["path","old_string","new_string"]}`)
}

func (editFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if err := guardPath(ctx, p.Path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	content := string(b)
	count := strings.Count(content, p.OldString)
	if count == 0 {
		// The model may have copied a numbered read line ("42\tcode"); strip the
		// line-number gutter and retry before giving up.
		if stripped := gutterRE.ReplaceAllString(p.OldString, ""); stripped != p.OldString {
			if c := strings.Count(content, stripped); c >= 1 {
				p.OldString, count = stripped, c
			}
		}
	}
	// Whitespace-tolerant fallback: a model frequently gets indentation (tabs vs
	// spaces) slightly wrong, so an exact match fails even though the text is
	// unambiguously present. If old_string matches a UNIQUE line range ignoring each
	// line's leading/trailing whitespace, apply the edit there directly — the change
	// lands in one round-trip instead of bouncing an error back for the model to
	// re-guess bytes, and the post-edit formatter (when the project has one) settles
	// the indentation. Skipped for replace_all, which needs exact, possibly-multiple
	// matches. General: it's plain whitespace normalization, no language assumptions.
	if count == 0 && !p.ReplaceAll {
		if lo, hi, unique := locateFlexible(content, p.OldString); unique {
			updated := spliceLines(content, lo, hi, p.NewString)
			return finishEdit(ctx, p.Path, content, updated,
				fmt.Sprintf("edited %s (lines %d-%d, matched ignoring whitespace)", p.Path, lo, hi))
		}
	}
	switch {
	case count == 0:
		return "", editNotFound(content, p.OldString, p.Path)
	case count > 1 && !p.ReplaceAll:
		return "", fmt.Errorf("old_string appears %d times in %s — add surrounding context so it matches exactly one, or set replace_all to change every occurrence", count, p.Path)
	}
	n := 1
	if p.ReplaceAll {
		n = -1 // strings.Replace: n<0 replaces all
	}
	updated := strings.Replace(content, p.OldString, p.NewString, n)
	replaced := 1
	if p.ReplaceAll {
		replaced = count
	}
	return finishEdit(ctx, p.Path, content, updated, fmt.Sprintf("edited %s (replaced %s)", p.Path, plural(replaced, "occurrence")))
}

// editResult joins an edit's one-line summary with a unified diff of the change
// (when there is one), so the result SHOWS what changed — the model confirms the
// edit landed without re-reading, and the TUI renders the diff. The summary stays
// the first line so callers can split summary (display) from diff (preview).
func editResult(summary, oldContent, newContent, path string) string {
	if d := unifiedDiff(oldContent, newContent, path); d != "" {
		return summary + "\n" + d
	}
	return summary
}

// editNotFound builds a helpful "old_string not found" error. A small model most
// often fails an edit only on whitespace/indentation; when the text IS present
// ignoring whitespace and sits on a UNIQUE line range, point the model straight
// at edit_lines with that exact range (precise, no string matching) instead of
// sending it back to re-copy bytes it keeps getting wrong (the thrash that turned
// a one-line fix into dozens of failed edit attempts).
func editNotFound(content, old, path string) error {
	if lo, hi, unique := locateFlexible(content, old); unique {
		return fmt.Errorf("old_string not found in %s due to a whitespace/indentation mismatch (tabs vs spaces), but the text is at lines %d-%d. Use edit_lines with start_line=%d end_line=%d to replace that range precisely (no string matching), or re-read the file and copy the exact characters",
			path, lo, hi, lo, hi)
	}
	if collapseWS(old) != "" && strings.Contains(collapseWS(content), collapseWS(old)) {
		return fmt.Errorf("old_string not found in %s — the same text exists but with different whitespace/indentation (tabs vs spaces, or trailing spaces). Re-read the file with read_file and copy the exact characters", path)
	}
	return fmt.Errorf("old_string not found in %s — re-read the file with read_file and copy the exact current text (it may have changed, or never matched)", path)
}

// collapseWS reduces all runs of whitespace to a single space (and trims), for a
// whitespace-insensitive comparison.
func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// locateFlexible finds where old's lines appear in content ignoring each line's
// leading/trailing whitespace. It returns the 1-based inclusive line range and
// whether the match is UNIQUE — so a failed exact edit can be redirected to
// edit_lines with precise coordinates. lo/hi are meaningful only when unique.
func locateFlexible(content, old string) (lo, hi int, unique bool) {
	oldLines := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	for i := range oldLines {
		oldLines[i] = strings.TrimSpace(oldLines[i])
	}
	if len(oldLines) == 0 || (len(oldLines) == 1 && oldLines[0] == "") {
		return 0, 0, false
	}
	fileLines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	matches := 0
	for i := 0; i+len(oldLines) <= len(fileLines); i++ {
		ok := true
		for j := range oldLines {
			if strings.TrimSpace(fileLines[i+j]) != oldLines[j] {
				ok = false
				break
			}
		}
		if ok {
			matches++
			lo, hi = i+1, i+len(oldLines)
		}
	}
	return lo, hi, matches == 1
}

// edit_lines --------------------------------------------------------------

type editLines struct{}

func (editLines) Name() string { return "edit_lines" }
func (editLines) Description() string {
	return "Replace an inclusive 1-based line range [start_line, end_line] with new_text, using the line numbers shown by read_file. Precise and robust — no string matching. Line numbers shift after any edit, so read_file the file again before using this if you've changed it since. new_text may be multiple lines, or empty to delete the range. Returns a unified diff of the change."
}
func (editLines) Mutating() bool { return true }
func (editLines) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","description":"First line to replace (1-based, inclusive)."},"end_line":{"type":"integer","description":"Last line to replace (1-based, inclusive)."},"new_text":{"type":"string","description":"Replacement for those lines (may be multiple lines; empty to delete them)."}},"required":["path","start_line","end_line","new_text"]}`)
}

func (editLines) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Start   int    `json:"start_line"`
		End     int    `json:"end_line"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if err := guardPath(ctx, p.Path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	content := string(b)
	total := len(strings.Split(strings.TrimSuffix(content, "\n"), "\n"))
	if p.Start < 1 || p.End < p.Start {
		return "", fmt.Errorf("invalid line range %d-%d", p.Start, p.End)
	}
	if p.Start > total {
		return "", fmt.Errorf("start_line %d is past the end of %s (it has %d lines) — re-read the file", p.Start, p.Path, total)
	}
	end := p.End
	if end > total {
		end = total
	}
	updated := spliceLines(content, p.Start, end, p.NewText)
	newLineCount := 0
	if p.NewText != "" {
		newLineCount = strings.Count(strings.TrimSuffix(p.NewText, "\n"), "\n") + 1
	}
	return finishEdit(ctx, p.Path, content, updated,
		fmt.Sprintf("edited %s (lines %d-%d → %s)", p.Path, p.Start, end, plural(newLineCount, "line")))
}

// spliceLines replaces the inclusive 1-based line range [start, end] of content with
// newText and returns the result, preserving a trailing newline. end is clamped to
// the line count and newText may be empty (deletes the range). Shared by edit_lines
// and edit_file's whitespace-tolerant fallback so both splice identically.
func spliceLines(content string, start, end int, newText string) string {
	trailingNL := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if end > len(lines) {
		end = len(lines)
	}
	var newLines []string
	if newText != "" {
		newLines = strings.Split(strings.TrimSuffix(newText, "\n"), "\n")
	}
	result := make([]string, 0, len(lines)-(end-start+1)+len(newLines))
	result = append(result, lines[:start-1]...)
	result = append(result, newLines...)
	result = append(result, lines[end:]...)
	updated := strings.Join(result, "\n")
	if trailingNL {
		updated += "\n"
	}
	return updated
}

// maxDiffLines caps a returned/displayed diff so a big rewrite can't flood the
// model's context (it's re-sent every step) or the terminal. Beyond it the diff
// is cut with a marker; the file is already written regardless.
const maxDiffLines = 80

// unifiedDiff returns a compact, git-style unified diff between old and new for an
// edit result — a header, then one hunk: a few lines of context, the removed lines
// (-), and the added lines (+). It trims the common leading/trailing lines so only
// the changed region (plus context) shows. "" when old == new. This is what both
// the model (to confirm the edit) and the TUI (the preview) see.
func unifiedDiff(old, newContent, path string) string {
	if old == newContent {
		return ""
	}
	const ctxLines = 3
	ol := diffLines(old)
	nl := diffLines(newContent)
	pre := 0
	for pre < len(ol) && pre < len(nl) && ol[pre] == nl[pre] {
		pre++
	}
	suf := 0
	for suf < len(ol)-pre && suf < len(nl)-pre && ol[len(ol)-1-suf] == nl[len(nl)-1-suf] {
		suf++
	}
	oEnd, nEnd := len(ol)-suf, len(nl)-suf
	start := max(0, pre-ctxLines)
	oCtxEnd := min(len(ol), oEnd+ctxLines)
	nCtxEnd := min(len(nl), nEnd+ctxLines)

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", path, path)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", start+1, oCtxEnd-start, start+1, nCtxEnd-start)
	for i := start; i < pre; i++ {
		b.WriteString(" " + ol[i] + "\n")
	}
	for i := pre; i < oEnd; i++ {
		b.WriteString("-" + ol[i] + "\n")
	}
	for i := pre; i < nEnd; i++ {
		b.WriteString("+" + nl[i] + "\n")
	}
	for i := oEnd; i < oCtxEnd; i++ {
		b.WriteString(" " + ol[i] + "\n")
	}
	return capDiff(strings.TrimRight(b.String(), "\n"))
}

// diffLines splits content into lines, dropping the single trailing empty element
// a final newline would otherwise produce (so the diff has no phantom blank line).
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// capDiff bounds a diff to maxDiffLines lines with a truncation marker.
func capDiff(d string) string {
	lines := strings.Split(d, "\n")
	if len(lines) <= maxDiffLines {
		return d
	}
	return strings.Join(lines[:maxDiffLines], "\n") +
		fmt.Sprintf("\n… (+%d more diff lines)", len(lines)-maxDiffLines)
}

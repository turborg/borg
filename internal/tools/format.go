package tools

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// formatTimeout bounds a formatter run so a misbehaving tool can't hang an edit.
const formatTimeout = 15 * time.Second

// formatCmdEnv is the user/project OVERRIDE: a format command run after every edit
// (this is borg's equivalent of a Claude Code PostToolUse format hook). It wins
// over auto-detection. `{file}` is replaced with the edited path; if absent, the
// path is appended. Set via the `format` setting (settings.json) or the env var.
//
//	format = "vendor/bin/pint {file}"      # or: "gofmt -w {file}", "npx prettier -w {file}"
const formatCmdEnv = "BORG_FORMAT_CMD"

// autoFormat formats path in place after an edit and reports whether it changed.
// Resolution: (1) the `format` override if set; (2) else the formatter the PROJECT
// itself declares (a project-local binary like vendor/bin/pint, or a formatter
// config file like .prettierrc / pyproject.toml, with the tool resolvable); (3)
// else nothing. It is ALWAYS a safe no-op when no formatter applies or the
// formatter fails (e.g. the file isn't valid syntax yet mid-edit) — the model's
// content is left exactly as written for verify / the next edit to fix. borg never
// imposes a formatter the project didn't ask for, and never fails an edit because a
// formatter is missing.
func autoFormat(ctx context.Context, path string) bool {
	if tmpl := strings.TrimSpace(os.Getenv(formatCmdEnv)); tmpl != "" {
		cmdline := tmpl
		if strings.Contains(tmpl, "{file}") {
			cmdline = strings.ReplaceAll(tmpl, "{file}", shellQuote(path))
		} else {
			cmdline += " " + shellQuote(path)
		}
		return applyFormat(ctx, path, "sh", []string{"-c", cmdline})
	}
	if bin, args, ok := detectFormatter(path); ok {
		return applyFormat(ctx, path, bin, append(args, path))
	}
	return false
}

// detectFormatter returns the command for the formatter the PROJECT declares for
// path's type, or ok=false if none is evident/installed. Every non-Go formatter is
// gated on EVIDENCE that the project actually uses it — a project-local install or
// a formatter config file — so borg runs the project's chosen tool, not a guess.
// (gofmt is the one exception: it's the canonical, toolchain-bundled Go formatter,
// so a .go file is its own evidence.) This is a small, extensible set of the major
// ecosystems, NOT an attempt to enumerate every language — anything else is handled
// by the `format` override above.
//
// FORMATTERS ONLY — tools that deterministically rewrite style/whitespace. Linters
// are deliberately excluded: pure ones (flake8, golangci-lint, phpstan) only report,
// and linter auto-fixers can make SEMANTIC changes that must never be applied
// silently after an edit. The compile/error space is covered by `verify`.
func detectFormatter(path string) (bin string, args []string, ok bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		if p := resolveBin("", "gofmt"); p != "" {
			return p, []string{"-w"}, true
		}
	case ".php":
		if p := resolveBin("vendor/bin/pint", "pint"); p != "" && anyExists("vendor/bin/pint", "pint.json", ".pint.json") {
			return p, nil, true
		}
		if p := resolveBin("vendor/bin/php-cs-fixer", "php-cs-fixer"); p != "" && anyExists("vendor/bin/php-cs-fixer", ".php-cs-fixer.php", ".php-cs-fixer.dist.php") {
			return p, []string{"fix", "-q"}, true
		}
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".json", ".css", ".scss",
		".less", ".html", ".vue", ".md", ".yaml", ".yml", ".graphql":
		if p := resolveBin("node_modules/.bin/prettier", "prettier"); p != "" && prettierConfigured() {
			return p, []string{"--write", "--log-level", "silent"}, true
		}
	case ".py":
		if anyExists("pyproject.toml", "setup.cfg", "ruff.toml", ".ruff.toml", ".black", "tox.ini") {
			if p := resolveBin("", "ruff"); p != "" {
				return p, []string{"format", "-q"}, true
			}
			if p := resolveBin("", "black"); p != "" {
				return p, []string{"-q"}, true
			}
		}
	case ".rs":
		if anyExists("Cargo.toml", "rustfmt.toml", ".rustfmt.toml") {
			if p := resolveBin("", "rustfmt"); p != "" {
				return p, nil, true
			}
		}
	case ".rb":
		// rubocop is a linter+formatter; use -a (SAFE autocorrect only — layout/style),
		// never -A (which applies semantic-changing fixes). Auto-run stays formatting.
		if p := resolveBin("bin/rubocop", "rubocop"); p != "" && anyExists("bin/rubocop", ".rubocop.yml", ".rubocop.yaml") {
			return p, []string{"-a"}, true
		}
	}
	return "", nil, false
}

// prettierConfigured reports whether the project declares Prettier — a local
// install, a prettier config file, or a "prettier" key in package.json.
func prettierConfigured() bool {
	if anyExists("node_modules/.bin/prettier", ".prettierrc", ".prettierrc.json", ".prettierrc.yaml",
		".prettierrc.yml", ".prettierrc.js", ".prettierrc.cjs", ".prettierrc.mjs",
		"prettier.config.js", "prettier.config.cjs", "prettier.config.mjs") {
		return true
	}
	if b, err := os.ReadFile("package.json"); err == nil && strings.Contains(string(b), "\"prettier\"") {
		return true
	}
	return false
}

// applyFormat runs a formatter over path and reports whether the file changed. A
// formatter error (e.g. the file isn't valid syntax yet) leaves the file untouched.
func applyFormat(ctx context.Context, path, bin string, args []string) bool {
	before, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	runCtx, cancel := context.WithTimeout(ctx, formatTimeout)
	defer cancel()
	if err := exec.CommandContext(runCtx, bin, args...).Run(); err != nil {
		return false
	}
	after, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return !bytes.Equal(before, after)
}

// resolveBin returns the executable to use: a project-local binary (relative to the
// working dir) if it exists, else the binary on PATH, else "".
func resolveBin(local, bin string) string {
	if local != "" {
		if st, err := os.Stat(local); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(local); err == nil {
				return abs
			}
		}
	}
	if bin != "" {
		if p, err := exec.LookPath(bin); err == nil {
			return p
		}
	}
	return ""
}

// anyExists reports whether any of the given paths exists relative to the working dir.
func anyExists(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// shellQuote single-quotes s for safe use in an `sh -c` command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// finishEdit writes an edit's new content, auto-formats it, and returns the result
// the model sees: a one-line summary plus a unified diff. baseForDiff is what the
// diff is computed against — the file's PRIOR content for an in-place edit/overwrite
// (so the diff shows the real change, formatting included), or the model's own
// just-written content for a fresh create (so the diff is empty unless the
// formatter changed something, avoiding whole-file noise). When formatting alters
// the bytes, the summary says so, so the model knows not to re-edit for whitespace.
func finishEdit(ctx context.Context, path, baseForDiff, writeContent, summary string) (string, error) {
	if err := os.WriteFile(path, []byte(writeContent), 0o644); err != nil {
		return "", err
	}
	final := writeContent
	if autoFormat(ctx, path) {
		if b, err := os.ReadFile(path); err == nil {
			final = string(b)
			summary += " (auto-formatted)"
		}
	}
	return editResult(summary, baseForDiff, final, path), nil
}

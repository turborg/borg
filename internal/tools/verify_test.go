package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeTools makes lookPath report exactly the named tools as installed, so the
// language-detection branches can be exercised without depending on the CI
// image actually having php/node/ruby/python. Restores the real lookPath.
func fakeTools(t *testing.T, installed ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, n := range installed {
		set[n] = true
	}
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = orig })
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
}

// No tool may advertise an empty `properties` object. An empty JSON `{}` is turned
// into an empty array (`[]`) by the metered proxy's PHP json round-trip, which the
// guided-decoding grammar compiler rejects ("properties must be an object") — and
// because Floko runs under tool_choice=required, that breaks EVERY turn. Guard it
// so a no-arg tool can never silently reintroduce the regression.
func TestNoToolHasEmptyProperties(t *testing.T) {
	for _, d := range DefaultRegistry().Definitions() {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(d.Function.Parameters, &s), "tool %q has invalid schema", d.Function.Name)
		if s.Properties != nil {
			require.NotEmptyf(t, s.Properties,
				"tool %q advertises an empty properties object; use {\"type\":\"object\"} for a no-arg tool", d.Function.Name)
		}
	}
}

// detectVerify selects the right safe check per language — by source extension and
// toolchain availability — and stays nil when the toolchain is absent or nothing
// is recognized.
func TestDetectVerifyLanguages(t *testing.T) {
	t.Run("python by .py + interpreter", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"app.py": "print(1)\n"})
		t.Chdir(dir)
		fakeTools(t, "python3")
		c := detectVerify()
		require.NotNil(t, c)
		require.Equal(t, []string{"python3", "-m", "compileall", "-q", "."}, c.whole)
	})

	t.Run("python falls back to python when python3 absent", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"app.py": "print(1)\n"})
		t.Chdir(dir)
		fakeTools(t, "python") // only the unversioned interpreter is installed
		require.Equal(t, "python", pythonExe())
		c := detectVerify()
		require.NotNil(t, c)
		require.Equal(t, []string{"python", "-m", "compileall", "-q", "."}, c.whole)
	})

	t.Run("php is per-file lint", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"index.php": "<?php echo 1;\n"})
		t.Chdir(dir)
		fakeTools(t, "php")
		c := detectVerify()
		require.NotNil(t, c)
		require.Nil(t, c.whole)
		require.Equal(t, "php -l", c.label)
		require.Equal(t, []string{"php", "-l", "x.php"}, c.fileCmd("x.php"))
	})

	t.Run("javascript by .mjs", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"m.mjs": "export const x = 1\n"})
		t.Chdir(dir)
		fakeTools(t, "node")
		c := detectVerify()
		require.NotNil(t, c)
		require.Equal(t, "node --check", c.label)
		require.True(t, c.match("a.cjs"))
	})

	t.Run("ruby by .rb", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"app.rb": "puts 1\n"})
		t.Chdir(dir)
		fakeTools(t, "ruby")
		require.Equal(t, "ruby -c", detectVerify().label)
	})

	t.Run("source present but toolchain missing → nil", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"app.py": "print(1)\n"})
		t.Chdir(dir)
		fakeTools(t /* nothing installed */)
		require.Nil(t, detectVerify())
		require.False(t, VerifyApplicable())
	})

	t.Run("unrecognized project → nil", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"notes.txt": "hi\n"})
		t.Chdir(dir)
		fakeTools(t, "php", "node", "ruby", "python3")
		require.Nil(t, detectVerify())
	})
}

// runPerFileCheck parses each matching file, aggregates failures, skips dependency
// dirs, and reports a clean PASS when there's nothing to check. Uses gofmt -e
// (present wherever Go is) as a stand-in syntax parser so the runner is covered
// without needing php/node/ruby in the test image.
func TestRunPerFileCheck(t *testing.T) {
	gofmtCheck := &verifyCheck{
		label: "gofmt -e", match: extMatch(".src"),
		fileCmd: func(p string) []string { return []string{"gofmt", "-e", p} },
	}
	ctx := context.Background()

	t.Run("all parse → PASS, dependency dirs skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"a.src":                   "package a\n",
			"node_modules/broken.src": "this is } not go {", // must be skipped, not fail the run
		})
		t.Chdir(dir)
		out, err := runPerFileCheck(ctx, gofmtCheck)
		require.NoError(t, err)
		require.Contains(t, out, "PASS")
		require.Contains(t, out, "1 files") // the node_modules file was not counted
	})

	t.Run("a syntax error → FAIL", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"bad.src": "package a\nfunc ( {\n"})
		t.Chdir(dir)
		out, err := runPerFileCheck(ctx, gofmtCheck)
		require.NoError(t, err)
		require.Contains(t, out, "FAIL")
	})

	t.Run("no matching files → PASS", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"only.txt": "x\n"})
		t.Chdir(dir)
		out, err := runPerFileCheck(ctx, gofmtCheck)
		require.NoError(t, err)
		require.Contains(t, out, "no files")
	})
}

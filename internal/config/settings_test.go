package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// resetShadow clears the package-level shadow map between tests (LoadSettingsFile
// accumulates into it) and restores it afterward.
func resetShadow(t *testing.T) {
	t.Helper()
	prev := shadowed
	shadowed = map[string]bool{}
	t.Cleanup(func() { shadowed = prev })
}

// resetInjected clears the package-level injected map between tests (LoadSettingsFile
// and SetSetting accumulate into it) and restores it afterward.
func resetInjected(t *testing.T) {
	t.Helper()
	prev := injected
	injected = map[string]bool{}
	t.Cleanup(func() { injected = prev })
}

// unsetEnv schedules cleanup of vars our code sets via os.Setenv (not t.Setenv, so
// the framework won't restore them).
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, k := range keys {
			_ = os.Unsetenv(k)
		}
	})
}

func writeSettings(t *testing.T, dir string, m map[string]any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "borg"), 0o755))
	data, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "borg", "settings.json"), data, 0o600))
}

// LoadSettingsFile fills BORG_* from settings.json for unset vars; an explicit
// export wins and is recorded as shadowed; bools become "true"/"false".
func TestLoadSettingsFile(t *testing.T) {
	resetShadow(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeSettings(t, dir, map[string]any{
		"git_attribution": false,
		"escalate_model":  "axiom",
		"force_device":    true,
	})

	// escalate_model is set explicitly → the export must win and be flagged shadowed.
	t.Setenv("BORG_ESCALATE_MODEL", "")
	_ = os.Unsetenv("BORG_GIT_ATTRIBUTION")
	_ = os.Unsetenv("BORG_FORCE_DEVICE")
	unsetEnv(t, "BORG_GIT_ATTRIBUTION", "BORG_FORCE_DEVICE")

	LoadSettingsFile()

	require.Equal(t, "false", os.Getenv("BORG_GIT_ATTRIBUTION")) // filled from file
	require.Equal(t, "true", os.Getenv("BORG_FORCE_DEVICE"))     // bool true → "true"
	require.Equal(t, "", os.Getenv("BORG_ESCALATE_MODEL"))       // export wins
	require.True(t, IsShadowed("BORG_ESCALATE_MODEL"))
	require.False(t, IsShadowed("BORG_GIT_ATTRIBUTION"))
}

func TestLoadSettingsFileMissingIsNoop(t *testing.T) {
	resetShadow(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no settings.json present
	require.NotPanics(t, LoadSettingsFile)
}

// SubprocessEnv strips the vars the harness injected from settings.json (so a child
// process sees the user's real shell env) but keeps real exports and other env vars.
func TestSubprocessEnvStripsInjectedVars(t *testing.T) {
	resetShadow(t)
	resetInjected(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeSettings(t, dir, map[string]any{"force_device": true})

	_ = os.Unsetenv("BORG_FORCE_DEVICE") // unset → LoadSettingsFile injects it
	unsetEnv(t, "BORG_FORCE_DEVICE")
	LoadSettingsFile()
	require.Equal(t, "true", os.Getenv("BORG_FORCE_DEVICE")) // harness injected it

	env := SubprocessEnv()
	require.NotNil(t, env)
	var hasPath bool
	for _, kv := range env {
		require.False(t, strings.HasPrefix(kv, "BORG_FORCE_DEVICE="), "injected var must be stripped: %s", kv)
		if strings.HasPrefix(kv, "PATH=") {
			hasPath = true
		}
	}
	require.True(t, hasPath, "real shell vars must be preserved")
}

// With nothing injected, SubprocessEnv returns nil so the caller inherits the
// environment unchanged.
func TestSubprocessEnvNilWhenNothingInjected(t *testing.T) {
	resetInjected(t)
	require.Nil(t, SubprocessEnv())
}

// SetSetting persists to settings.json, updates the env when not shadowed, and
// preserves keys it doesn't manage.
func TestSetSettingPersistsAndPreservesUnknownKeys(t *testing.T) {
	resetShadow(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeSettings(t, dir, map[string]any{"some_future_key": "keep me"})
	_ = os.Unsetenv("BORG_GIT_ATTRIBUTION")
	unsetEnv(t, "BORG_GIT_ATTRIBUTION")

	norm, shadow, err := SetSetting("git_attribution", "off")
	require.NoError(t, err)
	require.Equal(t, "false", norm)
	require.False(t, shadow)
	require.Equal(t, "false", os.Getenv("BORG_GIT_ATTRIBUTION")) // env updated live

	m, err := readSettingsFile()
	require.NoError(t, err)
	require.Equal(t, "false", m["git_attribution"])
	require.Equal(t, "keep me", m["some_future_key"]) // unrelated key survives
}

// A shadowed var is still written to the file but the live env is left untouched.
func TestSetSettingShadowedLeavesEnv(t *testing.T) {
	resetShadow(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("BORG_ESCALATE_MODEL", "axiom")
	LoadSettingsFile() // records BORG_ESCALATE_MODEL as shadowed

	norm, shadow, err := SetSetting("escalate_model", "off")
	require.NoError(t, err)
	require.Equal(t, "", norm)
	require.True(t, shadow)
	require.Equal(t, "axiom", os.Getenv("BORG_ESCALATE_MODEL")) // export untouched

	m, err := readSettingsFile()
	require.NoError(t, err)
	require.Equal(t, "", m["escalate_model"]) // file still records the intent
}

func TestSetSettingValidates(t *testing.T) {
	resetShadow(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, _, err := SetSetting("nope", "x")
	require.Error(t, err) // unknown key

	_, _, err = SetSetting("escalate_model", "gpt5")
	require.Error(t, err) // not in the enum

	_, _, err = SetSetting("git_attribution", "maybe")
	require.Error(t, err) // not a bool
}

func TestNormalize(t *testing.T) {
	enum, _ := SettingByKey("escalate_model")
	got, err := enum.Normalize("off")
	require.NoError(t, err)
	require.Equal(t, "", got)
	got, err = enum.Normalize("axiom")
	require.NoError(t, err)
	require.Equal(t, "axiom", got)

	b, _ := SettingByKey("git_attribution")
	got, err = b.Normalize("ON")
	require.NoError(t, err)
	require.Equal(t, "true", got)
}

func TestNormalizeString(t *testing.T) {
	s := Setting{Key: "x", Kind: KindString}
	got, err := s.Normalize("  hello world  ")
	require.NoError(t, err)
	require.Equal(t, "hello world", got)
	require.Equal(t, "hello world", s.DisplayValue("hello world"))
}

func TestNormalizeInt(t *testing.T) {
	s, ok := SettingByKey("learn_stale_after")
	require.True(t, ok)
	require.Equal(t, KindInt, s.Kind)

	for _, in := range []string{"off", "OFF", "none", "0", ""} {
		got, err := s.Normalize(in)
		require.NoError(t, err)
		require.Equal(t, "0", got, "input %q", in)
	}
	got, err := s.Normalize("50")
	require.NoError(t, err)
	require.Equal(t, "50", got)

	for _, bad := range []string{"-1", "banana", "1.5"} {
		_, err := s.Normalize(bad)
		require.Error(t, err, "input %q", bad)
	}

	require.Equal(t, "off", s.DisplayValue("0")) // 0 shows as off
	require.Equal(t, "25", s.DisplayValue("25"))
}

func TestLearnStaleThreshold(t *testing.T) {
	s, _ := SettingByKey("learn_stale_after")
	unsetEnv(t, s.Env)
	require.Equal(t, 25, LearnStaleThreshold()) // built-in default
	t.Setenv(s.Env, "0")
	require.Equal(t, 0, LearnStaleThreshold()) // disabled
	t.Setenv(s.Env, "40")
	require.Equal(t, 40, LearnStaleThreshold())
}

func TestDisplayAndNextEnum(t *testing.T) {
	b, _ := SettingByKey("git_attribution")
	require.Equal(t, "on", b.DisplayValue("true"))
	require.Equal(t, "off", b.DisplayValue("false"))

	e, _ := SettingByKey("escalate_model")
	require.Equal(t, "off", e.DisplayValue(""))
	require.Equal(t, "axiom", e.NextEnum(""))
	require.Equal(t, "", e.NextEnum("axiom")) // wraps
}

func TestEffectiveFallsBackToDefault(t *testing.T) {
	s, _ := SettingByKey("git_attribution")
	_ = os.Unsetenv(s.Env)
	unsetEnv(t, s.Env)
	require.Equal(t, s.Default, s.Effective())
	t.Setenv(s.Env, "false")
	require.Equal(t, "false", s.Effective())
}

func TestSettingByKeyUnknown(t *testing.T) {
	_, ok := SettingByKey("does_not_exist")
	require.False(t, ok)
}

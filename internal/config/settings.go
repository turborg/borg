package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SettingKind classifies how a Setting is edited and validated in the /settings
// UI (and the `borg settings` CLI).
type SettingKind int

const (
	// KindString is a free-form text value (e.g. an attribution name).
	KindString SettingKind = iota
	// KindBool is an on/off toggle, stored as a JSON bool.
	KindBool
	// KindEnum is one of a fixed set of values (Enum); the empty string means "off".
	KindEnum
	// KindInt is a non-negative integer where 0 (also "off"/"none") means disabled —
	// edited as free text like a string, but validated as a number.
	KindInt
)

// Setting describes one user-tweakable knob: the bridge between a friendly key
// (persisted in settings.json and typed in /settings) and the BORG_* env var that
// actually drives the runtime config. The registry below is the SINGLE source of
// truth for what's editable — it powers the /settings picker, the `borg settings`
// CLI, validation, and the on-disk load/save, so the three can't drift.
type Setting struct {
	Key     string      // friendly key, persisted in settings.json and typed in /settings
	Env     string      // the BORG_* env var it maps to
	Label   string      // human label for the UI
	Kind    SettingKind // how it's edited/validated
	Enum    []string    // allowed values when Kind == KindEnum ("" = off, listed first)
	Default string      // value when neither an env export nor the file sets it
	Desc    string      // one-line explanation
	Hot     bool        // applies to a running agent immediately (vs only on restart)
}

// Settings is the registry of user-tweakable settings.
//
// Which backend borg talks to IS a user setting: `provider`, `base_url` and
// `model` are registered here so pointing borg at your own Ollama/OpenAI/
// OpenRouter/whatever endpoint is a first-class, persistent choice rather than an
// export you have to remember. (This reverses an earlier stance that endpoint
// knobs were a footgun to be kept out — that was written when the hosted proxy was
// the only backend there was. Bring-your-own is now the feature, so the knobs are
// part of the product.) The xShellz-account vars (BORG_API_BASE_URL, BORG_APP_URL,
// BORG_OAUTH_CLIENT_ID) do stay out: those are set by `borg auth login` from the
// token's own environment, so editing them by hand only ever desyncs them from the
// stored token.
//
// The API key is NOT here, and must never be added — see secretEnv. `api_key_env`
// holds the NAME of the env var to read the key from; the key itself only ever
// lives in the environment.
var Settings = []Setting{
	{
		Key: "provider", Env: "BORG_PROVIDER", Label: "Model provider",
		Kind: KindEnum, Enum: Providers, Default: ProviderXShellz,
		Desc: "which OpenAI-compatible backend to use: xshellz (hosted + metered) or your own",
		Hot:  false, // the LLM client is built at startup from this
	},
	{
		Key: "base_url", Env: "BORG_BASE_URL", Label: "Provider base URL",
		Kind: KindString, Default: "",
		Desc: "OpenAI-compatible API root incl. /v1 (e.g. http://localhost:11434/v1); empty = the provider's default",
		Hot:  false,
	},
	{
		Key: "api_key_env", Env: "BORG_API_KEY_ENV", Label: "API-key env var",
		Kind: KindString, Default: "",
		Desc: "NAME of the env var holding the provider's API key (e.g. OPENAI_API_KEY) — the key itself is never stored on disk",
		Hot:  false,
	},
	{
		Key: "model", Env: "BORG_MODEL", Label: "Default model",
		Kind: KindString, Default: "chuppa",
		Desc: "model new sessions start on (/model switches the current session)",
		Hot:  false,
	},
	{
		Key: "context", Env: "BORG_CONTEXT", Label: "Context window override",
		Kind: KindInt, Default: "0",
		Desc: "your model's context window in tokens; off = auto (the catalog, or a conservative guess on a backend that serves none)",
		Hot:  false,
	},
	{
		Key: "escalate_model", Env: "BORG_ESCALATE_MODEL", Label: "Auto-escalate model",
		Kind: KindEnum, Enum: []string{"", "axiom"}, Default: "",
		Desc: "tier up to a stronger model once a task keeps struggling (off = never, no surprise spend)",
		Hot:  true,
	},
	{
		Key: "think", Env: "BORG_THINK", Label: "Reasoning by default",
		Kind: KindBool, Default: "false",
		Desc: "start new sessions with reasoning on (per-session /think still wins)",
		Hot:  true,
	},
	{
		Key: "git_attribution", Env: "BORG_GIT_ATTRIBUTION", Label: "Git attribution",
		Kind: KindBool, Default: "true",
		Desc: "add a Turborg Co-Authored-By trailer to commits borg makes",
		Hot:  true,
	},
	{
		Key: "force_device", Env: "BORG_FORCE_DEVICE", Label: "Force device login",
		Kind: KindBool, Default: "false",
		Desc: "skip the browser/PKCE flow and use the device flow",
		Hot:  false,
	},
	{
		Key: "debug", Env: "BORG_DEBUG_ENABLED", Label: "Debug diagnostics",
		Kind: KindBool, Default: "false",
		Desc: "verbose tool / LLM / HTTP traces",
		Hot:  true,
	},
	{
		Key: "learn_stale_after", Env: "BORG_LEARN_STALE_AFTER", Label: "BORG.md staleness nudge",
		Kind: KindInt, Default: "25",
		Desc: "git repos only: remind to run /learn once BORG.md is this many commits behind HEAD (off = never)",
		Hot:  true,
	},
	{
		Key: "format", Env: "BORG_FORMAT_CMD", Label: "Format-on-edit command",
		Kind: KindString, Default: "",
		Desc: "override the formatter run after each edit ({file} = path); empty = auto-detect the project's own formatter",
		Hot:  true,
	},
}

// secretEnv reports whether env names a CREDENTIAL, which settings.json must
// never hold. borg's headline claim is that no provider key sits on your machine;
// that stays literally true only if there is no code path that writes one to disk
// or reads one back. The registry above deliberately has no key entry, so this is
// belt-and-braces — but it's enforced rather than assumed, because "a key leaked
// into a config file" is exactly the kind of thing a well-meaning later edit to
// the registry would cause. A key is env-only, always; `api_key_env` names the
// variable to read it from.
func secretEnv(env string) bool {
	return env == EnvAPIKey || env == EnvAccessToken
}

// SettingByKey looks up a setting by its friendly key.
func SettingByKey(key string) (Setting, bool) {
	for _, s := range Settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// SettingsFilePath is the JSON settings file, ~/.config/borg/settings.json.
func SettingsFilePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "borg", "settings.json")
}

// shadowed records which BORG_* vars were already present in the real process
// environment at startup, BEFORE LoadSettingsFile filled any from disk. Such an
// explicit export always wins over the file, so /settings warns that a saved
// change won't take effect until the export is unset.
var shadowed = map[string]bool{}

// injected records the env vars the harness set ITSELF from the settings file (or a
// runtime /settings change) — i.e. those that were NOT an explicit shell export.
// SubprocessEnv strips exactly these from commands the harness spawns, so a child
// process sees the user's real shell environment. See SubprocessEnv.
var injected = map[string]bool{}

// SubprocessEnv returns the environment to use for commands the harness runs on the
// user's behalf (builds, tests, the bash tool): the current process environment with
// the variables the harness itself injected from its settings file removed. A child
// process then sees the user's REAL shell environment, never the harness's own
// configuration — which keeps spawned commands hermetic, so a saved setting can't
// bleed into a test and make it pass in CI but fail locally (or vice-versa).
//
// Returns nil when the harness injected nothing, so the caller leaves the command's
// environment untouched (inherit as usual). This is general infrastructure — it
// strips only the harness's OWN settings-derived vars and assumes nothing about the
// target repo, its language, or its tooling.
func SubprocessEnv() []string {
	if len(injected) == 0 {
		return nil // nothing injected → inherit the environment unchanged
	}
	all := os.Environ()
	out := make([]string, 0, len(all))
	for _, kv := range all {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if injected[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// IsShadowed reports whether env was set in the environment independently of
// settings.json (an explicit export), so it overrides the file's value.
func IsShadowed(env string) bool { return shadowed[env] }

// LoadSettingsFile fills BORG_* vars from ~/.config/borg/settings.json for any var
// not already set in the environment — an explicit `export BORG_*` always wins, so
// the file only fills what's unset. Call once at startup BEFORE Load() so the
// parsed config sees the file's values. A missing or invalid file is a no-op.
func LoadSettingsFile() {
	// Snapshot exports first, while the environment still reflects only the shell.
	for _, s := range Settings {
		if _, ok := os.LookupEnv(s.Env); ok {
			shadowed[s.Env] = true
		}
	}
	m, err := readSettingsFile()
	if err != nil {
		return
	}
	for _, s := range Settings {
		if secretEnv(s.Env) {
			continue // a credential is never loaded from disk, whatever the file says
		}
		v, ok := m[s.Key]
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(s.Env); set {
			continue // explicit export wins
		}
		_ = os.Setenv(s.Env, v)
		injected[s.Env] = true // harness-set, not a shell export → strip from subprocesses
	}
}

// readSettingsFile reads settings.json into a key→string map, normalizing JSON
// bools/strings/numbers to strings. A missing/corrupt file returns an error.
func readSettingsFile() (map[string]string, error) {
	path := SettingsFilePath()
	if path == "" {
		return nil, errors.New("no user config dir")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = jsonScalar(v)
	}
	return out, nil
}

// jsonScalar renders a JSON value as the string borg stores in the environment:
// strings verbatim, bools as "true"/"false", everything else (numbers) as-is.
func jsonScalar(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var b bool
	if json.Unmarshal(v, &b) == nil {
		return strconv.FormatBool(b)
	}
	return strings.TrimSpace(string(v))
}

// SetSetting validates value for key, persists it to settings.json, and — unless
// an explicit export shadows the var — updates the process environment so the
// change takes effect immediately. Returns the normalized value actually stored
// and whether an export shadows it (the caller surfaces that as a warning).
func SetSetting(key, value string) (norm string, shadow bool, err error) {
	s, ok := SettingByKey(key)
	if !ok {
		return "", false, fmt.Errorf("unknown setting %q", key)
	}
	if secretEnv(s.Env) {
		return "", false, fmt.Errorf("%s is a credential: set it with `export %s=…` — borg never writes a key to disk", key, s.Env)
	}
	norm, err = s.Normalize(value)
	if err != nil {
		return "", false, err
	}
	if err := writeSettingFile(s, norm); err != nil {
		return "", false, err
	}
	shadow = IsShadowed(s.Env)
	if !shadow {
		_ = os.Setenv(s.Env, norm)
		injected[s.Env] = true // harness-set, not a shell export → strip from subprocesses
	}
	return norm, shadow, nil
}

// writeSettingFile read-modify-writes settings.json, preserving any keys it
// doesn't know about, and writes the value with its natural JSON type (bools as
// true/false, everything else as a string).
func writeSettingFile(s Setting, value string) error {
	path := SettingsFilePath()
	if path == "" {
		return errors.New("no user config dir")
	}
	raw := map[string]json.RawMessage{}
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &raw) // tolerate empty/corrupt: we rewrite it cleanly
	}
	raw[s.Key] = s.encode(value)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

// encode renders value as the JSON it's stored as in settings.json.
func (s Setting) encode(value string) json.RawMessage {
	if s.Kind == KindBool {
		b, _ := strconv.ParseBool(value)
		return json.RawMessage(strconv.FormatBool(b))
	}
	b, _ := json.Marshal(value)
	return b
}

// Normalize validates value for this setting and returns its canonical stored
// form: bools as "true"/"false"; enums mapped to a member ("off"/"none"/"" → the
// empty "off" value when the enum allows it); strings trimmed.
func (s Setting) Normalize(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch s.Kind {
	case KindBool:
		b, ok := parseToggle(value)
		if !ok {
			return "", fmt.Errorf("%s expects on/off (true/false)", s.Key)
		}
		return strconv.FormatBool(b), nil
	case KindEnum:
		if v := strings.ToLower(value); v == "off" || v == "none" {
			value = ""
		}
		for _, e := range s.Enum {
			if value == e {
				return value, nil
			}
		}
		return "", fmt.Errorf("%s must be one of %s", s.Key, strings.Join(s.enumLabels(), ", "))
	case KindInt:
		if v := strings.ToLower(value); v == "off" || v == "none" || v == "" {
			return "0", nil // 0 = disabled
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return "", fmt.Errorf("%s expects a non-negative number or off", s.Key)
		}
		return strconv.Itoa(n), nil
	default:
		return value, nil
	}
}

// parseToggle accepts the human on/off vocabulary in addition to strconv's
// true/false/1/0, so /settings and `borg settings set` read naturally.
func parseToggle(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "yes", "enable", "enabled":
		return true, true
	case "off", "no", "disable", "disabled":
		return false, true
	}
	b, err := strconv.ParseBool(value)
	return b, err == nil
}

// enumLabels lists the enum's choices for an error message ("" shown as "off").
func (s Setting) enumLabels() []string {
	out := make([]string, len(s.Enum))
	for i, e := range s.Enum {
		if e == "" {
			out[i] = "off"
		} else {
			out[i] = e
		}
	}
	return out
}

// Effective returns the value currently in force: the env var (a file value
// loaded at startup, or an export) if set, else the built-in default.
func (s Setting) Effective() string {
	if v, ok := os.LookupEnv(s.Env); ok {
		return v
	}
	return s.Default
}

// Display formats the effective value for humans (bools as on/off, an empty enum
// as off). DisplayValue does the same for an arbitrary value.
func (s Setting) Display() string { return s.DisplayValue(s.Effective()) }

// DisplayValue formats a specific value the way Display formats the effective one.
func (s Setting) DisplayValue(v string) string {
	switch s.Kind {
	case KindBool:
		if b, _ := strconv.ParseBool(v); b {
			return "on"
		}
		return "off"
	case KindEnum:
		if v == "" {
			return "off"
		}
	case KindInt:
		if v == "" || v == "0" {
			return "off"
		}
	}
	return v
}

// NextEnum returns the enum value after cur (wrapping), for cycle-on-Enter in the
// picker and the no-value `/settings <enum>` / `borg settings set <enum>` forms.
func (s Setting) NextEnum(cur string) string {
	for i, e := range s.Enum {
		if e == cur {
			return s.Enum[(i+1)%len(s.Enum)]
		}
	}
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}
	return cur
}

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// clearProviderEnv unsets every var these tests touch, so a developer's own
// exports can't leak in and flip a result.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{
		"BORG_PROVIDER", "BORG_BASE_URL", "BORG_API_KEY", "BORG_API_KEY_ENV",
		"BORG_ACCESS_TOKEN", "BORG_CONTEXT", "BORG_LLM_PROXY_URL", "BORG_API_BASE_URL", "BORG_MODEL",
	} {
		t.Setenv(e, "")
		require.NoError(t, os.Unsetenv(e))
	}
}

// The default must remain the hosted proxy, endpoint and all — nobody's existing
// setup changes because bring-your-own now exists.
func TestLoadDefaultsToXShellz(t *testing.T) {
	clearProviderEnv(t)
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, ProviderXShellz, c.Provider)
	require.False(t, c.BringYourOwn())
	require.Equal(t, "https://api.xshellz.com", c.APIBaseURL)
	require.Equal(t, "https://api.xshellz.com/v1/llm", c.LLMProxyURL)
	require.Empty(t, c.BaseURL)
}

func TestLoadProviderDefaultBaseURLs(t *testing.T) {
	for provider, want := range map[string]string{
		ProviderOllama:     "http://localhost:11434/v1",
		ProviderOpenAI:     "https://api.openai.com/v1",
		ProviderOpenRouter: "https://openrouter.ai/api/v1",
	} {
		t.Run(provider, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("BORG_PROVIDER", provider)
			t.Setenv("BORG_MODEL", "qwen2.5-coder:7b") // not about the model; a codename is rejected off-platform
			c, err := Load()
			require.NoError(t, err)
			require.True(t, c.BringYourOwn())
			require.Equal(t, want, c.BaseURL)
			// The OpenAI-compatible root IS the endpoint off-platform: no /v1/llm
			// proxy path is spliced on.
			require.Equal(t, want, c.LLMProxyURL)
		})
	}
}

// An explicit base URL wins over the kind's default, and a trailing slash is
// normalized away (so the client's baseURL+"/models" can't become "//models").
func TestLoadExplicitBaseURL(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("BORG_PROVIDER", "ollama")
	t.Setenv("BORG_MODEL", "qwen2.5-coder:7b")
	t.Setenv("BORG_BASE_URL", "http://gpu-box:8000/v1/")
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, "http://gpu-box:8000/v1", c.LLMProxyURL)
}

// `custom` has no sensible default endpoint, so it must be told one — and say so.
func TestLoadCustomRequiresBaseURL(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("BORG_PROVIDER", "custom")
	t.Setenv("BORG_MODEL", "some-local-model")
	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "BORG_BASE_URL")

	t.Setenv("BORG_BASE_URL", "http://127.0.0.1:8080/v1")
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8080/v1", c.LLMProxyURL)
}

// A typo'd provider must FAIL rather than quietly fall back to the hosted proxy:
// silently sending a conversation meant for a local box to xShellz instead would
// be the worst possible outcome of a typo.
func TestLoadRejectsUnknownProvider(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("BORG_PROVIDER", "ollamaa")
	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown BORG_PROVIDER")
	require.Contains(t, err.Error(), "ollama", "the error lists the valid kinds")
}

func TestLoadProviderIsCaseInsensitive(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("BORG_PROVIDER", "  OLLAMA ")
	t.Setenv("BORG_MODEL", "qwen2.5-coder:7b")
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, ProviderOllama, c.Provider)
}

// The bearer resolves from the canonical var, an indirected one, or the original
// BORG_ACCESS_TOKEN alias — which CI depends on and must keep working.
func TestResolveAPIKeyPrecedence(t *testing.T) {
	t.Run("canonical", func(t *testing.T) {
		clearProviderEnv(t)
		t.Setenv("BORG_API_KEY", "sk-canonical")
		c, err := Load()
		require.NoError(t, err)
		require.Equal(t, "sk-canonical", c.APIKey)
	})

	t.Run("named env var indirection", func(t *testing.T) {
		clearProviderEnv(t)
		t.Setenv("OPENAI_API_KEY", "sk-from-openai-var")
		t.Setenv("BORG_API_KEY_ENV", "OPENAI_API_KEY")
		c, err := Load()
		require.NoError(t, err)
		require.Equal(t, "sk-from-openai-var", c.APIKey, "an existing export is reused without copying the secret")
	})

	t.Run("BORG_ACCESS_TOKEN still works", func(t *testing.T) {
		clearProviderEnv(t)
		t.Setenv("BORG_ACCESS_TOKEN", "pat-ci")
		c, err := Load()
		require.NoError(t, err)
		require.Equal(t, "pat-ci", c.APIKey, "the CI/eval-bot variable must not break")
	})

	t.Run("canonical wins over the alias", func(t *testing.T) {
		clearProviderEnv(t)
		t.Setenv("BORG_API_KEY", "sk-wins")
		t.Setenv("BORG_ACCESS_TOKEN", "pat-loses")
		c, err := Load()
		require.NoError(t, err)
		require.Equal(t, "sk-wins", c.APIKey)
	})
}

func TestValidProviderAndDefaultBaseURL(t *testing.T) {
	for _, p := range Providers {
		require.True(t, ValidProvider(p))
	}
	require.False(t, ValidProvider("gemini"))
	require.False(t, ValidProvider(""))
	require.Empty(t, DefaultBaseURL(ProviderXShellz), "the hosted endpoint comes from the login")
	require.Empty(t, DefaultBaseURL(ProviderCustom), "custom must be told its endpoint")
}

// BringYourOwn treats a zero-value Config as hosted (the historical default), so
// embedders and tests that never set Provider behave exactly as they used to.
func TestBringYourOwnZeroValue(t *testing.T) {
	require.False(t, (&Config{}).BringYourOwn())
	require.False(t, (&Config{Provider: ProviderXShellz}).BringYourOwn())
	require.True(t, (&Config{Provider: ProviderOllama}).BringYourOwn())
}

// A stored login must not drag the endpoint away from a BYO backend.
func TestApplyEndpointFallbackLeavesByoAlone(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("BORG_PROVIDER", "ollama")
	t.Setenv("BORG_MODEL", "qwen2.5-coder:7b") // not about the model; a codename is rejected off-platform
	c, err := Load()
	require.NoError(t, err)
	c.ApplyEndpointFallback("https://api.local.xshellz.com", "https://app.local.xshellz.com")
	require.Equal(t, "http://localhost:11434/v1", c.LLMProxyURL,
		"a leftover xShellz login must never redirect a local provider's traffic")
}

// The account cache/login must target xShellz even on a BYO-configured machine —
// otherwise logging in would ask localhost for the plan and catalog.
func TestHostedPinsToTheProxy(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("BORG_PROVIDER", "ollama")
	t.Setenv("BORG_MODEL", "qwen2.5-coder:7b") // not about the model; a codename is rejected off-platform
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, "http://localhost:11434/v1", c.LLMProxyURL)

	h := c.Hosted()
	require.Equal(t, ProviderXShellz, h.Provider)
	require.False(t, h.BringYourOwn())
	require.Equal(t, "https://api.xshellz.com/v1/llm", h.LLMProxyURL)
	require.Empty(t, h.BaseURL)
	require.Equal(t, "http://localhost:11434/v1", c.LLMProxyURL, "the original config is not mutated")

	// An explicit proxy export is still honored on the hosted copy.
	t.Setenv("BORG_LLM_PROXY_URL", "https://api.local.xshellz.com/v1/llm")
	require.Equal(t, "https://api.local.xshellz.com/v1/llm", c.Hosted().LLMProxyURL)
}

// Hosted is a no-op copy when the provider already IS the hosted one.
func TestHostedOnHostedIsACopy(t *testing.T) {
	clearProviderEnv(t)
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, c.LLMProxyURL, c.Hosted().LLMProxyURL)
	require.Equal(t, *c, *c.Hosted())
}

// SECURITY: an API key must never be loadable from settings.json. The registry
// has no key entry, so this drives the guard by force-registering one — proving
// the protection is in the loader, not merely in the registry's current contents.
func TestSettingsFileCanNeverProvideAKey(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "borg"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "borg", "settings.json"),
		[]byte(`{"leaked_key":"sk-should-never-load","provider":"ollama"}`), 0o600))

	orig := Settings
	Settings = append(append([]Setting(nil), Settings...), Setting{
		Key: "leaked_key", Env: EnvAPIKey, Label: "leaked", Kind: KindString,
	})
	t.Cleanup(func() { Settings = orig })

	LoadSettingsFile()
	_, set := os.LookupEnv(EnvAPIKey)
	require.False(t, set, "a credential must never be read off disk into the environment")
	require.Equal(t, "ollama", os.Getenv("BORG_PROVIDER"), "non-secret settings still load normally")

	// …and it can't be written there either.
	_, _, err := SetSetting("leaked_key", "sk-nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "never writes a key to disk")
}

func TestSecretEnv(t *testing.T) {
	require.True(t, secretEnv(EnvAPIKey))
	require.True(t, secretEnv(EnvAccessToken), "the alias is just as secret")
	require.False(t, secretEnv("BORG_API_KEY_ENV"), "the NAME of a var is not a secret")
	require.False(t, secretEnv("BORG_PROVIDER"))
}

// The backend knobs are editable, persist, and round-trip through the registry.
func TestProviderSettingsAreRegistered(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, key := range []string{"provider", "base_url", "model", "api_key_env"} {
		_, ok := SettingByKey(key)
		require.True(t, ok, "%q must be a first-class setting: bring-your-own is the feature", key)
	}

	p, _ := SettingByKey("provider")
	require.Equal(t, KindEnum, p.Kind)
	require.Equal(t, Providers, p.Enum)
	require.Equal(t, ProviderXShellz, p.Default)

	norm, shadow, err := SetSetting("provider", "ollama")
	require.NoError(t, err)
	require.False(t, shadow)
	require.Equal(t, "ollama", norm)
	require.Equal(t, "ollama", os.Getenv("BORG_PROVIDER"))

	_, _, err = SetSetting("provider", "gemini")
	require.Error(t, err, "an unknown provider is rejected at the edit, not at startup")
}

// BORG_ACCESS_TOKEN holds an xShellz PAT. CI, the eval bot and the documented
// local-eval workflow all export it, so it is routinely present in a shell that
// then experiments with a local or third-party backend. It must never be handed
// to one: borg used to pass it through as the Bearer for whatever provider was
// active, which sent the platform credential to (say) openrouter.ai.
func TestXShellzTokenNeverLeavesTheHostedProvider(t *testing.T) {
	for _, p := range []string{ProviderOllama, ProviderOpenAI, ProviderOpenRouter, ProviderCustom} {
		t.Run(p, func(t *testing.T) {
			t.Setenv(EnvAccessToken, "xshellz-pat-secret")
			t.Setenv("BORG_PROVIDER", p)
			t.Setenv("BORG_BASE_URL", "http://127.0.0.1:1/v1")
			t.Setenv("BORG_MODEL", "qwen2.5-coder:7b") // this test is about the key, not the model
			c, err := Load()
			require.NoError(t, err)
			require.Empty(t, c.APIKey, "%s must not be sent the xShellz PAT", p)
		})
	}
}

// The same variable IS the bearer on the hosted provider — that's what CI relies on.
func TestXShellzTokenStillWorksOnTheHostedProvider(t *testing.T) {
	t.Setenv(EnvAccessToken, "xshellz-pat-secret")
	t.Setenv("BORG_PROVIDER", ProviderXShellz)
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, "xshellz-pat-secret", c.APIKey)
}

// An explicit BORG_API_KEY is the user's own key for their own backend, so it is
// honored off-platform — only the xShellz-issued one is scoped out.
func TestExplicitKeyIsHonoredOffPlatform(t *testing.T) {
	t.Setenv(EnvAccessToken, "xshellz-pat-secret")
	t.Setenv(EnvAPIKey, "sk-users-own")
	t.Setenv("BORG_PROVIDER", ProviderOpenRouter)
	t.Setenv("BORG_MODEL", "gpt-4o")
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, "sk-users-own", c.APIKey)
}

// Hosted() re-aims a config at the platform, so it must drop the key that belonged
// to the endpoint it just dropped.
func TestHostedDropsTheByoKey(t *testing.T) {
	c := &Config{Provider: ProviderOpenAI, BaseURL: "https://api.openai.com/v1", APIKey: "sk-users-own", APIKeyEnv: "OPENAI_API_KEY", APIBaseURL: "https://api.xshellz.com"}
	h := c.Hosted()
	require.Empty(t, h.APIKey, "an OpenAI key must not ride along to xShellz")
	require.Empty(t, h.APIKeyEnv)
	require.Equal(t, "sk-users-own", c.APIKey, "the original config is untouched")
}

// BORG_MODEL defaults to a codename, so the first run of the exact setup this
// feature exists for — BORG_PROVIDER=ollama and nothing else — used to post
// {"model":"chuppa"} at the user's daemon and return `model 'chuppa' not found`:
// a 404 naming a hosted catalog entry during a fully local session.
func TestCodenameIsRejectedOffPlatform(t *testing.T) {
	for _, name := range Codenames {
		t.Run(name, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("BORG_PROVIDER", ProviderOllama)
			t.Setenv("BORG_MODEL", name)
			_, err := Load()
			require.Error(t, err, "a codename means nothing to ollama")
			require.Contains(t, err.Error(), "codename")
			require.Contains(t, err.Error(), "BORG_MODEL", "the error must name the knob that fixes it")
			require.Contains(t, err.Error(), ProviderOllama)
		})
	}
}

// The untouched default is the case that actually bites: nothing set but the provider.
func TestDefaultModelIsRejectedOffPlatform(t *testing.T) {
	clearProviderEnv(t) // the point is the UNTOUCHED default, so nothing may be pre-set
	t.Setenv("BORG_PROVIDER", ProviderOllama)
	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "chuppa", "the default is a codename and must not reach a local daemon")
}

// A real model id is what borg is being pointed at — it must load cleanly.
func TestRealModelIsFineOffPlatform(t *testing.T) {
	for _, m := range []string{"qwen2.5-coder:7b", "llama3.1:8b", "gpt-4o", "deepseek/deepseek-v4"} {
		t.Setenv("BORG_PROVIDER", ProviderOllama)
		t.Setenv("BORG_MODEL", m)
		c, err := Load()
		require.NoError(t, err, m)
		require.Equal(t, m, c.Model)
	}
}

// The hosted path is unchanged: a codename is exactly what belongs there.
func TestCodenamesStillWorkOnXShellz(t *testing.T) {
	for _, name := range Codenames {
		t.Setenv("BORG_PROVIDER", ProviderXShellz)
		t.Setenv("BORG_MODEL", name)
		c, err := Load()
		require.NoError(t, err, "%s is a hosted model", name)
		require.Equal(t, name, c.Model)
	}
	// ...including the default, untouched.
	t.Setenv("BORG_PROVIDER", ProviderXShellz)
	require.NoError(t, os.Unsetenv("BORG_MODEL"))
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, CodenameChuppa, c.Model)
}

func TestIsCodename(t *testing.T) {
	require.True(t, IsCodename("chuppa"))
	require.True(t, IsCodename("  CHUPPA  "), "matched case- and space-insensitively, like BORG_PROVIDER")
	require.False(t, IsCodename("qwen2.5-coder:7b"))
	require.False(t, IsCodename(""))
	require.False(t, IsCodename("chuppa-ish"))
}

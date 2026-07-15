package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestDebugDefault(t *testing.T) {
	t.Setenv("BORG_DEBUG_ENABLED", "true")
	require.True(t, DebugDefault())
	t.Setenv("BORG_DEBUG_ENABLED", "false")
	require.False(t, DebugDefault())
	t.Setenv("BORG_DEBUG_ENABLED", "") // unset/empty → off
	require.False(t, DebugDefault())
}

func TestThinkDefault(t *testing.T) {
	t.Setenv("BORG_THINK", "true")
	require.True(t, ThinkDefault())
	t.Setenv("BORG_THINK", "")
	require.False(t, ThinkDefault())
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "https://api.xshellz.com", cfg.APIBaseURL)
	require.Equal(t, "90228cb6-3de1-472f-bb30-cd5b44101d6d", cfg.OAuthClientID)
	require.Equal(t, "chuppa", cfg.Model)
	require.False(t, cfg.ForceDevice)
}

func TestLLMProxyDerivedFromAPIBase(t *testing.T) {
	t.Setenv("BORG_API_BASE_URL", "https://api.local.xshellz.com")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "https://api.local.xshellz.com/v1/llm", cfg.LLMProxyURL)
}

func TestApplyEndpointFallbackUsesStoredWhenEnvUnset(t *testing.T) {
	cfg, err := Load() // BORG_API_BASE_URL unset -> prod default
	require.NoError(t, err)
	cfg.ApplyEndpointFallback("https://api.local.xshellz.com", "https://app.local.xshellz.com:4200")
	require.Equal(t, "https://api.local.xshellz.com", cfg.APIBaseURL)
	require.Equal(t, "https://app.local.xshellz.com:4200", cfg.AppURL)
	require.Equal(t, "https://api.local.xshellz.com/v1/llm", cfg.LLMProxyURL)
}

func TestApplyEndpointFallbackEnvWins(t *testing.T) {
	t.Setenv("BORG_API_BASE_URL", "https://api.xshellz.com")
	cfg, err := Load()
	require.NoError(t, err)
	cfg.ApplyEndpointFallback("https://api.local.xshellz.com", "")
	require.Equal(t, "https://api.xshellz.com", cfg.APIBaseURL) // explicit env wins over stored
}

func TestLoadOverride(t *testing.T) {
	t.Setenv("BORG_API_BASE_URL", "https://api.local.xshellz.com")
	t.Setenv("BORG_FORCE_DEVICE", "true")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "https://api.local.xshellz.com", cfg.APIBaseURL)
	require.True(t, cfg.ForceDevice)
}

package account_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/account"
	"github.com/turborg/borg/internal/llm"
)

func TestSaveLoadClear(t *testing.T) {
	d := t.TempDir()
	// Use XDG_CONFIG_HOME so account.cachePath picks a temp dir
	require.NoError(t, os.Setenv("XDG_CONFIG_HOME", d))
	defer func() { _ = os.Unsetenv("XDG_CONFIG_HOME") }()

	info := &account.Info{
		Tier:   "pro",
		Models: []llm.ModelInfo{{ID: "m1", Label: "M1", Version: "v"}},
	}
	// Save should write the file and stamp UpdatedAt
	require.NoError(t, account.Save(info))
	loaded, err := account.Load()
	require.NoError(t, err)
	require.Equal(t, "pro", loaded.Tier)
	require.NotZero(t, loaded.UpdatedAt)
	// UpdatedAt should be recent
	require.WithinDuration(t, time.Now(), loaded.UpdatedAt, 5*time.Second)

	// Clear removes file; subsequent Load returns error
	require.NoError(t, account.Clear())
	_, err = account.Load()
	require.Error(t, err)
}

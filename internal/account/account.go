// Package account caches the caller's plan tier and model catalog so the REPL
// can render plan/model info instantly on startup (from disk) while it refreshes
// the truth from the API in the background. The cache is best-effort: a miss or
// stale value never blocks — it's only there to make the first paint fast.
package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/turborg/borg/internal/llm"
)

// Info is the cached account snapshot shown in the banner.
type Info struct {
	Tier      string          `json:"tier"`
	Models    []llm.ModelInfo `json:"models"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Load reads the cached info, or returns an error when none exists yet.
func Load() (*Info, error) {
	path, err := cachePath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("parse account cache: %w", err)
	}
	return &info, nil
}

// Save writes the cache (0600), stamping UpdatedAt.
func Save(info *Info) error {
	path, err := cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	info.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Clear removes the cached info (e.g. on logout). A missing file is not an error.
func Clear() error {
	path, err := cachePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func cachePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "borg", "account.json"), nil
}

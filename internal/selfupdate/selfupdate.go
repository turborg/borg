// Package selfupdate checks for and installs newer turborg releases from the
// release host (dl.turborg.com) — the same artifacts install.sh fetches. The
// check is throttled to one network call per day via an on-disk cache so the
// startup nudge never hammers the CDN or blocks the REPL.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	manifestPath  = "/latest/version.json"
	checksumsPath = "/latest/checksums.txt"
	cacheTTL      = 24 * time.Hour
	maxDownload   = 100 << 20 // 100 MiB ceiling on any fetched artifact
)

// ErrUpToDate is returned by Update when the running version is already latest.
var ErrUpToDate = errors.New("already on the latest version")

// Injected for tests.
var (
	httpClient     = &http.Client{Timeout: 30 * time.Second}
	osExecutable   = os.Executable
	timeNow        = time.Now
	windowsReplace = runtime.GOOS == "windows" // move-aside replace strategy
)

type manifest struct {
	Version string `json:"version"`
}

// Latest fetches the published latest version from base/latest/version.json.
func Latest(ctx context.Context, base string) (string, error) {
	b, err := fetch(ctx, strings.TrimRight(base, "/")+manifestPath)
	if err != nil {
		return "", err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse version manifest: %w", err)
	}
	v := strings.TrimSpace(m.Version)
	if v == "" {
		return "", errors.New("version manifest had no version")
	}
	return v, nil
}

// Check returns the latest version and whether it is newer than current,
// throttled to one network call per day via an on-disk cache. A network error
// is returned without disturbing the cache so the caller can stay quiet.
func Check(ctx context.Context, base, current string) (latest string, newer bool, err error) {
	if c, ok := readCache(); ok && timeNow().Sub(c.CheckedAt) < cacheTTL {
		return c.Latest, IsNewer(current, c.Latest), nil
	}
	latest, err = Latest(ctx, base)
	if err != nil {
		return "", false, err
	}
	writeCache(cache{Latest: latest, CheckedAt: timeNow()})
	return latest, IsNewer(current, latest), nil
}

// IsNewer reports whether latest is a higher semver than current. A non-release
// current ("dev", "", non-numeric) is never treated as behind, so local/dev
// builds are not nagged.
func IsNewer(current, latest string) bool {
	cur, ok := parseVersion(current)
	if !ok {
		return false
	}
	lat, ok := parseVersion(latest)
	if !ok {
		return false
	}
	return compare(lat, cur) > 0
}

// Update downloads the latest archive for this OS/arch, verifies its checksum,
// and atomically replaces the running executable. Returns the new version, or
// ErrUpToDate (with the current version) when already latest.
func Update(ctx context.Context, base, current string) (string, error) {
	base = strings.TrimRight(base, "/")
	latest, err := Latest(ctx, base)
	if err != nil {
		return "", err
	}
	if !IsNewer(current, latest) {
		return current, ErrUpToDate
	}

	name := artifactName()
	archive, err := fetch(ctx, base+"/latest/"+name)
	if err != nil {
		return "", err
	}
	sums, err := fetch(ctx, base+checksumsPath)
	if err != nil {
		return "", err
	}
	if err := verifyChecksum(name, archive, sums); err != nil {
		return "", err
	}
	bin, err := extractBinary(archive, runtime.GOOS == "windows")
	if err != nil {
		return "", err
	}
	if err := replaceExecutable(bin); err != nil {
		return "", err
	}
	writeCache(cache{Latest: latest, CheckedAt: timeNow()})
	return latest, nil
}

// artifactName is the release archive for this platform, matching .goreleaser.yml
// and install.sh (turborg_<os>_<arch>.tar.gz, .zip on Windows).
func artifactName() string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("turborg_%s_%s.%s", runtime.GOOS, runtime.GOARCH, ext)
}

func verifyChecksum(name string, data, checksums []byte) error {
	want := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = strings.ToLower(f[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum for %s", name)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s", name)
	}
	return nil
}

// extractBinary pulls the turborg (or turborg.exe) binary out of a release archive.
func extractBinary(archive []byte, isZip bool) ([]byte, error) {
	want := "turborg"
	if isZip {
		want = "turborg.exe"
	}
	if isZip {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("open zip: %w", err)
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) != want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, maxDownload))
		}
		return nil, errors.New("archive did not contain turborg.exe")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if filepath.Base(h.Name) == want {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
	return nil, errors.New("archive did not contain turborg")
}

// replaceExecutable writes bin over the running binary. Symlinks (the `borg`
// alias) are resolved so the real turborg file is replaced. On Windows the
// running exe is moved aside first (it cannot be overwritten in place).
func replaceExecutable(bin []byte) error {
	exe, err := osExecutable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".turborg-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if windowsReplace {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		if err := os.Rename(tmpName, exe); err != nil {
			_ = os.Rename(old, exe) // roll back
			_ = os.Remove(tmpName)
			return err
		}
		return nil
	}
	if err := os.Rename(tmpName, exe); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload))
}

// --- version parsing (no external semver dep) ---

func parseVersion(v string) ([3]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 { // drop prerelease/build metadata
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func compare(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// --- check throttle cache ---

type cache struct {
	Latest    string    `json:"latest"`
	CheckedAt time.Time `json:"checked_at"`
}

func cachePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "borg", "update.json"), nil
}

func readCache() (cache, bool) {
	p, err := cachePath()
	if err != nil {
		return cache{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cache{}, false
	}
	var c cache
	if err := json.Unmarshal(b, &c); err != nil {
		return cache{}, false
	}
	return c, true
}

func writeCache(c cache) {
	p, err := cachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	if b, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

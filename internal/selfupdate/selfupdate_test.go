package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// makeTarGz builds a release tar.gz containing a single `turborg` file.
func makeTarGz(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "turborg", Mode: 0o755, Size: int64(len(content))}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func makeZip(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("turborg.exe")
	require.NoError(t, err)
	_, err = w.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releaseServer serves version.json, the platform archive, and checksums.txt.
func releaseServer(t *testing.T, version string, archive []byte, hits *int) *httptest.Server {
	t.Helper()
	name := artifactName()
	checks := fmt.Sprintf("%s  %s\n", sha(archive), name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		switch r.URL.Path {
		case manifestPath:
			fmt.Fprintf(w, `{"version":%q}`, version)
		case "/latest/" + name:
			_, _ = w.Write(archive)
		case checksumsPath:
			fmt.Fprint(w, checks)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, lat string
		want     bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "0.1.1", true},
		{"1.0.0", "1.0.0", false},
		{"0.2.0", "0.1.9", false},
		{"v0.1.0", "v0.2.0", true},
		{"dev", "0.2.0", false},     // dev never nagged
		{"", "0.2.0", false},        // empty never nagged
		{"0.1.0", "garbage", false}, // bad remote
		{"0.1.0-rc1", "0.1.0", false},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, IsNewer(c.cur, c.lat), "IsNewer(%q,%q)", c.cur, c.lat)
	}
}

func TestLatest(t *testing.T) {
	srv := releaseServer(t, "0.3.0", []byte("x"), nil)
	v, err := Latest(context.Background(), srv.URL)
	require.NoError(t, err)
	require.Equal(t, "0.3.0", v)
}

func TestLatestErrors(t *testing.T) {
	// bad JSON
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	t.Cleanup(bad.Close)
	_, err := Latest(context.Background(), bad.URL)
	require.Error(t, err)

	// empty version
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":""}`)
	}))
	t.Cleanup(empty.Close)
	_, err = Latest(context.Background(), empty.URL)
	require.Error(t, err)

	// non-200
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	_, err = Latest(context.Background(), down.URL)
	require.Error(t, err)
}

func TestCheckCaching(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	hits := 0
	srv := releaseServer(t, "0.5.0", []byte("x"), &hits)

	latest, newer, err := Check(context.Background(), srv.URL, "0.4.0")
	require.NoError(t, err)
	require.Equal(t, "0.5.0", latest)
	require.True(t, newer)
	require.Equal(t, 1, hits)

	// second call within TTL → served from cache, no new request
	latest, newer, err = Check(context.Background(), srv.URL, "0.4.0")
	require.NoError(t, err)
	require.Equal(t, "0.5.0", latest)
	require.True(t, newer)
	require.Equal(t, 1, hits, "should not re-fetch within TTL")

	// expire the cache → re-fetch
	old := timeNow
	timeNow = func() time.Time { return old().Add(cacheTTL + time.Hour) }
	defer func() { timeNow = old }()
	_, _, err = Check(context.Background(), srv.URL, "0.4.0")
	require.NoError(t, err)
	require.Equal(t, 2, hits)
}

func TestCheckNetworkErrorQuiet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, newer, err := Check(context.Background(), "http://127.0.0.1:0", "0.1.0")
	require.Error(t, err)
	require.False(t, newer)
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello")
	name := "turborg_linux_amd64.tar.gz"
	good := fmt.Sprintf("%s  %s\n", sha(data), name)
	require.NoError(t, verifyChecksum(name, data, []byte(good)))
	require.Error(t, verifyChecksum(name, data, []byte("deadbeef  "+name)))
	require.Error(t, verifyChecksum(name, data, []byte("xx  other.tar.gz")))
}

func TestExtractBinary(t *testing.T) {
	content := []byte("#!binary")
	bin, err := extractBinary(makeTarGz(t, content), false)
	require.NoError(t, err)
	require.Equal(t, content, bin)

	bin, err = extractBinary(makeZip(t, content), true)
	require.NoError(t, err)
	require.Equal(t, content, bin)

	_, err = extractBinary([]byte("not a gzip"), false)
	require.Error(t, err)
	_, err = extractBinary([]byte("not a zip"), true)
	require.Error(t, err)

	// archive without the wanted binary
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "README", Size: 1}))
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	_, err = extractBinary(buf.Bytes(), false)
	require.Error(t, err)
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "turborg")
	require.NoError(t, os.WriteFile(exe, []byte("old"), 0o755))
	old := osExecutable
	osExecutable = func() (string, error) { return exe, nil }
	defer func() { osExecutable = old }()

	require.NoError(t, replaceExecutable([]byte("new-binary")))
	got, err := os.ReadFile(exe)
	require.NoError(t, err)
	require.Equal(t, "new-binary", string(got))
}

func TestUpdateFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz flow assumes a non-windows runner")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	content := []byte("the-new-turborg-binary")
	srv := releaseServer(t, "0.9.0", makeTarGz(t, content), nil)

	dir := t.TempDir()
	exe := filepath.Join(dir, "turborg")
	require.NoError(t, os.WriteFile(exe, []byte("old"), 0o755))
	old := osExecutable
	osExecutable = func() (string, error) { return exe, nil }
	defer func() { osExecutable = old }()

	v, err := Update(context.Background(), srv.URL, "0.8.0")
	require.NoError(t, err)
	require.Equal(t, "0.9.0", v)
	got, _ := os.ReadFile(exe)
	require.Equal(t, content, got)
}

func TestReplaceExecutableWindowsPath(t *testing.T) {
	// The move-aside strategy works on any OS; force it to cover that branch.
	old := windowsReplace
	windowsReplace = true
	defer func() { windowsReplace = old }()

	dir := t.TempDir()
	exe := filepath.Join(dir, "turborg")
	require.NoError(t, os.WriteFile(exe, []byte("old"), 0o755))
	oe := osExecutable
	osExecutable = func() (string, error) { return exe, nil }
	defer func() { osExecutable = oe }()

	require.NoError(t, replaceExecutable([]byte("new")))
	got, _ := os.ReadFile(exe)
	require.Equal(t, "new", string(got))
	moved, _ := os.ReadFile(exe + ".old")
	require.Equal(t, "old", string(moved)) // the running binary was set aside
}

func TestReplaceExecutableUnwritable(t *testing.T) {
	oe := osExecutable
	osExecutable = func() (string, error) { return filepath.Join("/no/such/dir", "turborg"), nil }
	defer func() { osExecutable = oe }()
	require.Error(t, replaceExecutable([]byte("x")))
}

func TestUpdateFetchErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	name := artifactName()
	// version present, but the archive 404s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifestPath {
			fmt.Fprint(w, `{"version":"3.0.0"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	_, err := Update(context.Background(), srv.URL, "1.0.0")
	require.Error(t, err)

	// archive ok, but checksums 404.
	archive := makeTarGz(t, []byte("bin"))
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case manifestPath:
			fmt.Fprint(w, `{"version":"3.0.0"}`)
		case "/latest/" + name:
			_, _ = w.Write(archive)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv2.Close)
	_, err = Update(context.Background(), srv2.URL, "1.0.0")
	require.Error(t, err)
}

func TestExtractZipMissing(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("README.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	_, err := extractBinary(buf.Bytes(), true)
	require.Error(t, err)
}

func TestUpdateUpToDate(t *testing.T) {
	srv := releaseServer(t, "1.0.0", []byte("x"), nil)
	v, err := Update(context.Background(), srv.URL, "1.0.0")
	require.ErrorIs(t, err, ErrUpToDate)
	require.Equal(t, "1.0.0", v)
}

func TestUpdateChecksumMismatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	name := artifactName()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case manifestPath:
			fmt.Fprint(w, `{"version":"2.0.0"}`)
		case "/latest/" + name:
			_, _ = w.Write([]byte("corrupt"))
		case checksumsPath:
			fmt.Fprintf(w, "%s  %s\n", sha([]byte("different")), name)
		}
	}))
	t.Cleanup(srv.Close)
	_, err := Update(context.Background(), srv.URL, "1.0.0")
	require.ErrorContains(t, err, "checksum")
}

func TestArtifactName(t *testing.T) {
	require.Contains(t, artifactName(), runtime.GOOS)
	require.Contains(t, artifactName(), runtime.GOARCH)
}

func TestUpdateExtractError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz flow assumes a non-windows runner")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	name := artifactName()
	bad := []byte("not a valid archive")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case manifestPath:
			fmt.Fprint(w, `{"version":"9.9.9"}`)
		case "/latest/" + name:
			_, _ = w.Write(bad)
		case checksumsPath:
			fmt.Fprintf(w, "%s  %s\n", sha(bad), name) // checksum matches, extract fails
		}
	}))
	t.Cleanup(srv.Close)
	_, err := Update(context.Background(), srv.URL, "0.1.0")
	require.Error(t, err)
}

func TestCacheRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, ok := readCache()
	require.False(t, ok)
	writeCache(cache{Latest: "1.2.3", CheckedAt: timeNow()})
	c, ok := readCache()
	require.True(t, ok)
	require.Equal(t, "1.2.3", c.Latest)
}

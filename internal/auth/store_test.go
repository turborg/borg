package auth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestStoreRoundTrip(t *testing.T) {
	s := store{path: filepath.Join(t.TempDir(), "creds.json")}
	want := &Credentials{
		AccessToken:  "atok",
		RefreshToken: "rtok",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}
	require.NoError(t, s.save(want))

	got, err := s.load()
	require.NoError(t, err)
	require.Equal(t, want.AccessToken, got.AccessToken)
	require.Equal(t, want.RefreshToken, got.RefreshToken)
	require.Equal(t, want.TokenType, got.TokenType)
	require.False(t, got.Expired())
}

func TestCredentialsExpired(t *testing.T) {
	past := &Credentials{Expiry: time.Now().Add(-time.Minute)}
	require.True(t, past.Expired())

	zero := &Credentials{}
	require.False(t, zero.Expired()) // zero expiry => treated as non-expiring
}

func TestStoreClearMissingIsNoError(t *testing.T) {
	s := store{path: filepath.Join(t.TempDir(), "nope.json")}
	require.NoError(t, s.clear())
}

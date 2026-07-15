package tui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// versionServer serves the release version manifest the self-updater reads.
func versionServer(t *testing.T, v string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/version.json" {
			fmt.Fprintf(w, `{"version":%q}`, v)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateNudgeText(t *testing.T) {
	s := updateNudgeText("0.9.0")
	require.Contains(t, s, "0.9.0")
	require.Contains(t, s, "/update")
}

func TestCheckUpdateCmd(t *testing.T) {
	srv := versionServer(t, "9.9.9")
	t.Setenv("BORG_INSTALL_BASE", srv.URL)
	m := newTestModel(t)
	msg := m.checkUpdate()()
	um, ok := msg.(updateMsg)
	require.True(t, ok)
	require.Equal(t, "9.9.9", um.latest)
	require.False(t, um.newer) // version.Version is "dev" in tests → never behind
}

func TestCheckUpdateCmdQuietOnError(t *testing.T) {
	t.Setenv("BORG_INSTALL_BASE", "http://127.0.0.1:0")
	m := newTestModel(t)
	require.Equal(t, updateMsg{}, m.checkUpdate()())
}

func TestRunUpdateCmd(t *testing.T) {
	srv := versionServer(t, "9.9.9")
	t.Setenv("BORG_INSTALL_BASE", srv.URL)
	m := newTestModel(t)
	msg := m.runUpdate()()
	dm, ok := msg.(updateDoneMsg)
	require.True(t, ok)
	require.True(t, dm.upToDate) // dev build is never "behind" a real release
}

func TestUpdateMsgNudge(t *testing.T) {
	m := newTestModel(t)
	_, cmd := step(t, m, updateMsg{latest: "9.9.9", newer: true})
	require.NotNil(t, cmd) // prints the nudge to scrollback
	_, cmd = step(t, m, updateMsg{newer: false})
	require.Nil(t, cmd)
}

func TestUpdateDoneMsg(t *testing.T) {
	m := newTestModel(t)
	m2, _ := step(t, m, updateDoneMsg{version: "1.2.3"})
	require.Contains(t, m2.status, "updated to 1.2.3")
	m2, _ = step(t, m, updateDoneMsg{version: "1.0.0", upToDate: true})
	require.Contains(t, m2.status, "up to date")
	m2, _ = step(t, m, updateDoneMsg{err: errors.New("boom")})
	require.Contains(t, m2.status, "update failed")
}

func TestUpdateCommandDispatch(t *testing.T) {
	srv := versionServer(t, "9.9.9")
	t.Setenv("BORG_INSTALL_BASE", srv.URL)
	m := newTestModel(t)
	tm, cmd := m.command("/update")
	require.Equal(t, "updating…", tm.(model).status)
	require.NotNil(t, cmd)
}

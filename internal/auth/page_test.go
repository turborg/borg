package auth

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteBrowserPageSuccess(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBrowserPage(rec, true, "You're all set", "signed in")

	body := rec.Body.String()
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	require.NotContains(t, body, "{{")              // all placeholders substituted
	require.Contains(t, body, "xShellz")            // brand wordmark
	require.Contains(t, body, "badge ok")           // success badge
	require.Contains(t, body, "You&#39;re all set") // heading is HTML-escaped
}

func TestWriteBrowserPageEscapesAndMarksError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBrowserPage(rec, false, "Authorization failed", "<script>x</script>")

	body := rec.Body.String()
	require.Contains(t, body, "badge err")
	require.NotContains(t, body, "<script>x</script>") // detail is escaped, not injected
	require.Contains(t, body, "&lt;script&gt;")
}

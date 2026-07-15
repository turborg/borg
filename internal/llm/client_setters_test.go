package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/config"
)

func TestClientSettersAndUserInfoGuard(t *testing.T) {
	c := &Client{model: "m0"}
	require.Equal(t, "m0", c.Model())
	c.SetModel("m1")
	require.Equal(t, "m1", c.Model())
	c.SetEffort("high")
	require.Equal(t, "high", c.effort)

	c.SetDebug(func(s string) {})
	c.SetDebug(nil)

	// CloseIdleConnections is a safe no-op on a client with no open connections.
	c2 := New(&config.Config{}, "tok")
	require.NotPanics(t, c2.CloseIdleConnections)

	// UserInfo requires apiBase
	_, err := c.UserInfo(context.Background())
	require.Error(t, err)
}

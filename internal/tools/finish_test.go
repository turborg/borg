package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinishToolBasics(t *testing.T) {
	var f finishTool
	require.Equal(t, "finish", f.Name())
	d := f.Description()
	require.Contains(t, d, "End your turn")
	require.False(t, f.Mutating())
	// Schema should be valid JSON and require summary.
	s := f.Schema()
	var m map[string]any
	require.NoError(t, json.Unmarshal(s, &m))
	props, ok := m["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, props, "summary")
	// Execute is a no-op sentinel; it returns without error.
	out, err := f.Execute(context.Background(), json.RawMessage(`{"summary":"ok"}`))
	require.NoError(t, err)
	require.Equal(t, "", out)
}

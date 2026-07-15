package agent

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatsLineVariants(t *testing.T) {
	var s Stats
	require.Equal(t, "", s.Line())

	s = Stats{InTokens: 10, OutTokens: 20, CachedTokens: 3, Elapsed: 2 * time.Second}
	line := s.Line()
	require.Contains(t, line, "10 in")
	require.Contains(t, line, "20 out")
	require.Contains(t, line, "tok/s")
}

func TestPlainUIDebugAndToolResultAndPermit(t *testing.T) {
	out := &bytes.Buffer{}
	err := &bytes.Buffer{}
	p := &plainUI{out: out, errw: err, in: bufio.NewReader(strings.NewReader("y\n"))}

	p.Debug("one\ntwo\n")
	req := err.String()
	require.Contains(t, req, "· one")
	require.Contains(t, req, "· two")

	p.ToolResult("tname", true, "ok-summary")
	require.Contains(t, err.String(), "✓ ok-summary")

	p.ToolResult("tname", false, "bad-summary")
	require.Contains(t, err.String(), "✗ bad-summary")

	p.ThinkingStart() // no-op
	p.Delta("hello ")
	p.Delta("world")
	require.Contains(t, out.String(), "hello world")
	p.AssistantEnd(Stats{InTokens: 1, OutTokens: 2, Elapsed: time.Second})
	require.Contains(t, err.String(), "1 in") // stats line flushed to stderr
	p.ToolBatch(3)
	require.Contains(t, err.String(), "3 tools")
	p.ToolCall("read_file", `{"path":"x.go"}`)
	require.Contains(t, err.String(), "read_file")

	// Permit should read from the provided in and return AllowOnce for "y"
	d := p.Permit("mutate")
	require.Equal(t, AllowOnce, d)
}

func TestDecide(t *testing.T) {
	require.Equal(t, AllowAlways, decide("a\n"))
	require.Equal(t, AllowOnce, decide("y\n"))
	require.Equal(t, DenyOnce, decide("n\n"))
}

func TestPlainUIAskUser(t *testing.T) {
	req := AskRequest{Question: "Which?", Options: []AskOption{
		{Label: "Alpha", Description: "the first"},
		{Label: "Beta"},
	}}

	// A number picks that option; the prompt shows the question + options.
	out, errw := &bytes.Buffer{}, &bytes.Buffer{}
	p := &plainUI{out: out, errw: errw, in: bufio.NewReader(strings.NewReader("2\n"))}
	require.Equal(t, AskResult{Choice: "Beta"}, p.AskUser(req))
	require.Contains(t, errw.String(), "Which?")
	require.Contains(t, errw.String(), "Alpha — the first")

	// Any non-numeric text is taken as the user's own answer (Freeform).
	p = &plainUI{out: &bytes.Buffer{}, errw: &bytes.Buffer{}, in: bufio.NewReader(strings.NewReader("let's discuss B\n"))}
	require.Equal(t, AskResult{Choice: "let's discuss B", Freeform: true}, p.AskUser(req))

	// A blank line and EOF both dismiss (zero result).
	for _, in := range []string{"\n", ""} {
		p := &plainUI{out: &bytes.Buffer{}, errw: &bytes.Buffer{}, in: bufio.NewReader(strings.NewReader(in))}
		require.Equal(t, AskResult{}, p.AskUser(req))
	}
}

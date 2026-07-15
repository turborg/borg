package llm

import "strings"

// FinishReasonRepetition is a synthetic finish_reason the streaming loop sets when
// it aborts a turn for degenerate repetition (a small model looping on the same
// prose instead of acting). It is not an OpenAI value — the agent loop treats it
// as a signal to force a structured tool call rather than letting the model burn
// its whole output budget rambling. See internal/agent/loop.go.
const FinishReasonRepetition = "repetition"

// repetitionGuard detects a model stuck emitting the same line of prose over and
// over (the classic small-model "Wait… Actually… I'm ready… Wait…" loop) so the
// stream can be cut short instead of running to the output-token cap. It only
// ever sees assistant *content* — tool-call arguments stream on a separate channel
// — so a legitimately repetitive file body (written via write_file) can't trip it.
type repetitionGuard struct {
	counts  map[string]int
	order   []string // recent counted lines, for sliding-window eviction
	pending string   // bytes since the last newline (deltas split lines arbitrarily)
	minLen  int      // ignore short lines (punctuation, "ok", code braces)
	limit   int      // identical lines within the window that means "looping"
	window  int      // how many recent lines to keep in scope
}

func newRepetitionGuard() *repetitionGuard {
	return &repetitionGuard{
		counts: make(map[string]int),
		minLen: 12,
		limit:  6,
		window: 300,
	}
}

// maxPending bounds the un-newlined buffer, both to cap memory and to trigger the
// run-on repetition check (a model looping with no line breaks).
const maxPending = 4096

// feed consumes a content delta and reports whether degenerate repetition has been
// detected. Complete lines (>= minLen after trimming) are counted; once any line
// recurs limit times within the window, the model is looping. A model that repeats
// with NO newlines is caught by the run-on check on the pending buffer.
func (g *repetitionGuard) feed(s string) bool {
	g.pending += s
	for {
		i := strings.IndexByte(g.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(g.pending[:i])
		g.pending = g.pending[i+1:]
		if len(line) < g.minLen {
			continue
		}
		g.counts[line]++
		g.order = append(g.order, line)
		if g.counts[line] >= g.limit {
			return true
		}
		if len(g.order) > g.window {
			old := g.order[0]
			g.order = g.order[1:]
			if g.counts[old]--; g.counts[old] <= 0 {
				delete(g.counts, old)
			}
		}
	}
	// Run-on loop: a long stretch with no newline that ends in the same block
	// repeated. Bounded so pending can't grow without limit.
	if len(g.pending) > maxPending {
		if hasRepeatedTail(g.pending, g.minLen, g.limit) {
			return true
		}
		g.pending = g.pending[len(g.pending)-maxPending/2:]
	}
	return false
}

// hasRepeatedTail reports whether s ends with a block of period minLen..256
// repeated at least limit times back-to-back — the run-on form of a loop ("…just
// write it.just write it.just write it…" with no line breaks). minLen avoids
// tripping on trivially short periods.
func hasRepeatedTail(s string, minLen, limit int) bool {
	n := len(s)
	const maxPeriod = 256
	for p := minLen; p <= maxPeriod && p*limit <= n; p++ {
		match := true
		for k := 1; k < limit; k++ {
			if s[n-(k+1)*p:n-k*p] != s[n-k*p:n-(k-1)*p] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

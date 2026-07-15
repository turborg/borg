package llm

import (
	"strings"
	"testing"
)

func TestRepetitionGuardTripsOnLoopedLine(t *testing.T) {
	g := newRepetitionGuard()
	line := "Actually, I'll just write it.\n"
	tripped := false
	for i := 0; i < 10; i++ {
		if g.feed(line) {
			tripped = true
			if i+1 < g.limit {
				t.Fatalf("tripped too early at %d (limit %d)", i+1, g.limit)
			}
			break
		}
	}
	if !tripped {
		t.Fatal("guard never tripped on a line repeated 10x")
	}
}

func TestRepetitionGuardIgnoresShortLines(t *testing.T) {
	g := newRepetitionGuard()
	for i := 0; i < 50; i++ {
		if g.feed("ok\n") { // below minLen
			t.Fatal("short line should never trip the guard")
		}
	}
}

func TestRepetitionGuardAllowsVariedProse(t *testing.T) {
	g := newRepetitionGuard()
	lines := []string{
		"First I will read the configuration file.\n",
		"Then I will inspect the service layer for the bug.\n",
		"The plan validation lives in PlanCapabilities.\n",
		"I will write a focused fix and a regression test.\n",
		"Finally I will run the compile check before finishing.\n",
	}
	for i := 0; i < 20; i++ {
		if g.feed(lines[i%len(lines)]) {
			t.Fatalf("varied prose tripped the guard at iteration %d", i)
		}
	}
}

func TestRepetitionGuardHandlesSplitDeltas(t *testing.T) {
	// A line arriving across several deltas must still be counted once per newline.
	g := newRepetitionGuard()
	full := "Wait, I should double-check the listeners directory first.\n"
	tripped := false
	for i := 0; i < 8; i++ {
		// feed the line in three arbitrary pieces
		third := len(full) / 3
		if g.feed(full[:third]) || g.feed(full[third:2*third]) {
			tripped = true
		}
		if g.feed(full[2*third:]) {
			tripped = true
		}
		if tripped {
			break
		}
	}
	if !tripped {
		t.Fatal("guard did not trip when the looped line was split across deltas")
	}
}

func TestRepetitionGuardTripsOnRunOnLoop(t *testing.T) {
	// A model that loops with NO newlines (one run-on line) is caught by the
	// pending-buffer run-on check, not the line counter.
	g := newRepetitionGuard()
	block := "and then I will just write the file right now. " // no newline
	tripped := false
	for i := 0; i < 500; i++ {
		if g.feed(block) {
			tripped = true
			break
		}
	}
	if !tripped {
		t.Fatal("run-on repetition (no newlines) did not trip the guard")
	}
}

func TestRepetitionGuardRunOnIgnoresVariedText(t *testing.T) {
	// A long stream with no exact repeated tail must not trip.
	g := newRepetitionGuard()
	for i := 0; i < 500; i++ {
		if g.feed("unique sentence number " + string(rune('A'+i%26)) + " carries on here. ") {
			t.Fatalf("varied run-on text tripped the guard at %d", i)
		}
	}
}

func TestRepetitionGuardWindowEviction(t *testing.T) {
	// Distinct lines beyond the window are evicted, so an old line's count can't
	// accumulate forever from sparse recurrences far apart.
	g := newRepetitionGuard()
	g.window = 4
	g.limit = 3
	feedLine := func(s string) bool { return g.feed(s + strings.Repeat("x", 12) + "\n") }
	// "A" appears, then 5 distinct fillers (evicting A), then A again twice: should
	// not reach 3 within any window of 4.
	feedLine("A")
	for i := 0; i < 5; i++ {
		feedLine(string(rune('a' + i)))
	}
	again1 := feedLine("A")
	again2 := feedLine("A")
	if again1 || again2 {
		t.Fatal("evicted line should not trip across a sparse window")
	}
}

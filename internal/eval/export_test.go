package eval

// Test-only handles so the external eval_test package can unit-test the small,
// pure formatting helpers (every branch of the int/duration humanizers) directly.

var (
	HumanIntForTest = humanInt
	HumanDurForTest = humanDur
)

package state

import "testing"

func TestPhaseString(t *testing.T) {
	cases := map[Phase]string{
		PhaseStarting:   "STARTING",
		PhaseBootstrap:  "BOOTSTRAP_SCAN",
		PhaseCatchingUp: "CATCHING_UP",
		PhaseLive:       "LIVE_TAILING",
		PhaseShutdown:   "SHUTDOWN",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", p, got, want)
		}
	}
}

func TestValidTransitions(t *testing.T) {
	m := New()
	if m.Phase() != PhaseStarting {
		t.Fatalf("initial phase = %v, want STARTING", m.Phase())
	}
	if !m.Advance(PhaseBootstrap) {
		t.Fatalf("STARTING → BOOTSTRAP should succeed")
	}
	if !m.Advance(PhaseCatchingUp) {
		t.Fatalf("BOOTSTRAP → CATCHING_UP should succeed")
	}
	if !m.Advance(PhaseLive) {
		t.Fatalf("CATCHING_UP → LIVE should succeed")
	}
	if m.Advance(PhaseBootstrap) {
		t.Fatalf("LIVE → BOOTSTRAP should be rejected")
	}
	if !m.Advance(PhaseShutdown) {
		t.Fatalf("LIVE → SHUTDOWN should always succeed")
	}
}

func TestSkipBootstrapWhenSnapshotPresent(t *testing.T) {
	m := New()
	if !m.Advance(PhaseCatchingUp) {
		t.Fatalf("STARTING → CATCHING_UP should succeed (snapshot path)")
	}
}

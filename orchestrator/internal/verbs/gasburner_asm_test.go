package verbs

import "testing"

// TestGasBurnerCreationCode asserts the assembled gasburnertx creation
// bytecode is internally consistent: the init code's CODECOPY/RETURN size must
// equal the worker runtime length, and the @.start offset must equal the init
// code length.
func TestGasBurnerCreationCode(t *testing.T) {
	code := gasBurnerCreationCode()

	// Init code is 11 bytes; the @.start operand (PUSH1 at byte 1) must match.
	const wantInitLen = 11
	if len(code) <= wantInitLen {
		t.Fatalf("creation code shorter than init code: %d", len(code))
	}
	if code[0] != 0x60 { // PUSH1
		t.Fatalf("byte 0: want PUSH1 0x60, got 0x%02x", code[0])
	}
	if int(code[1]) != wantInitLen {
		t.Fatalf("@.start operand: want %d, got %d", wantInitLen, code[1])
	}

	// The worker segment is everything after the init code.
	if got := gasBurnerRuntimeLen(); got != len(code)-wantInitLen {
		t.Fatalf("runtime len: helper says %d, code says %d", got, len(code)-wantInitLen)
	}

	// The deployed worker must contain a JUMPDEST at the exit (9) and loop
	// (19) offsets the jumps target.
	worker := code[wantInitLen:]
	if worker[9] != 0x5b {
		t.Fatalf("worker[9]: want JUMPDEST 0x5b (exit), got 0x%02x", worker[9])
	}
	if worker[19] != 0x5b {
		t.Fatalf("worker[19]: want JUMPDEST 0x5b (loop), got 0x%02x", worker[19])
	}
	// Worker must end in JUMP (0x56) back to the loop.
	if worker[len(worker)-1] != 0x56 {
		t.Fatalf("worker tail: want JUMP 0x56, got 0x%02x", worker[len(worker)-1])
	}
}

// TestContractCatalogComplete asserts every contract a verb targets has a
// catalog entry with non-empty init code.
func TestContractCatalogComplete(t *testing.T) {
	for verb, name := range contractVerbTargets {
		spec, ok := ContractSpecFor(name)
		if !ok {
			t.Fatalf("verb %s targets %q which has no catalog spec", verb, name)
		}
		if len(spec.InitCode) == 0 {
			t.Fatalf("contract %q (verb %s) has empty init code", name, verb)
		}
		if spec.Gas == 0 {
			t.Fatalf("contract %q (verb %s) has zero deploy gas", name, verb)
		}
	}
}

package verbs

import "encoding/binary"

// gasBurnerCreationCode returns CREATE-tx initcode for the gasburnertx
// contract. Spamoor's gasburnertx scenario has no Solidity source; it compiles
// the contract from geas templates at runtime (spamoor/scenarios/gasburnertx/
// gasburnertx.go, sendDeploymentTx). This function replicates that assembly in
// Go from the same templates with Spamoor's default options: default burn
// opcodes (PUSH2 0x1337; POP), default init opcode (PUSH1 0), and the default
// gas_remainder of 10000.
//
// The geas templates Spamoor uses:
//
//	init:   push @.start; codesize; sub; dup1; push @.start; push0;
//	        codecopy; push0; return
//	worker: <init opcode>; gas; push 0; jump @loop;
//	        exit: push 0; mstore; push 32; push 0; log1; stop;
//	        loop: push <gas_remainder>; gas; lt; jumpi @exit;
//	              push 1; add; <burn opcodes>; jump @loop
//
// The deployed runtime is the worker segment; calling it loops, burning gas
// until fewer than gas_remainder units remain, then emits one LOG1 and stops.
// gasburnertx exec txs carry a 4-byte calldata payload (the tx index); the
// contract ignores calldata, so any payload runs the same burn loop.
func gasBurnerCreationCode() []byte {
	const gasRemainder = 10_000

	// Worker runtime — assembled first so the init code can embed its length.
	// PUSH2 is used for every jump target so offsets are fixed-width and the
	// two labels (exit, loop) resolve in a single pass.
	const (
		opSTOP     = 0x00
		opADD      = 0x01
		opLT       = 0x10
		opGAS      = 0x5a
		opJUMP     = 0x56
		opJUMPI    = 0x57
		opJUMPDEST = 0x5b
		opPOP      = 0x50
		opMSTORE   = 0x52
		opLOG1     = 0xa1
		opCODECOPY = 0x39
		opCODESIZE = 0x38
		opSUB      = 0x03
		opDUP1     = 0x80
		opRETURN   = 0xf3
		opPUSH1    = 0x60
		opPUSH2    = 0x61
		opPUSH0    = 0x5f
	)

	push2 := func(v uint16) []byte {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], v)
		return []byte{opPUSH2, b[0], b[1]}
	}

	// --- worker runtime ---
	// Prologue length is fixed, so the exit and loop label offsets are known
	// statically. Layout (byte offsets within the deployed runtime):
	//   prologue: PUSH1 0 GAS PUSH1 0 PUSH2 loop JUMP = 2+1+2+3+1 = 9 bytes
	//   exit:     JUMPDEST PUSH1 0 MSTORE PUSH1 32 PUSH1 0 LOG1 STOP = 10 bytes
	//   loop:     JUMPDEST PUSH2 gr GAS LT PUSH2 exit JUMPI PUSH1 1 ADD
	//             PUSH2 0x1337 POP PUSH2 loop JUMP
	const (
		prologueLen = 9
		exitOffset  = prologueLen
		exitBodyLen = 10
		loopOffset  = exitOffset + exitBodyLen
	)

	worker := make([]byte, 0, 64)
	// prologue: stash the init opcode, push remaining gas + loop_counter,
	// then jump into the loop.
	worker = append(worker, opPUSH1, 0x00)
	worker = append(worker, opGAS)
	worker = append(worker, opPUSH1, 0x00)
	worker = append(worker, push2(loopOffset)...)
	worker = append(worker, opJUMP)
	// exit: store loop_counter at mem 0 and emit it as a 32-byte LOG1, stop.
	worker = append(worker, opJUMPDEST)
	worker = append(worker, opPUSH1, 0x00)
	worker = append(worker, opMSTORE)
	worker = append(worker, opPUSH1, 0x20)
	worker = append(worker, opPUSH1, 0x00)
	worker = append(worker, opLOG1)
	worker = append(worker, opSTOP)
	// loop: exit once gas drops below gas_remainder, else bump the counter,
	// run the burn opcodes (PUSH2 0x1337; POP) and repeat.
	worker = append(worker, opJUMPDEST)
	worker = append(worker, push2(gasRemainder)...)
	worker = append(worker, opGAS)
	worker = append(worker, opLT)
	worker = append(worker, push2(exitOffset)...)
	worker = append(worker, opJUMPI)
	worker = append(worker, opPUSH1, 0x01)
	worker = append(worker, opADD)
	worker = append(worker, push2(0x1337)...)
	worker = append(worker, opPOP)
	worker = append(worker, push2(loopOffset)...)
	worker = append(worker, opJUMP)

	// --- init code ---
	// CODECOPY the worker runtime out of the creation calldata and RETURN it.
	// @.start resolves to the init-code length (where the worker begins); the
	// init segment is < 256 bytes so it fits a PUSH1. The sequence is
	// PUSH1 start CODESIZE SUB DUP1 PUSH1 start PUSH0 CODECOPY PUSH0 RETURN,
	// which is 2+1+1+1+2+1+1+1+1 = 11 bytes.
	const initLen = 11
	startOffset := byte(initLen)

	init := make([]byte, 0, initLen)
	init = append(init, opPUSH1, startOffset)
	init = append(init, opCODESIZE)
	// CODESIZE - .start is the worker (runtime) length.
	init = append(init, opSUB)
	// DUP1 keeps a second copy of the length for RETURN's size argument.
	init = append(init, opDUP1)
	init = append(init, opPUSH1, startOffset)
	init = append(init, opPUSH0)
	init = append(init, opCODECOPY)
	init = append(init, opPUSH0)
	init = append(init, opRETURN)

	return append(init, worker...)
}

// gasBurnerRuntimeLen returns the length of the deployed runtime emitted by
// gasBurnerCreationCode; exposed for tests asserting the init code's RETURN
// size matches the worker segment.
func gasBurnerRuntimeLen() int {
	code := gasBurnerCreationCode()
	const initLen = 11
	return len(code) - initLen
}

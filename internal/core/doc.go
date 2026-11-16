// Package core holds the loop's pure decision logic: the item lifecycle,
// next-stage and merge rules, round counting and limits (TECHNICAL-SPEC §4).
// Decide is deterministic and does no I/O, so it can run inside Temporal
// workflow code. Core imports only the standard library and contract/; the
// test in imports_test.go enforces this.
package core

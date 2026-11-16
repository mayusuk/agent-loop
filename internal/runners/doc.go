// Package runners implements the Runner port, which starts stage handlers:
// the local runner (a git worktree inside Claude Code's OS sandbox) in v0.1,
// and a Docker runner from v0.2 (TECHNICAL-SPEC §7.2).
package runners

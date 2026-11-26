# Work plan: Spec follow-up from #96: Sandboxed handlers can't run go build or go test: the Go build cache is outside the run's writable paths

**Issue:** [#97](https://github.com/mayusuk/agent-loop/issues/97) (feature)
**Branch:** `issue/97-spec-follow-up-from-96-sandboxed`
**Base:** `origin/main @ bc820ec` (checked before branching)
**Status:** Draft

## Summary

Applies the owner's ruling on #96 D2: the local runner points a fixed default list of toolchain caches into each run's temp directory (now also npm, pip, the Go module cache and Cargo), and a project profile can declare more cache variables with a new `caches` key. Caches stay per run; nothing is shared between runs.

## Context

- TECHNICAL-SPEC §7.2: a run may write only the worktree, its out and result paths, and a per-run temp directory.
- #96 added `runners.ToolCaches` (`GOCACHE`, `XDG_CACHE_HOME`, `GH_CONFIG_DIR`) in `internal/runners/local.go`; the stage env (`RunSpec.Env`) and `PassEnv` win over it.
- Owner ruling on #97 (2026-11-25): a fixed default list plus profile additions; caches per run, never shared.
- Profile keys are defined in `internal/profile/profile.go` and `schema/profile.schema.json`; per-project sandbox settings reach the runner through `workflows.Config` and `ports.RunSpec` (as `egress` does).

## Approach

- `internal/runners/local.go`: add `npm_config_cache`, `PIP_CACHE_DIR`, `GOMODCACHE`, `CARGO_HOME` to `ToolCaches`. New `RunSpec.Caches` (names): each one unset by the stage env or PassEnv points at `$TMPDIR/caches/<NAME>`. Remove the run directory even when the Go module cache made parts of it read-only.
- `internal/profile`: top-level `caches` key, a list of unique variable names (schema pattern), default `[]`.
- `workflows.Config.Caches` → `RunSpec.Caches`; `cmd/agent-loop/wire.go` fills it from the profile. `project validate` reports a reserved or secret name (`cache_reserved`) using `runners.CheckCacheName`.
- Specs: TECHNICAL-SPEC §7.2 (cache list, precedence, per-run), §13 key table; INTEGRATION-SPEC §13 profile reference.

## Steps

- [ ] Runner: default list extended, profile caches, read-only cleanup, tests.
- [ ] Profile key `caches`, schema, wiring through workflows and validate, tests.
- [ ] Specs and profile docs.

## Acceptance criteria

- The runner sets `npm_config_cache`, `PIP_CACHE_DIR`, `GOMODCACHE` and `CARGO_HOME` (with `GOCACHE`, `XDG_CACHE_HOME`, `GH_CONFIG_DIR`) inside the run's temp directory by default.
- A profile can declare more cache variables; they point inside the run's temp directory.
- The stage env and PassEnv still win.
- Caches are per run, never shared between runs.
- TECHNICAL-SPEC §7.2 and §13 and the profile docs describe it.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §13, INTEGRATION-SPEC §13: the profile key is top-level `caches`, a list of environment variable names (`caches = ["BUNDLE_PATH"]`), default `[]`, unique, each matching `^[A-Za-z_][A-Za-z0-9_]*$`. Why: the runner picks the directory, so a list of names is all a project needs; a flat top-level list matches `gates` and `manual_comment_bots`, and the issue's `sandbox.caches` would be the only `sandbox` key.
- **D2** [gap] TECHNICAL-SPEC §7.2: a declared cache points at `$TMPDIR/caches/<NAME>`. The directory is not created in advance, like the defaults. Why: one place per variable, with no clash with the default subdirectories.
- **D3** [gap] TECHNICAL-SPEC §7.2: the default subdirectories are `npm`, `pip`, `go-mod` and `cargo` (with `go-build`, `cache` and `gh` from #96). Why: the variable's tool, short.
- **D4** [gap] TECHNICAL-SPEC §7.2: precedence is the stage env, then PassEnv (the worker's value), then the default list, then the profile's `caches`. A profile naming a default cache changes nothing. Why: the ruling keeps the stage env and PassEnv winning; a default already points into the temp directory.
- **D5** [gap] TECHNICAL-SPEC §13: a `caches` name the runner sets itself (`TMPDIR`, `GH_TOKEN`, ...) or an `AGENT_LOOP_SECRET_*` name is an error in `project validate` (code `cache_reserved`), and the runner refuses the run before it starts. Why: the same rule as the stage env and PassEnv.
- **D6** [gap] TECHNICAL-SPEC §7.2: `CARGO_HOME` in the temp directory also moves Cargo's config and installed binaries, so `~/.cargo/config.toml` doesn't apply in a run (the `cargo` on `PATH` still does). Why: the owner's ruling names Cargo, and Cargo has no separate cache variable.
- **D7** [gap] TECHNICAL-SPEC §7.2: the runner makes the run directory writable before removing it, because the Go module cache writes read-only directories. Why: otherwise every run that downloads a module leaves its temp directory behind.

## Verification

Standard checks, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

None.

## Outcome

_Filled in before shipping._

# Work plan: server init: config path quoting and XDG path without a home directory (follow-up from #132)

**Issue:** [#133](https://github.com/mayusuk/agent-loop/issues/133) (bug)
**Branch:** `issue/133-server-init-config-path-quoting-and-xdg`
**Base:** `origin/main @ d5b7478` (checked before branching)
**Status:** Done

## Summary

`server init` writes the deployment config's string values with a TOML encoder instead of Go's `%q`, so a path holding a byte such as 0x7f gives a config that parses. A path that is not valid UTF-8, which TOML can't hold, is refused with a clear error before anything is written. When the home directory can't be found, an absolute `$XDG_STATE_HOME` is still tried for the projection database.

## Context

- `cmd/agent-loop/serverinit.go` fills `deploymentTemplate` with `fmt.Sprintf` and `%q`. Go's `%q` writes 0x7f as `\x7f`, which TOML doesn't accept, so `LoadDeployment` fails, `server init` exits 1, and the bad file is kept on later runs (PR #132 R1).
- `pickProjectionPath` breaks out of the candidate loop when `os.UserHomeDir` fails, so it skips the absolute `$XDG_STATE_HOME` candidate too, though that needs no home (PR #132 R2).
- `internal/profile` reads TOML with `github.com/BurntSushi/toml` v1.5.0, whose encoder escapes control characters (0x00–0x1f, 0x7f) as `\uXXXX`.

## Approach

- Add `tomlString(s) (string, error)` in `serverinit.go`: it refuses invalid UTF-8 and otherwise encodes the value with `toml.Marshal` and returns the quoted string. The template uses `%s` with these encoded values. The body is built before the file is written, so a refused value leaves no file behind.
- In `pickProjectionPath`, skip only `~/` candidates when the home directory is unknown, and try absolute candidates regardless.

## Steps

- [x] Tests that fail: a config dir / XDG path holding 0x7f gives a config that loads; invalid UTF-8 is refused and nothing is written; with no home directory, an absolute `XDG_STATE_HOME` is picked.
- [x] Encode config values with the TOML encoder; refuse invalid UTF-8.
- [x] Try absolute projection candidates without a home directory.

## Acceptance criteria

- A path containing a byte such as 0x7f is written so the config parses, and this is tested.
- When the home directory can't be found, the absolute `$XDG_STATE_HOME` path is tried first.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §11 / §13 (server init): a value `server init` would write that is not valid UTF-8 (in practice a projection path from `XDG_STATE_HOME` or `--config`) is refused with an error naming it, and no config is written. Why: TOML files must be valid UTF-8, so the value can't be written faithfully; refusing keeps a broken file from being kept on later runs.
- **D2** [open] TECHNICAL-SPEC §13: with no home directory, the default `api.socket` (`~/.config/agent-loop/api.sock`) can't be expanded, so `server init --config <path>` still fails when it loads the config it wrote. This issue only fixes the projection path choice.

## Verification

Full local suite (gofmt, vet, build, `go test -race`), plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

None.

## Outcome

- `server init` now writes every config value through `tomlString`, which uses the BurntSushi TOML encoder (the library `internal/profile` decodes with) and refuses invalid UTF-8 before the file is written.
- `pickProjectionPath` skips only `~/` candidates when the home directory is unknown, so an absolute `$XDG_STATE_HOME` is tried first.
- Tests: a 0x7f byte in the projection path round-trips; invalid UTF-8 is refused and no config is written (end to end on file systems that allow such names, such as Linux ext4; macOS APFS refuses them, so there it is covered by the `tomlString` unit test); with no home directory the XDG path is picked, otherwise the config directory. The new tests fail on the old code.
- No deviations from the approach. No follow-up issues filed; D2 is recorded for the spec follow-up.

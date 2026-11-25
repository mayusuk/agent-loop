# Work plan: Test gaps left from the reviews of #107 and #109

**Issue:** [#112](https://github.com/mayusuk/agent-loop/issues/112) (bug)
**Branch:** `issue/112-test-gaps-left-from-the-reviews-of-107`
**Base:** `origin/main @ f2861f7` (checked before branching)
**Status:** Done

## Summary

Adds the three tests the reviews of #107 and #109 found missing: the app-secret cache re-reads an invalid value once it is fixed, an address run gets the coder signature, and a coder run's handler gets `GIT_CONFIG_NOSYSTEM=1`.

## Context

The code paths exist and are believed correct; only the tests are missing (PR #107 R1, PR #109 R1 and R2). `internal/secrets/githubapp.go` caches each identity's app secrets per process (#94) and must not cache a failed read or parse. `internal/workflows/stage.go` sets `RunSpec.Author` for coder stages (implement and address). `internal/runners/local.go` `authorEnv` sets `GIT_CONFIG_NOSYSTEM=1` with the author (work plan #102 D1). Root cause: none in the code unless a new test fails; the gap is test coverage.

## Approach

Extend `TestAppSecretsAreReadOncePerProcess` (internal/secrets) with invalid `app_id` "abc" and `installation_id` "x" cases that fail, are fixed, and then succeed. Extend `TestRunAuthorIsTheCoderSignature` (internal/workflows) with an address run. Extend `TestHandlerCommitsAsTheAuthor` (internal/runners) to check that a coder run's handler environment has `GIT_CONFIG_NOSYSTEM=1` and a run-owned `GIT_CONFIG_GLOBAL`. If any test fails, fix the code in the same branch.

## Steps

- [x] Secret cache: an invalid app or installation ID is read again once fixed.
- [x] Address run gets the coder signature.
- [x] Coder run's handler gets `GIT_CONFIG_NOSYSTEM=1`.

## Acceptance criteria

- An invalid `app_id` ("abc") or `installation_id` ("x") fails the call; after fixing the value the next call reads it again and succeeds.
- A test checks that an address run's `RunSpec.Author` is the coder signature.
- A test checks that `GIT_CONFIG_NOSYSTEM=1` reaches the handler.

## Decisions

None.

## Verification

Standard checks plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

None.

## Outcome

Added the three tests; all passed against the existing code, so no bug was found and no code changed. A mutation check (dropping `GIT_CONFIG_NOSYSTEM=1` from `authorEnv`) makes the runner test fail. The secret-cache test also counts reads, so it fails if an invalid value were cached. No deviations, no follow-up issues.

# Work plan: wf plan-issues and agent-loop disagree on ready labels and dependencies

**Issue:** [#104](https://github.com/mayusuk/agent-loop/issues/104) (bug)
**Branch:** `issue/104-wf-plan-issues-and-agent-loop-disagree`
**Base:** `origin/main @ 48f3dfd` (checked before branching)
**Status:** Draft

## Summary

Issues made by the wf `plan-issues` skill carry the label `status:ready` and "Blocked by #n" body text, which agent-loop doesn't read. `agent-loop init` now offers to map the ready marker to an existing `status:ready` label, `project validate`'s "Blocked by" warning names the `gh api` call (and the GitHub UI) that adds the native dependency, and GETTING-STARTED explains both.

## Context

- INTEGRATION-SPEC I3, §9.5: dependencies are native GitHub dependencies; body text isn't read, and `project validate` warns about it (`text_dependency`, `internal/adapters/github/validate.go`).
- INTEGRATION-SPEC §12.2 step 3: init writes the profile with the default markers; step 4 creates the labels of the profile's marker mapping. TECHNICAL-SPEC §13 `[markers]` lets each marker map to any label.
- Root cause: init always writes `ready = { label = "ready" }` and creates a `ready` label, even when the repository already uses `status:ready`; the validate hint says only "use the relationship", not how.
- Tests use the `githubtest` stub (`cmd/agent-loop/init_test.go`, `internal/adapters/github/*_test.go`).

## Approach

- `internal/adapters/github/setup.go`: `Setup.ReadyLabelOffer(ctx, ready)` returns `status:ready` when the repository has it and no label named `ready` (case-insensitive). The label listing is shared with `Labels`.
- `cmd/agent-loop/init.go` step 3, new profile only: ask "Map the ready marker to ...?" before writing; `--yes` accepts, `--check` doesn't ask. `profileText` takes the marker mapping. Step 4 then creates labels from the mapping, so no `ready` label is created.
- `internal/adapters/github/validate.go`: the `text_dependency` hint lists one `gh api -X POST .../dependencies/blocked_by -F issue_id=$(gh api .../issues/<m> --jq .id)` per blocker number in the text, and the GitHub UI path.
- `docs/GETTING-STARTED.md`: "Issues created by other tools" under step 3.

## Steps

- [x] Tests: `ReadyLabelOffer` cases, init maps with `--yes`, declined and not-offered cases, validate hint names the fix.
- [x] `ReadyLabelOffer` and the init offer.
- [x] Validate hint.
- [x] GETTING-STARTED section.

## Acceptance criteria

- `agent-loop init` offers, when the repository has a `status:ready` label and no `ready` label, to map the profile's ready marker to it; `--yes` takes the offer.
- `project validate`'s warning about "Blocked by" text without a native dependency names how to fix it (the `gh api` call, or the GitHub UI).
- GETTING-STARTED has a short section on using issues created by other tools.

## Decisions

- **D1** [gap] INTEGRATION-SPEC §12.2 step 3: init offers to map the ready marker only to the label `status:ready` (the wf convention), matched case-insensitively, and only when the repository has no label named `ready`. Other ready-like labels aren't detected. Why: the spec says step 3 writes the default markers and is silent on adopting existing labels; the issue names this one case.
- **D2** [gap] §12.2 step 3: the offer is made only when init writes a new profile; an existing profile is kept as it is, as step 3 already does. With `--yes` it is taken, with `--check` it isn't asked (as for the admins-as-owners question), and declining keeps `ready`. The other wf labels (`status:in-progress` and so on) aren't mapped. Why: only the ready marker decides what is picked; the loop's other markers are its own.
- **D3** [gap] INTEGRATION-SPEC §9.5 / `text_dependency`: the hint gives one `gh api -X POST repos/<repo>/issues/<n>/dependencies/blocked_by -F issue_id=$(gh api repos/<repo>/issues/<m> --jq .id)` per distinct blocker number on the "Blocked by" lines, and the GitHub UI path (Relationships, "Mark as blocked by"). Why: the spec requires the warning but not its fix text; the REST call takes the blocker's ID, not its number.

## Verification

gofmt, vet, build, `go test -race -count=1 ./...`, `GOOS=linux go build ./...`, `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

None: the wf skill creating native dependencies is out of scope here (it lives outside this repository).

## Outcome

_Filled in before shipping._

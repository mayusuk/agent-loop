# Work plan: Define the ports and common types (`internal/ports`)

**Issue:** [#8](https://github.com/mayusuk/agent-loop/issues/8) (feature)
**Branch:** `issue/8-define-the-ports-and-common-types`
**Base:** `origin/main @ 399150e` (checked before branching)
**Status:** Done

## Summary

Adds the Go interfaces and types of INTEGRATION-SPEC §8 (`WorkTracker`, `CodeHost` and their types) and the internal ports of TECHNICAL-SPEC §5 (`Workspace`, `Runner`, `SecretStore`, `Notifier`) to `internal/ports`, with classified errors. Tracker and agent text is carried as `UntrustedText`, whose content can be read only through the separate package `internal/ports/untrusted`, which `internal/core` can't import.

## Context

- INTEGRATION-SPEC §8.1–§8.4 is the source of truth for the method sets. TECHNICAL-SPEC §5 sketches the internal ports. Plan task T3.
- TECHNICAL-SPEC §4 and NFR-4 AC3: the core has no operation that reads untrusted text.
- TECHNICAL-SPEC §3.1 and plan D1: `internal/core` imports only the standard library and `contract/`, enforced by `internal/core/imports_test.go`, which already rejects every `internal/...` path, so core can't import `internal/ports` at all.
- The spec names many supporting types without fields (`Item`, `Change`, `NewItem`, `ReviewPost`, `HostCaps`, ...). Their fields come from the method comments and the sections that use them (§6, §9, §13). Each choice is in Decisions.
- `contract/` (T2, #7) is being built in parallel, so `ports` does not import it.

## Approach

- `internal/ports/` files: `types.go` (common types, markers, records, identities), `errors.go`, `tracker.go` (`WorkTracker`, items, item events), `codehost.go` (`CodeHost`, changes, change events, reviews, manual comments, gates, merge, trust), `internal.go` (`Workspace`, `Runner`, `SecretStore`, `Notifier`), `untrusted.go` (the `UntrustedText` alias).
- `internal/ports/untrusted/`: the `Text` type with an unexported field, `New` and `Read`. `ports.UntrustedText` is an alias of `untrusted.Text`, so the type itself has no accessor and only code that imports `untrusted` can read it. `String`/`GoString` return a placeholder so `fmt` never prints the content. A test limits the importers of `untrusted` to adapters, the worker (`internal/workflows`) and `conformance/`.
- Errors: sentinel errors for the five classes. `ErrRateLimited` is matched through `*RateLimitError`, which carries `RetryAt` and wraps a cause.
- Event variants are a sealed interface (`ItemEventDetail`, `ChangeEventDetail`) inside a struct carrying the shared fields.
- Tests: a method-set test that parses the Go blocks of INTEGRATION-SPEC §8 and compares `WorkTracker`, `CodeHost` and the §8.1 types with the code, method for method and in order (the internal ports, whose spec is a sketch, are listed by reflection); `errors.Is`/`errors.As` tests; `UntrustedText` tests (no exported fields, no method returning content, no leak through `fmt` or `encoding/json`); a core test that the guard refuses both `internal/ports` and `internal/ports/untrusted`.

## Steps

- [x] Add `internal/ports/untrusted` and the `UntrustedText` alias, with leak tests and the importer allowlist test.
- [x] Add the core guard test for NFR-4 AC3.
- [x] Add the error classes with tests.
- [x] Add the common types, `WorkTracker`, `CodeHost` and their types.
- [x] Add the internal ports.
- [x] Add the method-set test.

## Acceptance criteria

- The interfaces match INTEGRATION-SPEC §8 method for method. A test lists the method sets, so drift shows up.
- `errors.Is` works for each error class, and `ErrRateLimited` exposes its retry time.
- A test proves that `internal/core` can't read the contents of an `UntrustedText`, through the import guard plus an API with no accessor (NFR-4 AC3).

## Decisions

- **D1** [ambiguity] TECHNICAL-SPEC §4: "tracker text reaches the core only as `UntrustedText`" is read together with §3.1, which keeps `internal/core` from importing `internal/ports`. So no `UntrustedText` value reaches the core at all: workflows pass the core references (comment IDs, finding IDs), and the worker composes and posts text. Why: the import rule is enforced, and a reference is the "pass it on by reference" of §4. If T5 needs an opaque text handle in core events, it must live in `contract/` (open for T5).
- **D2** [gap] TECHNICAL-SPEC §4, plan T3: the accessor package is `internal/ports/untrusted` (`untrusted.New(string) Text`, `untrusted.Read(Text) string`), and `ports.UntrustedText` is an alias of `untrusted.Text`. Only `internal/adapters/...`, `internal/workflows/...` and `conformance/...` may import it, checked by a test. Why: "only adapters and the worker can read it"; conformance must check C11 against content.
- **D3** [gap] TECHNICAL-SPEC §4: `UntrustedText` prints as `[untrusted text]` through `fmt` (`String`, `GoString`), and `encoding/json` encodes it as `{}`, dropping the content. So untrusted text never enters logs or Temporal workflow history; an activity that needs the text reads it from the adapter. Why: both are paths that would otherwise read the content without the accessor.
- **D4** [gap] INTEGRATION-SPEC §8: the five error classes are sentinel errors matched with `errors.Is`. `ErrRateLimited` errors are `*RateLimitError{RetryAt, Err}`, and `ports.RetryAt(err)` returns the time. Why: the spec names the classes and the retry time but not the Go shape.
- **D5** [gap] INTEGRATION-SPEC §8.2, §8.3: names that clash in one Go package are prefixed. Item events `Closed` and `Reopened` are `ItemClosed` and `ItemReopened`; change events `Closed` and `Reopened` are `ChangeClosed` and `ChangeReopened`. Enum constants carry their type's name: `MergeMerged`, `MergeHeadMoved`, `MergeNotMergeable`, `MergeFailed`; `PusherCoder`, `PusherOwner`, `PusherUnknown`; `GatePass`, `GateFail`, `GatePending`, `GateMissing`; `CloseCompleted`, `CloseNotDone`, `CloseOpen`; `ActorHuman`, `ActorBot`; `IdentityCoder`, `IdentityReviewer`, `IdentityLoop`; `MergeMethodSquash`, `MergeMethodMerge`, `MergeMethodRebase`; `ChangeStateOpen`, `ChangeStateClosed`, `ChangeStateMerged`; `ClearedByNone`, `ClearedByHumanResolved`, `ClearedByHumanDismissed`. String values are lower snake case (`not_mergeable`, `human_resolved`). Why: `Merged`, `HeadMoved`, `Coder`, `Closed` and `Reopened` each have two meanings in §8.
- **D6** [gap] INTEGRATION-SPEC §8.2, §8.3: an event is a struct with the shared fields (`ItemEvent{Item, At, Version, Detail}`, `ChangeEvent{Change, At, Detail}`) and a sealed `Detail` interface whose variants are the spec's event types. Why: the spec says each event "carries the item ref, the time and the item's new Version"; change events get the ref and time, since `Version` is defined only for items.
- **D7** [gap] INTEGRATION-SPEC §6.1, §8: `Record` is `{Type RecordType, Key IdemKey, Data json.RawMessage, Author Actor, Ref CommentRef, At time.Time}`, where `Data` is the marker's whole JSON object. `contract/` (T2) encodes and decodes `Data`; adapters only place, find and author-check it. `RecordType` constants are the 11 types of §6.1. Why: keeps `ports` independent of `contract/` while T2 is in flight, and records are written only by the loop's identities, so their JSON is trusted.
- **D8** [gap] INTEGRATION-SPEC §8.1: `Actor.ID`, `OwnerAccount.ID` and the other account IDs are strings, so a numeric GitHub ID and a Jira `accountId` share one type. `Marker` constants use the §8.1 spellings (`in_progress`, `type:feature`, ...).
- **D9** [gap] INTEGRATION-SPEC §8.2: `Item` has `Ref`, `URL`, `Markers`, `CloseReason` (`CloseOpen` while open, so there is no separate state field), `ClosedBy`, `Assignees`, `CreatedAt` (for the `oldest`/`newest` priority, FR-2 AC2) and `Version`. It carries no title or body. `Dependency` has `Ref`, `CloseReason`, `ClosedBy` and `ChangeMerged`. `NewItem` has `Markers`, `Title` and `Body` as `UntrustedText`, `Record` and `Since`. Why: the method comments list these fields; text stays out of the item so it can't reach decisions.
- **D10** [gap] INTEGRATION-SPEC §8.2, §8.3, §13: `Validate` returns `[]Problem{Severity (error, warning), Code, Message, Hint}`. `ProfileTracker` holds the adapter name, repository, marker mapping, identities, owner accounts and an adapter-specific `Settings` map; `ProfileCodeHost` adds the base branch, branch prefix, gates, manual-comment bots, merge method and release apps. Why: plan T11 asks for "each problem with a fix hint", and TECHNICAL-SPEC §13 says adapter-specific settings are checked by the adapter.
- **D11** [gap] INTEGRATION-SPEC §8.3: `HostCaps` has `PushRule` (the code host enforces a push rule that is the FR-13 AC5 push record, D40), `FileLevelComments` (file-level threads, I8) and `MaxBody`. Why: the spec names `HostCaps` without fields; these are the code-host features the specs branch on.
- **D12** [gap] INTEGRATION-SPEC §8.3, §9.8: `TrustReport` is `{Causes []TrustCause, Commits []Commit}`. Causes are `fork`, `branch_outside_prefix`, `ruleset_missing`, `ruleset_changed` (includes disabled, I9), `admin_not_owner`, `pushed_before_ruleset` and `pusher_unknown`. `Commits` are the commits not reachable from the base tip that the report names. The adapter does not apply owner approvals: the core removes approved commits (FR-13 AC6) and never approves a fork. Why: `Trust` takes only the change ref, and approvals are tracker records the core already holds.
- **D13** [gap] INTEGRATION-SPEC §8.3: `Change` has `Ref`, `URL`, `Item`, `Branch`, `Base`, `Head`, `State` (open, closed, merged), `Fork`, `Author`, `MergedBy`, `MergedAt` and `Markers`. `NewChange` has `Item`, `Branch`, `Base`, `Title` and `Body` (`UntrustedText`, from the handler's result) and the `change` `Record`. `ReviewPost` has `Commit`, `Approve`, `Summary` (`UntrustedText`), `Findings` (each with `ID`, `Score`, `Status`, `Path`, `Line`, `Body` and its `finding` `Record`; `Line` 0 is file level and an empty `Path` is a review-text finding, I8) and the `review` `Record`; the adapter builds the status table. `ReviewRecord` has `Ref`, `Author`, `SubmittedAt`, `Record` and `Threads` (finding ID to thread). `ManualComment` adds `Thread` and `CreatedAt` to the §8.3 fields. Why: the method comments name these facts; `Approve` is FR-4 AC7.
- **D14** [gap] TECHNICAL-SPEC §5: the internal ports follow the sketch exactly; their argument types are minimal and owned by the tasks that implement them (T12 `SecretStore`, T13 `Workspace`/`Runner`, T14 `Notifier`), which may add fields. `Token` keeps its secret in an unexported field read with `Secret()`, and prints redacted. Why: the spec gives only the method sketch, and FR-13 AC3 forbids tokens in logs.

## Verification

Standard checks. `go doc ./internal/ports` reads cleanly.

## Risks and open questions

- D1 shapes how T5 models text in core events. Flagged for the reviewer.

## Outcome

- Added `internal/ports` (`types.go`, `errors.go`, `tracker.go`, `codehost.go`, `internal.go`, `untrusted.go`) and `internal/ports/untrusted`, plus `internal/core/untrusted_guard_test.go`.
- The method-set test reads `specs/INTEGRATION-SPEC.md` itself, so a change to the spec's §8 code blocks without the code (or the reverse) fails CI.
- Beyond the plan: `Token` keeps its secret unexported and prints redacted (D14), tested under FR-13 AC3.
- No follow-up issues filed. D1 matters to T5 (#10) and is flagged in the PR.

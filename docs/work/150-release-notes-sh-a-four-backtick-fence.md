# Work plan: release-notes.sh: a four-backtick fence is closed by a three-backtick line (follow-up from #145 R4)

**Issue:** [#150](https://github.com/mayusuk/agent-loop/issues/150) (bug)
**Branch:** `issue/150-release-notes-sh-a-four-backtick-fence`
**Base:** `origin/main @ 56b540a` (checked before branching)
**Status:** Done

## Summary

`scripts/release-notes.sh` now tracks code fences the way CommonMark does: a fence opens on a run of three or more backticks or tildes indented at most three spaces, and only a run of the same character, at least as long, with nothing after it, closes it. A ```` ``` ```` line inside a ```` ```` ```` block no longer closes the block, so a `## ` line after it can't cut the release notes short.

## Context

- #141 / PR #145 added the script; its D6 says a `## ` line inside a fence is not a heading. R4 of the PR #145 review found the fence tracking compares only the first three characters.
- Root cause: the awk program matches `/^[[:space:]]*```/` and stores the marker ```` ``` ````, so any line starting with three backticks toggles the fence. A ```` ```` ```` opener is closed by a ```` ``` ```` line, and a closer with an info string (```` ```sh ````) closes it too. Indentation of any width, including tabs, counts.
- CommonMark 0.31 §4.5: opener = up to 3 spaces of indent, then 3+ of `` ` `` or `~` (a backtick opener's info string may not contain a backtick); closer = up to 3 spaces of indent, then a run of the same character at least as long as the opener, followed only by spaces or tabs. An unclosed fence runs to the end of the document.
- The script must run under macOS BSD awk and Ubuntu's mawk 1.3.3, which has no regex interval expressions (`{0,3}`), so the indent and run lengths are counted with `substr`.

## Approach

Replace the two marker rules in the awk program with a per-line scan: count leading spaces (stop at 4), then the run of `` ` `` or `~` that follows, and the rest of the line. Outside a fence, a run of 3+ opens one (unless it is backticks and the rest holds a backtick), recording its character and length. Inside one, a run of the same character, at least as long, with only spaces or tabs after it, closes it. The heading checks stay as they are and still run against the fence state before the line. Tests go in `cmd/agent-loop/release_test.go` next to the #141 ones.

## Steps

- [x] Add `TestIssue150_` tests for nested and mismatched fences (fails on the old script), then fix the awk fence tracking.

## Acceptance criteria

- A ```` ``` ```` line inside a ```` ```` ```` block does not close it; a `## ` line after it is not a heading.
- A fence closes only on the same character (backtick vs tilde), at least as long as the opener, with nothing but spaces or tabs after it.
- Fences indented up to 3 spaces count; 4 or more spaces do not.
- Works with BSD awk and mawk.

## Decisions

- **D1** [gap] #141 D6, fence rules: fences follow CommonMark §4.5 (indent of at most 3 spaces; closer of the same character, at least as long, with only spaces or tabs after it; a backtick opener's info string may not contain a backtick). A line indented 4 or more spaces, or with a tab, is not a fence, including fences inside list items, whose content indent the script does not track. Why: the issue asks for CommonMark's closing rule; tracking list containers would need a Markdown parser, and a list item's fenced lines are themselves indented, so a `## ` line at column 0 ends the item anyway.

## Verification

Run the tests with macOS `/usr/bin/awk` (BSD) and with mawk (Homebrew, by putting it first on PATH as `awk`).

## Risks and open questions

The same script lives in mayusuk/agent-loop-handlers (#60); that copy is not changed here.

## Outcome

`scripts/release-notes.sh` scans each line for a fence run (indent, character, length) with `substr` instead of matching a three-character prefix, and closes a fence only per CommonMark. `TestIssue150_ReleaseNotesFencesFollowCommonMark` covers 17 cases (the issue's four-backtick case, mismatched characters, longer closers, info strings on closers, trailing whitespace, 3 vs 4 space and tab indents, backticks in info strings, two-backtick runs); 7 of them failed on the old script. The #141 tests still pass. Checked with macOS `/usr/bin/awk` and Homebrew mawk 1.3.4. No deviations from the approach; no follow-up issues (the agent-loop-handlers copy is already tracked there as #60).

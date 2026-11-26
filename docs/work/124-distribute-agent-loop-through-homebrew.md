# Work plan: Distribute agent-loop through Homebrew

**Issue:** [#124](https://github.com/mayusuk/agent-loop/issues/124) (feature)
**Branch:** `issue/124-distribute-agent-loop-through-homebrew`
**Base:** `origin/main @ 415bdfd` (checked before branching)
**Status:** Done

## Summary

agent-loop gets a release pipeline and a Homebrew tap. Pushing a `v*` tag runs GoReleaser in GitHub Actions: it builds macOS (arm64, amd64) and Linux (amd64, arm64) binaries, publishes a GitHub release, and writes the `agent-loop` formula to `mayusuk/homebrew-tap`. GETTING-STARTED makes `brew install mayusuk/tap/agent-loop` the first install path.

## Context

- Owner decision on #95 (2026-11-25): agent-loop is distributed through Homebrew. TECHNICAL-SPEC §7.2 says the formula runs `agent-loop sign` after install and upgrade (#124).
- `agent-loop sign` (#95, `cmd/agent-loop/sign.go`) signs the binary with the self-signed `agent-loop code signing` certificate in the owner's login keychain, creating it on first use. macOS may ask once whether `codesign` may use the key.
- The Keychain backend needs cgo on macOS (`internal/secrets/keychain_darwin.go` is `darwin && cgo`; without cgo the Keychain backend is unavailable). The rest is pure Go (modernc SQLite), so Linux cross-compiles with `CGO_ENABLED=0`.
- `main.version` is set at link time with `-X main.version=<v>` (`cmd/agent-loop/version.go`).
- Homebrew runs `post_install` in a sandbox that denies reads of the home directory (`Sandbox#deny_read_home` in Homebrew 7), so it can't reach `~/Library/Keychains/login.keychain-db`, and can't show the key-access prompt.
- `mayusuk/agent-loop-handlers` is private and has no releases, so the `agent-loop-claude` formula can't be written yet.
- `.github/workflows/ci.yml` is unchanged.

## Approach

- `.goreleaser.yaml` (GoReleaser v2, pinned to v2.20.3 in the workflow): two builds of `./cmd/agent-loop`, darwin arm64/amd64 with `CGO_ENABLED=1`, and linux amd64/arm64 with `CGO_ENABLED=0`; `-s -w -X main.version={{.Version}}`; tar.gz archives `agent-loop_<version>_<os>_<arch>.tar.gz`; `checksums.txt`; a `brews` entry that writes `Formula/agent-loop.rb` to `mayusuk/homebrew-tap` with a token from `HOMEBREW_TAP_GITHUB_TOKEN`, a `test` that runs `agent-loop version`, and caveats on macOS telling the owner to run `agent-loop sign` after install and every upgrade.
- `.github/workflows/release.yml`: on `push` of `v*` tags only, on `macos-latest` (cgo for both darwin arches with the host clang; Linux is pure Go), runs `goreleaser release --clean` with `GITHUB_TOKEN` (contents: write) and `HOMEBREW_TAP_GITHUB_TOKEN`.
- Tap repository `mayusuk/homebrew-tap` (public): README and an empty `Formula/` directory. GoReleaser writes the formula there on the first release.
- GETTING-STARTED §2: `brew install mayusuk/tap/agent-loop` then `agent-loop sign`; the source install stays as the alternative; the handlers stay a source install until they're released.
- `docs/RELEASING.md` is not added: the owner steps go in the PR body and in the tap README.

## Steps

- [x] Plan committed.
- [x] `.goreleaser.yaml` and `.github/workflows/release.yml`; validated with `goreleaser check` and `goreleaser release --snapshot --clean`.
- [x] Create `mayusuk/homebrew-tap` with a README and `Formula/`.
- [x] GETTING-STARTED §2 install path; the signing comment in `sign.go` brought in line with the caveat (the spec text is left to the spec follow-up, D1).
- [x] Outcome; full local checks.

## Acceptance criteria

- A tap repository with a formula for `agent-loop` (and `agent-loop-claude` when the handlers have a release).
- Tagged releases build macOS (arm64, amd64) and Linux binaries and update the formula.
- The installed binary is signed with the stable self-signed certificate from #95, so Keychain items keep trusting it across `brew upgrade` (formula `post_install` or caveat that runs `agent-loop sign`).
- GETTING-STARTED: `brew install` is the first install path; the source install stays.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §7.2, signing after install: the formula shows a caveat ("run `agent-loop sign` after install and after every `brew upgrade`") instead of running `agent-loop sign` in `post_install`. Why: Homebrew runs `post_install` in a sandbox that denies reads of the home directory, so it can't reach the login keychain that holds the certificate, and the first signing may need the owner to click **Always Allow**. The items keep their trust across upgrades: after `agent-loop sign` an upgraded build matches the same requirement, with no new approval. §7.2 says "the Homebrew formula runs it after install and upgrade" and should say the formula tells the owner to run it.
- **D2** [gap] Release targets: macOS arm64 and amd64 built with cgo (needed for the Keychain backend), Linux amd64 and arm64 built without cgo. No Windows. Why: the issue names macOS (arm64, amd64) and Linux without arches; NFR-3 covers macOS and Linux.
- **D3** [gap] Release mechanics: GoReleaser v2.20.3, triggered only by pushing a `v*` tag, on `macos-latest`; archives `agent-loop_<version>_<os>_<arch>.tar.gz` plus `checksums.txt`; the version printed by `agent-loop version` is the tag without the `v`. Why: the issue says "for example GoReleaser" and leaves the rest open.
- **D4** [gap] Tap name and token: the tap is `mayusuk/homebrew-tap` (`brew install mayusuk/tap/agent-loop`); the release workflow pushes the formula with a `HOMEBREW_TAP_GITHUB_TOKEN` repository secret, a token with `contents: write` on the tap only. Why: `GITHUB_TOKEN` can't write to another repository.
- **D5** [open] `agent-loop-claude` formula: not added, because `mayusuk/agent-loop-handlers` is private and has no release. Until then GETTING-STARTED keeps the source install for the handlers. A release pipeline in that repository can add `Formula/agent-loop-claude.rb` to the same tap (TECHNICAL-SPEC §15, X7).
- **D6** [open] Release-time tag protection: INTEGRATION-SPEC §9.8 (I10) puts a tag ruleset on adopter repositories; whether agent-loop's own repository protects `v*` tags (so only the owner can start a release) isn't specified.
- **D7** [gap] GoReleaser's `brews` (formula) section is used although GoReleaser v2.10+ marks it deprecated in favour of `homebrew_casks`, so `goreleaser check` reports "configuration is valid, but uses deprecated properties" and exits non-zero. Why: the issue and TECHNICAL-SPEC §7.2 name a formula, a formula covers Linux, and the GoReleaser version is pinned, so the deprecation can't break a release. Moving to a cask would be a spec change.

## Verification

- `goreleaser check` and `goreleaser release --snapshot --clean` locally (no publish); inspect the generated formula and check `agent-loop version` in a built archive.
- `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- The release workflow can't be run before the first tag; the snapshot run covers the build and the formula template.
- GoReleaser marks `brews` as deprecated (D7). A future GoReleaser major may remove it; the pin protects releases until then.

## Outcome

- Added `.goreleaser.yaml` (GoReleaser v2.20.3) and `.github/workflows/release.yml` (only on `v*` tags). `ci.yml` is unchanged. `dist/` is gitignored.
- `goreleaser check`: the configuration is valid but uses the deprecated `brews` section, so it exits non-zero (D7). `goreleaser release --snapshot --clean` succeeded: four archives, `checksums.txt` and `dist/homebrew/Formula/agent-loop.rb` (Ruby syntax OK); the darwin arm64 binary prints `agent-loop 0.0.0-SNAPSHOT-<sha>`.
- Created the public tap `https://github.com/mayusuk/homebrew-tap` with a README and an empty `Formula/` (GoReleaser writes `agent-loop.rb` on the first release).
- Deviation: the formula has a caveat, not a `post_install` that runs `agent-loop sign`, because Homebrew's post-install sandbox can't read the home directory (D1).
- Not done: the `agent-loop-claude` formula (D5); the handlers repository is private with no release.
- Owner steps before the first release: add the `HOMEBREW_TAP_GITHUB_TOKEN` secret (D4), then push a `v*` tag.

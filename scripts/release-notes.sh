#!/bin/sh
# Prints release notes from CHANGELOG sections: the body of a "## <version>"
# section, without the heading and the blank lines at either end (#141). The
# release workflow passes them to GoReleaser with --release-notes, so the
# GitHub release shows the CHANGELOG, not a commit list.
#
# Usage: scripts/release-notes.sh <tag-or-version> [changelog]
#   scripts/release-notes.sh v0.2.0 > notes.md     (from the repository root)
#   scripts/release-notes.sh v0.2.0 handlers/CHANGELOG.md
#
# With a changelog, prints that file's section. Without one, prints a release's
# notes: one tag releases agent-loop and agent-loop-claude at the same version
# (work plan #179 D1, D2), so the notes are CHANGELOG.md's section, then
# "## agent-loop-claude <version>" and handlers/CHANGELOG.md's section. That
# also fails when <version> is not Release in
# handlers/cmd/agent-loop-claude/main.go, so agent-loop-claude's `version`
# can't disagree with its tag (agent-loop-handlers #58 D2). A fence left open
# at the end of either section is closed there, so it does not swallow the
# agent-loop-claude heading or the release footer (work plan #179 D9).
#
# Exits 1, printing nothing to stdout, if a section is missing or empty, so
# a release without notes fails before it publishes anything.
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ] || [ -z "$1" ]; then
	echo "usage: $0 <tag-or-version> [changelog]" >&2
	exit 2
fi
version=${1#v}

# section <changelog> [close]: prints the body of its "## $version" section,
# or fails. With close, a fence still open at the end of the section is closed
# there, so text printed after it is not part of the code block (work plan
# #179 D9).
section() {
	if [ ! -r "$1" ]; then
		echo "release-notes: can't read $1" >&2
		return 1
	fi
	# A "## " line inside a ``` or ~~~ code fence is not a heading (#141 D6).
	# Fences follow CommonMark (#150 D1): an opener is a run of 3 or more ` or ~
	# indented at most 3 spaces (a ` opener's info string has no `); only a run
	# of the same character, at least as long and indented at most 3 spaces,
	# with nothing but spaces or tabs after it, closes it. fence and flen hold
	# the open fence's character and length. Plain substr scanning, because
	# mawk 1.3.3 has no regex intervals.
	body=$(awk -v want="## $version" -v closefence="${2:-}" '
		fence == "" && $0 == want { found = 1; next }
		found && fence == "" && /^## / { exit }
		{
			ind = 0
			while (ind < 4 && substr($0, ind + 1, 1) == " ") ind++
			c = substr($0, ind + 1, 1); n = 0
			if (ind < 4 && (c == "`" || c == "~"))
				while (substr($0, ind + n + 1, 1) == c) n++
			if (n >= 3) {
				rest = substr($0, ind + n + 1)
				if (fence == "") {
					if (c == "~" || index(rest, "`") == 0) { fence = c; flen = n }
				} else if (c == fence && n >= flen && rest ~ /^[ \t]*$/) {
					fence = ""
				}
			}
		}
		found {
			if ($0 ~ /^[[:space:]]*$/) { blank++; next }
			if (started) for (; blank > 0; blank--) print ""
			blank = 0; started = 1; print
		}
		END {
			if (closefence != "" && started && fence != "") {
				s = ""
				for (i = 0; i < flen; i++) s = s fence
				print s
			}
		}
	' "$1")
	if [ -z "$body" ]; then
		echo "release-notes: $1 has no \"## $version\" section, or it is empty" >&2
		return 1
	fi
	printf '%s\n' "$body"
}

if [ $# -eq 2 ]; then
	section "$2"
	exit 0
fi

main=handlers/cmd/agent-loop-claude/main.go
if [ ! -r "$main" ]; then
	echo "release-notes: can't read $main (run from the repository root)" >&2
	exit 1
fi
release=$(sed -n 's/^const Release = "\(.*\)"$/\1/p' "$main")
if [ "$release" != "$version" ]; then
	echo "release-notes: version $version is not Release ($release) in $main" >&2
	exit 1
fi
core=$(section CHANGELOG.md close)
handlers=$(section handlers/CHANGELOG.md close)
printf '%s\n\n## agent-loop-claude %s\n\n%s\n' "$core" "$version" "$handlers"

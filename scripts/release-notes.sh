#!/bin/sh
# Prints the release notes for one version: the body of its "## <version>"
# section in CHANGELOG.md, without the heading and the blank lines at either
# end (#141). The release workflow passes them to GoReleaser with
# --release-notes, so the GitHub release shows the CHANGELOG, not a commit list.
#
# Usage: scripts/release-notes.sh <tag-or-version> [changelog]
#   scripts/release-notes.sh v0.1.0 > notes.md
#
# Exits 1, printing nothing to stdout, if the section is missing or empty, so
# a release without notes fails before it publishes anything.
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ] || [ -z "$1" ]; then
	echo "usage: $0 <tag-or-version> [changelog]" >&2
	exit 2
fi
version=${1#v}
changelog=${2:-CHANGELOG.md}
if [ ! -r "$changelog" ]; then
	echo "release-notes: can't read $changelog" >&2
	exit 1
fi

notes=$(awk -v want="## $version" '
	$0 == want { found = 1; next }
	found && /^## / { exit }
	found {
		if ($0 ~ /^[[:space:]]*$/) { blank++; next }
		if (started) for (; blank > 0; blank--) print ""
		blank = 0; started = 1; print
	}
' "$changelog")

if [ -z "$notes" ]; then
	echo "release-notes: $changelog has no \"## $version\" section, or it is empty" >&2
	exit 1
fi
printf '%s\n' "$notes"

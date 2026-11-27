#!/bin/sh
# Checks every plan under plans/ with agent-loop plan validate, and fails if
# any is invalid (work plan #166 D9). CI and .wf.toml run it in the same
# position.
set -u
cd "$(dirname "$0")/.." || exit 1
go build -o "${TMPDIR:-/tmp}/agent-loop-validate-plans.$$" ./cmd/agent-loop || exit 1
bin="${TMPDIR:-/tmp}/agent-loop-validate-plans.$$"
trap 'rm -f "$bin"' EXIT
found=0
status=0
for dir in plans/*/; do
	[ -d "$dir" ] || continue
	found=1
	"$bin" plan validate --profile .agent-loop.toml "$dir" || status=1
done
if [ "$found" = 0 ]; then
	echo "no plans under plans/"
fi
exit "$status"

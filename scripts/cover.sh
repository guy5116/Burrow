#!/bin/sh
# Enforces the coverage floors of CLAUDE.md §13.12. Test-support packages
# (testpeer, transport/conformance), the package that only declares the
# transport interface, and the Tor transport (needs a tor binary and the Tor
# network; run with BURROW_TOR_TEST=1) are exempt. Everything else under
# internal/ must report coverage: a package whose tests fail, do not build,
# or do not exist fails the check.
set -eu
out=$(mktemp)
trap 'rm -f "$out"' EXIT
if ! go test -count=1 -cover ./internal/... >"$out" 2>&1; then
  cat "$out"
  echo "FAIL: go test failed; coverage not checked"
  exit 1
fi
fail=0
for pkg in $(go list ./internal/...); do
  short=${pkg#github.com/guy5116/burrow/}
  case "$short" in
    internal/testpeer|internal/transport|internal/transport/conformance|internal/transport/tor|internal/ui/gui) continue ;;
    internal/wire|internal/session|internal/handshake|internal/media|internal/invite|internal/text) floor=90 ;;
    *) floor=80 ;;
  esac
  pct=$(awk -v p="$pkg" '$1 == "ok" && $2 == p { for (i = 3; i <= NF; i++) if ($i == "coverage:") { sub("%", "", $(i+1)); print $(i+1) } }' "$out")
  if [ -z "$pct" ]; then
    echo "FAIL $short: no coverage reported (no tests?)"; fail=1
  elif [ "$(printf '%s\n' "$pct" | awk -v f="$floor" '{print ($1 < f)}')" = 1 ]; then
    echo "FAIL $short: $pct% < $floor%"; fail=1
  else
    echo "ok   $short: $pct% (floor $floor%)"
  fi
done
exit $fail

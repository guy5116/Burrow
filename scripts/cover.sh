#!/bin/sh
# Enforces the coverage floors of CLAUDE.md §13.12. Test-support packages
# (testpeer, transport/conformance) and the Tor transport (needs a tor binary
# and the Tor network; run with BURROW_TOR_TEST=1) are exempt.
set -eu
fail=0
for line in $(go test -count=1 -cover ./internal/... 2>/dev/null | awk '/^ok/ && /coverage:/ {print $2 "=" $5}'); do
  pkg=${line%%=*}; pct=${line##*=}; pct=${pct%\%}
  short=${pkg#github.com/guy5116/burrow/}
  case "$short" in
    internal/testpeer|internal/transport/conformance|internal/transport/tor) continue ;;
    internal/wire|internal/session|internal/handshake|internal/media|internal/invite|internal/text) floor=90 ;;
    *) floor=80 ;;
  esac
  if [ "$(printf '%s\n' "$pct" | awk -v f="$floor" '{print ($1 < f)}')" = 1 ]; then
    echo "FAIL $short: $pct% < $floor%"; fail=1
  else
    echo "ok   $short: $pct% (floor $floor%)"
  fi
done
exit $fail

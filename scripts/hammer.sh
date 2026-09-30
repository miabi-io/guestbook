#!/usr/bin/env bash
# Hit an endpoint in a loop during a deploy and report any non-2xx responses,
# plus which version/host answered. Proves zero-downtime rollouts.
#
#   scripts/hammer.sh https://guestbook.example.com            # /api/time, 10 req/s
#   scripts/hammer.sh https://guestbook.example.com 20 /healthz
set -u

BASE="${1:?usage: hammer.sh <base-url> [req-per-sec] [path]}"
RATE="${2:-10}"
PATH_="${3:-/api/time}"
URL="${BASE%/}${PATH_}"
SLEEP=$(awk "BEGIN { print 1 / $RATE }")

ok=0 fail=0 total=0
# A temp file instead of an associative array: macOS still ships bash 3.2.
seen=$(mktemp)

summary() {
  echo
  echo "── $total requests · $ok ok · $fail failed ──"
  sort "$seen" | uniq -c | sort -rn
  rm -f "$seen"
  exit 0
}
trap summary INT TERM

echo "hammering $URL at ~${RATE} req/s — Ctrl-C for the summary"
while true; do
  resp=$(curl -s -m 5 -w '\n%{http_code}' "$URL" 2>/dev/null)
  code="${resp##*$'\n'}"
  body="${resp%$'\n'*}"
  total=$((total + 1))
  version=$(printf '%s' "$body" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
  host=$(printf '%s' "$body" | sed -n 's/.*"host":"\([^"]*\)".*/\1/p')
  key="${code} v${version:-?} ${host:-?}"
  echo "$key" >> "$seen"
  if [[ "$code" =~ ^2 ]]; then
    ok=$((ok + 1))
    printf '\r%s  ok=%d fail=%d  last: %-40s' "$(date +%T)" "$ok" "$fail" "$key"
  else
    fail=$((fail + 1))
    printf '\n%s  FAIL %s\n' "$(date +%T)" "${code:-000}"
  fi
  sleep "$SLEEP"
done

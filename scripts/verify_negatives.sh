#!/usr/bin/env bash
# Negative controls: every FR with a kill switch must FAIL its own
# conformance test when the implementation is disabled
# (VA_DISABLE_FR_<ID>=1). A test that still passes with the feature off
# proves nothing, so this script exits non-zero if any control "passes".
set -uo pipefail
cd "$(dirname "$0")/.."

FRS=(A1 A2 A3 A4 A5 A6 A7 A8 A9 C1 C1_AfterTurn D1 D2 D3 E1 E2 F1)
go test -c -o .bin/conformance.test ./conformance/ || exit 2

bad=0
printf "%-5s %-10s %s\n" FR CONTROL RESULT
for fr in "${FRS[@]}"; do
  out=$(cd conformance && env "VA_DISABLE_FR_${fr%%_*}=1" ../.bin/conformance.test -test.run "^TestFR_${fr}\$" -test.count=1 2>&1)
  if echo "$out" | grep -q -- "--- FAIL"; then
    reason=$(echo "$out" | grep -m1 -E "_test.go:[0-9]+:" | sed -E 's/^[[:space:]]+//' | cut -c1-110)
    printf "%-5s %-10s %s\n" "$fr" "FAILS ✅" "$reason"
  else
    printf "%-5s %-10s %s\n" "$fr" "PASSES ❌" "test did not detect the disabled feature"
    bad=1
  fi
done
exit $bad

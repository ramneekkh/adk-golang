#!/usr/bin/env bash
# Full FR conformance: positive suite (real ADK runner, real agents/plugins/
# gateway; only the LLM and the Vertex HTTP endpoint are scripted) followed
# by negative controls (each FR's test must fail with its kill switch on).
set -uo pipefail
cd "$(dirname "$0")/.."
mkdir -p .out
echo "== Positive suite"
go test ./conformance/ -count=1 -v 2>&1 | grep -E "^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)" | grep -v "=== RUN" | tee .out/conformance.txt
pos=${PIPESTATUS[0]}
echo
echo "== Negative controls"
./scripts/verify_negatives.sh | tee .out/negatives.txt
neg=${PIPESTATUS[0]}
exit $(( pos || neg ))

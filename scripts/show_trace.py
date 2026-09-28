#!/usr/bin/env python3
"""Print the redacted OTel spans (.out/traces.jsonl) for one ADK Web session.

Usage: scripts/show_trace.py [SESSION_ID]   (default: most recent session)
Shows span tree order, key gen_ai.* / va.* attributes and span events, and
checks that no vectors or raw PII-looking values are present (FR-D1/D2/D3).
"""
import json, re, sys

PATH = ".out/traces.jsonl"
spans = [json.loads(l) for l in open(PATH)]
sid = sys.argv[1] if len(sys.argv) > 1 else next(
    (s["attributes"].get("va.session_id") for s in reversed(spans)
     if s.get("attributes", {}).get("va.session_id")), None)
mine = [s for s in spans if s.get("attributes", {}).get("va.session_id") == sid]
mine.sort(key=lambda s: s["start"])
print(f"session {sid}: {len(mine)} spans\n")

SHOW = ("gen_ai.operation.name", "gen_ai.request.model", "gen_ai.usage.input_tokens",
        "gen_ai.usage.output_tokens", "gen_ai.embeddings.input", "gen_ai.tool.name",
        "va.embed.kind", "gcp.vertex.agent.llm_request", "gcp.vertex.agent.tool_call_args")
for s in mine:
    a = s.get("attributes", {})
    print(f"- {s['name']}  ({s['duration_ms']:.0f} ms)  trace={s['trace_id'][:8]}")
    for k in SHOW:
        if k in a:
            v = json.dumps(a[k], ensure_ascii=False)
            print(f"    {k} = {v[:220]}{'…' if len(v) > 220 else ''}")
    for e in s.get("events") or []:
        ea = json.dumps(e.get("attributes", {}), ensure_ascii=False)
        print(f"    * event {e.get('name')} {ea[:200]}")

IDS = {"va.trace_id", "va.request_id", "va.session_id", "gcp.vertex.agent.invocation_id",
       "gcp.vertex.agent.event_id", "gcp.vertex.agent.session_id"}
raw = json.dumps([{k: v for k, v in s.get("attributes", {}).items() if k not in IDS}
                  for s in mine] + [s.get("events") or [] for s in mine], ensure_ascii=False)
print("\nchecks:")
print("  vectors on any span  :", any("embeddings.vector" in k or "response.embeddings" in k
                                        for s in mine for k in s.get("attributes", {})))
print("  raw email in spans   :", bool(re.search(r"[\w.+-]+@[\w-]+\.[\w.]+", raw)))
print("  raw phone in spans   :", bool(re.search(r"\b0\d{1,2}-?\d{3}-?\d{4}\b", raw)))
print("  9+ digit run in spans:", bool(re.search(r"(?<![0-9a-f])\d(?:-?\d){8,}(?![0-9a-f])", raw)))

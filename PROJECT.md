# PROJECT — Thai-Banking Virtual Assistant Pilot on Go ADK

Authoritative index for the whole build. Sources of truth that outrank this file:
`.agents/teamwork/ORIGINAL_REQUEST.md` (intent) and `docs/PRD.md` (the 26 FRs).

Status: **v1 — written after survey, before implementation dispatch.**
Launcher/endpoint specifics marked ⏳ are pending execution proof from `explorer_uiproof`.

---

## 1. Established facts (survey, command-verified)

| Fact | Evidence |
|------|----------|
| Module is `google.golang.org/adk/v2`, v2.4.0 on public proxy, fetches natively | explorer #1 + #2 |
| Go 1.26.6; `proxy.golang.org` reachable; no vendoring/`replace` needed | explorer #2 |
| Vertex ADC works; `gemini-2.5-flash` + `gemini-2.5-pro` HTTP 200, `us-central1`, project `elevate-data-508005`; `gemini-2.0-flash` 404s | explorer #2 curl |
| LiteLLM not installed and not installable here (no pip path, no Docker) → gateway is ours, in Go, in-repo | explorer #2 |
| No Docker/Podman → `otel-desktop-viewer` installed via `go install` is the local trace viewer | explorer #2 |
| Prior-art `va_banking_poc` **does not exist** in google3 | independently confirmed by explorer #1 AND #2 |
| Dev UI is an embedded bundle in ADK, agents registered programmatically via `agent.NewMultiLoader`, served through `full.NewLauncher()` | ✅ **EXECUTION-PROVEN**: UI served at `/ui/` (200, real HTML), `GET /api/list-apps` returned both test agents. Note the `/api` prefix — first-pass source reading guessed `/list-apps` and was wrong. |


## 2. ADK mechanisms we build on — ALL EXECUTION-VERIFIED (see `.agents/teamwork/explorer_uiproof/ui_proof.md`)

| Mechanism | Package | Used for | Status |
|-----------|---------|----------|--------|
| `llmagent.New(llmagent.Config{Name,Description,Instruction,Model})` | `agent/llmagent` | all four agents | ✅ ran |
| `agent.NewMultiLoader(...)` + `launcher.Config` + `full.NewLauncher().Execute(ctx,cfg,[]string{"web","--host=localhost","--port=8080","webui","api"})` | `agent`, `cmd/launcher/full` | R2 web UI | ✅ ran, UI served |
| `BeforeModelCallback` → non-nil `*model.LLMResponse` **short-circuits the LLM call** | `agent/llmagent` | A2, A3, A8, E1 | ✅ source-confirmed |
| `AfterModelCallback` | `agent/llmagent` | E2, redaction | ✅ |
| `BeforeToolCallback` (veto by error) | `agent/llmagent` | A5 allowlist + max-tools | ✅ |
| `AfterToolCallback` | `agent/llmagent` | A3 verbatim capture, A9 chain | ✅ |
| `session.State.Set/Get`, `ctx.State()` | `session` | A7, A8, A9 | ✅ |
| `loopagent.MaxIterations` | `agent/workflowagents/loopagent` | A6 | ⚠️ **cap works but returns SILENTLY — no native loop-exceeded event. We must emit `va.loop_exceeded` ourselves. This is host code around ADK, and must be reported as such.** |
| `tool/toolconfirmation` (`Confirmed bool`) | `tool/toolconfirmation` | F1 HITL | ✅ native |
| `openaimodel.NewModel(ctx,"<model>",&openaimodel.ClientConfig{BaseURL:"http://host:port/v1"})` | `model/openaimodel` | R3/H1 gateway seam | ✅ **PROVEN routing to a non-vendor OpenAI-compatible server** |
| `telemetry.WithSpanProcessors(...)` | `telemetry` | D1, D2, D3 | ✅ **custom processor really received `invoke_agent`/`generate_content` spans** |
| Event `usageMetadata` (prompt/candidates/total token counts) | events | H2 real token accounting | ✅ observed on the wire |
| Event `actions.stateDelta` | events | A9 state-version evidence | ✅ observed |

### Verified HTTP surface (these differ from first-pass source reading — trust these)
- `GET  /api/list-apps` → JSON array of agent names, e.g. `["alpha","beta"]`
- `POST /api/apps/{app}/users/{user}/sessions/{session}` body `{}` → session object
- `POST /api/run` → array of Event objects
- `POST /api/run_sse` → SSE, each frame `data: {Event}`
- `GET  /ui/` → the real embedded dev UI HTML (200, ~53 KB)
- Launcher defaults to **in-memory** session/artifact/memory services (state lost on restart).


## 3. Architecture

```
  ADK web dev UI (embedded in ADK)  ──► cmd/web  ──► NewMultiLoader(4 agents)
                                                        │
        orchestrator ──(code switch, no LLM between tools)──┬── rag
                                                            ├── loan (scratchpad → completeness → chain)
                                                            └── transfer (HITL signed payload)
                                                        │
                                   all model calls ─► openaimodel(base=VA_GATEWAY_URL)
                                                        │
                                          cmd/gateway (in-repo, OpenAI-compatible)
                                                        │
                                      Vertex AI Gemini (ADC, elevate-data-508005)

  telemetry: custom SpanProcessors (emit/omit • redaction • correlation) ─► OTLP ─► otel-desktop-viewer
```

**Key invariant:** agent code never imports a model vendor SDK. Its only model egress is
`openaimodel` pointed at our gateway. The gateway is the sole component holding Vertex/ADC logic.

## 4. Code layout (write ownership — concurrent workers MUST NOT cross these lines)

| Path | Owner milestone | Contents |
|------|----------------|----------|
| `go.mod`, `go.sum` | M0 (others request changes via M0) | official `google.golang.org/adk/v2` |
| `cmd/gateway/`, `internal/gateway/` | M0 | LiteLLM-compatible gateway → Vertex |
| `internal/config/` | M0 | model/config surface, config-only switching |
| `internal/killswitch/` | M0 | central `VA_DISABLE_FR_*` registry |
| `cmd/web/`, `scripts/run_web.sh` | M1 | R2 launcher wiring |
| `internal/agents/{orchestrator,rag,loan,transfer}/` | M1 skeleton → M2 depth | the four agents |
| `internal/scratchpad/` | M2 | shared-state helpers |
| `internal/telemetry/`, `internal/privacy/`, `internal/cache/`, `internal/cost/`, `internal/replay/` | M3 | D1-D3, E1-E2, C1, H2, H3 |
| `conformance/`, `scripts/run_conformance.sh`, `scripts/verify_negatives.sh` | M4 | 26 FR tests, report, negative runner |
| `eval/`, `fixtures/` | M5 | Group B harness + fixtures |
| `docs/` | FINAL (PRD.md is immutable input) | RUNBOOK.md, FR_STATUS.md |

## 5. Interface contracts (frozen now so milestones can proceed in parallel)

### 5.1 Gateway (M0 ↔ everyone)
- `POST /v1/chat/completions` — OpenAI-shaped request/response; `stream` supported.
- `GET /v1/models` — lists configured models.
- `GET /healthz` — 200 when ready.
- Env: `VA_GATEWAY_URL` (default `http://127.0.0.1:8081`), `VA_MODEL` (default `gemini-2.5-flash`),
  `VA_VERTEX_PROJECT=elevate-data-508005`, `VA_VERTEX_LOCATION=us-central1`, `VA_GATEWAY_API_KEY`.
- **Model switching is config-only**: changing `VA_MODEL` (env or `config/models.yaml`) changes the
  served model with **zero source edits**. The gateway maps model name → Vertex publisher model.

### 5.2 Kill-switch registry (M0 ↔ M2/M4)
- `killswitch.Disabled(fr string) bool` reads `VA_DISABLE_FR_<ID>=1`.
- **Integrity rule (binding on all workers):** a kill-switch MUST disable the *real* mechanism
  (e.g. A4 replaces the code `switch` router with an LLM "which route?" call; A8 demotes the
  deterministic completeness trigger to an ordinary LLM-decided tool). It MUST NOT be a
  test-only short-circuit that fakes a failure. The forensic auditor will check each of the 10.

### 5.3 Shared state keys (M2 ↔ M4)
`loan.scratchpad` (object: `amount`, `tenure_months`, `monthly_income`), `loan.scratchpad.version`
(int, incremented per mutation — needed for A9 "state versions visible"), `loan.result`,
`transfer.pending_payload`, `transfer.signature`, `route.last` (code branch taken — A4 evidence).

### 5.4 Telemetry attributes (M3 ↔ M4)
Every span carries `va.request_id`, `va.session_id`, `va.user_id`, plus OTel `trace_id` (D3).
Named events/log records: `va.loop_exceeded` (A6), `va.policy_violation` (A5),
`va.tamper_rejected` (F1c), `va.chain_step` (A9), `va.completeness_trigger` (A8).

### 5.5 Conformance report (M4 → FINAL)
`conformance/report.json`: `{"generated_at":..., "results":[{"fr":"A1","status":"PASS|FAIL|ERROR",
"detail":"...","evidence":"..."}], "summary":{"total":26,"passed":N}}` — exactly 26 entries.
Plus a human-readable `conformance/report.md`. One command: `scripts/run_conformance.sh`.

### 5.6 Group B eval output (M5 → FINAL)
`eval/out/<stage>.json` per stage with the PRD's required metrics: B1 accuracy + confusion
matrix; B2 per-tool precision/recall; B3 field-level exact-match + F1; B4 recall@k + MRR;
B5 groundedness + correctness (golden chunks injected); B6 final-answer vs golden set.
Each stage scored **in isolation with upstream frozen by fixtures**, not by running the pipeline.

## 6. Feature inventory → milestone assignment (all 26 assigned; none unassigned)

| FR | Pri | Milestone | FR | Pri | Milestone |
|----|-----|-----------|----|-----|-----------|
| A1 | P0 | M2 | D1 | P0 | M3 |
| A2 | P0 | M2 | D2 | P1 | M3 |
| A3 | P0 | M2 | D3 | P0 | M3 |
| A4 | P0 | M2 | E1 | P1 | M3 |
| A5 | P0 | M2 | E2 | P1 | M3 |
| A6 | P0 | M2 | F1 | P1 | M2 |
| A7 | P0 | M2 | G1 | P0 | M0 |
| A8 | P0 | M2 | H1 | P0 | M0 |
| A9 | P0 | M2 | H2 | P0 | M3 |
| B1-B6 | P0 | M5 | H3 | P1 | M3 |
| C1 | P0 | M3 | | | |

Cross-check: A(9) + B(6) + C(1) + D(3) + E(2) + F(1) + G(1) + H(3) = **26. All assigned.**
Every FR additionally gets a conformance test in M4.

## 7. Milestones

| # | Name | Scope | Depends | Status |
|---|------|-------|---------|--------|
| M0 | Foundation & gateway | go.mod on official ADK; in-repo OpenAI-compatible gateway → Vertex via ADC; config surface; kill-switch registry; G1 + H1 | survey | PLANNED |
| M1 | Four agents + real web UI | 4 agents wired via NewMultiLoader; one documented launch command; automated smoke check proving all four registered and reachable | M0 | PLANNED |
| M2 | Group A lifecycle control + F1 HITL | A1-A9 + F1 with their 10 real kill-switches | M1 | PLANNED |
| M3 | Telemetry, privacy, cost, cache, replay | D1-D3, E1-E2, C1, H2, H3 | M1 | PLANNED |
| M4 | Conformance suite | 26 FR tests driving the RUNNING agents; report keyed by FR; negative-check runner | M1 (grows with M2/M3) | PLANNED |
| M5 | Group B eval harness | B1-B6 isolated-stage scoring + machine-readable per-stage scores | M1 | PLANNED |
| FINAL | Integration & truth | 26/26 PASS, 10 negative checks demonstrated, web-UI smoke green, live traces viewable, honest FR_STATUS.md | M0-M5 | PLANNED |

## 8. Standing rules for every worker

1. **Tests must drive the RUNNING ADK agents.** A hand-written stub mirroring a requirement is
   a rejected deliverable and an audit failure.
2. **Implement the procedure the PRD states**, not a looser paraphrase (A3 byte-compare; A4
   100 runs with no LLM call between tools; A5 adversarial attempt blocked *and logged*; A6
   halt at cap with a loop-exceeded event; A8 fires only on completeness and never on partial;
   F1 all three sub-cases).
3. **Kill-switches disable real mechanisms** (§5.2).
4. **No fabrication.** If something cannot be made to work, write it plainly in your handoff.
   An honest gap is acceptable; a fake pass is not.
5. **Respect write ownership** (§4). Do not edit another milestone's files.
6. **FR-G1 honesty note:** the real module path is `google.golang.org/adk/v2`. The G1 check must
   assert the official ADK module is required in `go.mod` and must be documented to state the
   `/v2` major-version suffix explicitly. Do not disguise this.
7. **FR-H1 honesty note:** ADK's own internals may transitively link a vendor SDK. The claim we
   make and test is: no package *we own* directly imports a vendor model SDK, and runtime egress
   from agent code goes only to the in-repo gateway. Document this precisely; do not overclaim
   "zero vendor SDK anywhere in the dependency graph" if that is untrue.

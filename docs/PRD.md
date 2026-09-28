# PRD — Virtual Assistant on ADK: Adoption PoC


1. Background

The Virtual Assistant (VA) is a customer-facing LLM system on a national-scale banking channel: 150k DAU, 500+ TPS sustained at peak, operating under financial-institution compliance requirements 

**Principle:** every P0 requirement below is something the in-house core **does in production today**. Nothing here is invented to make ADK fail.

2. Scope

**In scope — deliberately small: one agent microservice** on official `google.golang.org/adk` (Go), containing four agents that exercise the full lifecycle (intent handling → routing → tool/retrieval → completion → guardrail hooks → response):

1. **Orchestrator agent(P0)** — routes to sub-agents/tools, owns flow control.
1. **RAG agent/workflow(P0)** — retrieval-grounded Q&A.
1. **Loan calculation agent(P0)** — multi-turn with a **scratchpad**: jots relevant details across turns so nothing is forgotten; when required fields are complete, the loan-calculation **function** is triggered deterministically.
1. **Transfer agent(P1)** — given an intent like `"โอนเงิน 500 บาท เลขบัญชี 023536333"`, prepares a function call that executes **only after human-in-the-loop approval** (see FR-F1).

**Context-control requirements central to this scope:** the host code must be able to manipulate context freely between agents — e.g. inject the scratchpad's current state into the loan agent's context **without the agent making an explicit call to fetch it** — and run conditional, code-driven tool chains that mutate shared state in sequence (save final details to scratchpad → execute loan calculation → write the result back to the scratchpad). See FR-A7..A9.

PoC simplifications, granted up front:

- **Transport:** plain HTTP request/response is fine. No message broker integration required.
- **Vector DB:** any — vendor's choice. Retrieval quality is not under test; retrieval *evaluability* is. (mock is also fine, since vector db is not relevant to the main issue here)
- **Infra:** single service, non-prod cluster, no HA requirements.
- **Transfer execution:** the transfer function may be a stub — what is under test is the approval gate and payload integrity, not core-bank integration.

**Out of scope / non-goals:**

- Broker integration, cross-microservice agent mesh, channel integration, UI. (In-process multi-agent composition IS in scope — it is the point.)
- Replacing our guardrail service, LLM gateway(Litellm), or observability pipeline(Langfuse Otel) — ADK must integrate with these, not replace them.
- Community forks or Python ADK. The claim is evaluated on the official Go implementation only (FR-G1).

The simplifications remove infrastructure noise so the test isolates the actual question: **does ADK give framework-level control equal to the in-house core?** Framework control does not get easier at production scale — if it fails at PoC scale, it fails.

## 3. Current production baseline (what "everything" must cover)

The in-house core, in production today. Items marked *(excluded from PoC)* stay in the baseline record but are not tested in this round.

- Stage-modular prompts and lifecycle: tool-selection prompt separate from completion prompt; completion stage removable; tool output returnable as final response; deterministic code-routing between tools; code-enforced tool allowlists and loop caps.
- Framework-level context control: host code injects and reads shared state (scratchpad) across agents and turns without agent-explicit fetch calls; completeness conditions trigger deterministic function execution; code-driven tool chains mutate shared state in sequence.
- Human-in-the-loop tool confirmation with payload integrity: signed payload required before execution; AI cannot mutate a signed payload.
- Component-level eval across the lifecycle (routing, tool selection, argument extraction, retrieval, completion-given-golden-chunks, behavior, E2E)
- Placeholder swap: PII or any info that must not let LLM see it, hallucinate or generate by itself before the model, restored after (e.g. `test@mail.com` → `[EMAIL]` → restored or [www.krungthai.co.th](http://www.krungthai.co.th) → \[URL\_1\] → retored).
- All inference through the internal LLM gateway (Litellm).
- Custom OTel telemetry: attribute-level emit/omit, pluggable redaction (e.g. `โอนเงิน 023536333` → `โอนเงิน [NUMERIC_9_DIGITS]`), cross-service correlation (request\_id / session\_id / user\_id / trace\_id), per-stage token & cost accounting.

## 5. Functional requirements

Legend: **P0** = in production today, verdict-counting. **P1** = roadmap, advisory.

### Group A — Prompt & lifecycle modularity

| ID | Pri | Requirement | Acceptance test |
| --- | --- | --- | --- |
| FR-A1 | P0 | Separate prompts per lifecycle stage (tool selection ≠ final completion ≠ other stages) | Change tool-selection prompt without touching completion prompt; both visible in trace |
| FR-A2 | P0 | Final completion stage removable; orchestrator performs tool selection only | Response returns after tool execution with zero second LLM call |
| FR-A3 | P0 | Tool output returned directly as agent response (no LLM rewrite) | Byte-compare: tool output == delivered response |
| FR-A4 | P0 | Deterministic routing; branching on tool output decided by code (switch on values/state), never by LLM | Same input → same route 100/100 runs; route visible in trace as code branch, no LLM call between tools |
| FR-A5 | P0 | Deterministic tool-selection constraints; max N tools/turn, allowlist by intent/session state; enforced in code, not prompt | Configure max=1 + allowlist; adversarial prompt attempts extra/forbidden tool → blocked at framework level, logged |
| FR-A6 | P0 | Agentic loop control; hard iteration cap, exit conditions, e.g. max agentic loop = 3 | Forced-loop scenario halts at cap N, emits loop-exceeded event, safe fallback returned |
| FR-A7 | P0 | Host-side context injection; code injects shared state (scratchpad) into an agent's context between agents/turns; agent never makes an explicit fetch call | Loan agent's context contains current scratchpad state; trace shows zero fetch tool call by the agent |
| FR-A8 | P0 | Multi-turn scratchpad with completeness trigger; agent jots details across turns into shared state; when required fields complete, target function fires **deterministically** (code condition, not LLM decision) | Multi-turn loan dialog with partial info per turn: scratchpad accumulates; on final field, loan calc executes with no LLM "decide to call" step; incomplete state never triggers |
| FR-A9 | P0 | Conditional tool chain with state mutation — code-sequenced chain: save final details → execute loan calc → write result back to scratchpad | Trace shows the three steps in order with state versions visible; no LLM call between steps; chain branches correctly on condition |

### Group B — Modular lifecycle evaluation

Method: freeze upstream stages with fixtures, vary one stage, score it in isolation.

| ID | Pri | Stage | Inject | Score |
| --- | --- | --- | --- | --- |
| FR-B1 | P0 | Intent routing | user message | correct handling path ; accuracy + confusion matrix |
| FR-B2 | P0 | Tool selection | fixed intent + context | correct tool ; per-tool precision/recall |
| FR-B3 | P0 | Argument extraction | fixed tool choice | field-level correctness (exact match / F1) |
| FR-B4 | P0 | Retrieval | fixed query | golden chunks found — recall@k, MRR (any vector DB; evaluability is the test, not engine quality) |
| FR-B5 | P0 | Completion | **golden chunks injected** | groundedness + correctness with retrieval blame removed |
|  |  |  |  |  |
|  |  |  |  |  |
|  |  |  |  |  |
| FR-B6 | P0 | End-to-end | raw message | final answer vs golden set |

Requirement: FR-B1..B8 must run **without executing the full pipeline** and produce per-stage reports consumable by CI (release gating).

### Group C — Inference cost & latency

| ID | Pri | Requirement | Acceptance test |
| --- | --- | --- | --- |
|  |  |  |  |
| FR-C1 | P0 | Explicit caching control (cache breakpoints, TTL) | Deliberate breakpoint set; hit/miss verified |

### Group D — Tracing (OpenTelemetry)

| ID | Pri | Requirement | Acceptance test |
| --- | --- | --- | --- |
| FR-D1 | P0 | Attribute-level emit/omit control (e.g. emit embedding input text, omit output vector) | Exported span shows input text, no vector payload |
| FR-D2 | P1 | Pluggable redaction on trace input AND output (e.g. `โอนเงิน 023536333` → `โอนเงิน [NUMERIC_9_DIGITS]`) | Raw PII absent from all exported spans; redaction rules are code we own |
| FR-D3 | P0 | Correlation: request\_id, session\_id, user\_id, trace\_id + custom metadata attached and propagated through all spans | Single query in trace backend returns the full request journey |

### Group E — Sensitive-data placeholder swap

| ID | Pri | Requirement | Acceptance test |
| --- | --- | --- | --- |
| FR-E1 | P1 | Swap-out pre-inference: model never receives real value (`test@mail.com` → `[EMAIL]`) | LLM request payload logged: placeholder only |
| FR-E2 | P1 | Swap-in post-inference: placeholders restored in delivered response; covers canonicalization guards (e.g. URL corrections) | User sees real value end-to-end; hallucinated variants normalized |

### Group F — Human-in-the-loop integrity

| ID | Pri | Requirement | Acceptance test |
| --- | --- | --- | --- |
| FR-F1 | P1 | Tool confirmation requires human signature over the exact payload; AI cannot execute or mutate a signed payload. Test scenario: transfer agent on `"โอนเงิน 500 บาท เลขบัญชี 023536333"` | (a) No approval → no execution, ever. (b) Approval → execution with exactly the signed payload. (c) Tamper test: AI or code mutates amount/account post-signature → execution rejected, event logged |

### Group H — Platform integration (financial-institution standards)

| ID | Pri | Requirement | Acceptance test |
| --- | --- | --- | --- |
| FR-H1 | P0 | LLM gateway pluggability: all inference through internal gateway; zero direct vendor SDK calls; model swap = config | Network capture shows gateway-only egress; model switched without code change |
|  |  |  |  |
|  |  |  |  |
|  |  |  |  |
| FR-H2 | P0 | Token/cost accounting per request per stage | Report: cost breakdown by stage for a sample request set |
| FR-H3 | P1 | Deterministic replay of recorded requests for incident reproduction | Recorded request replays through identical flow |
|  |  |  |  |

### Group G — Runtime & performance

| ID | Pri | Requirement | Acceptance test |
| --- | --- | --- | --- |
| FR-G1 | P0 | All of the above on official `google.golang.org/adk` (Go). Community forks and Python ADK out of scope | Module path verified in go.mod |
|  |  |  |  |



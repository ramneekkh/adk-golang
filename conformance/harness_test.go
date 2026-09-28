package conformance

// Harness for the FR conformance suite.
//
// Every test drives the REAL agents through the REAL ADK runner (plugins,
// callbacks, tools, session state, HITL confirmation, telemetry pipeline).
// Only the LLM is replaced — by ScriptedLLM, which returns a scripted response
// per agent and records every request exactly as it would leave the host.
// Assertions are made on runner events, session state, recorded LLM requests
// and exported spans — never on values re-computed inside the test.

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"va_adk_pilot/internal/agents/loan"
	"va_adk_pilot/internal/agents/orchestrator"
	"va_adk_pilot/internal/agents/rag"
	"va_adk_pilot/internal/agents/transfer"
	"va_adk_pilot/internal/privacy"
	vatel "va_adk_pilot/internal/telemetry"
)

const (
	appName = "va_conformance"
	userID  = "u-conformance"
)

// ---- telemetry: one global provider for the whole test binary ---------------

var (
	telOnce  sync.Once
	exported = &vatel.Recorder{} // after our D1/D2/D3 processor
	raw      = &vatel.Recorder{} // raw spans, no processor (control group)
)

func initTelemetry() {
	telOnce.Do(func() {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(vatel.NewProcessor(exported)),
			sdktrace.WithSpanProcessor(raw),
		)
		otel.SetTracerProvider(tp)
	})
}

// ---- scripted LLM -------------------------------------------------------------

// Step produces a response for the n-th call (0-based) to an agent.
type Step func(n int, req *model.LLMRequest) *model.LLMResponse

type RecordedRequest struct {
	Agent  string
	System string
	JSON   string // full request as sent (after all callbacks/plugins)
	Req    *model.LLMRequest
}

type ScriptedLLM struct {
	mu       sync.Mutex
	scripts  map[string]Step
	calls    map[string]int
	Requests []RecordedRequest
}

func NewScripted(scripts map[string]Step) *ScriptedLLM {
	return &ScriptedLLM{scripts: scripts, calls: map[string]int{}}
}

func (s *ScriptedLLM) Name() string { return "scripted" }

var agentNameRe = regexp.MustCompile(`Your internal name is "([a-z_]+)"`)

func systemText(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil {
			b.WriteString(p.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func agentOf(req *model.LLMRequest) string {
	if m := agentNameRe.FindStringSubmatch(systemText(req)); m != nil {
		return m[1]
	}
	// The orchestrator replaces its system instruction per stage (FR-A1).
	for _, t := range reqTools(req) {
		if t == "RouteIntent" {
			return "orchestrator"
		}
	}
	return "unknown"
}

func reqTools(req *model.LLMRequest) []string {
	var names []string
	if req.Config == nil {
		return names
	}
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			names = append(names, fd.Name)
		}
	}
	return names
}

func (s *ScriptedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	b, _ := json.Marshal(req)
	a := agentOf(req)
	s.mu.Lock()
	n := s.calls[a]
	s.calls[a]++
	s.Requests = append(s.Requests, RecordedRequest{Agent: a, System: systemText(req), JSON: string(b), Req: req})
	step := s.scripts[a]
	s.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		if step == nil {
			yield(nil, fmt.Errorf("no script for agent %q", a))
			return
		}
		yield(step(n, req), nil)
	}
}

func (s *ScriptedLLM) CallsFor(a string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[a]
}

func (s *ScriptedLLM) RequestsFor(a string) []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RecordedRequest
	for _, r := range s.Requests {
		if r.Agent == a {
			out = append(out, r)
		}
	}
	return out
}

func (s *ScriptedLLM) AllJSON() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, r := range s.Requests {
		b.WriteString(r.JSON)
		b.WriteString("\n")
	}
	return b.String()
}

var callSeq struct {
	sync.Mutex
	n int
}

func nextID() string {
	callSeq.Lock()
	defer callSeq.Unlock()
	callSeq.n++
	return fmt.Sprintf("call-%d", callSeq.n)
}

// Call returns a model response invoking one tool.
func Call(name string, args map[string]any) *model.LLMResponse {
	return Calls(map[string]map[string]any{name: args})
}

// Calls returns a model response invoking several tools in one turn.
func Calls(calls map[string]map[string]any) *model.LLMResponse {
	c := &genai.Content{Role: genai.RoleModel}
	for name, args := range calls {
		c.Parts = append(c.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: nextID(), Name: name, Args: args}})
	}
	return &model.LLMResponse{Content: c, UsageMetadata: usage(120, 15)}
}

// Text returns a plain text model response.
func Text(s string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}}, UsageMetadata: usage(200, 40)}
}

func usage(in, out int32) *genai.GenerateContentResponseUsageMetadata {
	return &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: in, CandidatesTokenCount: out, TotalTokenCount: in + out}
}

// Seq plays the given responses in order, repeating the last one.
func Seq(rs ...*model.LLMResponse) Step {
	return func(n int, _ *model.LLMRequest) *model.LLMResponse {
		if n >= len(rs) {
			n = len(rs) - 1
		}
		return rs[n]
	}
}

// Route scripts the orchestrator to route every turn to intent.
func Route(intent string) Step {
	return func(int, *model.LLMRequest) *model.LLMResponse {
		return Call("RouteIntent", map[string]any{"intent": intent})
	}
}

// ---- app ----------------------------------------------------------------------

type App struct {
	T       *testing.T
	LLM     *ScriptedLLM
	Runner  *runner.Runner
	Service session.Service
	turns   map[string]int
}

type appOpts struct {
	plugins []*plugin.Plugin
}

type Opt func(*appOpts)

func WithPlugin(p *plugin.Plugin) Opt { return func(o *appOpts) { o.plugins = append(o.plugins, p) } }

// NewApp builds the full production agent tree on a scripted LLM.
func NewApp(t *testing.T, scripts map[string]Step, opts ...Opt) *App {
	t.Helper()
	llm := NewScripted(scripts)
	app := NewAppWithModel(t, llm, opts...)
	app.LLM = llm
	return app
}

// NewAppWithModel builds the production agent tree on any model.LLM.
func NewAppWithModel(t *testing.T, llm model.LLM, opts ...Opt) *App {
	t.Helper()
	initTelemetry()
	if os.Getenv("VA_EMBEDDER") == "" {
		t.Setenv("VA_EMBEDDER", "local")
	}
	o := &appOpts{}
	for _, f := range opts {
		f(o)
	}
	ragA, err := rag.NewAgent(llm)
	must(t, err)
	loanA, err := loan.NewAgent(llm)
	must(t, err)
	trA, err := transfer.NewAgent(llm)
	must(t, err)
	orch, err := orchestrator.NewAgent(llm, ragA, loanA, trA)
	must(t, err)
	pp, err := privacy.NewPlugin()
	must(t, err)
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          orch,
		SessionService: svc,
		PluginConfig:   runner.PluginConfig{Plugins: append([]*plugin.Plugin{pp}, o.plugins...)},
	})
	must(t, err)
	return &App{T: t, Runner: r, Service: svc, turns: map[string]int{}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (a *App) NewSession() string {
	a.T.Helper()
	resp, err := a.Service.Create(context.Background(), &session.CreateRequest{AppName: appName, UserID: userID})
	must(a.T, err)
	return resp.Session.ID()
}

func (a *App) Send(sid string, c *genai.Content) []*session.Event {
	a.T.Helper()
	var evs []*session.Event
	for ev, err := range a.Runner.Run(context.Background(), userID, sid, c, agent.RunConfig{}) {
		if err != nil {
			a.T.Fatalf("runner error: %v", err)
		}
		evs = append(evs, ev)
	}
	a.awaitSpans(sid)
	return evs
}

// awaitSpans waits until this turn's root span has been exported. ADK ends
// the invocation span after the event iterator is drained, so reading the
// recorder immediately can race the exporter.
func (a *App) awaitSpans(sid string) {
	a.turns[sid]++
	want := a.turns[sid]
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		roots := 0
		for _, s := range SpansFor(sid) {
			if s.ParentID == "" {
				roots++
			}
		}
		if roots >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// SendErr runs a turn and returns the first runner error instead of failing.
func (a *App) SendErr(sid string, c *genai.Content) ([]*session.Event, error) {
	var evs []*session.Event
	for ev, err := range a.Runner.Run(context.Background(), userID, sid, c, agent.RunConfig{}) {
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
	a.awaitSpans(sid)
	return evs, nil
}

func (a *App) Say(sid, text string) []*session.Event {
	return a.Send(sid, genai.NewContentFromText(text, genai.RoleUser))
}

func (a *App) State(sid, key string) any {
	a.T.Helper()
	resp, err := a.Service.Get(context.Background(), &session.GetRequest{AppName: appName, UserID: userID, SessionID: sid})
	must(a.T, err)
	v, err := resp.Session.State().Get(key)
	if err != nil {
		return nil
	}
	return v
}

func (a *App) Events(sid string) []*session.Event {
	resp, err := a.Service.Get(context.Background(), &session.GetRequest{AppName: appName, UserID: userID, SessionID: sid})
	must(a.T, err)
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		out = append(out, ev)
	}
	return out
}

// FinalText is the last non-empty text produced by an agent in evs.
func FinalText(evs []*session.Event) string {
	for i := len(evs) - 1; i >= 0; i-- {
		ev := evs[i]
		if ev == nil || ev.Author == "user" || ev.Content == nil {
			continue
		}
		var b strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" && !p.Thought {
				b.WriteString(p.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

// FunctionResponses returns tool responses by tool name.
func FunctionResponses(evs []*session.Event, name string) []map[string]any {
	var out []map[string]any
	for _, ev := range evs {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				out = append(out, p.FunctionResponse.Response)
			}
		}
	}
	return out
}

// FunctionCalls returns function calls by name.
func FunctionCalls(evs []*session.Event, name string) []*genai.FunctionCall {
	var out []*genai.FunctionCall
	for _, ev := range evs {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == name {
				out = append(out, p.FunctionCall)
			}
		}
	}
	return out
}

// SpansFor returns exported (or raw) spans carrying va.session_id == sid, or
// for the raw recorder, spans from traces that the exported ones belong to.
func SpansFor(sid string) []vatel.SpanRecord {
	var out []vatel.SpanRecord
	for _, s := range exported.Snapshot() {
		if s.Attributes["va.session_id"] == sid {
			out = append(out, s)
		}
	}
	return out
}

func traceIDs(spans []vatel.SpanRecord) map[string]bool {
	ids := map[string]bool{}
	for _, s := range spans {
		ids[s.TraceID] = true
	}
	return ids
}

func spansInTraces(rec *vatel.Recorder, ids map[string]bool) []vatel.SpanRecord {
	var out []vatel.SpanRecord
	for _, s := range rec.Snapshot() {
		if ids[s.TraceID] {
			out = append(out, s)
		}
	}
	return out
}

func dump(spans []vatel.SpanRecord) string {
	b, _ := json.Marshal(spans)
	return string(b)
}

func hasEvent(spans []vatel.SpanRecord, name string) bool {
	for _, s := range spans {
		for _, e := range s.Events {
			if e["name"] == name {
				return true
			}
		}
	}
	return false
}

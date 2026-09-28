// Package evalharness implements the PRD Group B stage evaluations (B1–B6).
//
// Stage isolation ("without executing the full pipeline"):
//   - B1 intent routing: the real orchestrator (real prompt, callbacks, tool
//     declarations, privacy plugin) makes ONE live model call; the chosen
//     tool is recorded and NOT executed.
//   - B2/B3 tool selection / argument extraction: the routing decision is
//     FROZEN from the fixture (the orchestrator's model is replaced by the
//     fixture's intent), the target agent makes ONE live model call, the
//     chosen tool/args are recorded and NOT executed.
//   - B4 retrieval: Retriever.Search only (real embeddings), no LLM.
//   - B5 completion: golden chunks are injected as the SearchBankingDocs
//     result; ONE live completion call; retrieval blame removed.
//   - B6 end to end: the full production runner.
//
// Every metric is computed from live model output; nothing is hardcoded.
package evalharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	"va_adk_pilot/internal/embed"
	"va_adk_pilot/internal/privacy"
)

// Env configures a run.
type Env struct {
	// NewModel returns the model client for an agent (production: models.ForAgent).
	NewModel    func(agentName string) (model.LLM, error)
	Embedder    embed.Embedder
	Concurrency int
	Retries     int
}

var agentNames = []string{"orchestrator", "rag", "loan", "transfer"}

const haltText = "[eval: stage boundary — downstream not executed]"

// ---- stage probe ----------------------------------------------------------

// probe forwards exactly one call to the live model, records the response,
// and returns a terminal text so nothing downstream (tools, other agents)
// runs. Further calls are halted without reaching the model.
type probe struct {
	inner model.LLM
	mu    sync.Mutex
	calls int
	resp  *model.LLMResponse
	err   error
}

func (p *probe) Name() string { return p.inner.Name() }

func (p *probe) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		p.mu.Lock()
		p.calls++
		first := p.calls == 1
		p.mu.Unlock()
		if first {
			for r, err := range p.inner.GenerateContent(ctx, req, false) {
				if err != nil {
					p.err = err
					yield(nil, err)
					return
				}
				p.resp = r
			}
		}
		yield(halt(), nil)
	}
}

// frozen replays a fixed decision (no model call) once, then halts.
type frozen struct {
	name  string
	first *model.LLMResponse
	n     int
	mu    sync.Mutex
}

func (f *frozen) Name() string { return f.name }

func (f *frozen) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.mu.Lock()
		f.n++
		n := f.n
		f.mu.Unlock()
		if n == 1 && f.first != nil {
			yield(f.first, nil)
			return
		}
		yield(halt(), nil)
	}
}

func halt() *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: haltText}}}}
}

func routeCall(intent string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: "frozen-route", Name: "RouteIntent", Args: map[string]any{"intent": intent}}}}}}
}

// ---- running a turn through the production tree ---------------------------

type turn struct {
	events []*session.Event
	state  session.State
}

func buildRunner(mods map[string]model.LLM) (*runner.Runner, session.Service, error) {
	ragA, err := rag.NewAgent(mods["rag"])
	if err != nil {
		return nil, nil, err
	}
	loanA, err := loan.NewAgent(mods["loan"])
	if err != nil {
		return nil, nil, err
	}
	trA, err := transfer.NewAgent(mods["transfer"])
	if err != nil {
		return nil, nil, err
	}
	orch, err := orchestrator.NewAgent(mods["orchestrator"], ragA, loanA, trA)
	if err != nil {
		return nil, nil, err
	}
	pp, err := privacy.NewPlugin()
	if err != nil {
		return nil, nil, err
	}
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: "va_eval", Agent: orch, SessionService: svc,
		PluginConfig: runner.PluginConfig{Plugins: []*plugin.Plugin{pp}}})
	return r, svc, err
}

func runTurn(ctx context.Context, mods map[string]model.LLM, msg string, state map[string]any) (*turn, error) {
	r, svc, err := buildRunner(mods)
	if err != nil {
		return nil, err
	}
	cr, err := svc.Create(ctx, &session.CreateRequest{AppName: "va_eval", UserID: "eval", State: state})
	if err != nil {
		return nil, err
	}
	sid := cr.Session.ID()
	t := &turn{}
	for ev, err := range r.Run(ctx, "eval", sid, genai.NewContentFromText(msg, genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			return t, err
		}
		t.events = append(t.events, ev)
	}
	gr, err := svc.Get(ctx, &session.GetRequest{AppName: "va_eval", UserID: "eval", SessionID: sid})
	if err != nil {
		return t, err
	}
	t.state = gr.Session.State()
	return t, nil
}

func stateGet(s session.State, k string) any {
	if s == nil {
		return nil
	}
	v, err := s.Get(k)
	if err != nil {
		return nil
	}
	return v
}

// stageCall runs one turn where `target` makes the single live call.
// For target != orchestrator the route is frozen to `intent`.
func (e *Env) stageCall(ctx context.Context, target, intent, msg string, state map[string]any) (*probe, *turn, error) {
	mods := map[string]model.LLM{}
	var pr *probe
	for _, a := range agentNames {
		switch {
		case a == target:
			m, err := e.NewModel(a)
			if err != nil {
				return nil, nil, err
			}
			pr = &probe{inner: m}
			mods[a] = pr
		case a == "orchestrator":
			mods[a] = &frozen{name: "frozen-route", first: routeCall(intent)}
		default:
			mods[a] = &frozen{name: "halt"}
		}
	}
	t, err := runTurn(ctx, mods, msg, state)
	if err == nil && pr.err != nil {
		err = pr.err
	}
	if err == nil && pr.calls == 0 {
		err = fmt.Errorf("target agent %q was never invoked", target)
	}
	return pr, t, err
}

func firstCall(r *model.LLMResponse) *genai.FunctionCall {
	if r == nil || r.Content == nil {
		return nil
	}
	for _, p := range r.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			return p.FunctionCall
		}
	}
	return nil
}

func respText(r *model.LLMResponse) string {
	if r == nil || r.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range r.Content.Parts {
		if p != nil && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func tokens(r *model.LLMResponse) (in, out int) {
	if r == nil || r.UsageMetadata == nil {
		return 0, 0
	}
	return int(r.UsageMetadata.PromptTokenCount), int(r.UsageMetadata.CandidatesTokenCount)
}

// ---- report ---------------------------------------------------------------

type Report struct {
	Stage       string             `json:"stage"`
	Description string             `json:"description"`
	Isolation   string             `json:"isolation"`
	Model       string             `json:"model"`
	Cases       int                `json:"cases"`
	Errors      int                `json:"errors"`
	Metrics     map[string]float64 `json:"metrics"`
	Thresholds  map[string]float64 `json:"thresholds"`
	Failed      []string           `json:"failed_thresholds,omitempty"`
	Pass        bool               `json:"pass"`
	Extra       map[string]any     `json:"extra,omitempty"`
	LiveCalls   int                `json:"live_model_calls"`
	InputTok    int                `json:"input_tokens"`
	OutputTok   int                `json:"output_tokens"`
	StartedAt   time.Time          `json:"started_at"`
	DurationSec float64            `json:"duration_sec"`
	Details     []map[string]any   `json:"details"`
}

func (r *Report) gate() {
	r.Pass = true
	keys := make([]string, 0, len(r.Thresholds))
	for k := range r.Thresholds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := r.Metrics[k]; !ok || v+1e-9 < r.Thresholds[k] {
			r.Pass = false
			r.Failed = append(r.Failed, fmt.Sprintf("%s=%.3f < %.3f", k, r.Metrics[k], r.Thresholds[k]))
		}
	}
	if r.Errors > 0 {
		r.Pass = false
		r.Failed = append(r.Failed, fmt.Sprintf("%d case(s) errored", r.Errors))
	}
}

// Fixture is the common fixture envelope.
type Fixture struct {
	Stage       string             `json:"stage"`
	Description string             `json:"description"`
	Thresholds  map[string]float64 `json:"thresholds"`
	Cases       json.RawMessage    `json:"cases"`
}

func LoadFixture(path string, cases any) (*Fixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fixture
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, json.Unmarshal(f.Cases, cases)
}

// parallel runs fn over n items with bounded concurrency and retries.
func (e *Env) parallel(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	c := e.Concurrency
	if c <= 0 {
		c = 4
	}
	sem := make(chan struct{}, c)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			for attempt := 0; ; attempt++ {
				errs[i] = fn(i)
				if errs[i] == nil || attempt >= e.Retries {
					return
				}
				time.Sleep(time.Duration(1<<attempt) * time.Second)
			}
		}(i)
	}
	wg.Wait()
	return errs
}

// ---- classification metrics -----------------------------------------------

func classification(gold, pred []string, labels []string) (map[string]float64, map[string]map[string]int) {
	cm := map[string]map[string]int{}
	correct := 0
	for i := range gold {
		if cm[gold[i]] == nil {
			cm[gold[i]] = map[string]int{}
		}
		cm[gold[i]][pred[i]]++
		if gold[i] == pred[i] {
			correct++
		}
	}
	m := map[string]float64{"accuracy": ratio(correct, len(gold))}
	var f1s float64
	for _, l := range labels {
		tp, fp, fn := 0, 0, 0
		for i := range gold {
			switch {
			case gold[i] == l && pred[i] == l:
				tp++
			case gold[i] != l && pred[i] == l:
				fp++
			case gold[i] == l && pred[i] != l:
				fn++
			}
		}
		p, r := ratio(tp, tp+fp), ratio(tp, tp+fn)
		f := 0.0
		if p+r > 0 {
			f = 2 * p * r / (p + r)
		}
		m["precision."+l], m["recall."+l], m["f1."+l] = p, r, f
		f1s += f
	}
	m["macro_f1"] = f1s / float64(len(labels))
	return m, cm
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// ---- B1 intent routing ----------------------------------------------------

type B1Case struct {
	ID       string `json:"id"`
	Message  string `json:"user_message"`
	Gold     string `json:"gold_route"`
	Register string `json:"register"`
}

// RouteOf maps the orchestrator's first response to a route label.
func RouteOf(r *model.LLMResponse) (route, raw string) {
	fc := firstCall(r)
	switch {
	case fc == nil:
		return "unknown", "text"
	case fc.Name == "RouteIntent":
		intent, _ := fc.Args["intent"].(string)
		return strings.ToLower(strings.TrimSpace(intent)), "RouteIntent(" + intent + ")"
	case fc.Name == "GetServiceStatus":
		return "rag", "GetServiceStatus" // banking-info path
	default:
		return "unknown", fc.Name
	}
}

func (e *Env) B1(ctx context.Context, path string) (*Report, error) {
	var cases []B1Case
	fx, err := LoadFixture(path, &cases)
	if err != nil {
		return nil, err
	}
	rep := newReport("B1", fx, "orchestrator: 1 live call per case, chosen tool not executed")
	gold, pred := make([]string, len(cases)), make([]string, len(cases))
	details := make([]map[string]any, len(cases))
	var mu sync.Mutex
	errs := e.parallel(len(cases), func(i int) error {
		c := cases[i]
		pr, _, err := e.stageCall(ctx, "orchestrator", "", c.Message, nil)
		if err != nil {
			return err
		}
		route, raw := RouteOf(pr.resp)
		in, out := tokens(pr.resp)
		mu.Lock()
		gold[i], pred[i] = c.Gold, route
		rep.LiveCalls++
		rep.InputTok += in
		rep.OutputTok += out
		details[i] = map[string]any{"id": c.ID, "message": c.Message, "register": c.Register, "gold": c.Gold, "pred": route, "raw": raw, "ok": route == c.Gold}
		mu.Unlock()
		return nil
	})
	g, p := collect(gold, pred, details, errs, rep)
	m, cm := classification(g, p, []string{"loan", "rag", "transfer", "unknown"})
	rep.Metrics = m
	rep.Extra = map[string]any{"confusion_matrix": cm}
	rep.Details = details
	return rep.finish(), nil
}

func collect(gold, pred []string, details []map[string]any, errs []error, rep *Report) ([]string, []string) {
	var g, p []string
	for i, err := range errs {
		if err != nil {
			rep.Errors++
			details[i] = map[string]any{"index": i, "error": err.Error()}
			continue
		}
		g, p = append(g, gold[i]), append(p, pred[i])
	}
	rep.Cases = len(errs)
	return g, p
}

func newReport(stage string, fx *Fixture, iso string) *Report {
	th := fx.Thresholds
	if th == nil {
		th = map[string]float64{}
	}
	return &Report{Stage: stage, Description: fx.Description, Isolation: iso, Model: os.Getenv("VA_MODEL"),
		Thresholds: th, StartedAt: time.Now().UTC()}
}

func (r *Report) finish() *Report {
	r.DurationSec = time.Since(r.StartedAt).Seconds()
	if r.Model == "" {
		r.Model = "gemini-2.5-flash"
	}
	r.gate()
	return r
}

// ---- B2 tool selection ----------------------------------------------------

type B2Case struct {
	ID      string         `json:"id"`
	Agent   string         `json:"agent"`
	State   map[string]any `json:"state"`
	Message string         `json:"user_message"`
	Gold    string         `json:"gold_tool"`
}

// ToolOf maps an agent's first response to a tool label. transfer_to_agent
// is ADK's hand-back, not a domain tool, so it counts as "none".
func ToolOf(r *model.LLMResponse) (label, raw string) {
	fc := firstCall(r)
	if fc == nil {
		return "none", "text"
	}
	if fc.Name == "transfer_to_agent" {
		return "none", "transfer_to_agent"
	}
	return fc.Name, fc.Name
}

func (e *Env) B2(ctx context.Context, path string) (*Report, error) {
	var cases []B2Case
	fx, err := LoadFixture(path, &cases)
	if err != nil {
		return nil, err
	}
	rep := newReport("B2", fx, "route frozen from fixture; target agent: 1 live call per case, tool not executed")
	gold, pred := make([]string, len(cases)), make([]string, len(cases))
	details := make([]map[string]any, len(cases))
	var mu sync.Mutex
	errs := e.parallel(len(cases), func(i int) error {
		c := cases[i]
		var label, raw string
		var resp *model.LLMResponse
		if c.Agent == "orchestrator" {
			pr, _, err := e.stageCall(ctx, "orchestrator", "", c.Message, c.State)
			if err != nil {
				return err
			}
			resp = pr.resp
		} else {
			pr, _, err := e.stageCall(ctx, c.Agent, c.Agent, c.Message, c.State)
			if err != nil {
				return err
			}
			resp = pr.resp
		}
		label, raw = ToolOf(resp)
		in, out := tokens(resp)
		mu.Lock()
		gold[i], pred[i] = c.Gold, label
		rep.LiveCalls++
		rep.InputTok += in
		rep.OutputTok += out
		details[i] = map[string]any{"id": c.ID, "agent": c.Agent, "message": c.Message, "gold": c.Gold, "pred": label, "raw": raw, "ok": label == c.Gold}
		mu.Unlock()
		return nil
	})
	g, p := collect(gold, pred, details, errs, rep)
	labels := uniq(append(append([]string{}, g...), p...))
	m, cm := classification(g, p, labels)
	rep.Metrics = m
	rep.Extra = map[string]any{"confusion_matrix": cm, "labels": labels}
	rep.Details = details
	return rep.finish(), nil
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// ---- B3 argument extraction -----------------------------------------------

type B3Case struct {
	ID       string         `json:"id"`
	Tool     string         `json:"tool"`
	Message  string         `json:"user_message"`
	State    map[string]any `json:"state"`
	Expected map[string]any `json:"expected_arguments"`
}

var toolAgent = map[string]string{"SaveLoanDetails": "loan", "ExecuteTransfer": "transfer", "SearchBankingDocs": "rag"}

var nonDigit = regexp.MustCompile(`\D`)

// argEqual compares numbers numerically and account numbers by digits.
func argEqual(key string, want, got any) bool {
	wf, wok := toF(want)
	gf, gok := toF(got)
	if wok && gok {
		return math.Abs(wf-gf) < 1e-6
	}
	ws, gs := fmt.Sprint(want), fmt.Sprint(got)
	if key == "account" {
		return nonDigit.ReplaceAllString(ws, "") == nonDigit.ReplaceAllString(gs, "")
	}
	return strings.TrimSpace(ws) == strings.TrimSpace(gs)
}

func toF(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(t, ",", ""), 64)
		return f, err == nil && t != ""
	}
	return 0, false
}

func (e *Env) B3(ctx context.Context, path string) (*Report, error) {
	var cases []B3Case
	fx, err := LoadFixture(path, &cases)
	if err != nil {
		return nil, err
	}
	rep := newReport("B3", fx, "route frozen; tool owner agent: 1 live call; args restored host-side from the privacy vault; tool not executed")
	details := make([]map[string]any, len(cases))
	var mu sync.Mutex
	var tp, fp, fn, exact int
	errs := e.parallel(len(cases), func(i int) error {
		c := cases[i]
		ag := toolAgent[c.Tool]
		pr, t, err := e.stageCall(ctx, ag, ag, c.Message, c.State)
		if err != nil {
			return err
		}
		got := map[string]any{}
		called := ""
		if fc := firstCall(pr.resp); fc != nil {
			called = fc.Name
			if fc.Name == c.Tool {
				vault := privacy.VaultFromState(stateGet(t.state, privacy.StateVault))
				for k, v := range fc.Args {
					if s, ok := v.(string); ok {
						v, _ = privacy.Restore(s, vault)
					}
					got[k] = v
				}
			}
		}
		ctp, cfp, cfn := 0, 0, 0
		for k, w := range c.Expected {
			if g, ok := got[k]; ok && argEqual(k, w, g) {
				ctp++
			} else {
				cfn++
			}
		}
		for k, g := range got {
			if w, ok := c.Expected[k]; !ok {
				if f, isNum := toF(g); !(isNum && f == 0) { // omitted-as-zero is not an extraction
					cfp++
				}
			} else if !argEqual(k, w, g) {
				cfp++
			}
		}
		in, out := tokens(pr.resp)
		mu.Lock()
		tp, fp, fn = tp+ctp, fp+cfp, fn+cfn
		if cfp == 0 && cfn == 0 {
			exact++
		}
		rep.LiveCalls++
		rep.InputTok += in
		rep.OutputTok += out
		details[i] = map[string]any{"id": c.ID, "message": c.Message, "tool": c.Tool, "called": called,
			"expected": c.Expected, "got": got, "exact": cfp == 0 && cfn == 0}
		mu.Unlock()
		return nil
	})
	for i, err := range errs {
		if err != nil {
			rep.Errors++
			details[i] = map[string]any{"id": cases[i].ID, "error": err.Error()}
		}
	}
	rep.Cases = len(cases)
	p, r := ratio(tp, tp+fp), ratio(tp, tp+fn)
	f := 0.0
	if p+r > 0 {
		f = 2 * p * r / (p + r)
	}
	rep.Metrics = map[string]float64{"exact_match": ratio(exact, len(cases)-rep.Errors), "field_precision": p, "field_recall": r, "field_f1": f}
	rep.Details = details
	return rep.finish(), nil
}

// ---- B4 retrieval ---------------------------------------------------------

type B4Case struct {
	ID     string   `json:"id"`
	Query  string   `json:"query"`
	Golden []string `json:"golden_chunk_ids"`
	K      int      `json:"k"`
}

func (e *Env) B4(ctx context.Context, path string) (*Report, error) {
	var cases []B4Case
	fx, err := LoadFixture(path, &cases)
	if err != nil {
		return nil, err
	}
	rep := newReport("B4", fx, "Retriever.Search only (real embeddings: "+e.Embedder.Name()+"); no LLM")
	rep.Model = e.Embedder.Name()
	// Rank the whole corpus so MRR is defined even beyond k.
	ret := rag.NewRetriever(e.Embedder, len(rag.Corpus))
	details := make([]map[string]any, len(cases))
	var recall, mrr, hit float64
	errs := make([]error, len(cases))
	for i, c := range cases {
		for _, g := range c.Golden {
			if _, ok := rag.Corpus[g]; !ok {
				return nil, fmt.Errorf("B4 case %s: golden id %q not in corpus", c.ID, g)
			}
		}
		docs, err := ret.Search(ctx, c.Query)
		if err != nil {
			errs[i] = err
			rep.Errors++
			details[i] = map[string]any{"id": c.ID, "error": err.Error()}
			continue
		}
		k := c.K
		if k <= 0 {
			k = 3
		}
		gold := map[string]bool{}
		for _, g := range c.Golden {
			gold[g] = true
		}
		var top []string
		found, rr := 0, 0.0
		for rank, d := range docs {
			if rank < k {
				top = append(top, d.ID)
				if gold[d.ID] {
					found++
				}
			}
			if gold[d.ID] && rr == 0 {
				rr = 1 / float64(rank+1)
			}
		}
		rc := ratio(found, len(gold))
		recall += rc
		mrr += rr
		if found > 0 {
			hit++
		}
		details[i] = map[string]any{"id": c.ID, "query": c.Query, "golden": c.Golden, "top_k": top, "recall_at_k": rc, "reciprocal_rank": rr}
	}
	n := float64(len(cases) - rep.Errors)
	rep.Cases = len(cases)
	rep.Metrics = map[string]float64{"recall_at_k": recall / n, "mrr": mrr / n, "hit_rate_at_k": hit / n}
	rep.Details = details
	return rep.finish(), nil
}

// ---- B5 completion --------------------------------------------------------

type B5Case struct {
	ID        string     `json:"id"`
	Message   string     `json:"user_message"`
	ChunkIDs  []string   `json:"injected_chunk_ids"`
	Required  [][]string `json:"required_facts"` // all groups; any synonym within a group
	Forbidden []string   `json:"forbidden_hallucinations"`
}

var numRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
var listMarker = regexp.MustCompile(`(?m)^\s*\d+[\.\)]\s`)
var docRef = regexp.MustCompile(`doc\d+`)

func numbers(s string) []string {
	s = docRef.ReplaceAllString(listMarker.ReplaceAllString(s, " "), " ")
	var out []string
	for _, m := range numRe.FindAllString(s, -1) {
		f, err := strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64)
		if err == nil {
			out = append(out, strconv.FormatFloat(f, 'f', -1, 64))
		}
	}
	return out
}

func declOf(tools []interface{ Name() string }, name string) *genai.FunctionDeclaration {
	for _, t := range tools {
		if d, ok := t.(interface{ Declaration() *genai.FunctionDeclaration }); ok && t.Name() == name {
			return d.Declaration()
		}
	}
	return nil
}

func (e *Env) B5(ctx context.Context, path string) (*Report, error) {
	var cases []B5Case
	fx, err := LoadFixture(path, &cases)
	if err != nil {
		return nil, err
	}
	rep := newReport("B5", fx, "golden chunks injected as the SearchBankingDocs result; rag agent completion: 1 live call; no retrieval")
	tools, err := rag.NewTools(nil)
	if err != nil {
		return nil, err
	}
	var named []interface{ Name() string }
	for _, t := range tools {
		named = append(named, t)
	}
	decl := declOf(named, "SearchBankingDocs")
	details := make([]map[string]any, len(cases))
	var mu sync.Mutex
	var correct, grounded, clean float64
	errs := e.parallel(len(cases), func(i int) error {
		c := cases[i]
		var docs []rag.RetrievedDoc
		var chunkText strings.Builder
		for _, id := range c.ChunkIDs {
			txt, ok := rag.Corpus[id]
			if !ok {
				return fmt.Errorf("chunk %q not in corpus", id)
			}
			docs = append(docs, rag.RetrievedDoc{ID: id, Content: txt, Score: 1})
			chunkText.WriteString(txt + "\n")
		}
		m, err := e.NewModel("rag")
		if err != nil {
			return err
		}
		req := &model.LLMRequest{
			Contents: []*genai.Content{
				genai.NewContentFromText(c.Message, genai.RoleUser),
				{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "golden", Name: "SearchBankingDocs", Args: map[string]any{"query": c.Message}}}}},
				{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "golden", Name: "SearchBankingDocs",
					Response: map[string]any{"results": docs}}}}},
			},
			Config: &genai.GenerateContentConfig{
				SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: rag.Instruction}}},
				Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{decl}}},
			},
		}
		var resp *model.LLMResponse
		for r, err := range m.GenerateContent(ctx, req, false) {
			if err != nil {
				return err
			}
			resp = r
		}
		ans := respText(resp)
		if ans == "" {
			return errors.New("empty completion")
		}
		met := 0
		var missing []string
		for _, grp := range c.Required {
			ok := false
			for _, syn := range grp {
				if strings.Contains(ans, syn) {
					ok = true
					break
				}
			}
			if ok {
				met++
			} else {
				missing = append(missing, strings.Join(grp, "|"))
			}
		}
		allowed := map[string]bool{}
		for _, n := range numbers(chunkText.String() + " " + c.Message) {
			allowed[n] = true
		}
		var ungrounded []string
		for _, n := range numbers(ans) {
			if !allowed[n] {
				ungrounded = append(ungrounded, n)
			}
		}
		var halluc []string
		for _, f := range c.Forbidden {
			if strings.Contains(ans, f) {
				halluc = append(halluc, f)
			}
		}
		in, out := tokens(resp)
		mu.Lock()
		correct += ratio(met, len(c.Required))
		if len(ungrounded) == 0 {
			grounded++
		}
		if len(halluc) == 0 {
			clean++
		}
		rep.LiveCalls++
		rep.InputTok += in
		rep.OutputTok += out
		details[i] = map[string]any{"id": c.ID, "message": c.Message, "chunks": c.ChunkIDs, "answer": ans,
			"missing_facts": missing, "ungrounded_numbers": ungrounded, "forbidden_found": halluc}
		mu.Unlock()
		return nil
	})
	for i, err := range errs {
		if err != nil {
			rep.Errors++
			details[i] = map[string]any{"id": cases[i].ID, "error": err.Error()}
		}
	}
	n := float64(len(cases) - rep.Errors)
	rep.Cases = len(cases)
	rep.Metrics = map[string]float64{"correctness": correct / n, "groundedness": grounded / n, "no_forbidden": clean / n}
	rep.Details = details
	return rep.finish(), nil
}

// ---- B6 end to end --------------------------------------------------------

type B6Case struct {
	ID       string            `json:"id"`
	Message  string            `json:"user_message"`
	Route    string            `json:"expected_route"` // rag|loan|transfer|status|none
	Keywords [][]string        `json:"expected_keywords"`
	State    map[string]string `json:"expected_state"` // key -> substring the JSON value must contain
}

func (e *Env) B6(ctx context.Context, path string) (*Report, error) {
	var cases []B6Case
	fx, err := LoadFixture(path, &cases)
	if err != nil {
		return nil, err
	}
	rep := newReport("B6", fx, "full production runner (all agents, tools, plugins); live model")
	details := make([]map[string]any, len(cases))
	var mu sync.Mutex
	var passed, routeOK, kwOK, stOK float64
	errs := e.parallel(len(cases), func(i int) error {
		c := cases[i]
		mods := map[string]model.LLM{}
		counters := map[string]*counting{}
		for _, a := range agentNames {
			m, err := e.NewModel(a)
			if err != nil {
				return err
			}
			cm := &counting{inner: m}
			counters[a] = cm
			mods[a] = cm
		}
		t, err := runTurn(ctx, mods, c.Message, nil)
		if err != nil {
			return err
		}
		final := finalText(t.events)
		route := "none"
		if v, _ := stateGet(t.state, "route.last").(string); v != "" {
			route = v
		} else if hasCall(t.events, "GetServiceStatus") {
			route = "status"
		}
		kw := true
		var missing []string
		for _, grp := range c.Keywords {
			ok := false
			for _, s := range grp {
				if strings.Contains(final, s) {
					ok = true
				}
			}
			if !ok {
				kw = false
				missing = append(missing, strings.Join(grp, "|"))
			}
		}
		st := true
		stGot := map[string]any{}
		for k, want := range c.State {
			v := stateGet(t.state, k)
			b, _ := json.Marshal(v)
			stGot[k] = v
			if v == nil || !strings.Contains(string(b), want) {
				st = false
			}
		}
		calls, in, out := 0, 0, 0
		for _, cm := range counters {
			calls += cm.calls
			in += cm.in
			out += cm.out
		}
		ok := route == c.Route && kw && st
		mu.Lock()
		if ok {
			passed++
		}
		if route == c.Route {
			routeOK++
		}
		if kw {
			kwOK++
		}
		if st {
			stOK++
		}
		rep.LiveCalls += calls
		rep.InputTok += in
		rep.OutputTok += out
		details[i] = map[string]any{"id": c.ID, "message": c.Message, "expected_route": c.Route, "route": route,
			"final_text": final, "missing_keywords": missing, "state": stGot, "llm_calls": calls, "pass": ok}
		mu.Unlock()
		return nil
	})
	for i, err := range errs {
		if err != nil {
			rep.Errors++
			details[i] = map[string]any{"id": cases[i].ID, "error": err.Error()}
		}
	}
	n := float64(len(cases) - rep.Errors)
	rep.Cases = len(cases)
	rep.Metrics = map[string]float64{"pass_rate": passed / n, "route_accuracy": routeOK / n, "keyword_match": kwOK / n, "state_match": stOK / n}
	rep.Details = details
	return rep.finish(), nil
}

type counting struct {
	inner          model.LLM
	mu             sync.Mutex
	calls, in, out int
}

func (c *counting) Name() string { return c.inner.Name() }

func (c *counting) GenerateContent(ctx context.Context, req *model.LLMRequest, s bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for r, err := range c.inner.GenerateContent(ctx, req, s) {
			if err == nil {
				i, o := tokens(r)
				c.mu.Lock()
				c.calls++
				c.in += i
				c.out += o
				c.mu.Unlock()
			}
			if !yield(r, err) {
				return
			}
		}
	}
}

func finalText(evs []*session.Event) string {
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

func hasCall(evs []*session.Event, name string) bool {
	for _, ev := range evs {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == name {
				return true
			}
		}
	}
	return false
}

// ---- output ---------------------------------------------------------------

// Write stores the report as <dir>/<stage>.json.
func Write(dir string, r *Report) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, r.Stage+".json")
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return p, os.WriteFile(p, b, 0o644)
}

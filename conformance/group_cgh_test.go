package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/genai"

	"va_adk_pilot/internal/agents/loan"
	"va_adk_pilot/internal/agents/orchestrator"
	"va_adk_pilot/internal/agents/transfer"
	"va_adk_pilot/internal/config"
	"va_adk_pilot/internal/cost"
	"va_adk_pilot/internal/gateway"
	"va_adk_pilot/internal/models"
	"va_adk_pilot/internal/replay"
)

// ---------------------------------------------------------------- fake Vertex
//
// The only fake in these tests is the Vertex HTTP endpoint (no network in
// CI). The gateway, its cache logic, the openai-compatible ADK model and the
// agents are the production code paths.

type fakeVertex struct {
	mu        sync.Mutex
	srv       *httptest.Server
	creates   int
	genPaths  []string
	genBodies []map[string]any
	reply     string
}

func newFakeVertex(t *testing.T) *fakeVertex {
	f := &fakeVertex{reply: "สวัสดีค่ะ ยินดีให้บริการ"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cachedContents"):
			f.creates++
			fmt.Fprintf(w, `{"name":"projects/p/locations/l/cachedContents/cc-%d"}`, f.creates)
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			f.genPaths = append(f.genPaths, r.URL.Path)
			f.genBodies = append(f.genBodies, body)
			cached := 0
			if body["cachedContent"] != nil {
				cached = 1500
			}
			fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]}}],"usageMetadata":{"promptTokenCount":1600,"candidatesTokenCount":12,"totalTokenCount":1612,"cachedContentTokenCount":%d}}`, f.reply, cached)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("VA_VERTEX_BASE_URL", f.srv.URL)
	return f
}

func newGateway(t *testing.T) *httptest.Server {
	t.Setenv("VA_GATEWAY_AUDIT_LOG", filepath.Join(t.TempDir(), "audit.jsonl"))
	gw := gateway.NewServerWithTokenSource(config.GatewayConfig{VertexProject: "p", VertexLocation: "l"},
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}))
	s := httptest.NewServer(gw.Handler())
	t.Cleanup(s.Close)
	return s
}

func postResponses(t *testing.T, gw, instructions, ttl string) (status string, cached int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":        "gemini-2.5-flash",
		"instructions": instructions,
		"input":        []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		"tools":        []any{map[string]any{"type": "function", "name": "RouteIntent", "parameters": map[string]any{"type": "object"}}},
	})
	req, _ := http.NewRequest(http.MethodPost, gw+"/v1/responses", bytes.NewReader(body))
	req.Header.Set(gateway.HeaderBreakpoint, "system_and_tools")
	req.Header.Set(gateway.HeaderTTL, ttl)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Usage struct {
			Details struct {
				Cached int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.Header.Get("X-VA-Cache"), out.Usage.Details.Cached
}

// FR-C1: deliberate breakpoint after system+tools; hit/miss verified; TTL.
func TestFR_C1(t *testing.T) {
	fv := newFakeVertex(t)
	gw := newGateway(t)
	instr := fmt.Sprintf("C1 conformance prefix %d. %s", time.Now().UnixNano(), strings.Repeat("นโยบายธนาคาร ", 200))

	st, cached := postResponses(t, gw.URL, instr, "2s")
	if st != "MISS" || fv.creates != 1 {
		t.Fatalf("first request: status=%s creates=%d, want MISS/1", st, fv.creates)
	}
	st, cached = postResponses(t, gw.URL, instr, "2s")
	if st != "HIT" || fv.creates != 1 {
		t.Fatalf("second request: status=%s creates=%d, want HIT/1", st, fv.creates)
	}
	last := fv.genBodies[len(fv.genBodies)-1]
	if last["cachedContent"] == nil || last["systemInstruction"] != nil || last["tools"] != nil {
		t.Fatalf("HIT must send cachedContent instead of the prefix: %v", last)
	}
	if cached != 1500 {
		t.Fatalf("cached token count not propagated to client usage: %d", cached)
	}
	time.Sleep(2100 * time.Millisecond)
	if st, _ = postResponses(t, gw.URL, instr, "2s"); st != "MISS" || fv.creates != 2 {
		t.Fatalf("after TTL: status=%s creates=%d, want MISS/2", st, fv.creates)
	}
}

// FR-H1: every model call goes through the gateway; swapping the model is a
// config change; no agent code imports a vendor SDK client.
func TestFR_H1(t *testing.T) {
	fv := newFakeVertex(t)
	gw := newGateway(t)
	t.Setenv("VA_GATEWAY_URL", gw.URL+"/v1")
	t.Setenv("VA_CACHE_AGENTS", "")

	run := func(modelName string) string {
		t.Setenv("VA_MODEL", modelName)
		llm, err := models.ForAgent(context.Background(), "orchestrator")
		must(t, err)
		app := NewAppWithModel(t, llm)
		sid := app.NewSession()
		return FinalText(app.Say(sid, "สวัสดีค่ะ"))
	}
	if got := run("gemini-2.5-flash"); !strings.Contains(got, fv.reply) {
		t.Fatalf("answer did not come from the gateway: %q", got)
	}
	if got := run("gemini-2.5-pro"); !strings.Contains(got, fv.reply) {
		t.Fatalf("answer did not come from the gateway: %q", got)
	}
	var flash, pro int
	for _, p := range fv.genPaths {
		switch {
		case strings.Contains(p, "/models/gemini-2.5-flash:"):
			flash++
		case strings.Contains(p, "/models/gemini-2.5-pro:"):
			pro++
		}
	}
	if flash == 0 || pro == 0 {
		t.Fatalf("model swap via VA_MODEL not reflected upstream: %v", fv.genPaths)
	}
	audit, _ := os.ReadFile(gateway.AuditPath())
	if n := strings.Count(string(audit), "\n"); n < len(fv.genPaths) {
		t.Fatalf("gateway audit has %d entries for %d upstream calls", n, len(fv.genPaths))
	}
	// Static check: only the gateway talks to Vertex, over plain HTTP.
	vendor := regexp.MustCompile(`"cloud\.google\.com/go/(vertexai|aiplatform)|"google\.golang\.org/adk/v2/model/gemini"|genai\.NewClient\(`)
	_ = filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() && (d.Name() == ".bin" || d.Name() == ".out" || d.Name() == "conformance") {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			if b, _ := os.ReadFile(p); vendor.Match(b) {
				t.Errorf("%s bypasses the gateway with a vendor SDK client", p)
			}
		}
		return nil
	})
}

// FR-G1: the stock upstream ADK module, no fork or replace directive.
func TestFR_G1(t *testing.T) {
	b, err := os.ReadFile("../go.mod")
	must(t, err)
	mod := string(b)
	if !regexp.MustCompile(`(?m)^\s*google\.golang\.org/adk/v2 v\d+\.\d+\.\d+\s*$`).MatchString(mod) {
		t.Fatal("go.mod does not require a released google.golang.org/adk/v2")
	}
	if regexp.MustCompile(`(?m)^\s*replace\b.*adk`).MatchString(mod) {
		t.Fatal("go.mod replaces ADK with a fork")
	}
	sum, err := os.ReadFile("../go.sum")
	must(t, err)
	if !strings.Contains(string(sum), "google.golang.org/adk/v2 ") {
		t.Fatal("go.sum has no checksum for google.golang.org/adk/v2")
	}
}

// FR-H2: per-request, per-agent, per-stage token/cost report from real
// runner events.
func TestFR_H2(t *testing.T) {
	app := NewApp(t, ragScript("ค่าธรรมเนียมบัตรเดบิต", "200 บาทต่อปี (doc27)"))
	sid := app.NewSession()
	app.Say(sid, "บัตรเดบิตค่าธรรมเนียมเท่าไหร่")
	price := cost.DefaultPrice()
	rep := cost.FromEvents(app.Events(sid), price)
	if rep.Total.InputTokens != 440 || rep.Total.OutputTokens != 70 || rep.Total.Calls != 3 {
		t.Fatalf("totals: %+v (want 3 calls, 440 in, 70 out)", rep.Total)
	}
	have := map[string]bool{}
	for _, l := range rep.Lines {
		have[l.Agent+"/"+l.Stage] = true
		if l.InvocationID == "" {
			t.Errorf("line without request id: %+v", l)
		}
	}
	for _, want := range []string{"orchestrator/tool_selection", "rag/tool_selection", "rag/completion"} {
		if !have[want] {
			t.Errorf("missing %s line: %+v", want, rep.Lines)
		}
	}
	wantCost := (440*price.InputPerM + 70*price.OutputPerM) / 1e6
	if math.Abs(rep.Total.CostUSD-wantCost) > 1e-12 {
		t.Fatalf("cost %.10f != %.10f", rep.Total.CostUSD, wantCost)
	}
}

// FR-H3: record a session's model exchanges, replay through the identical
// flow with no model, get identical output; divergence is detected.
func TestFR_H3(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rec.jsonl")
	const q = "ค่าธรรมเนียมบัตรเดบิต"
	scripted := NewScripted(ragScript(q, "200 บาทต่อปี (doc27)"))
	rec := NewAppWithModel(t, &replay.Recorder{LLM: scripted, File: file})
	sid := rec.NewSession()
	recText := FinalText(rec.Say(sid, q))
	recIDs, _ := json.Marshal(rec.State(sid, "rag.last_retrieved_ids"))

	player, err := replay.Load(file)
	must(t, err)
	rep := NewAppWithModel(t, player)
	sid2 := rep.NewSession()
	repEvs := rep.Say(sid2, q)
	repIDs, _ := json.Marshal(rep.State(sid2, "rag.last_retrieved_ids"))
	if FinalText(repEvs) != recText || string(repIDs) != string(recIDs) || player.Remaining() != 0 {
		t.Fatalf("replay differs: %q vs %q, ids %s vs %s, remaining=%d", FinalText(repEvs), recText, repIDs, recIDs, player.Remaining())
	}
	if len(FunctionCalls(repEvs, "SearchBankingDocs")) != 1 {
		t.Fatal("replay did not drive the real tool")
	}

	player2, err := replay.Load(file)
	must(t, err)
	div := NewAppWithModel(t, player2)
	sid3 := div.NewSession()
	if _, err := div.SendErr(sid3, genai.NewContentFromText("อยากกู้เงินซื้อบ้าน", genai.RoleUser)); err == nil {
		t.Fatal("a different input replayed silently; divergence not detected")
	}
}

var (
	_ = loan.Instruction
	_ = orchestrator.ServiceStatus
	_ = transfer.StateStatus
)

// FR-C1 (after_turn:N): the breakpoint can also cover leading conversation
// turns (a long reference document); only the turns after it are sent.
func TestFR_C1_AfterTurn(t *testing.T) {
	fv := newFakeVertex(t)
	gw := newGateway(t)
	doc := fmt.Sprintf("manual %d %s", time.Now().UnixNano(), strings.Repeat("policy clause. ", 300))
	body, _ := json.Marshal(map[string]any{"model": "gemini-2.5-flash", "instructions": "sys",
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": doc}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "question"}}},
		}})
	send := func() string {
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/responses", bytes.NewReader(body))
		req.Header.Set(gateway.HeaderBreakpoint, "after_turn:2")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		resp.Body.Close()
		return resp.Header.Get("X-VA-Cache")
	}
	if a, b := send(), send(); a != "MISS" || b != "HIT" || fv.creates != 1 {
		t.Fatalf("got %s,%s creates=%d; want MISS,HIT,1", a, b, fv.creates)
	}
	last := fv.genBodies[len(fv.genBodies)-1]
	if n := len(last["contents"].([]any)); n != 1 {
		t.Fatalf("HIT must send only the turns after the breakpoint, sent %d", n)
	}
}

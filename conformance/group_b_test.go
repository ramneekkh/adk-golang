package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/model"

	"va_adk_pilot/internal/embed"
	"va_adk_pilot/internal/evalharness"
)

// Group B: the stage harness must evaluate each stage in isolation (one
// model call for the stage under test, nothing downstream executes) and
// produce a CI-usable per-stage report whose gate reflects the scores.

func writeFixture(t *testing.T, stage string, thresholds map[string]float64, cases any) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"stage": stage, "thresholds": thresholds, "cases": cases})
	p := filepath.Join(t.TempDir(), stage+".json")
	must(t, os.WriteFile(p, b, 0o644))
	return p
}

func stageEnv(llm model.LLM) *evalharness.Env {
	return &evalharness.Env{
		NewModel:    func(string) (model.LLM, error) { return llm, nil },
		Embedder:    embed.Local{Dims: 256},
		Concurrency: 1,
	}
}

func TestFR_B1(t *testing.T) {
	initTelemetry()
	t.Setenv("VA_EMBEDDER", "local")
	llm := NewScripted(map[string]Step{"orchestrator": Route("loan"), "loan": Seq(Text("SHOULD NOT RUN"))})
	p := writeFixture(t, "B1", map[string]float64{"accuracy": 0.5}, []map[string]any{
		{"id": "1", "user_message": "อยากกู้เงิน", "gold_route": "loan"},
		{"id": "2", "user_message": "โอนเงิน 500", "gold_route": "transfer"},
	})
	rep, err := stageEnv(llm).B1(context.Background(), p)
	must(t, err)
	if llm.CallsFor("orchestrator") != 2 || llm.CallsFor("loan") != 0 {
		t.Fatalf("not isolated: orchestrator=%d loan=%d calls", llm.CallsFor("orchestrator"), llm.CallsFor("loan"))
	}
	if rep.Metrics["accuracy"] != 0.5 || !rep.Pass || rep.LiveCalls != 2 {
		t.Fatalf("report: %+v", rep.Metrics)
	}
}

func TestFR_B2(t *testing.T) {
	initTelemetry()
	t.Setenv("VA_EMBEDDER", "local")
	// Orchestrator must NOT be called (route frozen); rag makes one call.
	llm := NewScripted(map[string]Step{"rag": Seq(Call("SearchBankingDocs", map[string]any{"query": "x"}))})
	p := writeFixture(t, "B2", map[string]float64{"accuracy": 1}, []map[string]any{
		{"id": "1", "agent": "rag", "user_message": "เปิดบัญชี", "gold_tool": "SearchBankingDocs"},
		{"id": "2", "agent": "rag", "user_message": "ขอบคุณค่ะ", "gold_tool": "none"},
	})
	rep, err := stageEnv(llm).B2(context.Background(), p)
	must(t, err)
	if llm.CallsFor("orchestrator") != 0 || llm.CallsFor("rag") != 2 {
		t.Fatalf("not isolated: orchestrator=%d rag=%d", llm.CallsFor("orchestrator"), llm.CallsFor("rag"))
	}
	if rep.Metrics["accuracy"] != 0.5 || rep.Pass {
		t.Fatalf("gate must fail at 0.5 < 1.0: %+v pass=%v", rep.Metrics, rep.Pass)
	}
}

func TestFR_B3(t *testing.T) {
	initTelemetry()
	t.Setenv("VA_EMBEDDER", "local")
	llm := NewScripted(map[string]Step{"transfer": Seq(Call("ExecuteTransfer", map[string]any{"amount": 500, "account": "[ACCOUNT_1]"}))})
	p := writeFixture(t, "B3", map[string]float64{"field_f1": 1}, []map[string]any{
		{"id": "1", "tool": "ExecuteTransfer", "user_message": "โอนเงิน 500 บาท เลขบัญชี 023536333", "expected_arguments": map[string]any{"amount": 500, "account": "023536333"}},
	})
	rep, err := stageEnv(llm).B3(context.Background(), p)
	must(t, err)
	if rep.Metrics["field_f1"] != 1 || !rep.Pass {
		t.Fatalf("placeholder arg not restored host-side before scoring: %+v %v", rep.Metrics, rep.Details)
	}
	if llm.CallsFor("transfer") != 1 {
		t.Fatalf("transfer called %d times; tool must not execute", llm.CallsFor("transfer"))
	}
}

func TestFR_B4(t *testing.T) {
	p := writeFixture(t, "B4", map[string]float64{"recall_at_k": 0.5}, []map[string]any{
		{"id": "1", "query": "บัตรเดบิตมีค่าธรรมเนียมรายปี 200 บาท", "golden_chunk_ids": []string{"doc27"}, "k": 3},
	})
	rep, err := stageEnv(nil).B4(context.Background(), p)
	must(t, err)
	if rep.Metrics["recall_at_k"] != 1 || rep.Metrics["mrr"] != 1 || rep.LiveCalls != 0 {
		t.Fatalf("retrieval-only stage: %+v", rep.Metrics)
	}
}

func TestFR_B5(t *testing.T) {
	llm := NewScripted(map[string]Step{"unknown": Seq(Text("ค่าธรรมเนียมรายปี 200 บาท และส่วนลด 999 บาท (doc27)"))})
	p := writeFixture(t, "B5", map[string]float64{"groundedness": 1}, []map[string]any{
		{"id": "1", "user_message": "ค่าธรรมเนียมบัตรเดบิต", "injected_chunk_ids": []string{"doc27"}, "required_facts": [][]string{{"200"}}},
	})
	rep, err := stageEnv(llm).B5(context.Background(), p)
	must(t, err)
	if rep.Metrics["correctness"] != 1 || rep.Metrics["groundedness"] != 0 || rep.Pass {
		t.Fatalf("ungrounded number 999 must fail groundedness: %+v", rep.Metrics)
	}
}

func TestFR_B6(t *testing.T) {
	initTelemetry()
	t.Setenv("VA_EMBEDDER", "local")
	llm := NewScripted(ragScript("ค่าธรรมเนียมบัตรเดบิต", "200 บาทต่อปี (doc27)"))
	p := writeFixture(t, "B6", map[string]float64{"pass_rate": 1}, []map[string]any{
		{"id": "1", "user_message": "บัตรเดบิตค่าธรรมเนียมเท่าไหร่", "expected_route": "rag",
			"expected_keywords": [][]string{{"200"}}, "expected_state": map[string]string{"rag.last_retrieved_ids": "doc"}},
	})
	rep, err := stageEnv(llm).B6(context.Background(), p)
	must(t, err)
	if !rep.Pass || llm.CallsFor("orchestrator") != 1 || llm.CallsFor("rag") != 2 {
		t.Fatalf("full pipeline not exercised: %+v %v", rep.Metrics, rep.Details)
	}
}

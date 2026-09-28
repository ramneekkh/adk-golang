// Command eval runs the PRD Group B stage evaluations against the live model
// (through the in-repo gateway) and writes per-stage reports for CI.
//
//	go run ./cmd/eval                       # all stages
//	go run ./cmd/eval -stages B1,B4         # selected stages
//
// Exit status is non-zero if any stage misses a threshold in its fixture.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/adk/v2/model"

	"va_adk_pilot/internal/config"
	"va_adk_pilot/internal/embed"
	"va_adk_pilot/internal/evalharness"
	"va_adk_pilot/internal/gateway"
	"va_adk_pilot/internal/models"
)

var fixtureFor = map[string]string{
	"B1": "b1_intent_routing.json",
	"B2": "b2_tool_selection.json",
	"B3": "b3_argument_extraction.json",
	"B4": "b4_retrieval.json",
	"B5": "b5_completion.json",
	"B6": "b6_end_to_end.json",
}

func main() {
	stages := flag.String("stages", "B1,B2,B3,B4,B5,B6", "comma-separated stages")
	fixtures := flag.String("fixtures", "eval/fixtures", "fixture directory")
	out := flag.String("out", "eval/out", "report directory")
	conc := flag.Int("concurrency", 8, "parallel cases")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	if _, ok := os.LookupEnv("VA_CACHE_AGENTS"); !ok {
		os.Setenv("VA_CACHE_AGENTS", "") // stage evals measure quality, not caching
	}
	if os.Getenv("VA_GATEWAY_AUDIT_LOG") == "" {
		os.Setenv("VA_GATEWAY_AUDIT_LOG", filepath.Join(*out, "gateway_audit.jsonl"))
	}
	if os.Getenv("VA_GATEWAY_URL") == "" {
		// Self-contained: start the real gateway in-process (Vertex via ADC).
		gw, err := gateway.NewServer(config.LoadGatewayConfig())
		if err != nil {
			log.Fatalf("gateway (ADC): %v — run `gcloud auth application-default login`", err)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		go http.Serve(l, gw.Handler())
		os.Setenv("VA_GATEWAY_URL", "http://"+l.Addr().String()+"/v1")
	}
	gwURL := config.GetGatewayURL()
	log.Printf("gateway=%s model=%s", gwURL, config.GetModel())

	ctx := context.Background()
	env := &evalharness.Env{
		NewModel:    func(a string) (model.LLM, error) { return models.ForAgent(ctx, a) },
		Embedder:    embed.FromEnv(gwURL),
		Concurrency: *conc,
		Retries:     2,
	}
	run := map[string]func(context.Context, string) (*evalharness.Report, error){
		"B1": env.B1, "B2": env.B2, "B3": env.B3, "B4": env.B4, "B5": env.B5, "B6": env.B6,
	}

	var sel []string
	for _, s := range strings.Split(*stages, ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			sel = append(sel, s)
		}
	}
	sort.Strings(sel)
	allPass := true
	var summary strings.Builder
	summary.WriteString("| Stage | Pass | Cases | Errors | Key metrics | Live calls |\n|---|---|---|---|---|---|\n")
	for _, s := range sel {
		fn, ok := run[s]
		if !ok {
			log.Fatalf("unknown stage %s", s)
		}
		log.Printf("running %s ...", s)
		rep, err := fn(ctx, filepath.Join(*fixtures, fixtureFor[s]))
		if err != nil {
			log.Fatalf("%s: %v", s, err)
		}
		p, err := evalharness.Write(*out, rep)
		if err != nil {
			log.Fatal(err)
		}
		var keys []string
		for k := range rep.Thresholds {
			keys = append(keys, fmt.Sprintf("%s=%.3f (≥%.2f)", k, rep.Metrics[k], rep.Thresholds[k]))
		}
		sort.Strings(keys)
		verdict := "PASS"
		if !rep.Pass {
			verdict = "FAIL"
			allPass = false
		}
		log.Printf("%s %s  %s  -> %s", s, verdict, strings.Join(keys, "  "), p)
		fmt.Fprintf(&summary, "| %s | %s | %d | %d | %s | %d |\n", s, verdict, rep.Cases, rep.Errors, strings.Join(keys, "<br>"), rep.LiveCalls)
	}
	_ = os.WriteFile(filepath.Join(*out, "SUMMARY.md"), []byte(summary.String()), 0o644)
	fmt.Print(summary.String())
	if !allPass {
		os.Exit(1)
	}
}

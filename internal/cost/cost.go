// Package cost implements FR-H2: token and cost accounting per request per
// stage, computed from the usage metadata ADK records on every model event.
package cost

import (
	"os"
	"sort"
	"strconv"

	"google.golang.org/adk/v2/session"
)

// Price in USD per 1M tokens. Override with VA_PRICE_INPUT_PER_M,
// VA_PRICE_CACHED_PER_M, VA_PRICE_OUTPUT_PER_M.
type Price struct{ InputPerM, CachedPerM, OutputPerM float64 }

func envF(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}

// DefaultPrice is gemini-2.5-flash list pricing (text), overridable by env.
func DefaultPrice() Price {
	return Price{
		InputPerM:  envF("VA_PRICE_INPUT_PER_M", 0.30),
		CachedPerM: envF("VA_PRICE_CACHED_PER_M", 0.03),
		OutputPerM: envF("VA_PRICE_OUTPUT_PER_M", 2.50),
	}
}

// Line is one (request, agent, stage) bucket.
type Line struct {
	InvocationID string  `json:"request_id"`
	Agent        string  `json:"agent"`
	Stage        string  `json:"stage"` // tool_selection | completion
	Calls        int     `json:"llm_calls"`
	InputTokens  int     `json:"input_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// Report is the per-stage breakdown plus totals.
type Report struct {
	Lines []Line `json:"lines"`
	Total Line   `json:"total"`
}

// StageOf classifies a model event: a response that calls a tool is the
// tool-selection stage; a text answer is the completion stage.
func StageOf(ev *session.Event) string {
	if ev.Content != nil {
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil {
				return "tool_selection"
			}
		}
	}
	return "completion"
}

// FromEvents aggregates usage from session events.
func FromEvents(events []*session.Event, price Price) Report {
	idx := map[string]*Line{}
	var order []string
	for _, ev := range events {
		if ev == nil || ev.UsageMetadata == nil {
			continue
		}
		u := ev.UsageMetadata
		stage := StageOf(ev)
		key := ev.InvocationID + "|" + ev.Author + "|" + stage
		l, ok := idx[key]
		if !ok {
			l = &Line{InvocationID: ev.InvocationID, Agent: ev.Author, Stage: stage}
			idx[key] = l
			order = append(order, key)
		}
		l.Calls++
		l.InputTokens += int(u.PromptTokenCount)
		l.CachedTokens += int(u.CachedContentTokenCount)
		l.OutputTokens += int(u.CandidatesTokenCount)
	}
	var r Report
	for _, k := range order {
		l := idx[k]
		uncached := l.InputTokens - l.CachedTokens
		if uncached < 0 {
			uncached = 0
		}
		l.CostUSD = (float64(uncached)*price.InputPerM + float64(l.CachedTokens)*price.CachedPerM + float64(l.OutputTokens)*price.OutputPerM) / 1e6
		r.Lines = append(r.Lines, *l)
		r.Total.Calls += l.Calls
		r.Total.InputTokens += l.InputTokens
		r.Total.CachedTokens += l.CachedTokens
		r.Total.OutputTokens += l.OutputTokens
		r.Total.CostUSD += l.CostUSD
	}
	sort.SliceStable(r.Lines, func(i, j int) bool { return r.Lines[i].InvocationID < r.Lines[j].InvocationID })
	r.Total.Agent, r.Total.Stage = "ALL", "ALL"
	return r
}

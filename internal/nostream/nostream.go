// Package nostream adapts a model.LLM so every call is made non-streaming.
//
// The in-repo gateway implements /v1/chat/completions without SSE streaming,
// so when the ADK Web UI requests token streaming (/run_sse) we still issue a
// single non-streaming inference and yield the complete response once.
package nostream

import (
	"context"
	"iter"

	"google.golang.org/adk/v2/model"
)

type llm struct{ model.LLM }

func Wrap(m model.LLM) model.LLM { return &llm{LLM: m} }

func (l *llm) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return l.LLM.GenerateContent(ctx, req, false)
}

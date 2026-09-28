// Package loopguard implements FR-A6: a hard cap on agentic loop iterations
// (LLM calls) per agent per invocation, enforced in code.
package loopguard

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	vatel "va_adk_pilot/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"va_adk_pilot/internal/killswitch"
)

// Fallback is the safe message returned when the cap is hit.
const Fallback = "ขออภัยค่ะ ระบบหยุดการประมวลผลอัตโนมัติเพื่อความปลอดภัย เนื่องจากเกินจำนวนรอบการทำงานสูงสุดที่กำหนด กรุณาลองใหม่อีกครั้งหรือติดต่อเจ้าหน้าที่ค่ะ"

// MaxFromEnv reads VA_MAX_AGENT_LOOPS (default 3).
func MaxFromEnv() int {
	if v, err := strconv.Atoi(os.Getenv("VA_MAX_AGENT_LOOPS")); err == nil && v > 0 {
		return v
	}
	return 3
}

var counts sync.Map // key: invocationID|agent -> int

// Count returns the LLM-call count recorded for an agent in an invocation.
func Count(invocationID, agentName string) int {
	if v, ok := counts.Load(invocationID + "|" + agentName); ok {
		return v.(int)
	}
	return 0
}

// New returns a BeforeModelCallback allowing at most max LLM calls per agent
// per invocation. Call max+1 is not sent to the model: a va.loop_exceeded
// span event is emitted, state is updated and the safe fallback returned.
func New(max int) llmagent.BeforeModelCallback {
	return func(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		key := ctx.InvocationID() + "|" + ctx.AgentName()
		n := 1
		if v, ok := counts.Load(key); ok {
			n = v.(int) + 1
		}
		counts.Store(key, n)
		ctx.State().Set(fmt.Sprintf("loop.%s.iterations", ctx.AgentName()), n)
		if killswitch.Disabled("A6") || n <= max {
			return nil, nil
		}
		ctx.State().Set(fmt.Sprintf("loop.%s.halted_at", ctx.AgentName()), max)
		vatel.Emit(ctx, "va.loop_exceeded",
			attribute.String("agent", ctx.AgentName()),
			attribute.Int("max_iterations", max),
		)
		return &model.LLMResponse{Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: Fallback}},
		}}, nil
	}
}

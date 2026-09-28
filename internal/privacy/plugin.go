package privacy

import (
	"encoding/json"
	"strings"
	vatel "va_adk_pilot/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/genai"

	"va_adk_pilot/internal/killswitch"
)

// Session-state keys written by the plugin (visible in the ADK Web State tab).
const (
	StateVault        = "privacy.vault"       // token -> real value (host only)
	StateE1Findings   = "privacy.e1.findings" // [{kind, token}] of the last masked request
	StateE1Masked     = "privacy.e1.last_user_text_sent_to_llm"
	StateE2Raw        = "privacy.e2.last_model_text_raw"
	StateE2Delivered  = "privacy.e2.last_text_delivered"
	StateE2Normalized = "privacy.e2.normalizations"
)

func loadVault(ctx agent.Context) *Vault {
	if v, err := ctx.State().Get(StateVault); err == nil && v != nil {
		return VaultFromState(v)
	}
	return NewVault()
}

func maskAny(v any, vault *Vault, findings *[]Finding) any {
	switch t := v.(type) {
	case string:
		s, f := Mask(t, vault)
		*findings = append(*findings, f...)
		return s
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = maskAny(val, vault, findings)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskAny(val, vault, findings)
		}
		return out
	default:
		// Structs (e.g. typed tool results) — round-trip through JSON so nested
		// strings are masked too.
		b, err := json.Marshal(t)
		if err != nil {
			return v
		}
		var generic any
		if json.Unmarshal(b, &generic) != nil {
			return v
		}
		if _, isScalar := generic.(string); !isScalar {
			if _, ok := generic.(map[string]any); ok {
				return maskAny(generic, vault, findings)
			}
			if _, ok := generic.([]any); ok {
				return maskAny(generic, vault, findings)
			}
		}
		return v
	}
}

func restoreAny(v any, vault *Vault, norms *[]Normalization) any {
	switch t := v.(type) {
	case string:
		s, n := Restore(t, vault)
		*norms = append(*norms, n...)
		return s
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = restoreAny(val, vault, norms)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = restoreAny(val, vault, norms)
		}
		return out
	}
	return v
}

// BeforeModel is FR-E1: every outbound LLM request (all agents, all history,
// tool results, system instruction) is masked before it leaves the host.
func BeforeModel(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	if killswitch.Disabled("E1") {
		return nil, nil
	}
	vault := loadVault(ctx)
	before := len(vault.Tokens)
	var findings []Finding
	lastUserMasked := ""

	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.Text == "" {
				continue
			}
			masked, f := Mask(p.Text, vault)
			findings = append(findings, f...)
			p.Text = masked
			if c.Role == genai.RoleUser && len(f) > 0 {
				lastUserMasked = masked
			}
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil && p.FunctionCall.Args != nil {
				p.FunctionCall.Args = maskAny(p.FunctionCall.Args, vault, &findings).(map[string]any)
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Response != nil {
				p.FunctionResponse.Response = maskAny(p.FunctionResponse.Response, vault, &findings).(map[string]any)
			}
		}
	}
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			if p != nil && p.Text != "" {
				var f []Finding
				p.Text, f = Mask(p.Text, vault)
				findings = append(findings, f...)
			}
		}
	}

	if len(findings) > 0 || len(vault.Tokens) != before {
		ctx.State().Set(StateVault, vault.Snapshot())
		uniq := map[string]Finding{}
		for _, f := range findings {
			uniq[f.Token] = f
		}
		list := make([]Finding, 0, len(uniq))
		for _, f := range uniq {
			list = append(list, f)
		}
		ctx.State().Set(StateE1Findings, list)
		if lastUserMasked != "" {
			ctx.State().Set(StateE1Masked, lastUserMasked)
		}
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Int("va.privacy.e1.findings", len(list)))
		span.AddEvent("va.pii_masked", trace.WithAttributes(attribute.Int("count", len(list))))
	}
	return nil, nil
}

// AfterModel is FR-E2: placeholders in the model output (text and tool-call
// arguments) are restored before the response is delivered or a tool runs.
func AfterModel(ctx agent.Context, resp *model.LLMResponse, respErr error) (*model.LLMResponse, error) {
	if killswitch.Disabled("E2") || resp == nil || resp.Content == nil {
		return nil, nil
	}
	vault := loadVault(ctx)
	if len(vault.Tokens) == 0 {
		return nil, nil
	}
	var norms []Normalization
	var raw, delivered []string
	changed := false
	for _, p := range resp.Content.Parts {
		if p == nil {
			continue
		}
		if p.Text != "" {
			r, n := Restore(p.Text, vault)
			if r != p.Text {
				raw = append(raw, p.Text)
				delivered = append(delivered, r)
				changed = true
			}
			norms = append(norms, n...)
			p.Text = r
		}
		if p.FunctionCall != nil && p.FunctionCall.Args != nil {
			before, _ := json.Marshal(p.FunctionCall.Args)
			p.FunctionCall.Args = restoreAny(p.FunctionCall.Args, vault, &norms).(map[string]any)
			after, _ := json.Marshal(p.FunctionCall.Args)
			if string(before) != string(after) {
				changed = true
			}
		}
	}
	if changed {
		if len(raw) > 0 {
			ctx.State().Set(StateE2Raw, strings.Join(raw, "\n"))
			ctx.State().Set(StateE2Delivered, strings.Join(delivered, "\n"))
		}
		ctx.State().Set(StateE2Normalized, norms)
		vatel.Emit(ctx, "va.pii_restored", attribute.Int("normalizations", len(norms)))
	}
	return resp, nil
}

// NewPlugin returns the ADK runner plugin that applies E1/E2 to every agent.
func NewPlugin() (*plugin.Plugin, error) {
	return plugin.New(plugin.Config{
		Name:                "va_privacy_placeholder_swap",
		BeforeModelCallback: BeforeModel,
		AfterModelCallback:  AfterModel,
	})
}

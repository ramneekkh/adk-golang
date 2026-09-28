package loan

import (
	"fmt"
	vatel "va_adk_pilot/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"va_adk_pilot/internal/killswitch"
	"va_adk_pilot/internal/loopguard"
)

type SaveInfoArgs struct {
	Amount float64 `json:"amount,omitempty"`
	Tenure int     `json:"tenure_months,omitempty"`
	Income float64 `json:"monthly_income,omitempty"`
}

type SaveInfoResult struct {
	Status            string         `json:"status"`
	Scratchpad        map[string]any `json:"scratchpad"`
	ScratchpadVersion int            `json:"scratchpad_version"`
	CalculationResult string         `json:"calculation_result,omitempty"`
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func toFloat(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, val > 0
	case int:
		return float64(val), val > 0
	case int64:
		return float64(val), val > 0
	}
	return 0, false
}

func A9LoanCalculationStepAndChain(ctx agent.Context, pad map[string]any, currentVersion int) (string, int) {
	// Step 2: Execute Loan Calc
	vatel.Emit(ctx, "va.chain_step", attribute.String("step", "execute"))

	amt, _ := toFloat(pad["amount"])
	inc, _ := toFloat(pad["monthly_income"])
	tenFloat, _ := toFloat(pad["tenure_months"])
	ten := int(tenFloat)

	var resultStr string
	if !killswitch.Disabled("A9") {
		if ten > 0 && inc < amt/float64(ten) {
			resultStr = "Declined: Income too low"
		} else {
			monthly := 0.0
			if ten > 0 {
				monthly = (amt * (1.0 + 0.035*(float64(ten)/12.0))) / float64(ten)
			}
			resultStr = fmt.Sprintf("Approved: 3.5%% interest (ค่างวดประมาณ %.2f บาท/เดือน)", monthly)
		}
	} else {
		resultStr = "A9DisabledCalc"
	}

	// Step 3: Write result back to scratchpad & state
	newVersion := currentVersion + 1
	updatedPad := cloneMap(pad)
	updatedPad["calculation_result"] = resultStr
	ctx.State().Set("loan.scratchpad", updatedPad)
	ctx.State().Set("loan.result", resultStr)
	ctx.State().Set("loan.scratchpad.version", newVersion)

	vatel.Emit(ctx, "va.chain_step",
		attribute.String("step", "write_result"),
		attribute.Int("version", newVersion),
		attribute.String("result", resultStr),
	)

	return resultStr, newVersion
}

func saveInfoHandler(ctx agent.Context, args SaveInfoArgs) (SaveInfoResult, error) {
	pad := make(map[string]any)
	if p, err := ctx.State().Get("loan.scratchpad"); err == nil && p != nil {
		if existing, ok := p.(map[string]any); ok {
			pad = cloneMap(existing)
		}
	}

	if args.Amount > 0 {
		pad["amount"] = args.Amount
	}
	if args.Tenure > 0 {
		pad["tenure_months"] = float64(args.Tenure)
	}
	if args.Income > 0 {
		pad["monthly_income"] = args.Income
	}
	ctx.State().Set("loan.scratchpad", pad)

	var version int
	if v, err := ctx.State().Get("loan.scratchpad.version"); err == nil && v != nil {
		if iv, ok := v.(int); ok {
			version = iv
		} else if fv, ok := v.(float64); ok {
			version = int(fv)
		}
	}
	version++
	ctx.State().Set("loan.scratchpad.version", version)
	vatel.Emit(ctx, "va.chain_step", attribute.String("step", "save"), attribute.Int("version", version))

	if !killswitch.Disabled("A8") {
		_, hasAmt := toFloat(pad["amount"])
		_, hasTen := toFloat(pad["tenure_months"])
		_, hasInc := toFloat(pad["monthly_income"])
		if hasAmt && hasTen && hasInc {
			vatel.Emit(ctx, "va.completeness_trigger")
			ctx.State().Set("loan.completeness_triggered", true)
			res, finalVer := A9LoanCalculationStepAndChain(ctx, pad, version)
			return SaveInfoResult{
				Status:            "Details saved and calculated deterministically via completeness trigger. Result: " + res,
				Scratchpad:        pad,
				ScratchpadVersion: finalVer,
				CalculationResult: res,
			}, nil
		}
	}

	return SaveInfoResult{
		Status:            "Details saved to scratchpad. Waiting for remaining fields.",
		Scratchpad:        pad,
		ScratchpadVersion: version,
	}, nil
}

type ExecuteCalcArgs struct{}
type ExecuteCalcResult struct {
	Status string `json:"status"`
}

func executeCalcHandler(ctx agent.Context, args ExecuteCalcArgs) (ExecuteCalcResult, error) {
	pad := make(map[string]any)
	if p, err := ctx.State().Get("loan.scratchpad"); err == nil && p != nil {
		if existing, ok := p.(map[string]any); ok {
			pad = cloneMap(existing)
		}
	}
	version := 0
	if v, err := ctx.State().Get("loan.scratchpad.version"); err == nil && v != nil {
		if iv, ok := v.(int); ok {
			version = iv
		} else if fv, ok := v.(float64); ok {
			version = int(fv)
		}
	}
	res, _ := A9LoanCalculationStepAndChain(ctx, pad, version)
	return ExecuteCalcResult{Status: res}, nil
}

// NewTools returns the loan agent's tools. With FR-A8 active only
// SaveLoanDetails is exposed: the calculation is fired by code, never chosen
// by the model. With A8 killed, the model must decide to call ExecuteLoanCalc.
func NewTools() ([]tool.Tool, error) {
	saveInfo, err := functiontool.New(functiontool.Config{
		Name:        "SaveLoanDetails",
		Description: "Saves any newly provided loan parameters (amount, tenure_months, monthly_income) into the session scratchpad.",
	}, saveInfoHandler)
	if err != nil {
		return nil, err
	}
	tools := []tool.Tool{saveInfo}
	if killswitch.Disabled("A8") {
		executeCalc, err := functiontool.New(functiontool.Config{
			Name:        "ExecuteLoanCalc",
			Description: "Executes the loan calculation once details are complete.",
		}, executeCalcHandler)
		if err != nil {
			return nil, err
		}
		tools = append(tools, executeCalc)
	}
	return tools, nil
}

func NewAgent(mdl model.LLM) (agent.Agent, error) {
	filtered, err := NewTools()
	if err != nil {
		return nil, err
	}

	return llmagent.New(llmagent.Config{
		Name:        "loan",
		Description: "Multi-turn loan calculation agent with host-injected scratchpad and deterministic completeness trigger.",
		Instruction: Instruction,
		Model:       mdl,
		Tools:       filtered,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			loopguard.New(loopguard.MaxFromEnv()),
			InjectScratchpad,
		},
	})
}

const Instruction = `You are the Krungthai Loan Calculation Agent.
You collect 3 required fields across turns:
1. amount (วงเงินกู้ บาท)
2. tenure_months (ระยะเวลาผ่อน เดือน)
3. monthly_income (รายได้ต่อเดือน บาท)

Rules:
- On EVERY turn where the user provides any of these numbers, you MUST call SaveLoanDetails with the newly provided field(s). Do NOT re-ask for fields that are already present in your injected Scratchpad Status.
- Notice that you NEVER call a fetch tool to read the scratchpad — the host injects 'Scratchpad Status' directly into your system instructions (FR-A7).
- When all 3 fields are present, SaveLoanDetails automatically triggers the deterministic loan calculation chain (FR-A8 & FR-A9) and returns calculation_result. Report the scratchpad state, version, and the exact calculation result clearly in Thai.`

// InjectScratchpad is FR-A7: the host injects the current scratchpad into the
// loan agent's system instruction before every model call (no fetch tool).
func InjectScratchpad(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	if !killswitch.Disabled("A7") {
		// FR-A7 Inject shared state dynamically into agent context without a fetch tool call
		stateBytes := "\n[FR-A7 Host-Injected Scratchpad Status]: [Empty]"
		if pad, err := ctx.State().Get("loan.scratchpad"); err == nil && pad != nil {
			ver, _ := ctx.State().Get("loan.scratchpad.version")
			stateBytes = fmt.Sprintf("\n[FR-A7 Host-Injected Scratchpad Status (version=%v)]: %+v", ver, pad)
		}

		if req.Config == nil {
			req.Config = &genai.GenerateContentConfig{}
		}
		if req.Config.SystemInstruction != nil && len(req.Config.SystemInstruction.Parts) > 0 {
			req.Config.SystemInstruction.Parts = append(req.Config.SystemInstruction.Parts, &genai.Part{Text: stateBytes})
		} else {
			req.Config.SystemInstruction = &genai.Content{
				Role:  "system",
				Parts: []*genai.Part{{Text: stateBytes}},
			}
		}
	}
	return nil, nil
}

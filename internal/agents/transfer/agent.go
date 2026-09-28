package transfer

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
	vatel "va_adk_pilot/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"va_adk_pilot/internal/killswitch"
	"va_adk_pilot/internal/privacy"
)

// State keys.
const (
	StatePending   = "transfer.pending"
	StateStatus    = "transfer.status"
	StateCount     = "transfer.execution.count"
	StateLast      = "transfer.last_executed"
	StateTamper    = "transfer.tamper_detail"
	StateExecNonce = "transfer.executed_nonces"
)

var (
	keyOnce sync.Once
	hmacKey []byte
)

// signingKey returns the HMAC key from VA_HITL_HMAC_KEY. If unset, a random
// per-process key is generated (signatures then do not survive a restart).
func signingKey() []byte {
	keyOnce.Do(func() {
		if k := os.Getenv("VA_HITL_HMAC_KEY"); k != "" {
			hmacKey = []byte(k)
			return
		}
		hmacKey = make([]byte, 32)
		if _, err := rand.Read(hmacKey); err != nil {
			panic(err)
		}
		log.Printf("[transfer] VA_HITL_HMAC_KEY not set; using an ephemeral random HMAC key for this process")
	})
	return hmacKey
}

// Payload is the exact object the human approves. Signature covers every
// other field via HMAC-SHA256 over its canonical JSON encoding.
type Payload struct {
	Amount    string `json:"amount"` // fixed 2dp string so float formatting can't drift
	Account   string `json:"account"`
	Currency  string `json:"currency"`
	Nonce     string `json:"nonce"`
	IssuedAt  string `json:"issued_at"`
	Signature string `json:"signature,omitempty"`
}

func (p Payload) canonical() []byte {
	c := p
	c.Signature = ""
	b, _ := json.Marshal(c) // struct field order is fixed => canonical
	return b
}

// Sign computes the HMAC-SHA256 signature of the payload.
func Sign(p Payload) string {
	m := hmac.New(sha256.New, signingKey())
	m.Write(p.canonical())
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks the payload's signature in constant time.
func Verify(p Payload) bool {
	want, err := hex.DecodeString(Sign(p))
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(p.Signature)
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}

func fmtAmount(a float64) string { return strconv.FormatFloat(a, 'f', 2, 64) }

func newNonce() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func payloadFrom(v any) (Payload, error) {
	var p Payload
	b, err := json.Marshal(v)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(b, &p)
	return p, err
}

func (p Payload) asMap() map[string]any {
	b, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func getExecCount(ctx agent.Context) int {
	if v, err := ctx.State().Get(StateCount); err == nil && v != nil {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
	}
	return 0
}

func executedNonces(ctx agent.Context) map[string]any {
	out := map[string]any{}
	if v, err := ctx.State().Get(StateExecNonce); err == nil && v != nil {
		if m, ok := v.(map[string]any); ok {
			for k, val := range m {
				out[k] = val
			}
		}
	}
	return out
}

type ExecuteTransferArgs struct {
	Amount  float64 `json:"amount"`
	Account string  `json:"account"`
}

type ExecuteTransferResult struct {
	Status         string         `json:"status"`
	Payload        map[string]any `json:"payload,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	ExecutionCount int            `json:"execution_count"`
}

// Ledger is the stub core-banking executor (the PRD allows a stubbed
// transfer execution). It only counts executions.
type Ledger interface {
	Execute(p Payload) error
}

type countingLedger struct{}

func (countingLedger) Execute(Payload) error { return nil }

var ledger Ledger = countingLedger{}

func tamper(ctx agent.Context, reason string, signed Payload, attempted map[string]any) ExecuteTransferResult {
	count := getExecCount(ctx)
	ctx.State().Set(StateStatus, "TAMPER_REJECTED")
	ctx.State().Set(StateTamper, map[string]any{"reason": reason, "signed": signed.asMap(), "attempted": attempted})
	vatel.Emit(ctx, "va.tamper_rejected",
		attribute.String("reason", reason),
		attribute.String("signed.amount", signed.Amount),
		attribute.String("signed.account", signed.Account),
	)
	return ExecuteTransferResult{Status: "TAMPER_REJECTED", Reason: reason, Payload: signed.asMap(), ExecutionCount: count}
}

func executeTransferHandler(ctx agent.Context, args ExecuteTransferArgs) (ExecuteTransferResult, error) {
	count := getExecCount(ctx)
	conf := ctx.ToolConfirmation()
	// Business attributes on the tool span; FR-D2 redacts the account on export.
	vatel.SetAttrs(ctx, "va.attrs",
		attribute.String("va.transfer.amount", fmtAmount(args.Amount)),
		attribute.String("va.transfer.account", args.Account),
		attribute.String("va.transfer.request", fmt.Sprintf("โอนเงิน %s บาท เลขบัญชี %s", fmtAmount(args.Amount), args.Account)),
	)

	// (a) First call: sign and halt at the human gate. Nothing executes.
	if conf == nil {
		p := Payload{
			Amount:   fmtAmount(args.Amount),
			Account:  args.Account,
			Currency: "THB",
			Nonce:    newNonce(),
			IssuedAt: time.Now().UTC().Format(time.RFC3339),
		}
		p.Signature = Sign(p)
		ctx.State().Set(StatePending, p.asMap())
		ctx.State().Set(StateStatus, "AWAITING_HUMAN_APPROVAL")
		ctx.State().Set(StateCount, count)
		if err := ctx.RequestConfirmation(
			fmt.Sprintf("Approve transfer of %s THB to account %s? The payload below is HMAC-signed; any edit invalidates it.", p.Amount, p.Account),
			p.asMap(),
		); err != nil {
			return ExecuteTransferResult{}, err
		}
		vatel.Emit(ctx, "va.hitl_confirmation_requested",
			attribute.String("nonce", p.Nonce))
		return ExecuteTransferResult{Status: "AWAITING_HUMAN_APPROVAL", Payload: p.asMap(), ExecutionCount: count}, nil
	}

	if !conf.Confirmed {
		ctx.State().Set(StateStatus, "REJECTED_BY_USER")
		ctx.State().Set(StatePending, nil)
		vatel.Emit(ctx, "va.hitl_rejected")
		return ExecuteTransferResult{Status: "REJECTED_BY_USER", ExecutionCount: count}, nil
	}

	approved, err := payloadFrom(conf.Payload)
	attempted := map[string]any{"amount": fmtAmount(args.Amount), "account": args.Account}
	if !killswitch.Disabled("F1") {
		if err != nil || approved.Signature == "" {
			// ADK Web's confirmation card is read-only and echoes the original
			// call args ({account, amount}) instead of the signed payload. Bind
			// the approval to the server-held signed payload, but only if every
			// value the human was shown equals what was signed.
			pendingAny, _ := ctx.State().Get(StatePending)
			pending, perr := payloadFrom(pendingAny)
			if perr != nil || pending.Signature == "" {
				return tamper(ctx, "approval carried no signed payload and none is pending", approved, attempted), nil
			}
			if reason := shownMismatch(ctx, conf.Payload, pending); reason != "" {
				return tamper(ctx, reason, pending, attemptedFrom(conf.Payload, attempted)), nil
			}
			approved = pending
		}
		// (c1) The approved payload must be exactly what was signed.
		if !Verify(approved) {
			return tamper(ctx, "approved payload does not match its HMAC signature (edited after signing)", approved, attempted), nil
		}
		// (c2) What is about to execute must equal what was approved.
		if approved.Amount != fmtAmount(args.Amount) || approved.Account != args.Account {
			return tamper(ctx, "execution arguments differ from the signed, approved payload", approved, attempted), nil
		}
		// (c3) It must be the payload this session is waiting on, used once.
		pendingAny, _ := ctx.State().Get(StatePending)
		pending, _ := payloadFrom(pendingAny)
		if pending.Nonce != approved.Nonce {
			return tamper(ctx, "approved payload is not the pending one (stale or foreign nonce)", approved, attempted), nil
		}
		if _, used := executedNonces(ctx)[approved.Nonce]; used {
			return tamper(ctx, "signed payload already executed (replay)", approved, attempted), nil
		}
	} else if err != nil {
		approved = Payload{Amount: fmtAmount(args.Amount), Account: args.Account}
	}

	// (b) Execute exactly the signed payload.
	if err := ledger.Execute(approved); err != nil {
		return ExecuteTransferResult{}, err
	}
	count++
	nonces := executedNonces(ctx)
	nonces[approved.Nonce] = time.Now().UTC().Format(time.RFC3339)
	ctx.State().Set(StateExecNonce, nonces)
	ctx.State().Set(StateCount, count)
	ctx.State().Set(StateStatus, "EXECUTED")
	ctx.State().Set(StateLast, approved.asMap())
	ctx.State().Set(StatePending, nil)
	vatel.Emit(ctx, "va.transfer_executed",
		attribute.String("nonce", approved.Nonce))
	return ExecuteTransferResult{Status: "EXECUTED", Payload: approved.asMap(), ExecutionCount: count}, nil
}

const Instruction = `You are the Krungthai transfer assistant.
When the user asks to transfer money, call ExecuteTransfer exactly once with the amount (number, THB) and the destination account exactly as the user wrote it (it may be a placeholder like [ACCOUNT_1] — pass it through unchanged).
The tool pauses for the customer's own approval; you never approve, sign or change a transfer yourself.
After the tool returns, tell the customer in Thai what happened, using the returned status:
- AWAITING_HUMAN_APPROVAL: ask them to review and approve the signed payload.
- EXECUTED: confirm the amount and account that were executed.
- REJECTED_BY_USER: confirm nothing was transferred.
- TAMPER_REJECTED: say the transfer was blocked because the payload changed after signing, and quote the reason.`

// NewTools returns the transfer agent's tools (exported for stage evals).
func NewTools() ([]tool.Tool, error) {
	t, err := functiontool.New(functiontool.Config{
		Name:        "ExecuteTransfer",
		Description: "Transfers money. Prepares an HMAC-signed payload and requires the customer's explicit approval before anything executes.",
	}, executeTransferHandler)
	if err != nil {
		return nil, err
	}
	return []tool.Tool{t}, nil
}

func NewAgent(mdl model.LLM, extra ...llmagent.BeforeModelCallback) (agent.Agent, error) {
	tools, err := NewTools()
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:                 "transfer",
		Description:          "Money transfers with a human-in-the-loop signed approval gate.",
		Instruction:          Instruction,
		Model:                mdl,
		Tools:                tools,
		BeforeModelCallbacks: extra,
		AfterToolCallbacks:   []llmagent.AfterToolCallback{RecordRejection},
	})
}

// RecordRejection records a human rejection. ADK's functiontool returns
// tool.ErrConfirmationRejected before the handler runs, so the handler's own
// !Confirmed branch is only a fallback.
func RecordRejection(ctx agent.Context, t tool.Tool, _, result map[string]any, err error) (map[string]any, error) {
	if t.Name() != "ExecuteTransfer" || err == nil || !errors.Is(err, tool.ErrConfirmationRejected) {
		return nil, nil
	}
	ctx.State().Set(StateStatus, "REJECTED_BY_USER")
	ctx.State().Set(StatePending, nil)
	vatel.Emit(ctx, "va.hitl_rejected")
	return map[string]any{"status": "REJECTED_BY_USER", "execution_count": getExecCount(ctx)}, nil
}

// shownAmount normalises an amount the UI echoed back (number or string).
func shownAmount(v any) (string, bool) {
	switch a := v.(type) {
	case float64:
		return fmtAmount(a), true
	case int:
		return fmtAmount(float64(a)), true
	case string:
		f, err := strconv.ParseFloat(a, 64)
		if err != nil {
			return a, true
		}
		return fmtAmount(f), true
	}
	return "", false
}

// shownMismatch returns a tamper reason if any amount/account in the approval
// response differs from the signed pending payload. Placeholders such as
// [ACCOUNT_1] are resolved through the session's privacy vault first.
func shownMismatch(ctx agent.Context, approval any, signed Payload) string {
	m, ok := approval.(map[string]any)
	if !ok {
		return ""
	}
	if a, ok := shownAmount(m["amount"]); ok && a != signed.Amount {
		return fmt.Sprintf("approved amount %s differs from signed amount %s", a, signed.Amount)
	}
	if acc, ok := m["account"].(string); ok {
		vaultAny, _ := ctx.State().Get(privacy.StateVault)
		acc, _ = privacy.Restore(acc, privacy.VaultFromState(vaultAny))
		if acc != signed.Account {
			return "approved account differs from signed account"
		}
	}
	return ""
}

func attemptedFrom(approval any, fallback map[string]any) map[string]any {
	if m, ok := approval.(map[string]any); ok && len(m) > 0 {
		return m
	}
	return fallback
}

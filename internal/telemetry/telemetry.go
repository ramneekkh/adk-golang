// Package telemetry holds the OpenTelemetry span pipeline we own:
//
//   - FR-D1 attribute-level emit/omit policy (VA_TRACE_OMIT_ATTRS)
//   - FR-D2 pluggable redaction of attribute values AND span-event attributes
//   - FR-D3 correlation: request_id / session_id / user_id / trace_id + custom
//     metadata stamped on every span of a request
package telemetry

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/adk/v2/agent"

	"va_adk_pilot/internal/killswitch"
)

// ---- FR-D1: emit/omit policy ----------------------------------------------

// OmitKeys returns attribute keys dropped before export. Default omits output
// embedding vectors while keeping the embedding input text.
func OmitKeys() map[string]bool {
	keys := "gen_ai.response.embeddings,vector.payload"
	if v, ok := os.LookupEnv("VA_TRACE_OMIT_ATTRS"); ok {
		keys = v
	}
	out := map[string]bool{}
	for _, k := range strings.Split(keys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out[k] = true
		}
	}
	return out
}

// ---- FR-D2: redaction rules (code we own) ---------------------------------

// Rule rewrites a string value.
type Rule struct {
	Name string
	Re   *regexp.Regexp
	Repl func(match string) string
}

// DefaultRules redacts emails, Thai mobile numbers, and any run of 9+ digits
// (account / citizen IDs) as [NUMERIC_<n>_DIGITS], per the PRD example
// "โอนเงิน 023536333" -> "โอนเงิน [NUMERIC_9_DIGITS]".
var DefaultRules = []Rule{
	{Name: "email", Re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), Repl: func(string) string { return "[EMAIL]" }},
	{Name: "phone", Re: regexp.MustCompile(`\b0[689]\d-\d{3}-\d{4}\b`), Repl: func(string) string { return "[PHONE]" }},
	{Name: "digits", Re: regexp.MustCompile(`\b\d(?:-?\d){8,}\b`), Repl: func(m string) string {
		n := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				n++
			}
		}
		return fmt.Sprintf("[NUMERIC_%d_DIGITS]", n)
	}},
}

// Redact applies rules to s.
func Redact(s string, rules []Rule) string {
	for _, r := range rules {
		s = r.Re.ReplaceAllStringFunc(s, r.Repl)
	}
	return s
}

func redactValue(v attribute.Value, rules []Rule) attribute.Value {
	switch v.Type() {
	case attribute.STRING:
		return attribute.StringValue(Redact(v.AsString(), rules))
	case attribute.STRINGSLICE:
		in := v.AsStringSlice()
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = Redact(s, rules)
		}
		return attribute.StringSliceValue(out)
	}
	return v
}

// idKeys are opaque correlation identifiers; redacting digit runs inside a
// hex trace ID would break FR-D3 journey queries.
var idKeys = map[string]bool{"va.trace_id": true, "va.request_id": true, "va.session_id": true,
	"gcp.vertex.agent.invocation_id": true, "gcp.vertex.agent.event_id": true, "gcp.vertex.agent.session_id": true}

func (p *Processor) filter(attrs []attribute.KeyValue) []attribute.KeyValue {
	omit := map[string]bool{}
	if !killswitch.Disabled("D1") {
		omit = OmitKeys()
	}
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if omit[string(a.Key)] {
			continue
		}
		if !killswitch.Disabled("D2") && !idKeys[string(a.Key)] {
			a = attribute.KeyValue{Key: a.Key, Value: redactValue(a.Value, p.rules)}
		}
		out = append(out, a)
	}
	return out
}

type redactedSpan struct {
	trace.ReadOnlySpan
	p     *Processor
	extra []attribute.KeyValue // correlation learned after this span started
}

func (r *redactedSpan) Attributes() []attribute.KeyValue {
	attrs := r.ReadOnlySpan.Attributes()
	if len(r.extra) > 0 {
		have := map[attribute.Key]bool{}
		for _, a := range attrs {
			have[a.Key] = true
		}
		for _, a := range r.extra {
			if !have[a.Key] {
				attrs = append(attrs, a)
			}
		}
	}
	return r.p.filter(attrs)
}

func (r *redactedSpan) Events() []trace.Event {
	evs := r.ReadOnlySpan.Events()
	out := make([]trace.Event, len(evs))
	for i, e := range evs {
		e.Attributes = r.p.filter(e.Attributes)
		out[i] = e
	}
	return out
}

func (r *redactedSpan) Name() string {
	if killswitch.Disabled("D2") {
		return r.ReadOnlySpan.Name()
	}
	return Redact(r.ReadOnlySpan.Name(), r.p.rules)
}

// ---- Processor --------------------------------------------------------------

// Processor applies D3 on start and D1/D2 on end, then forwards to next
// (an exporter processor such as a file or OTLP exporter).
type Processor struct {
	next  trace.SpanProcessor
	rules []Rule

	mu   sync.Mutex
	corr map[string][]attribute.KeyValue // trace_id -> correlation attrs
}

func NewProcessor(next trace.SpanProcessor) *Processor {
	return &Processor{next: next, rules: DefaultRules, corr: map[string][]attribute.KeyValue{}}
}

// WithRules replaces the redaction rules (pluggable).
func (p *Processor) WithRules(r []Rule) *Processor { p.rules = r; return p }

func (p *Processor) OnStart(parent context.Context, s trace.ReadWriteSpan) {
	if !killswitch.Disabled("D3") {
		tid := s.SpanContext().TraceID().String()
		var attrs []attribute.KeyValue
		if ic, ok := parent.(agent.InvocationContext); ok && ic.Session() != nil {
			attrs = []attribute.KeyValue{
				attribute.String("va.request_id", ic.InvocationID()),
				attribute.String("va.session_id", ic.Session().ID()),
				attribute.String("va.user_id", ic.Session().UserID()),
				attribute.String("va.app", ic.Session().AppName()),
				attribute.String("va.channel", channel()),
			}
			p.mu.Lock()
			p.corr[tid] = attrs
			p.mu.Unlock()
		} else {
			p.mu.Lock()
			attrs = p.corr[tid]
			p.mu.Unlock()
		}
		s.SetAttributes(attribute.String("va.trace_id", tid))
		s.SetAttributes(attrs...)
	}
	if p.next != nil {
		p.next.OnStart(parent, s)
	}
}

func channel() string {
	if c := os.Getenv("VA_CHANNEL"); c != "" {
		return c
	}
	return "adk-web-pilot"
}

func (p *Processor) OnEnd(s trace.ReadOnlySpan) {
	var extra []attribute.KeyValue
	if !killswitch.Disabled("D3") {
		p.mu.Lock()
		extra = p.corr[s.SpanContext().TraceID().String()]
		p.mu.Unlock()
	}
	if !s.Parent().IsValid() {
		// Root span finished: correlation entry no longer needed.
		defer func() {
			p.mu.Lock()
			delete(p.corr, s.SpanContext().TraceID().String())
			p.mu.Unlock()
		}()
	}
	if p.next != nil {
		p.next.OnEnd(&redactedSpan{ReadOnlySpan: s, p: p, extra: extra})
	}
}

func (p *Processor) Shutdown(ctx context.Context) error {
	if p.next != nil {
		return p.next.Shutdown(ctx)
	}
	return nil
}

func (p *Processor) ForceFlush(ctx context.Context) error {
	if p.next != nil {
		return p.next.ForceFlush(ctx)
	}
	return nil
}

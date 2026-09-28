package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/otel/sdk/trace"
)

// SpanRecord is the exported JSON form of one span.
type SpanRecord struct {
	TraceID    string           `json:"trace_id"`
	SpanID     string           `json:"span_id"`
	ParentID   string           `json:"parent_id,omitempty"`
	Name       string           `json:"name"`
	Start      time.Time        `json:"start"`
	DurationMs float64          `json:"duration_ms"`
	Attributes map[string]any   `json:"attributes"`
	Events     []map[string]any `json:"events,omitempty"`
}

// ToRecord converts a (redacted) span to a record.
func ToRecord(s trace.ReadOnlySpan) SpanRecord {
	r := SpanRecord{
		TraceID:    s.SpanContext().TraceID().String(),
		SpanID:     s.SpanContext().SpanID().String(),
		Name:       s.Name(),
		Start:      s.StartTime(),
		DurationMs: float64(s.EndTime().Sub(s.StartTime()).Microseconds()) / 1000,
		Attributes: map[string]any{},
	}
	if s.Parent().IsValid() {
		r.ParentID = s.Parent().SpanID().String()
	}
	for _, a := range s.Attributes() {
		r.Attributes[string(a.Key)] = a.Value.AsInterface()
	}
	for _, e := range s.Events() {
		ev := map[string]any{"name": e.Name}
		for _, a := range e.Attributes {
			ev[string(a.Key)] = a.Value.AsInterface()
		}
		r.Events = append(r.Events, ev)
	}
	return r
}

// FileExporter appends every finished span as one JSON line (local trace
// backend: `jq 'select(.attributes["va.session_id"]=="...")' traces.jsonl`).
type FileExporter struct {
	mu   sync.Mutex
	path string
}

// NewFileExporter writes to VA_TRACE_FILE (default .out/traces.jsonl).
func NewFileExporter() *FileExporter {
	p := os.Getenv("VA_TRACE_FILE")
	if p == "" {
		p = ".out/traces.jsonl"
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return &FileExporter{path: p}
}

func (f *FileExporter) Path() string { return f.path }

func (f *FileExporter) OnStart(context.Context, trace.ReadWriteSpan) {}

func (f *FileExporter) OnEnd(s trace.ReadOnlySpan) {
	b, err := json.Marshal(ToRecord(s))
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.Write(append(b, '\n'))
}

func (f *FileExporter) Shutdown(context.Context) error   { return nil }
func (f *FileExporter) ForceFlush(context.Context) error { return nil }

// Recorder keeps redacted spans in memory (tests).
type Recorder struct {
	mu    sync.Mutex
	Spans []SpanRecord
}

func (r *Recorder) OnStart(context.Context, trace.ReadWriteSpan) {}
func (r *Recorder) OnEnd(s trace.ReadOnlySpan) {
	rec := ToRecord(s)
	r.mu.Lock()
	r.Spans = append(r.Spans, rec)
	r.mu.Unlock()
}
func (r *Recorder) Shutdown(context.Context) error   { return nil }
func (r *Recorder) ForceFlush(context.Context) error { return nil }

// Snapshot returns a copy of recorded spans.
func (r *Recorder) Snapshot() []SpanRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SpanRecord(nil), r.Spans...)
}

// Reset clears recorded spans.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.Spans = nil
	r.mu.Unlock()
}

package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Emit adds a span event to the active span; if the context's span is not
// recording (ADK may hand tools a context whose span has already ended), a
// short child span named after the event carries it instead, in the same trace.
func Emit(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	if sp := trace.SpanFromContext(ctx); sp.IsRecording() {
		sp.AddEvent(name, trace.WithAttributes(attrs...))
		return
	}
	_, sp := otel.Tracer("va_adk_pilot").Start(ctx, name)
	sp.AddEvent(name, trace.WithAttributes(attrs...))
	sp.End()
}

// SetAttrs sets attributes on the active span, or on a child span named name
// if the active span is not recording.
func SetAttrs(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	if sp := trace.SpanFromContext(ctx); sp.IsRecording() {
		sp.SetAttributes(attrs...)
		return
	}
	_, sp := otel.Tracer("va_adk_pilot").Start(ctx, name)
	sp.SetAttributes(attrs...)
	sp.End()
}

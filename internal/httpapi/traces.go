package httpapi

import (
	"net/http"

	"github.com/KleitonBarone/webhook-redrive/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Trace only writes that create work. Polling history and scraping metrics do
// not drown out the delivery trace. No raw HTTP headers, URLs, or errors enter spans.
func (a *API) traced(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := a.tracer.Start(telemetry.Extract(r.Context(), r.Header.Get("traceparent")), name,
			trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(a.clock.Now()))
		response := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		telemetry.Inject(ctx, response.Header())
		defer func() {
			span.SetAttributes(attribute.Int("http.response.status_code", response.status))
			if response.status >= 400 {
				span.SetStatus(codes.Error, "request rejected")
			}
			span.End(trace.WithTimestamp(a.clock.Now()))
		}()
		next(response, r.WithContext(ctx))
	}
}

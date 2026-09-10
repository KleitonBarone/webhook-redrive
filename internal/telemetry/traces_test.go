package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestTraceContextBoundaryAndExporter(t *testing.T) {
	var output bytes.Buffer
	p, err := Provider("synthetic", "stdout", &output)
	if err != nil {
		t.Fatal(err)
	}
	parent := "00-11111111111111111111111111111111-2222222222222222-01"
	ctx, span := p.Tracer("test").Start(Extract(context.Background(), parent), "safe.operation")
	header := http.Header{}
	Inject(ctx, header)
	if len(header) != 1 || len(header.Get("traceparent")) != 55 || TraceID(ctx) != strings.Repeat("1", 32) {
		t.Fatalf("headers=%v", header)
	}
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "safe.operation") || !strings.Contains(output.String(), strings.Repeat("1", 32)) {
		t.Fatal("missing exported span")
	}
	for _, invalid := range []string{"", "synthetic-secret", "00-00000000000000000000000000000000-2222222222222222-01"} {
		if trace.SpanContextFromContext(Extract(ctx, invalid)).IsValid() {
			t.Fatal("invalid parent inherited another span")
		}
	}
	if _, err := Provider("test", "invalid", &output); err == nil {
		t.Fatal("invalid exporter accepted")
	}
}

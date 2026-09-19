// Package telemetry contains the explicit, payload-free tracing boundary.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Provider uses a bounded, non-blocking batch queue. Traces are diagnostic,
// not a durable audit log: a crash or full exporter queue can lose spans.
func Provider(service, exporter string, writer io.Writer) (*sdktrace.TracerProvider, error) {
	// Exporter errors may contain collector URLs, headers, or response bodies.
	// The SDK's default error handler writes raw errors to stderr.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { fmt.Fprintln(writer, "trace export failed") }))
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}
	switch exporter {
	case "stdout":
		e, err := stdouttrace.New(stdouttrace.WithWriter(writer))
		if err != nil {
			return nil, err
		}
		options = append(options, sdktrace.WithBatcher(e, sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(256), sdktrace.WithBatchTimeout(time.Second)))
	case "none":
	case "otlp":
		endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return nil, fmt.Errorf("OTLP requires an explicit HTTP(S) traces endpoint without credentials, query, or fragment")
		}
		e, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithTimeout(3*time.Second), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
		if err != nil {
			return nil, fmt.Errorf("cannot initialize OTLP exporter")
		}
		options = append(options, sdktrace.WithBatcher(e, sdktrace.WithMaxQueueSize(2048), sdktrace.WithMaxExportBatchSize(256), sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(3*time.Second)))
	default:
		return nil, fmt.Errorf("TRACE_EXPORTER must be stdout, otlp, or none")
	}
	return sdktrace.NewTracerProvider(options...), nil
}

// Only traceparent crosses trust boundaries. Never propagate baggage or
// tracestate, which can contain arbitrary caller-controlled values.
func Extract(ctx context.Context, parent string) context.Context {
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": parent})
}

func Parent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

func Inject(ctx context.Context, header http.Header) {
	if parent := Parent(ctx); parent != "" {
		header.Set("traceparent", parent)
	}
}

func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

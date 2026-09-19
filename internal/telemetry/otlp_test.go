package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTLPExportsToExistingCollector(t *testing.T) {
	received := make(chan *collector.ExportTraceServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("wrong OTLP request")
		}
		data, _ := io.ReadAll(r.Body)
		message := &collector.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(data, message); err != nil {
			t.Error(err)
		}
		received <- message
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/v1/traces")
	provider, err := Provider("synthetic-service", "otlp", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, span := provider.Tracer("test").Start(context.Background(), "synthetic-delivery")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = provider.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		if len(message.ResourceSpans) != 1 || message.ResourceSpans[0].ScopeSpans[0].Spans[0].Name != "synthetic-delivery" {
			t.Fatal("missing span")
		}
	default:
		t.Fatal("no export")
	}
}

func TestOTLPFailureDoesNotExposeCollectorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "synthetic-secret-payload", 400) }))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/v1/traces")
	var output bytes.Buffer
	provider, err := Provider("test", "otlp", &output)
	if err != nil {
		t.Fatal(err)
	}
	_, span := provider.Tracer("test").Start(context.Background(), "safe")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = provider.Shutdown(ctx)
	if strings.Contains(output.String(), "synthetic-secret") || strings.Contains(output.String(), server.URL) {
		t.Fatal("raw exporter error leaked")
	}
}

func TestOTLPSlowCollectorDoesNotBlockSpanProducers(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/v1/traces")
	provider, err := Provider("test", "otlp", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() { close(release); _ = provider.Shutdown(ctx) }()
	tracer := provider.Tracer("test")
	for i := 0; i < 256; i++ {
		_, span := tracer.Start(ctx, "safe")
		span.End()
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("collector was not contacted")
	}
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := 0; i < 5000; i++ {
			_, span := tracer.Start(ctx, "safe")
			span.End()
		}
	}()
	select {
	case <-produced:
	case <-ctx.Done():
		t.Fatal("span queue blocked producer")
	}
	// Canceled flush is bounded even while the collector has not responded.
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err = provider.ForceFlush(canceled); err == nil {
		t.Fatal("canceled flush succeeded")
	}
}

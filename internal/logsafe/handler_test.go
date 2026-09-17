package logsafe

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerRedactsPayloadsAndCredentials(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&output, nil))).With(
		"endpoint_secret", "do-not-log-secret",
	)
	logger.Info("attempt",
		"idempotency_key", "do-not-log-idempotency",
		"api_token", "do-not-log-token",
		"payload", `{"private":"do-not-log-payload"}`,
		"request", slog.GroupValue(
			slog.String("authorization", "do-not-log-auth"),
			slog.String("event_id", "event-123"),
		),
	)

	logged := output.String()
	for _, sensitive := range []string{"do-not-log-secret", "do-not-log-payload", "do-not-log-auth", "do-not-log-token", "do-not-log-idempotency"} {
		if strings.Contains(logged, sensitive) {
			t.Fatalf("log contains sensitive value %q: %s", sensitive, logged)
		}
	}
	if !strings.Contains(logged, "event-123") || strings.Count(logged, redacted) != 5 {
		t.Fatalf("unexpected sanitized log: %s", logged)
	}
}

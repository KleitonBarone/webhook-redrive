package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/delivery"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/signature"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

// lockedBuffer allows API and worker logs to share a race-safe capture.
type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.data.String() }

func TestHTTPRetryDeadLetterReplayAndRedaction(t *testing.T) {
	s, err := store.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &clock{now: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	if err := s.Migrate(context.Background(), c.Now()); err != nil {
		t.Fatal(err)
	}
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	logger := slog.New(logsafe.New(slog.NewJSONHandler(&logs, nil)))
	api := httptest.NewServer(httpapi.New(s, box, c, logger))
	defer api.Close()
	const endpointSecret = "synthetic-secret-never-in-logs"
	payload := []byte("{\n \"private\":\"payload-never-in-logs\"\n}\n")
	var mu sync.Mutex
	counts := map[string]int{}
	receivedIDs := map[string][]string{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(body, payload) {
			t.Error("payload bytes changed")
		}
		timestamp, err := strconv.ParseInt(r.Header.Get("X-Webhook-Timestamp"), 10, 64)
		if err != nil {
			t.Error(err)
		}
		if err := signature.Verify([]byte(endpointSecret), r.Header.Get("X-Webhook-Timestamp"), body, r.Header.Get("X-Webhook-Signature"), time.Unix(timestamp, 0), time.Minute); err != nil {
			t.Error(err)
		}
		mu.Lock()
		counts[r.URL.Path]++
		count := counts[r.URL.Path]
		receivedIDs[r.URL.Path] = append(receivedIDs[r.URL.Path], r.Header.Get("X-Webhook-ID"))
		mu.Unlock()
		if r.URL.Path == "/reject" {
			w.WriteHeader(400)
			return
		}
		if count <= 2 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	worker, err := delivery.NewWorker(s, box, c, logger, delivery.Config{
		WorkerID: "integration", Lease: 10 * time.Second, RequestTimeout: time.Second,
		BatchSize: 10, PollPeriod: time.Millisecond, Jitter: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		path  string
		max   int
		state string
	}{
		{"/eventual", 3, "succeeded"}, {"/exhaust", 2, "dead_letter"}, {"/reject", 3, "failed"},
	} {
		var endpoint store.Endpoint
		call(t, api.URL+"/v1/endpoints", http.MethodPost, map[string]any{
			"url": receiver.URL + scenario.path, "secret": endpointSecret, "max_attempts": scenario.max,
			"concurrency_limit": 1, "rate_limit": 2,
		}, http.StatusCreated, &endpoint)
		req, err := http.NewRequest(http.MethodPost, api.URL+"/v1/endpoints/"+endpoint.ID+"/events", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Event-Type", "test.event")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var event store.Event
		if err := json.NewDecoder(res.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 202 {
			t.Fatalf("ingestion status %d", res.StatusCode)
		}
		for n := 0; n < scenario.max; n++ {
			if _, err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			var history struct {
				Attempts []store.Attempt `json:"attempts"`
			}
			call(t, api.URL+"/v1/events/"+event.ID+"/attempts", http.MethodGet, nil, 200, &history)
			last := history.Attempts[len(history.Attempts)-1]
			if last.State != "pending" {
				break
			}
			if !last.AvailableAt.Equal(c.now.Add(3 * time.Second)) {
				t.Fatalf("Retry-After ignored: %s", last.AvailableAt)
			}
			c.now = last.AvailableAt.Add(-time.Microsecond)
			if count, err := worker.RunOnce(context.Background()); err != nil || count != 0 {
				t.Fatalf("early retry: %d %v", count, err)
			}
			c.now = last.AvailableAt
		}
		call(t, api.URL+"/v1/events/"+event.ID, http.MethodGet, nil, 200, &event)
		if event.State != scenario.state {
			t.Fatalf("%s state %s want %s", scenario.path, event.State, scenario.state)
		}
		if scenario.state == "dead_letter" {
			history, err := s.ListAttempts(context.Background(), event.ID)
			if err != nil {
				t.Fatal(err)
			}
			requestID, err := id.New()
			if err != nil {
				t.Fatal(err)
			}
			replay := store.ReplayRequest{AttemptID: history[len(history)-1].ID, RequestID: requestID, Actor: "demo-operator", Reason: "receiver repaired"}
			var result, duplicate store.ReplayResult
			call(t, api.URL+"/v1/events/"+event.ID+"/replays", http.MethodPost, replay, 202, &result)
			call(t, api.URL+"/v1/events/"+event.ID+"/replays", http.MethodPost, replay, 202, &duplicate)
			if result != duplicate {
				t.Fatal("repeated API replay created another attempt")
			}
			c.now = c.now.Add(time.Second)
			if _, err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			call(t, api.URL+"/v1/events/"+event.ID, http.MethodGet, nil, 200, &event)
			if event.State != "succeeded" {
				t.Fatalf("replay state: %s", event.State)
			}
		}
		mu.Lock()
		for _, receivedID := range receivedIDs[scenario.path] {
			if receivedID != event.ID {
				t.Error("retry or replay changed event ID")
			}
		}
		mu.Unlock()
	}
	for _, sensitive := range []string{endpointSecret, "payload-never-in-logs", "receiver repaired"} {
		if strings.Contains(logs.String(), sensitive) {
			t.Fatalf("logs exposed %q", sensitive)
		}
	}
}

func call(t *testing.T, url, method string, input any, status int, output any) {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s returned %d: %s", method, url, response.StatusCode, content)
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
}

type lostCompletion struct {
	*store.Store
	lose bool
}

func (s *lostCompletion) Complete(ctx context.Context, c store.ClaimedDelivery, worker string, now time.Time, outcome store.Outcome) (bool, error) {
	if s.lose {
		s.lose = false
		return false, errors.New("synthetic crash before completion commit")
	}
	return s.Store.Complete(ctx, c, worker, now, outcome)
}

func TestCrashAfterReceiverAcceptanceRedeliversSameEvent(t *testing.T) {
	s, err := store.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &clock{now: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	if err := s.Migrate(context.Background(), c.Now()); err != nil {
		t.Fatal(err)
	}
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Encrypt([]byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := id.New()
	endpointID, _ := id.New()
	attemptID, _ := id.New()
	var mu sync.Mutex
	count := 0
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		count++
		if r.Header.Get("X-Webhook-ID") != eventID {
			t.Error("duplicate changed event ID")
		}
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	if err := s.CreateEndpoint(context.Background(), store.Endpoint{ID: endpointID, URL: receiver.URL, CreatedAt: c.now}, encrypted); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateEvent(context.Background(), store.Event{ID: eventID, EndpointID: endpointID, EventType: "test", CreatedAt: c.now}, []byte("{}"), attemptID); err != nil {
		t.Fatal(err)
	}
	crash := &lostCompletion{Store: s, lose: true}
	worker, err := delivery.NewWorker(crash, box, c, slog.New(slog.NewTextHandler(io.Discard, nil)), delivery.Config{
		WorkerID: "same-worker-id", Lease: 10 * time.Second, RequestTimeout: time.Second, BatchSize: 1, PollPeriod: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("expected lost completion")
	}
	c.now = c.now.Add(9 * time.Second)
	if n, err := worker.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("claim before expiry: %d %v", n, err)
	}
	c.now = c.now.Add(time.Second)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListAttempts(context.Background(), eventID)
	if err != nil || len(history) != 1 || history[0].State != "succeeded" || history[0].ClaimCount != 2 {
		t.Fatalf("history: %v %v", history, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Fatalf("received %d deliveries, expected duplicate", count)
	}
}

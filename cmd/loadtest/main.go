// loadtest runs finite workloads against the local synthetic stack.
// It is a development tool, not an additional service.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type config struct {
	API, Receiver, History, Scenario, Output, Revision string
	Events, Concurrency                                int
	Deadline                                           time.Duration
	Rate, SlowEndpoints, SlowConcurrency               int
	SampleInterval                                     time.Duration
}

type percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}
type report struct {
	StartedAt           time.Time              `json:"started_at"`
	Revision            string                 `json:"revision"`
	Runtime             string                 `json:"runtime"`
	Scenario            string                 `json:"scenario"`
	Events              int                    `json:"events"`
	Concurrency         int                    `json:"ingestion_concurrency"`
	PayloadBytes        int                    `json:"payload_bytes"`
	IngestionSeconds    float64                `json:"ingestion_seconds"`
	DrainSeconds        float64                `json:"drain_seconds"`
	TotalSeconds        float64                `json:"total_seconds"`
	EventsPerSecond     float64                `json:"completed_events_per_second"`
	IngestionMS         percentiles            `json:"ingestion_ack_ms"`
	EndToEndMS          percentiles            `json:"event_completion_ms"`
	CompletionByKind    map[string]percentiles `json:"completion_ms_by_receiver"`
	States              map[string]int         `json:"states"`
	Attempts            int                    `json:"attempts"`
	VerifiedDeliveries  int                    `json:"verified_signed_unchanged_deliveries"`
	TraceSamples        map[string]string      `json:"trace_samples"`
	QueueAfterIngestion float64                `json:"unfinished_after_ingestion"`
	MaxScrapeMS         float64                `json:"max_of_three_scrape_ms"`
	CountersDelta       map[string]float64     `json:"committed_counter_deltas"`
	Rate                int                    `json:"target_ingestion_per_second"`
	MaxPacingLagMS      float64                `json:"max_pacing_lag_ms"`
	Samples             []queueSample          `json:"queue_samples,omitempty"`
	Timing              timingCheck            `json:"timing_check"`
	SlowEndpoints       int                    `json:"slow_endpoints,omitempty"`
	SlowConcurrency     int                    `json:"slow_endpoint_concurrency,omitempty"`
	PrimedInProgress    float64                `json:"primed_in_progress,omitempty"`
}

type eventResult struct {
	event     store.Event
	kind      string
	ingestion time.Duration
	started   time.Time
}

func main() {
	c := config{}
	flag.StringVar(&c.API, "api", "http://localhost:8080", "local API URL")
	flag.StringVar(&c.Receiver, "receiver", "http://receiver:9090", "synthetic receiver URL as seen by the worker")
	flag.StringVar(&c.History, "history", "http://localhost:9090", "local receiver history URL")
	flag.StringVar(&c.Scenario, "scenario", "success", "success, retry, mixed, fairness, or saturation")
	flag.StringVar(&c.Output, "output", "", "optional JSON report path")
	flag.StringVar(&c.Revision, "revision", "working-tree", "source revision label recorded in the report")
	flag.IntVar(&c.Events, "events", 200, "finite number of events; mixed needs multiples of 20, fairness of 10")
	flag.IntVar(&c.Concurrency, "concurrency", 10, "concurrent ingestion clients")
	flag.DurationVar(&c.Deadline, "deadline", 2*time.Minute, "whole workload deadline")
	flag.IntVar(&c.Rate, "rate", 0, "paced ingestion starts per second, 0 for an unpaced burst")
	flag.IntVar(&c.SlowEndpoints, "slow-endpoints", 1, "timeout endpoints in saturation, 1..10")
	flag.IntVar(&c.SlowConcurrency, "slow-concurrency", 10, "per-timeout-endpoint limit in saturation, 1..10")
	flag.DurationVar(&c.SampleInterval, "sample-interval", 0, "queue sampling interval, 0 to disable or 1s..1m")
	flag.Parse()
	if err := validate(c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	result, err := run(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	output := io.Writer(os.Stdout)
	if c.Output != "" {
		file, err := os.Create(c.Output)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer file.Close()
		output = io.MultiWriter(os.Stdout, file)
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func validate(c config) error {
	if c.Events < 1 || c.Events > 10000 || c.Concurrency < 1 || c.Concurrency > 64 || c.Deadline <= 0 || c.Deadline > 10*time.Minute {
		return errors.New("events must be 1..10000, concurrency 1..64, deadline 0..10m")
	}
	if c.Rate < 0 || c.Rate > 1000 || (c.Rate > 0 && time.Duration(c.Events)*time.Second/time.Duration(c.Rate) >= c.Deadline) {
		return errors.New("rate must be 0..1000 and paced ingestion must fit within the deadline")
	}
	if c.SampleInterval != 0 && (c.SampleInterval < time.Second || c.SampleInterval > time.Minute) {
		return errors.New("sample interval must be 0 or 1s..1m")
	}
	if c.Scenario != "success" && c.Scenario != "retry" && c.Scenario != "mixed" && c.Scenario != "fairness" && c.Scenario != "saturation" {
		return errors.New("scenario must be success, retry, mixed, fairness, or saturation")
	}
	if c.Scenario == "saturation" && (c.Events < 40 || c.SlowEndpoints < 1 || c.SlowEndpoints > 10 || c.SlowConcurrency < 1 || c.SlowConcurrency > 10) {
		return errors.New("saturation needs at least 40 events and slow endpoints/concurrency in 1..10")
	}
	if c.Scenario == "mixed" && c.Events%20 != 0 {
		return errors.New("mixed events must be a multiple of 20")
	}
	if c.Scenario == "fairness" && c.Events%10 != 0 {
		return errors.New("fairness events must be a multiple of 10")
	}
	for _, target := range []struct {
		raw      string
		receiver bool
	}{{c.API, false}, {c.History, false}, {c.Receiver, true}} {
		u, err := url.Parse(target.raw)
		if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("targets must be plain local HTTP base URLs")
		}
		host := u.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" && !(target.receiver && host == "receiver") {
			return errors.New("load tests only accept loopback targets and the synthetic receiver hostname")
		}
	}
	return nil
}

func kindFor(scenario string, i int) string {
	if scenario == "saturation" {
		if i < 30 {
			return "timeout"
		}
		return "success"
	}
	if scenario == "fairness" {
		if i%10 == 0 {
			return "timeout"
		}
		return "success"
	}
	if scenario == "retry" {
		return "retry"
	}
	if scenario == "success" {
		return "success"
	}
	switch n := i % 20; {
	case n < 14:
		return "success"
	case n < 18:
		return "retry"
	case n == 18:
		return "reject"
	default:
		return "timeout"
	}
}

func run(c config) (report, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Deadline)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	r := report{StartedAt: time.Now().UTC(), Revision: c.Revision, Runtime: runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		Scenario: c.Scenario, Events: c.Events, Concurrency: c.Concurrency, Rate: c.Rate, Timing: timingCheck{Valid: true}, States: map[string]int{}, TraceSamples: map[string]string{}, CountersDelta: map[string]float64{}, CompletionByKind: map[string]percentiles{}}
	api, history := strings.TrimRight(c.API, "/"), strings.TrimRight(c.History, "/")
	before, scrapeTime, err := metrics(ctx, client, api)
	if err != nil {
		return r, err
	}
	r.MaxScrapeMS = scrapeTime
	endpoints := map[string][]string{}
	for _, kind := range []string{"success", "retry", "reject", "timeout"} {
		needed := false
		for i := 0; i < c.Events; i++ {
			if kindFor(c.Scenario, i) == kind {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		route := map[string]string{"success": "/success", "retry": "/flaky?failures=1", "reject": "/reject", "timeout": "/timeout"}[kind]
		concurrency := 10
		if c.Scenario == "fairness" && kind == "timeout" {
			concurrency = 2
		}
		count := 1
		if c.Scenario == "saturation" && kind == "timeout" {
			count, concurrency = c.SlowEndpoints, c.SlowConcurrency
			r.SlowEndpoints, r.SlowConcurrency = count, concurrency
		}
		body, _ := json.Marshal(map[string]any{"url": strings.TrimRight(c.Receiver, "/") + route, "secret": "local-demo-secret-32-bytes-long", "max_attempts": 2, "concurrency_limit": concurrency, "rate_limit": 1000})
		for range count {
			var endpoint store.Endpoint
			if err := call(ctx, client, "POST", api+"/v1/endpoints", body, 201, &endpoint); err != nil {
				return r, err
			}
			endpoints[kind] = append(endpoints[kind], endpoint.ID)
		}
	}
	payload := []byte(`{"synthetic":"` + strings.Repeat("x", 240) + `"}`)
	r.PayloadBytes = len(payload)
	results := make([]eventResult, c.Events)
	jobs := make(chan int)
	var wg sync.WaitGroup
	failures := make(chan error, c.Concurrency)
	started := time.Now()
	observed := startSampling(ctx, client, api, started, c.SampleInterval)
	defer observed.stop()
	primeDone := make(chan struct{}, 30)
	for n := 0; n < c.Concurrency; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				kind := kindFor(c.Scenario, i)
				begin := time.Now()
				var event store.Event
				targets := endpoints[kind]
				if err := call(ctx, client, "POST", api+"/v1/endpoints/"+targets[i%len(targets)]+"/events", payload, 202, &event); err != nil {
					failures <- err
					cancel()
					return
				}
				results[i] = eventResult{event: event, kind: kind, ingestion: time.Since(begin), started: begin}
				if c.Scenario == "saturation" && i < 30 {
					primeDone <- struct{}{}
				}
			}
		}()
	}
	pace := newPacer(c.Rate, time.Now())
	var submitErr error
submit:
	for i := 0; i < c.Events; i++ {
		if c.Scenario == "saturation" && i == 30 {
			for range 30 {
				select {
				case <-primeDone:
				case <-ctx.Done():
					submitErr = ctx.Err()
					break submit
				}
			}
			r.PrimedInProgress, submitErr = awaitSaturation(ctx, client, api, min(10, c.SlowEndpoints*c.SlowConcurrency))
			if submitErr != nil {
				break
			}
			pace = newPacer(c.Rate, time.Now())
		}
		if c.Scenario != "saturation" || i >= 30 {
			if submitErr = pace.wait(ctx); submitErr != nil {
				break
			}
		}
		select {
		case jobs <- i:
			pace.observe(time.Now())
		case <-ctx.Done():
			submitErr = ctx.Err()
			break submit
		}
	}
	close(jobs)
	wg.Wait()
	close(failures)
	for err := range failures {
		return r, err
	}
	if submitErr != nil {
		return r, submitErr
	}
	r.MaxPacingLagMS = pace.maxLag.Seconds() * 1000
	r.IngestionSeconds = time.Since(started).Seconds()
	queued, scrapeTime, err := metrics(ctx, client, api)
	if err != nil {
		return r, err
	}
	r.MaxScrapeMS = max(r.MaxScrapeMS, scrapeTime)
	r.QueueAfterIngestion = queued["unfinished"]
	pending := append([]eventResult(nil), results...)
	var endToEnd, ingestion []float64
	byKind := map[string][]float64{}
	expectedDeliveries := map[string]int{}
	traces := map[string]string{}
	for len(pending) > 0 {
		next := pending[:0]
		for _, item := range pending {
			var event store.Event
			if err := call(ctx, client, "GET", api+"/v1/events/"+item.event.ID, nil, 200, &event); err != nil {
				return r, err
			}
			if event.State == "pending" || event.State == "in_progress" {
				next = append(next, item)
				continue
			}
			wantState, wantAttempts := "succeeded", 1
			if item.kind == "retry" {
				wantAttempts = 2
			}
			if item.kind == "reject" {
				wantState = "failed"
			}
			if item.kind == "timeout" {
				wantState = "dead_letter"
				wantAttempts = 2
			}
			var history struct {
				Attempts []store.Attempt `json:"attempts"`
			}
			if err := call(ctx, client, "GET", api+"/v1/events/"+event.ID+"/attempts", nil, 200, &history); err != nil {
				return r, err
			}
			if event.State != wantState || len(history.Attempts) != wantAttempts {
				return r, fmt.Errorf("unexpected %s outcome: state=%s attempts=%d", item.kind, event.State, len(history.Attempts))
			}
			for _, attempt := range history.Attempts {
				if attempt.ClaimCount != 1 {
					return r, errors.New("unexpected recovery in a no-crash workload")
				}
			}
			last := history.Attempts[len(history.Attempts)-1]
			if last.CompletedAt == nil {
				return r, errors.New("terminal attempt lacks completion")
			}
			if item.kind == "timeout" && (last.ErrorCode == nil || *last.ErrorCode != "timeout") {
				return r, errors.New("timeout receiver did not time out")
			}
			parent := history.Attempts[0].TraceParent
			if len(parent) != 55 {
				return r, errors.New("ingestion trace context missing")
			}
			traces[event.ID] = parent[3:35]
			r.TraceSamples[item.kind] = traces[event.ID]
			elapsed := last.CompletedAt.Sub(event.CreatedAt).Seconds() * 1000
			r.Timing.checkEvent(time.Duration(elapsed*float64(time.Millisecond)), time.Since(item.started))
			endToEnd = append(endToEnd, elapsed)
			byKind[item.kind] = append(byKind[item.kind], elapsed)
			ingestion = append(ingestion, item.ingestion.Seconds()*1000)
			r.States[event.State]++
			r.Attempts += wantAttempts
			expectedDeliveries[event.ID] = wantAttempts
		}
		pending = next
		if len(pending) > 0 {
			select {
			case <-ctx.Done():
				return r, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	r.TotalSeconds = time.Since(started).Seconds()
	observed.stop()
	r.Samples = observed.samples
	if observed.err != nil {
		return r, observed.err
	}
	r.Timing.checkClock(started, time.Now())
	for _, sample := range r.Samples {
		r.Timing.observeDrift(sample.ClockDriftMS)
	}
	r.DrainSeconds = r.TotalSeconds - r.IngestionSeconds
	r.EventsPerSecond = float64(c.Events) / r.TotalSeconds
	r.IngestionMS = quantiles(ingestion)
	r.EndToEndMS = quantiles(endToEnd)
	for kind, values := range byKind {
		r.CompletionByKind[kind] = quantiles(values)
	}
	var received struct {
		Deliveries []struct {
			EventID        string `json:"event_id"`
			SignatureValid bool   `json:"signature_valid"`
			BodySHA256     string `json:"body_sha256"`
			TraceID        string `json:"trace_id"`
		} `json:"deliveries"`
	}
	if err := call(ctx, client, "GET", history+"/deliveries", nil, 200, &received); err != nil {
		return r, err
	}
	digest := sha256.Sum256(payload)
	for _, item := range received.Deliveries {
		expected, ok := expectedDeliveries[item.EventID]
		if !ok {
			continue
		}
		if !item.SignatureValid || item.BodySHA256 != hex.EncodeToString(digest[:]) || item.TraceID != traces[item.EventID] || expected <= 0 {
			return r, errors.New("receiver signature, body, trace, or count mismatch")
		}
		expectedDeliveries[item.EventID]--
		r.VerifiedDeliveries++
	}
	for _, remaining := range expectedDeliveries {
		if remaining != 0 {
			return r, errors.New("receiver is missing deliveries")
		}
	}
	after, scrapeTime, err := metrics(ctx, client, api)
	if err != nil {
		return r, err
	}
	r.MaxScrapeMS = max(r.MaxScrapeMS, scrapeTime)
	for _, name := range []string{"webhook_events_accepted_total", "webhook_claims_total", "webhook_retries_scheduled_total", "webhook_claim_recoveries_total", "webhook_replays_total", "completed"} {
		r.CountersDelta[name] = after[name] - before[name]
	}
	if r.CountersDelta["webhook_events_accepted_total"] != float64(c.Events) || r.CountersDelta["webhook_claims_total"] != float64(r.Attempts) || r.CountersDelta["completed"] != float64(r.Attempts) || r.CountersDelta["webhook_retries_scheduled_total"] != float64(r.Attempts-c.Events) {
		return r, errors.New("metrics disagree with workload; use an isolated idle stack")
	}
	return r, nil
}

func call(ctx context.Context, client *http.Client, method, target string, body []byte, status int, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid local request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Event-Type", "load.synthetic")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("local HTTP request failed or workload deadline expired")
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		return fmt.Errorf("local HTTP status %d, expected %d", response.StatusCode, status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(output)
}

func metrics(ctx context.Context, client *http.Client, api string) (map[string]float64, float64, error) {
	started := time.Now()
	request, _ := http.NewRequestWithContext(ctx, "GET", api+"/metrics", nil)
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, errors.New("metrics request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, 0, fmt.Errorf("metrics returned %d", response.StatusCode)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	values := map[string]float64{}
	for name, family := range families {
		for _, metric := range family.Metric {
			if metric.Counter != nil {
				values[name] += metric.Counter.GetValue()
			}
			if name == "webhook_queue_depth" {
				values["unfinished"] += metric.Gauge.GetValue()
				for _, label := range metric.Label {
					if label.GetName() == "state" {
						values[label.GetValue()] += metric.Gauge.GetValue()
					}
				}
			}
			if name == "webhook_oldest_ready_seconds" {
				values["oldest_ready_seconds"] = metric.Gauge.GetValue()
			}
			if name == "webhook_attempts_completed_total" {
				values["completed"] += metric.Counter.GetValue()
			}
		}
	}
	return values, time.Since(started).Seconds() * 1000, nil
}

func quantiles(values []float64) percentiles {
	sort.Float64s(values)
	at := func(q float64) float64 {
		if len(values) == 0 {
			return 0
		}
		return values[max(0, int(math.Ceil(q*float64(len(values))))-1)]
	}
	return percentiles{at(.5), at(.95), at(.99)}
}

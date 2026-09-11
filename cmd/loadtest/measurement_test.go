package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMetricsKeepsQueueStatesForSampling(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE webhook_queue_depth gauge\nwebhook_queue_depth{state=\"ready\"} 7\nwebhook_queue_depth{state=\"in_progress\"} 10\n# TYPE webhook_oldest_ready_seconds gauge\nwebhook_oldest_ready_seconds 3\n# TYPE webhook_attempts_completed_total counter\nwebhook_attempts_completed_total{outcome=\"succeeded\"} 8\n"))
	}))
	defer s.Close()
	values, _, err := metrics(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if values["ready"] != 7 || values["in_progress"] != 10 || values["unfinished"] != 17 || values["oldest_ready_seconds"] != 3 || values["completed"] != 8 {
		t.Fatalf("metrics=%v", values)
	}
}

func TestPacingDoesNotCatchUpAfterDelay(t *testing.T) {
	start := time.Unix(100, 0)
	p := newPacer(10, start)
	p.observe(start)
	if !p.next.Equal(start.Add(100 * time.Millisecond)) {
		t.Fatal("incorrect interval")
	}
	p.observe(start.Add(time.Second))
	if p.maxLag != 900*time.Millisecond || !p.next.Equal(start.Add(1100*time.Millisecond)) {
		t.Fatalf("lag=%v next=%v", p.maxLag, p.next)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.wait(ctx); err != context.Canceled {
		t.Fatalf("canceled pacing=%v", err)
	}
}

func TestTimingFlagsDriftAndImpossibleDurations(t *testing.T) {
	check := timingCheck{Valid: true}
	check.observeDrift(-20)
	check.checkEvent(time.Second, time.Second)
	if !check.Valid {
		t.Fatal("consistent timing rejected")
	}
	check.observeDrift(-51)
	if check.Valid || check.MaxClockDriftMS != 51 {
		t.Fatal("clock drift accepted")
	}
	check = timingCheck{Valid: true}
	check.checkEvent(-time.Millisecond, time.Second)
	check.checkEvent(2*time.Second, time.Second)
	if check.Valid || check.InconsistentEvents != 2 {
		t.Fatalf("timing=%+v", check)
	}
}

func TestSaturationMixAndValidation(t *testing.T) {
	counts := map[string]int{}
	for i := 0; i < 130; i++ {
		counts[kindFor("saturation", i)]++
	}
	if counts["timeout"] != 30 || counts["success"] != 100 {
		t.Fatalf("mix=%v", counts)
	}
	c := config{API: "http://localhost:8080", History: "http://localhost:9090", Receiver: "http://receiver:9090", Events: 130, Concurrency: 10, Scenario: "saturation", SlowEndpoints: 5, SlowConcurrency: 2, Deadline: time.Minute, Rate: 10, SampleInterval: time.Second}
	if err := validate(c); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*config){
		func(c *config) { c.Rate = -1 }, func(c *config) { c.Rate = 1001 },
		func(c *config) { c.Rate = 1 }, func(c *config) { c.Events = 30 },
		func(c *config) { c.SlowEndpoints = 0 }, func(c *config) { c.SlowConcurrency = 11 },
		func(c *config) { c.SampleInterval = time.Millisecond },
	} {
		invalid := c
		change(&invalid)
		if validate(invalid) == nil {
			t.Fatalf("accepted %+v", invalid)
		}
	}
}

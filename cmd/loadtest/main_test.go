package main

import (
	"testing"
	"time"
)

func TestWorkloadMixAndLocalBoundary(t *testing.T) {
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[kindFor("mixed", i)]++
	}
	if counts["success"] != 70 || counts["retry"] != 20 || counts["reject"] != 5 || counts["timeout"] != 5 {
		t.Fatalf("mix=%v", counts)
	}
	counts = map[string]int{}
	for i := 0; i < 40; i++ {
		counts[kindFor("fairness", i)]++
	}
	if counts["success"] != 36 || counts["timeout"] != 4 {
		t.Fatalf("fairness mix=%v", counts)
	}
	c := config{API: "http://localhost:8080", History: "http://127.0.0.1:9090", Receiver: "http://receiver:9090", Events: 100, Concurrency: 10, Scenario: "mixed", Deadline: time.Minute}
	if err := validate(c); err != nil {
		t.Fatal(err)
	}
	c.Scenario, c.Events = "fairness", 40
	if err := validate(c); err != nil {
		t.Fatal(err)
	}
	c.Events = 41
	if validate(c) == nil {
		t.Fatal("accepted incomplete fairness mix")
	}
	c.Events = 40
	for _, target := range []string{"https://example.com", "http://example.com", "http://localhost:8080?secret=x", "http://user:pass@localhost:8080"} {
		c.API = target
		if validate(c) == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	q := quantiles([]float64{4, 2, 1, 3})
	if q.P50 != 2 || q.P95 != 4 || q.P99 != 4 {
		t.Fatalf("quantiles=%v", q)
	}
}

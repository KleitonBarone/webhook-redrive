package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func (a *API) metrics(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	snapshot, err := a.store.Metrics(ctx, a.clock.Now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "metrics unavailable")
		return
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snapshotCollector{snapshot})
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, request)
}

type snapshotCollector struct{ snapshot store.MetricsSnapshot }

func (c snapshotCollector) Describe(ch chan<- *prometheus.Desc) { prometheus.DescribeByCollect(c, ch) }

func (c snapshotCollector) Collect(ch chan<- prometheus.Metric) {
	m := c.snapshot
	for _, metric := range []struct {
		name, help string
		value      int64
	}{
		{"events_accepted_total", "Events committed to PostgreSQL.", m.Events},
		{"claims_total", "Committed claims, including recovery claims, not confirmed HTTP sends.", m.Claims},
		{"claim_recoveries_total", "Committed claims after an earlier claim expired.", m.Recoveries},
		{"retries_scheduled_total", "Automatically scheduled successor attempts.", m.Retries},
		{"replays_total", "Unique manual replay attempts committed.", m.Replays},
	} {
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("webhook_"+metric.name, metric.help, nil, nil), prometheus.CounterValue, float64(metric.value))
	}
	for _, state := range []string{"ready", "scheduled", "in_progress", "expired"} {
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("webhook_queue_depth", "Unfinished attempts by eligibility or lease state.", []string{"state"}, nil), prometheus.GaugeValue, float64(m.Queue[state]), state)
	}
	for _, state := range []string{"pending", "in_progress", "succeeded", "failed", "dead_letter"} {
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("webhook_events", "Current event state, determined by its latest attempt.", []string{"state"}, nil), prometheus.GaugeValue, float64(m.States[state]), state)
	}
	for _, state := range []string{"succeeded", "failed", "dead_letter"} {
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("webhook_attempts_completed_total", "Committed attempt outcomes, excluding ambiguous or stale completions.", []string{"outcome"}, nil), prometheus.CounterValue, float64(m.Completed[state]), state)
	}
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("webhook_oldest_ready_seconds", "Age of oldest eligible pending attempt or expired lease.", nil, nil), prometheus.GaugeValue, m.OldestReadySeconds)
	for _, kind := range []string{"attempt", "queue"} {
		h := m.Latency[kind]
		if h.Buckets == nil {
			h.Buckets = map[float64]uint64{}
			for _, b := range store.LatencyBuckets {
				h.Buckets[b] = 0
			}
		}
		help := "Seconds from last claim to committed completion; excludes unresolved claims."
		if kind == "queue" {
			help = "Seconds from eligibility to last claim for completed attempts; includes crash recovery delay."
		}
		ch <- prometheus.MustNewConstHistogram(prometheus.NewDesc("webhook_"+kind+"_duration_seconds", help, nil, nil), h.Count, h.Sum, h.Buckets)
	}
}

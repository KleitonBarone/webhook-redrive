package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type metricsStore struct {
	fakeStore
	fail bool
}

func (s *metricsStore) Metrics(ctx context.Context, _ time.Time) (store.MetricsSnapshot, error) {
	if _, ok := ctx.Deadline(); !ok {
		return store.MetricsSnapshot{}, errors.New("missing deadline")
	}
	if s.fail {
		return store.MetricsSnapshot{}, errors.New("synthetic-sensitive-database-error")
	}
	return store.MetricsSnapshot{Events: 3, Completed: map[string]int64{"succeeded": 2, "failed": 1}}, nil
}

func TestMetricsExposeBoundedLabelsAndFailClosed(t *testing.T) {
	s := &metricsStore{}
	a := New(s, nil, apiClock{now: time.Unix(100, 0)}, discardLogger(), nil)
	response := httptest.NewRecorder()
	a.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != 200 {
		t.Fatalf("metrics: %s", response.Body.String())
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(response.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 11 || families["webhook_events_accepted_total"].Metric[0].Counter.GetValue() != 3 {
		t.Fatalf("families=%v", families)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "state" && label.GetName() != "outcome" {
					t.Fatalf("unbounded label: %s", label)
				}
			}
		}
	}
	s.fail = true
	response = httptest.NewRecorder()
	a.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "sensitive") {
		t.Fatalf("failed scrape: %d %s", response.Code, response.Body.String())
	}
}

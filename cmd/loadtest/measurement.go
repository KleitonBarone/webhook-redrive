package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"
)

// Pacing never catches up with a burst. A slow client stretches the offered
// workload; the report records actual ingestion time and the largest delay.
type pacer struct {
	period, maxLag time.Duration
	next           time.Time
}

func newPacer(rate int, now time.Time) *pacer {
	p := &pacer{next: now}
	if rate > 0 {
		p.period = time.Second / time.Duration(rate)
	}
	return p
}

func (p *pacer) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.period == 0 {
		return nil
	}
	timer := time.NewTimer(time.Until(p.next))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *pacer) observe(now time.Time) {
	if p.period == 0 {
		return
	}
	p.maxLag = max(p.maxLag, now.Sub(p.next))
	p.next = now.Add(p.period)
}

const timingTolerance = 50 * time.Millisecond

type timingCheck struct {
	Valid              bool    `json:"valid"`
	MaxClockDriftMS    float64 `json:"max_clock_drift_ms"`
	InconsistentEvents int     `json:"inconsistent_events"`
}

func clockDrift(start, now time.Time) float64 {
	return float64(now.Round(0).Sub(start.Round(0))-now.Sub(start)) / float64(time.Millisecond)
}

func (t *timingCheck) observeDrift(ms float64) {
	t.MaxClockDriftMS = max(t.MaxClockDriftMS, math.Abs(ms))
	if t.MaxClockDriftMS > float64(timingTolerance)/float64(time.Millisecond) {
		t.Valid = false
	}
}

func (t *timingCheck) checkClock(start, now time.Time) { t.observeDrift(clockDrift(start, now)) }

func (t *timingCheck) checkEvent(stored, observed time.Duration) {
	if stored < 0 || stored > observed+timingTolerance {
		t.InconsistentEvents++
		t.Valid = false
	}
}

type queueSample struct {
	ElapsedSeconds     float64 `json:"elapsed_seconds"`
	Ready              float64 `json:"ready"`
	Scheduled          float64 `json:"scheduled"`
	InProgress         float64 `json:"in_progress"`
	Expired            float64 `json:"expired"`
	OldestReadySeconds float64 `json:"oldest_ready_seconds"`
	Completed          float64 `json:"completed"`
	ScrapeMS           float64 `json:"scrape_ms"`
	ClockDriftMS       float64 `json:"clock_drift_ms"`
}

type sampler struct {
	samples []queueSample
	err     error
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

func startSampling(ctx context.Context, client *http.Client, api string, started time.Time, interval time.Duration) *sampler {
	ctx, cancel := context.WithCancel(ctx)
	s := &sampler{cancel: cancel, done: make(chan struct{})}
	if interval == 0 {
		close(s.done)
		return s
	}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				values, elapsed, err := metrics(ctx, client, api)
				if err != nil {
					if ctx.Err() == nil {
						s.err = err
					}
					return
				}
				now := time.Now()
				s.samples = append(s.samples, queueSample{ElapsedSeconds: now.Sub(started).Seconds(),
					Ready: values["ready"], Scheduled: values["scheduled"], InProgress: values["in_progress"], Expired: values["expired"],
					OldestReadySeconds: values["oldest_ready_seconds"], Completed: values["completed"], ScrapeMS: elapsed, ClockDriftMS: clockDrift(started, now)})
			}
		}
	}()
	return s
}

func (s *sampler) stop() {
	s.once.Do(func() { s.cancel(); <-s.done })
}

// The saturation workload proves slots were occupied before healthy ingestion.
// It assumes the standard ten-slot worker and an otherwise idle database.
func awaitSaturation(ctx context.Context, client *http.Client, api string, target int) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		values, _, err := metrics(ctx, client, api)
		if err != nil {
			return 0, err
		}
		if values["in_progress"] >= float64(target) {
			return values["in_progress"], nil
		}
		select {
		case <-ctx.Done():
			return 0, errors.New("timeout endpoints did not occupy the expected slots")
		case <-ticker.C:
		}
	}
}

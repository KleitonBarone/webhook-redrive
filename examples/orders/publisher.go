package orders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrPublish = errors.New("outbox submission not acknowledged")

type Publisher struct {
	pool   *pgxpool.Pool
	api    string
	token  string
	client *http.Client
}

// NewPublisher accepts a deployment-controlled API origin, never event input.
// Call PublishOne periodically. A five-second persisted delay bounds retries.
func NewPublisher(pool *pgxpool.Pool, api, token string) (*Publisher, error) {
	u, err := url.Parse(api)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid API origin")
	}
	if _, err := auth.Hash(token); err != nil {
		return nil, errors.New("invalid API credential")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Publisher{pool: pool, api: api, token: token, client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (p *Publisher) Close() { p.client.CloseIdleConnections() }

// PublishOne sends one pending row without holding a transaction across HTTP.
// Concurrent publishers can submit the same row: API idempotency makes that
// safe. This deliberately favors a small reference over a second queue engine.
func (p *Publisher) PublishOne(ctx context.Context, now time.Time) (bool, error) {
	var businessID, endpointID string
	var body []byte
	err := p.pool.QueryRow(ctx, `SELECT business_event_id,endpoint_id,payload FROM outbox
        WHERE accepted_event_id IS NULL AND blocked_status IS NULL AND available_at <= $1
        ORDER BY available_at,business_event_id LIMIT 1`, now).Scan(&businessID, &endpointID, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", p.api+"/v1/endpoints/"+endpointID+"/events", bytes.NewReader(body))
	if err != nil {
		return true, ErrPublish
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Event-Type", "order.created")
	req.Header.Set("Idempotency-Key", businessID)
	response, err := p.client.Do(req)
	status := 0
	var accepted struct {
		ID string `json:"id"`
	}
	if err == nil {
		status = response.StatusCode
		decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
		err = decoder.Decode(&accepted)
		if err == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				err = ErrPublish
			}
		}
		response.Body.Close()
	}
	if err == nil && status == http.StatusAccepted && id.Valid(accepted.ID) {
		_, err = p.pool.Exec(ctx, `UPDATE outbox SET accepted_event_id=$2,blocked_status=NULL WHERE business_event_id=$1 AND accepted_event_id IS NULL`, businessID, accepted.ID)
		return true, err
	}
	// Keep ambiguous responses retryable. Permanent HTTP rejection requires an
	// operator to fix configuration and clear blocked_status, keeping the key.
	var blocked *int
	if status >= 300 && status < 500 && status != 408 && status != 429 {
		blocked = &status
	}
	_, updateErr := p.pool.Exec(ctx, `UPDATE outbox SET available_at=$2,blocked_status=$3 WHERE business_event_id=$1 AND accepted_event_id IS NULL`, businessID, now.Add(5*time.Second), blocked)
	if updateErr != nil {
		return true, updateErr
	}
	return true, ErrPublish
}

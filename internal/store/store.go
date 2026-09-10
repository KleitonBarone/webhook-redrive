package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/KleitonBarone/webhook-redrive/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflicting replay")

type Store struct {
	pool *pgxpool.Pool
}

type Endpoint struct {
	MaxAttempts      int       `json:"max_attempts"`
	ConcurrencyLimit int       `json:"concurrency_limit"`
	RateLimit        int       `json:"rate_limit"`
	ID               string    `json:"id"`
	URL              string    `json:"url"`
	CreatedAt        time.Time `json:"created_at"`
}

type Event struct {
	ID         string    `json:"id"`
	EndpointID string    `json:"endpoint_id"`
	EventType  string    `json:"event_type"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
}

type Attempt struct {
	AttemptNumber   int        `json:"attempt_number"`
	CycleAttempt    int        `json:"cycle_attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	AvailableAt     time.Time  `json:"available_at"`
	Retryable       *bool      `json:"retryable,omitempty"`
	ReplayOf        *string    `json:"replay_of,omitempty"`
	ReplayRequestID *string    `json:"replay_request_id,omitempty"`
	ReplayActor     *string    `json:"replay_actor,omitempty"`
	ReplayReason    *string    `json:"replay_reason,omitempty"`
	ID              string     `json:"id"`
	EventID         string     `json:"event_id"`
	State           string     `json:"state"`
	ClaimCount      int        `json:"claim_count"`
	LastStartedAt   *time.Time `json:"last_started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	ResponseStatus  *int       `json:"response_status,omitempty"`
	ErrorCode       *string    `json:"error_code,omitempty"`
	ErrorMessage    *string    `json:"error_message,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type ClaimedDelivery struct {
	CycleAttempt     int
	LeaseUntil       time.Time
	AttemptID        string
	EventID          string
	EventType        string
	Payload          []byte
	EndpointID       string
	EndpointURL      string
	SecretCiphertext []byte
	ClaimCount       int
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Migrate(ctx context.Context, now time.Time) error {
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		contents, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if err := s.applyMigration(ctx, entry.Name(), string(contents), now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, version, sql string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('webhook-redrive-migrations'))`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}

	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
		return fmt.Errorf("check migration %s: %w", version, err)
	}
	if applied {
		return nil
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`, version, now); err != nil {
		return fmt.Errorf("record migration %s: %w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", version, err)
	}
	return nil
}

func (s *Store) CreateEndpoint(ctx context.Context, endpoint Endpoint, secretCiphertext []byte) error {
	if endpoint.MaxAttempts == 0 {
		endpoint.MaxAttempts = 5
	}
	if endpoint.ConcurrencyLimit == 0 {
		endpoint.ConcurrencyLimit = 2
	}
	if endpoint.RateLimit == 0 {
		endpoint.RateLimit = 10
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_endpoints (id, url, secret_ciphertext, created_at, max_attempts, concurrency_limit, rate_limit)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		endpoint.ID, endpoint.URL, secretCiphertext, endpoint.CreatedAt,
		endpoint.MaxAttempts, endpoint.ConcurrencyLimit, endpoint.RateLimit)
	if err != nil {
		return fmt.Errorf("insert endpoint: %w", err)
	}
	return nil
}

// CreateEvent atomically records the exact payload bytes and its first attempt.
func (s *Store) CreateEvent(ctx context.Context, event Event, payload []byte, attemptID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin event ingestion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO events (id, endpoint_id, event_type, payload, created_at)
		VALUES ($1, $2, $3, $4, $5)`, event.ID, event.EndpointID, event.EventType, payload, event.CreatedAt); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO delivery_attempts (
			id, event_id, state, available_at, created_at, updated_at, endpoint_id, max_attempts
		) SELECT $1, $2, 'pending', $3, $3, $3, id, max_attempts FROM webhook_endpoints WHERE id=$4`, attemptID, event.ID, event.CreatedAt, event.EndpointID); err != nil {
		return fmt.Errorf("insert delivery attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit event ingestion: %w", err)
	}
	return nil
}

func (s *Store) EndpointExists(ctx context.Context, endpointID string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM webhook_endpoints WHERE id = $1)`, endpointID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check endpoint: %w", err)
	}
	return exists, nil
}

func (s *Store) GetEvent(ctx context.Context, eventID string) (Event, error) {
	var event Event
	err := s.pool.QueryRow(ctx, `
		SELECT event.id, event.endpoint_id, event.event_type, attempt.state, event.created_at
		FROM events AS event
		JOIN delivery_attempts AS attempt ON attempt.event_id = event.id
		WHERE event.id = $1 ORDER BY attempt.attempt_number DESC LIMIT 1`, eventID).Scan(
		&event.ID, &event.EndpointID, &event.EventType, &event.State, &event.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("get event: %w", err)
	}
	return event, nil
}

func (s *Store) ListAttempts(ctx context.Context, eventID string) ([]Attempt, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, event_id, state, claim_count, last_started_at, completed_at,
		       response_status, error_code, error_message, created_at, updated_at,
		       attempt_number, cycle_attempt, max_attempts, available_at, retryable,
		       replay_of, replay_request_id, replay_actor, replay_reason
		FROM delivery_attempts
		WHERE event_id = $1
		ORDER BY attempt_number`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()

	attempts := make([]Attempt, 0)
	for rows.Next() {
		var attempt Attempt
		if err := rows.Scan(
			&attempt.ID,
			&attempt.EventID,
			&attempt.State,
			&attempt.ClaimCount,
			&attempt.LastStartedAt,
			&attempt.CompletedAt,
			&attempt.ResponseStatus,
			&attempt.ErrorCode,
			&attempt.ErrorMessage,
			&attempt.CreatedAt,
			&attempt.UpdatedAt,
			&attempt.AttemptNumber,
			&attempt.CycleAttempt,
			&attempt.MaxAttempts,
			&attempt.AvailableAt,
			&attempt.Retryable,
			&attempt.ReplayOf,
			&attempt.ReplayRequestID,
			&attempt.ReplayActor,
			&attempt.ReplayReason,
		); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read attempts: %w", err)
	}
	if len(attempts) == 0 {
		return nil, ErrNotFound
	}
	return attempts, nil
}

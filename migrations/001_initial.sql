CREATE TABLE webhook_endpoints (
    id uuid PRIMARY KEY,
    url text NOT NULL,
    secret_ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE events (
    id uuid PRIMARY KEY,
    endpoint_id uuid NOT NULL REFERENCES webhook_endpoints(id),
    event_type text NOT NULL,
    payload bytea NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE delivery_attempts (
    id uuid PRIMARY KEY,
    event_id uuid NOT NULL UNIQUE REFERENCES events(id),
    state text NOT NULL CHECK (state IN ('pending', 'in_progress', 'succeeded', 'failed')),
    available_at timestamptz NOT NULL,
    lease_until timestamptz,
    claimed_by text,
    claim_count integer NOT NULL DEFAULT 0 CHECK (claim_count >= 0),
    last_started_at timestamptz,
    completed_at timestamptz,
    response_status integer,
    error_code text,
    error_message text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX delivery_attempts_claim_idx
    ON delivery_attempts (available_at, lease_until, created_at)
    WHERE state IN ('pending', 'in_progress');

ALTER TABLE webhook_endpoints
    ADD COLUMN max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts BETWEEN 1 AND 20),
    ADD COLUMN concurrency_limit integer NOT NULL DEFAULT 2 CHECK (concurrency_limit BETWEEN 1 AND 100),
    ADD COLUMN rate_limit integer NOT NULL DEFAULT 10 CHECK (rate_limit BETWEEN 1 AND 1000),
    ADD COLUMN rate_window timestamptz NOT NULL DEFAULT '-infinity',
    ADD COLUMN rate_used integer NOT NULL DEFAULT 0 CHECK (rate_used >= 0);

ALTER TABLE delivery_attempts
    DROP CONSTRAINT delivery_attempts_event_id_key,
    DROP CONSTRAINT delivery_attempts_state_check,
    ADD CONSTRAINT delivery_attempts_state_check CHECK (state IN ('pending', 'in_progress', 'succeeded', 'failed', 'dead_letter')),
    ADD COLUMN endpoint_id uuid REFERENCES webhook_endpoints(id),
    ADD COLUMN attempt_number integer NOT NULL DEFAULT 1 CHECK (attempt_number > 0),
    ADD COLUMN cycle_attempt integer NOT NULL DEFAULT 1 CHECK (cycle_attempt > 0),
    ADD COLUMN max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts BETWEEN 1 AND 20),
    ADD COLUMN retryable boolean,
    ADD COLUMN replay_of uuid REFERENCES delivery_attempts(id),
    ADD COLUMN replay_request_id uuid UNIQUE,
    ADD COLUMN replay_actor text,
    ADD COLUMN replay_reason text,
    ADD CONSTRAINT attempt_sequence UNIQUE (event_id, attempt_number);

UPDATE delivery_attempts AS attempt
SET endpoint_id = event.endpoint_id
FROM events AS event WHERE event.id = attempt.event_id;
ALTER TABLE delivery_attempts ALTER COLUMN endpoint_id SET NOT NULL;

CREATE UNIQUE INDEX one_active_attempt_per_event ON delivery_attempts (event_id)
    WHERE state IN ('pending', 'in_progress');
CREATE INDEX endpoint_active_attempts ON delivery_attempts (endpoint_id, available_at, lease_until)
    WHERE state IN ('pending', 'in_progress');

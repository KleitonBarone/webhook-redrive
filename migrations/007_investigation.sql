ALTER TABLE events ADD COLUMN producer_reference text NOT NULL DEFAULT ''
    CHECK (octet_length(producer_reference) <= 128);
CREATE INDEX events_created_cursor ON events(created_at,id);
CREATE INDEX events_endpoint_created_cursor ON events(endpoint_id,created_at,id);
CREATE INDEX events_reference_created_cursor ON events(producer_reference,created_at,id)
    WHERE producer_reference <> '';

CREATE TABLE replay_batches (
    id uuid PRIMARY KEY,
    principal_id uuid NOT NULL REFERENCES principals(id),
    actor text NOT NULL,
    reason text NOT NULL CHECK (octet_length(reason) BETWEEN 1 AND 500),
    selection jsonb NOT NULL CHECK (jsonb_array_length(selection) BETWEEN 1 AND 100),
    created_at timestamptz NOT NULL,
    started_at timestamptz
);
CREATE TABLE replay_batch_items (
    batch_id uuid NOT NULL REFERENCES replay_batches(id),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 99),
    event_id uuid NOT NULL REFERENCES events(id),
    expected_attempt_id uuid NOT NULL REFERENCES delivery_attempts(id),
    request_id uuid NOT NULL UNIQUE,
    preview_state text NOT NULL,
    preview_eligible boolean NOT NULL,
    result text NOT NULL DEFAULT 'pending' CHECK (result IN ('pending','replayed','skipped')),
    replay_attempt_id uuid REFERENCES delivery_attempts(id),
    completed_at timestamptz,
    PRIMARY KEY(batch_id,position),
    UNIQUE(batch_id,event_id),
    CHECK ((result='pending') = (completed_at IS NULL)),
    CHECK ((result='replayed') = (replay_attempt_id IS NOT NULL))
);

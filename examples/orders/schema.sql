-- Reference application's tables, installed only in its own schema/database.
CREATE TABLE orders (
    id uuid PRIMARY KEY,
    amount_cents bigint NOT NULL CHECK(amount_cents > 0)
);
CREATE TABLE outbox (
    business_event_id uuid PRIMARY KEY,
    endpoint_id uuid NOT NULL,
    payload bytea NOT NULL,
    available_at timestamptz NOT NULL,
    accepted_event_id uuid,
    blocked_status integer
);
CREATE INDEX outbox_pending ON outbox(available_at,business_event_id)
    WHERE accepted_event_id IS NULL AND blocked_status IS NULL;
CREATE TABLE receipts (
    business_event_id uuid PRIMARY KEY,
    body_hash bytea NOT NULL CHECK(octet_length(body_hash)=32)
);
-- Incrementing a total makes an accidental duplicate business action visible.
CREATE TABLE order_totals (
    id integer PRIMARY KEY CHECK(id=1),
    orders bigint NOT NULL,
    amount_cents bigint NOT NULL
);

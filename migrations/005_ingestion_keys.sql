-- Keys remain reserved for the lifetime of retained event history. No expiry.
CREATE TABLE ingestion_keys (
    principal_id uuid NOT NULL REFERENCES principals(id),
    endpoint_id uuid NOT NULL REFERENCES webhook_endpoints(id),
    key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    event_id uuid NOT NULL UNIQUE REFERENCES events(id) DEFERRABLE INITIALLY DEFERRED,
    attempt_id uuid NOT NULL REFERENCES delivery_attempts(id) DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (principal_id, endpoint_id, key_hash)
);

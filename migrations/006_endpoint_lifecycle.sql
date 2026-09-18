ALTER TABLE webhook_endpoints
    ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD COLUMN paused boolean NOT NULL DEFAULT false,
    ADD COLUMN signing_version bigint NOT NULL DEFAULT 1 CHECK (signing_version > 0),
    ADD COLUMN retiring_signing_version bigint,
    ADD COLUMN retire_after timestamptz,
    ADD COLUMN retry_base_seconds integer NOT NULL DEFAULT 1 CHECK (retry_base_seconds BETWEEN 1 AND 86400),
    ADD COLUMN retry_cap_seconds integer NOT NULL DEFAULT 60 CHECK (retry_cap_seconds BETWEEN retry_base_seconds AND 86400),
    ADD COLUMN event_ttl_seconds integer NOT NULL DEFAULT 86400 CHECK (event_ttl_seconds BETWEEN 1 AND 604800),
    ADD CONSTRAINT signing_retirement_pair CHECK (
        (retiring_signing_version IS NULL AND retire_after IS NULL) OR
        (retiring_signing_version IS NOT NULL AND retire_after IS NOT NULL AND retiring_signing_version < signing_version));

CREATE TABLE endpoint_audit (
    endpoint_id uuid NOT NULL REFERENCES webhook_endpoints(id),
    version bigint NOT NULL,
    principal_id uuid REFERENCES principals(id),
    action text NOT NULL CHECK (action IN ('created','updated','paused','resumed','rotated','retired')),
    reason text NOT NULL,
    configuration jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (endpoint_id, version)
);

ALTER TABLE delivery_attempts
    ADD COLUMN retry_base_seconds integer NOT NULL DEFAULT 1 CHECK (retry_base_seconds BETWEEN 1 AND 86400),
    ADD COLUMN retry_cap_seconds integer NOT NULL DEFAULT 60 CHECK (retry_cap_seconds BETWEEN retry_base_seconds AND 86400),
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN endpoint_version bigint,
    ADD COLUMN signing_version bigint;
-- Preserve pre-upgrade cycles without inventing a deadline or audit identity.
-- New ingestion and explicit replay always snapshot a finite deadline.
CREATE INDEX attempts_expiration ON delivery_attempts(expires_at)
    WHERE state IN ('pending','in_progress') AND expires_at IS NOT NULL;

-- Sequence gaps are harmless. Only a committed endpoint update records service.
CREATE SEQUENCE endpoint_service_sequence AS bigint;
ALTER TABLE webhook_endpoints
    ADD COLUMN last_service_seq bigint NOT NULL DEFAULT 0 CHECK (last_service_seq >= 0);
CREATE INDEX endpoint_service_order ON webhook_endpoints (last_service_seq, id)
    WHERE NOT paused;

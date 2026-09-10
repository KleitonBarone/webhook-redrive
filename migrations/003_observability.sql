-- Trace context travels with durable work, never with payload or credentials.
ALTER TABLE delivery_attempts ADD COLUMN trace_parent varchar(55) NOT NULL DEFAULT '';

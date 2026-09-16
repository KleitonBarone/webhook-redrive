CREATE TABLE principals (
    id uuid PRIMARY KEY,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    kind text NOT NULL CHECK (kind IN ('service','operator')),
    permissions text[] NOT NULL CHECK (cardinality(permissions) > 0 AND
        permissions <@ ARRAY['ingest','inspect','endpoints','replay','metrics']::text[]),
    created_at timestamptz NOT NULL
);

CREATE TABLE credentials (
    id uuid PRIMARY KEY,
    principal_id uuid NOT NULL REFERENCES principals(id),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    revoked_at timestamptz
);

-- Database administrators provision credentials out of band. db_actor records
-- the database identity, not a caller-supplied claim of a human identity.
CREATE TABLE credential_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    credential_id uuid NOT NULL REFERENCES credentials(id),
    action text NOT NULL CHECK (action IN ('issued','revoked')),
    db_actor text NOT NULL DEFAULT session_user,
    created_at timestamptz NOT NULL
);

ALTER TABLE webhook_endpoints ADD COLUMN created_by uuid REFERENCES principals(id);
ALTER TABLE delivery_attempts ADD COLUMN replay_principal_id uuid REFERENCES principals(id);

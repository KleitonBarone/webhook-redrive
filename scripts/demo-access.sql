-- Public synthetic credentials for the loopback-only Compose demo, never deployment.
-- Re-running does not revive revoked credentials or rewrite an existing identity.
BEGIN;
INSERT INTO principals(id,name,kind,permissions,created_at) VALUES
('00000000-0000-4000-8000-000000000001','demo-operator','operator',ARRAY['ingest','inspect','endpoints','replay','metrics'],now()),
('00000000-0000-4000-8000-000000000002','demo-producer','service',ARRAY['ingest'],now())
ON CONFLICT DO NOTHING;
WITH issued AS (
    INSERT INTO credentials(id,principal_id,token_hash,created_at,expires_at) VALUES
    ('00000000-0000-4000-8000-000000000011','00000000-0000-4000-8000-000000000001',sha256(convert_to('wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA','UTF8')),now(),now()+interval '1 year'),
    ('00000000-0000-4000-8000-000000000012','00000000-0000-4000-8000-000000000002',sha256(convert_to('wr_AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE','UTF8')),now(),now()+interval '1 year')
    ON CONFLICT DO NOTHING RETURNING id
) INSERT INTO credential_audit(credential_id,action,created_at) SELECT id,'issued',now() FROM issued;
COMMIT;

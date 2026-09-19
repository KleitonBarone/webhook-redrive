-- Cumulative accounting survives history retention. Gauge queries stay live.
CREATE TABLE cumulative_metrics (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    events bigint NOT NULL DEFAULT 0,
    claims bigint NOT NULL DEFAULT 0,
    recoveries bigint NOT NULL DEFAULT 0,
    retries bigint NOT NULL DEFAULT 0,
    replays bigint NOT NULL DEFAULT 0,
    succeeded bigint NOT NULL DEFAULT 0,
    failed bigint NOT NULL DEFAULT 0,
    dead_letter bigint NOT NULL DEFAULT 0,
    latency_count bigint NOT NULL DEFAULT 0,
    attempt_sum double precision NOT NULL DEFAULT 0,
    queue_sum double precision NOT NULL DEFAULT 0,
    attempt_buckets bigint[] NOT NULL DEFAULT ARRAY[0,0,0,0,0,0,0,0,0,0]::bigint[],
    queue_buckets bigint[] NOT NULL DEFAULT ARRAY[0,0,0,0,0,0,0,0,0,0]::bigint[]
);
INSERT INTO cumulative_metrics(singleton,events,claims,recoveries,retries,replays,succeeded,failed,dead_letter,latency_count,attempt_sum,queue_sum)
SELECT true,(SELECT count(*) FROM events),coalesce(sum(claim_count),0),coalesce(sum(greatest(claim_count-1,0)),0),
    count(*) FILTER(WHERE cycle_attempt>1),count(*) FILTER(WHERE replay_of IS NOT NULL),
    count(*) FILTER(WHERE completed_at IS NOT NULL AND state='succeeded'),
    count(*) FILTER(WHERE completed_at IS NOT NULL AND state='failed'),
    count(*) FILTER(WHERE completed_at IS NOT NULL AND state='dead_letter'),
    count(*) FILTER(WHERE completed_at IS NOT NULL AND last_started_at IS NOT NULL),
    coalesce(sum(greatest(0,extract(epoch FROM completed_at-last_started_at))) FILTER(WHERE completed_at IS NOT NULL AND last_started_at IS NOT NULL),0),
    coalesce(sum(greatest(0,extract(epoch FROM last_started_at-available_at))) FILTER(WHERE completed_at IS NOT NULL AND last_started_at IS NOT NULL),0)
FROM delivery_attempts;
UPDATE cumulative_metrics SET
    attempt_buckets=ARRAY(SELECT (SELECT count(*) FROM delivery_attempts WHERE completed_at IS NOT NULL AND last_started_at IS NOT NULL AND greatest(0,extract(epoch FROM completed_at-last_started_at))<=b) FROM unnest(ARRAY[0.005,0.025,0.1,0.5,1,2,5,10,30,60]) b),
    queue_buckets=ARRAY(SELECT (SELECT count(*) FROM delivery_attempts WHERE completed_at IS NOT NULL AND last_started_at IS NOT NULL AND greatest(0,extract(epoch FROM last_started_at-available_at))<=b) FROM unnest(ARRAY[0.005,0.025,0.1,0.5,1,2,5,10,30,60]) b);

CREATE FUNCTION count_accepted_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE cumulative_metrics SET events=events+1;
    RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER count_accepted_event AFTER INSERT ON events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION count_accepted_event();

CREATE FUNCTION count_attempt_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    inserted boolean := TG_OP='INSERT';
    previous_claims integer := 0;
    completion boolean;
    sampled boolean;
    attempt_seconds double precision;
    queue_seconds double precision;
BEGIN
    IF NOT inserted THEN previous_claims := OLD.claim_count; END IF;
    completion := NEW.completed_at IS NOT NULL AND (inserted OR OLD.completed_at IS NULL);
    sampled := completion AND NEW.last_started_at IS NOT NULL;
    attempt_seconds := greatest(0,extract(epoch FROM NEW.completed_at-NEW.last_started_at));
    queue_seconds := greatest(0,extract(epoch FROM NEW.last_started_at-NEW.available_at));
    IF NOT inserted AND NOT completion AND NEW.claim_count=previous_claims THEN RETURN NEW; END IF;
    UPDATE cumulative_metrics SET
        claims=claims+NEW.claim_count-previous_claims,
        recoveries=recoveries+greatest(NEW.claim_count-1,0)-greatest(previous_claims-1,0),
        retries=retries+(inserted AND NEW.cycle_attempt>1)::int,
        replays=replays+(inserted AND NEW.replay_of IS NOT NULL)::int,
        succeeded=succeeded+(completion AND NEW.state='succeeded')::int,
        failed=failed+(completion AND NEW.state='failed')::int,
        dead_letter=dead_letter+(completion AND NEW.state='dead_letter')::int,
        latency_count=latency_count+sampled::int,
        attempt_sum=attempt_sum+CASE WHEN sampled THEN attempt_seconds ELSE 0 END,
        queue_sum=queue_sum+CASE WHEN sampled THEN queue_seconds ELSE 0 END,
        attempt_buckets=ARRAY(SELECT v+(sampled AND attempt_seconds<=b)::int FROM unnest(attempt_buckets,ARRAY[0.005,0.025,0.1,0.5,1,2,5,10,30,60]) x(v,b)),
        queue_buckets=ARRAY(SELECT v+(sampled AND queue_seconds<=b)::int FROM unnest(queue_buckets,ARRAY[0.005,0.025,0.1,0.5,1,2,5,10,30,60]) x(v,b));
    RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER count_attempt_transition AFTER INSERT OR UPDATE ON delivery_attempts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION count_attempt_transition();

CREATE TABLE worker_progress (
    worker_id text PRIMARY KEY,
    polled_at timestamptz NOT NULL,
    claimed_at timestamptz,
    completed_at timestamptz
);
CREATE TABLE master_key_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    check_ciphertext bytea,
    generation bigint NOT NULL DEFAULT 1
);
INSERT INTO master_key_state(singleton) VALUES(true);
CREATE TABLE maintenance_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    action text NOT NULL CHECK(action IN ('retention','master_key_rotation')),
    db_actor text NOT NULL DEFAULT session_user,
    affected bigint NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX attempts_retention ON delivery_attempts(completed_at,event_id) WHERE completed_at IS NOT NULL;
CREATE INDEX batches_retention ON replay_batches(created_at,id);

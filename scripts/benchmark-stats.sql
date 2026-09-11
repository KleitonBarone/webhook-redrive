-- Read-only snapshots. Keep SQL text and bind values out of saved evidence.
WITH classified AS (
    SELECT CASE
        WHEN query LIKE '%pg_stat_statements%' THEN 'observer'
        WHEN query LIKE '%SELECT endpoint.id FROM webhook_endpoints%' THEN 'claim_endpoints'
        WHEN query LIKE '%WITH candidates AS (%' THEN 'claim_attempts'
        WHEN query LIKE '%SELECT concurrency_limit -%' THEN 'claim_capacity'
        WHEN query LIKE '%UPDATE webhook_endpoints SET%' THEN 'claim_rate'
        WHEN query LIKE '%delivery_attempts%' AND query LIKE '%GROUP BY%' THEN 'metrics'
        WHEN lower(trim(query)) IN ('begin', 'commit', 'rollback') THEN 'transaction'
        ELSE 'other'
    END AS category, calls, total_exec_time, rows, shared_blks_hit, shared_blks_read,
        temp_blks_written, wal_bytes, shared_blk_read_time, shared_blk_write_time
    FROM pg_stat_statements
    WHERE dbid = (SELECT oid FROM pg_database WHERE datname=current_database())
), grouped AS (
    SELECT category, sum(calls) AS calls, sum(total_exec_time) AS execution_ms,
        sum(rows) AS rows, sum(shared_blks_hit) AS buffer_hits,
        sum(shared_blks_read) AS buffer_reads, sum(temp_blks_written) AS temp_blocks_written,
        sum(wal_bytes) AS wal_bytes, sum(shared_blk_read_time) AS read_ms,
        sum(shared_blk_write_time) AS write_ms
    FROM classified GROUP BY category
)
SELECT json_build_object(
    'captured_at', clock_timestamp(),
    'server_version', current_setting('server_version'),
    'statistics_reset', (SELECT stats_reset FROM pg_stat_statements_info),
    'statistics_deallocations', (SELECT dealloc FROM pg_stat_statements_info),
    'database_bytes', pg_database_size(current_database()),
    'events', (SELECT count(*) FROM events),
    'attempts', (SELECT count(*) FROM delivery_attempts),
    'statements', (SELECT json_object_agg(category, to_jsonb(grouped)-'category') FROM grouped),
    'database', (SELECT json_build_object('commits', xact_commit, 'rollbacks', xact_rollback,
        'deadlocks', deadlocks, 'temp_bytes', temp_bytes, 'read_ms', blk_read_time,
        'write_ms', blk_write_time, 'statistics_reset', stats_reset)
        FROM pg_stat_database WHERE datname=current_database())
);

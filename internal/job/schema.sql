-- Concurrent startups can race on CREATE TABLE IF NOT EXISTS; this lock, held for the whole file, serializes them.
SELECT pg_advisory_xact_lock(hashtext('debrief schema'));

CREATE TABLE IF NOT EXISTS jobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key text UNIQUE,
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'processing', 'succeeded', 'failed')),
    filename        text NOT NULL,
    content_type    text NOT NULL,
    audio           bytea,
    audio_sha256    bytea NOT NULL,
    attempts        int NOT NULL DEFAULT 0,
    max_attempts    int NOT NULL CHECK (max_attempts > 0),
    run_after       timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz,
    last_error      text NOT NULL DEFAULT '',
    last_error_body text NOT NULL DEFAULT '',
    result          jsonb,
    delivery        jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'processing') = (locked_until IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS jobs_queued ON jobs (run_after) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS jobs_leased ON jobs (locked_until) WHERE status = 'processing';

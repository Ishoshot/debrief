package job

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Ishoshot/debrief/internal/ai"
)

// The row lock lasts only for this statement; the lease, not a lock, protects the job while it runs.
func (s *Store) Claim(ctx context.Context, lease time.Duration) (Claimed, error) {
	var c Claimed
	j, err := scanJob(s.pool.QueryRow(ctx, `
		UPDATE jobs
		SET status = 'processing', attempts = attempts + 1,
		    locked_until = now() + make_interval(secs => $1), updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE (status = 'queued' AND run_after <= now())
			   OR (status = 'processing' AND locked_until < now() AND attempts < max_attempts)
			ORDER BY run_after
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+jobColumns+`, audio`, lease.Seconds()), &c.Audio)
	if errors.Is(err, ErrNotFound) {
		return Claimed{}, ErrNoJob
	}
	if err != nil {
		return Claimed{}, fmt.Errorf("claim job: %w", err)
	}
	c.Job = j
	return c, nil
}

// Claim skips a job whose final lease expired, so without this the job stays processing.
func (s *Store) FailExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'failed', locked_until = NULL, updated_at = now(),
		    last_error = 'lease expired on the final attempt', last_error_body = ''
		WHERE status = 'processing' AND locked_until < now() AND attempts >= max_attempts`)
	if err != nil {
		return 0, fmt.Errorf("fail expired jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) SaveResult(ctx context.Context, l Lease, r ai.Result) error {
	return leased(s.pool.Exec(ctx, `
		UPDATE jobs SET result = $3, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'processing'`, l.ID, l.Attempt, r))
}

// The audio is dropped: once the report is delivered the recording has served its purpose.
func (s *Store) Succeed(ctx context.Context, l Lease, d Delivery) error {
	return leased(s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'succeeded', delivery = $3, audio = NULL, last_error = '', last_error_body = '',
		    locked_until = NULL, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'processing'`, l.ID, l.Attempt, d))
}

func (s *Store) Retry(ctx context.Context, l Lease, f Failure, delay time.Duration) (Status, error) {
	var st Status
	err := s.pool.QueryRow(ctx, `
		UPDATE jobs
		SET status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
		    run_after = now() + make_interval(secs => $5), last_error = $3, last_error_body = $4,
		    locked_until = NULL, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'processing'
		RETURNING status`, l.ID, l.Attempt, pgText(f.Message), pgText(f.ResponseBody), delay.Seconds()).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLeaseLost
	}
	if err != nil {
		return "", fmt.Errorf("retry job %s: %w", l.ID, err)
	}
	return st, nil
}

func (s *Store) Fail(ctx context.Context, l Lease, f Failure) error {
	return leased(s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'failed', last_error = $3, last_error_body = $4,
		    locked_until = NULL, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'processing'`, l.ID, l.Attempt, pgText(f.Message), pgText(f.ResponseBody)))
}

func (s *Store) Release(ctx context.Context, l Lease) error {
	return leased(s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'queued', max_attempts = max_attempts + 1, run_after = now(),
		    locked_until = NULL, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'processing'`, l.ID, l.Attempt))
}

func leased(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// A Postgres text column rejects NUL bytes and invalid UTF-8, and vendor replies and model output can contain both.
func pgText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

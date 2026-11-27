package job

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool        *pgxpool.Pool
	maxAttempts int
}

const jobColumns = `id, status, filename, content_type, attempts, max_attempts, last_error, result, delivery, created_at, updated_at`

//go:embed schema.sql
var schema string

func NewStore(pool *pgxpool.Pool, maxAttempts int) *Store {
	return &Store{pool: pool, maxAttempts: maxAttempts}
}

func CreateSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) Create(ctx context.Context, n New) (Job, bool, error) {
	sum := sha256.Sum256(n.Audio)
	key := nullIfEmpty(n.IdempotencyKey)
	j, err := s.insert(ctx, n, key, sum[:])
	switch {
	case err == nil:
		return j, true, nil
	case errors.Is(err, ErrNotFound):
		j, err = s.byIdempotencyKey(ctx, key, sum[:])
		return j, false, err
	default:
		return Job{}, false, fmt.Errorf("insert job: %w", err)
	}
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id))
	if err != nil {
		return Job{}, fmt.Errorf("get job %s: %w", id, err)
	}
	return j, nil
}

// ON CONFLICT DO NOTHING returns no row, so ErrNotFound means the idempotency key already exists.
func (s *Store) insert(ctx context.Context, n New, key *string, sum []byte) (Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `
		INSERT INTO jobs (idempotency_key, filename, content_type, audio, audio_sha256, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+jobColumns,
		key, n.Filename, n.ContentType, n.Audio, sum, s.maxAttempts))
}

func (s *Store) byIdempotencyKey(ctx context.Context, key *string, sum []byte) (Job, error) {
	var stored []byte
	j, err := scanJob(s.pool.QueryRow(ctx,
		`SELECT `+jobColumns+`, audio_sha256 FROM jobs WHERE idempotency_key = $1`, key), &stored)
	if err != nil {
		return Job{}, fmt.Errorf("load job by idempotency key: %w", err)
	}
	if !bytes.Equal(stored, sum) {
		return Job{}, ErrKeyReused
	}
	return j, nil
}

func scanJob(row pgx.Row, extra ...any) (Job, error) {
	var j Job
	dest := append([]any{&j.ID, &j.Status, &j.Filename, &j.ContentType, &j.Attempts, &j.MaxAttempts,
		&j.LastError, &j.Result, &j.Delivery, &j.CreatedAt, &j.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	return j, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

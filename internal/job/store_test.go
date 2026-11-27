//go:build integration

package job_test

import (
	"cmp"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/job"
)

const lease = time.Minute

func newStore(t *testing.T, maxAttempts int) (*job.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	url := cmp.Or(os.Getenv("DATABASE_URL"), "postgres://debrief:debrief@localhost:5433/debrief_test?sslmode=disable")
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := job.CreateSchema(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE jobs"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return job.NewStore(pool, maxAttempts), pool
}

func create(t *testing.T, s *job.Store, key string) job.Job {
	t.Helper()
	j, _, err := s.Create(context.Background(), job.New{
		IdempotencyKey: key, Filename: "note.m4a", ContentType: "audio/m4a", Audio: []byte("audio"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return j
}

func TestConcurrentStartupsCreateTheSchemaWithoutError(t *testing.T) {
	ctx := context.Background()
	_, pool := newStore(t, 3)
	if _, err := pool.Exec(ctx, "DROP TABLE jobs"); err != nil {
		t.Fatalf("drop: %v", err)
	}

	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { errs <- job.CreateSchema(ctx, pool) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent CreateSchema: %v", err)
		}
	}
}

func TestCreateWithIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 3)
	first := create(t, s, "key-1")

	again, created, err := s.Create(ctx, job.New{IdempotencyKey: "key-1", Filename: "x.m4a", ContentType: "audio/m4a", Audio: []byte("audio")})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("same key: got id %s created=%v err=%v; want id %s created=false", again.ID, created, err, first.ID)
	}

	_, _, err = s.Create(ctx, job.New{IdempotencyKey: "key-1", Filename: "x.m4a", ContentType: "audio/m4a", Audio: []byte("other")})
	if !errors.Is(err, job.ErrKeyReused) {
		t.Fatalf("same key, other audio: err = %v, want ErrKeyReused", err)
	}

	a, b := create(t, s, ""), create(t, s, "")
	if a.ID == b.ID {
		t.Fatal("two jobs without a key got the same ID")
	}
}

func TestClaimSkipsLockedRows(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t, 3)
	locked := create(t, s, "")
	free := create(t, s, "")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM jobs WHERE id = $1 FOR UPDATE", locked.ID); err != nil {
		t.Fatal(err)
	}

	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := s.Claim(claimCtx, lease)
	if err != nil {
		t.Fatalf("claim waited on the locked row instead of skipping it: %v", err)
	}
	if c.ID != free.ID || c.Status != job.Processing || c.Attempts != 1 || string(c.Audio) != "audio" {
		t.Errorf("claimed %s status=%s attempts=%d audio=%q; want %s processing 1 \"audio\"", c.ID, c.Status, c.Attempts, c.Audio, free.ID)
	}
}

func TestConcurrentClaimersGetDifferentJobs(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 3)
	const n = 20
	for range n {
		create(t, s, "")
	}

	var (
		mu   sync.Mutex
		seen = map[uuid.UUID]int{}
		wg   sync.WaitGroup
	)
	for range n {
		wg.Go(func() {
			c, err := s.Claim(ctx, lease)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			seen[c.ID]++
			mu.Unlock()
		})
	}
	wg.Wait()

	if len(seen) != n {
		t.Errorf("%d claimers got %d distinct jobs, want %d", n, len(seen), n)
	}
	if _, err := s.Claim(ctx, lease); !errors.Is(err, job.ErrNoJob) {
		t.Errorf("claim on drained queue: err = %v, want ErrNoJob", err)
	}
}

func TestExpiredLeaseIsReclaimedAndFencesTheOldWorker(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 3)
	j := create(t, s, "")

	first, err := s.Claim(ctx, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	second, err := s.Claim(ctx, lease)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if second.ID != j.ID || second.Attempts != 2 {
		t.Fatalf("reclaimed %s attempt %d, want %s attempt 2", second.ID, second.Attempts, j.ID)
	}

	if err := s.Succeed(ctx, first.Lease(), job.Delivery{Destination: "webhook"}); !errors.Is(err, job.ErrLeaseLost) {
		t.Errorf("old lease Succeed: err = %v, want ErrLeaseLost", err)
	}
	if err := s.Succeed(ctx, second.Lease(), job.Delivery{Destination: "webhook"}); err != nil {
		t.Errorf("current lease Succeed: %v", err)
	}
}

func TestRetryWaitsForBackoff(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 2)
	create(t, s, "")

	c, _ := s.Claim(ctx, lease)
	st, err := s.Retry(ctx, c.Lease(), job.Failure{Message: "openai: 503"}, time.Hour)
	if err != nil || st != job.Queued {
		t.Fatalf("retry: status %q err %v, want queued", st, err)
	}
	if _, err := s.Claim(ctx, lease); !errors.Is(err, job.ErrNoJob) {
		t.Fatalf("claim during backoff: err = %v, want ErrNoJob", err)
	}
	if _, err := s.Retry(ctx, c.Lease(), job.Failure{Message: "late"}, 0); !errors.Is(err, job.ErrLeaseLost) {
		t.Fatalf("retry on a job that is no longer processing: err = %v, want ErrLeaseLost", err)
	}
}

func TestRetryFailsAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 2)
	j := create(t, s, "")

	for attempt, want := range []job.Status{job.Queued, job.Failed} {
		c, err := s.Claim(ctx, lease)
		if err != nil {
			t.Fatalf("attempt %d claim: %v", attempt+1, err)
		}
		st, err := s.Retry(ctx, c.Lease(), job.Failure{Message: "openai: 503"}, 0)
		if err != nil || st != want {
			t.Fatalf("attempt %d retry: status %q err %v, want %q", attempt+1, st, err, want)
		}
	}

	got, err := s.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != job.Failed || got.Attempts != 2 || got.LastError != "openai: 503" {
		t.Errorf("got status=%s attempts=%d last_error=%q; want failed 2 \"openai: 503\"", got.Status, got.Attempts, got.LastError)
	}
}

func TestFailureStoresTheResponseBodyApart(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t, 3)
	j := create(t, s, "")
	stored := func() (msg, body string) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT last_error, last_error_body FROM jobs WHERE id = $1", j.ID).Scan(&msg, &body); err != nil {
			t.Fatal(err)
		}
		return msg, body
	}

	c, _ := s.Claim(ctx, lease)
	if _, err := s.Retry(ctx, c.Lease(), job.Failure{Message: "gemini: 503", ResponseBody: `{"error":"busy"}`}, 0); err != nil {
		t.Fatal(err)
	}
	if msg, body := stored(); msg != "gemini: 503" || body != `{"error":"busy"}` {
		t.Errorf("after retry: last_error=%q last_error_body=%q, want the message and the body apart", msg, body)
	}

	c, _ = s.Claim(ctx, lease)
	if _, err := s.Retry(ctx, c.Lease(), job.Failure{Message: "gemini: connection reset"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, body := stored(); body != "" {
		t.Errorf("after a failure without a body: last_error_body=%q, want empty", body)
	}

	c, _ = s.Claim(ctx, lease)
	if err := s.Fail(ctx, c.Lease(), job.Failure{Message: "model said \x00no", ResponseBody: "bad \xff byte"}); err != nil {
		t.Fatalf("fail with a NUL byte and invalid UTF-8: %v", err)
	}
	if msg, body := stored(); msg != "model said no" || body != "bad \uFFFD byte" {
		t.Errorf("after fail: last_error=%q last_error_body=%q, want both stored as clean text", msg, body)
	}
}

func TestSucceedClearsTheError(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t, 3)
	j := create(t, s, "")

	c, _ := s.Claim(ctx, lease)
	if _, err := s.Retry(ctx, c.Lease(), job.Failure{Message: "gemini: 503", ResponseBody: `{"error":"busy"}`}, 0); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Claim(ctx, lease)
	if err := s.Succeed(ctx, c.Lease(), job.Delivery{Destination: "webhook"}); err != nil {
		t.Fatal(err)
	}
	var msg, body string
	if err := pool.QueryRow(ctx, "SELECT last_error, last_error_body FROM jobs WHERE id = $1", j.ID).Scan(&msg, &body); err != nil {
		t.Fatal(err)
	}
	if msg != "" || body != "" {
		t.Errorf("after success: last_error=%q last_error_body=%q, want both empty", msg, body)
	}
}

func TestExpiredFinalAttemptFails(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 1)
	create(t, s, "")
	create(t, s, "")

	expired, err := s.Claim(ctx, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.Claim(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := s.Claim(ctx, lease); !errors.Is(err, job.ErrNoJob) {
		t.Fatalf("claim: err = %v, want ErrNoJob", err)
	}
	if n, err := s.FailExpired(ctx); n != 1 || err != nil {
		t.Fatalf("FailExpired = %d, %v; want 1, nil", n, err)
	}
	got, _ := s.Get(ctx, expired.ID)
	if got.Status != job.Failed || got.LastError != "lease expired on the final attempt" {
		t.Errorf("expired job: got %s %q, want failed with lease error", got.Status, got.LastError)
	}
	if got, _ := s.Get(ctx, live.ID); got.Status != job.Processing {
		t.Errorf("job with a live lease: got %s, want processing", got.Status)
	}
}

func TestResultSurvivesRetry(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 3)
	create(t, s, "")
	result := ai.Result{Transcript: "hello", Report: ai.VisitReport{Summary: "s", Sentiment: ai.Neutral}}

	c, _ := s.Claim(ctx, lease)
	if err := s.SaveResult(ctx, c.Lease(), result); err != nil {
		t.Fatalf("save result: %v", err)
	}
	if _, err := s.Retry(ctx, c.Lease(), job.Failure{Message: "hubspot: 502"}, 0); err != nil {
		t.Fatal(err)
	}

	c, _ = s.Claim(ctx, lease)
	if c.Result == nil || c.Result.Transcript != "hello" || c.Result.Report.Sentiment != ai.Neutral {
		t.Fatalf("result after retry = %+v, want the saved result", c.Result)
	}
}

func TestReleaseGivesAnExtraAttemptAndKeepsTheFence(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, 2)
	j := create(t, s, "")

	first, _ := s.Claim(ctx, lease)
	if _, err := s.Retry(ctx, first.Lease(), job.Failure{Message: "openai: 503"}, 0); err != nil {
		t.Fatal(err)
	}
	interrupted, _ := s.Claim(ctx, lease)
	if err := s.Release(ctx, interrupted.Lease()); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, _ := s.Get(ctx, j.ID)
	if got.Status != job.Queued || got.Attempts != 2 || got.MaxAttempts != 3 {
		t.Fatalf("after release: status=%s attempts=%d max=%d, want queued 2 3", got.Status, got.Attempts, got.MaxAttempts)
	}

	last, _ := s.Claim(ctx, lease)
	if last.Attempts != 3 {
		t.Fatalf("claim after release: attempt %d, want 3", last.Attempts)
	}
	if err := s.Fail(ctx, interrupted.Lease(), job.Failure{Message: "stale"}); !errors.Is(err, job.ErrLeaseLost) {
		t.Errorf("write with the released lease: err = %v, want ErrLeaseLost", err)
	}
	if st, err := s.Retry(ctx, last.Lease(), job.Failure{Message: "openai: 503"}, 0); err != nil || st != job.Failed {
		t.Errorf("second real failure: status %q err %v, want failed", st, err)
	}
}

func TestGetUnknownJob(t *testing.T) {
	s, _ := newStore(t, 3)
	if _, err := s.Get(context.Background(), uuid.New()); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Package worker claims jobs, analyzes the audio, delivers the report and records the outcome.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/job"
)

type (
	Store interface {
		Claim(ctx context.Context, lease time.Duration) (job.Claimed, error)
		FailExpired(ctx context.Context) (int64, error)
		SaveResult(ctx context.Context, l job.Lease, r ai.Result) error
		Succeed(ctx context.Context, l job.Lease, d job.Delivery) error
		Retry(ctx context.Context, l job.Lease, f job.Failure, delay time.Duration) (job.Status, error)
		Fail(ctx context.Context, l job.Lease, f job.Failure) error
		Release(ctx context.Context, l job.Lease) error
	}

	Sender interface {
		Send(ctx context.Context, id uuid.UUID, r ai.Result) (string, error)
	}

	Config struct {
		Workers        int
		Destination    string
		PollInterval   time.Duration
		Lease          time.Duration
		AnalyzeTimeout time.Duration
		DeliverTimeout time.Duration
		Grace          time.Duration
		RetryBase      time.Duration
	}

	Pool struct {
		store    Store
		provider ai.Provider
		sender   Sender
		cfg      Config
		log      *slog.Logger
	}

	outcome struct {
		level     slog.Level
		msg       string
		attrs     []slog.Attr
		recordErr error
	}

	permanentError interface {
		Permanent() bool
	}

	responseBodyError interface {
		ResponseBody() string
	}
)

const (
	dbTimeout    = 5 * time.Second
	maxBackoff   = 10 * time.Minute
	maxDoublings = 10
)

func New(store Store, provider ai.Provider, sender Sender, cfg Config, log *slog.Logger) *Pool {
	return &Pool{store: store, provider: provider, sender: sender, cfg: cfg, log: log}
}

func (p *Pool) Run(ctx context.Context) {
	jobCtx, cancelJobs := context.WithCancel(context.Background())
	defer cancelJobs()

	stopped := p.start(ctx, jobCtx)
	<-ctx.Done()
	p.awaitGrace(stopped, cancelJobs)
}

func (p *Pool) start(ctx, jobCtx context.Context) <-chan struct{} {
	var wg sync.WaitGroup
	wg.Go(func() { p.expireLoop(ctx) })
	for range p.cfg.Workers {
		wg.Go(func() { p.claimLoop(ctx, jobCtx) })
	}
	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()
	return stopped
}

func (p *Pool) awaitGrace(stopped <-chan struct{}, cancelJobs context.CancelFunc) {
	grace := time.NewTimer(p.cfg.Grace)
	defer grace.Stop()
	select {
	case <-stopped:
	case <-grace.C:
		cancelJobs()
		<-stopped
	}
}

func (p *Pool) claimLoop(ctx, jobCtx context.Context) {
	for {
		c, err := p.claim(ctx)
		switch {
		case err == nil:
			p.process(jobCtx, c)
			continue
		case ctx.Err() != nil:
			return
		case !errors.Is(err, job.ErrNoJob):
			p.log.ErrorContext(ctx, "claim job", slog.Any("error", err))
		}
		if !waitFor(ctx, time.After(p.cfg.PollInterval)) {
			return
		}
	}
}

// Runs once per lease, not once per poll, so an idle pool does not write on every poll.
func (p *Pool) expireLoop(ctx context.Context) {
	tick := time.NewTicker(p.cfg.Lease)
	defer tick.Stop()
	for {
		n, err := p.failExpired(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			p.log.ErrorContext(ctx, "fail expired jobs", slog.Any("error", err))
		case n > 0:
			p.log.WarnContext(ctx, "jobs failed after their last lease expired", slog.Int64("jobs", n))
		}
		if !waitFor(ctx, tick.C) {
			return
		}
	}
}

func (p *Pool) claim(ctx context.Context) (job.Claimed, error) {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return p.store.Claim(dctx, p.cfg.Lease)
}

func (p *Pool) failExpired(ctx context.Context) (int64, error) {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return p.store.FailExpired(dctx)
}

func (p *Pool) process(ctx context.Context, c job.Claimed) {
	start := time.Now()
	ref, err := p.pipeline(ctx, c)

	// The outcome must be recorded even when ctx was cancelled by shutdown.
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()

	var o outcome
	switch {
	case err == nil:
		o = p.succeed(dbCtx, c, ref, start)
	case errors.Is(err, job.ErrLeaseLost):
		o = leaseLost()
	case ctx.Err() != nil:
		o = p.release(dbCtx, c)
	case permanent(err):
		o = p.fail(dbCtx, c, err)
	default:
		o = p.retry(dbCtx, c, err)
	}
	p.logOutcome(dbCtx, c, o)
}

func (p *Pool) succeed(ctx context.Context, c job.Claimed, ref string, start time.Time) outcome {
	d := job.Delivery{Destination: p.cfg.Destination, Reference: ref, DeliveredAt: time.Now().UTC()}
	err := p.store.Succeed(ctx, c.Lease(), d)
	return outcome{
		level:     slog.LevelInfo,
		msg:       "job succeeded",
		attrs:     []slog.Attr{slog.Int64("duration_ms", time.Since(start).Milliseconds())},
		recordErr: err,
	}
}

func leaseLost() outcome {
	return outcome{
		level: slog.LevelWarn,
		msg:   "lease lost; analysis result discarded",
	}
}

func (p *Pool) release(ctx context.Context, c job.Claimed) outcome {
	err := p.store.Release(ctx, c.Lease())
	return outcome{
		level:     slog.LevelWarn,
		msg:       "job released on shutdown",
		recordErr: err,
	}
}

func (p *Pool) fail(ctx context.Context, c job.Claimed, cause error) outcome {
	err := p.store.Fail(ctx, c.Lease(), failure(cause))
	return outcome{
		level:     slog.LevelError,
		msg:       "job failed",
		attrs:     []slog.Attr{slog.Any("error", cause)},
		recordErr: err,
	}
}

func (p *Pool) retry(ctx context.Context, c job.Claimed, cause error) outcome {
	delay := backoff(p.cfg.RetryBase, c.Attempts)
	status, err := p.store.Retry(ctx, c.Lease(), failure(cause), delay)
	if status == job.Failed {
		return outcome{
			level:     slog.LevelError,
			msg:       "job failed after its last attempt",
			attrs:     []slog.Attr{slog.Any("error", cause)},
			recordErr: err,
		}
	}
	return outcome{
		level:     slog.LevelWarn,
		msg:       "job attempt failed",
		attrs:     []slog.Attr{slog.Any("error", cause), slog.String("retry_in", delay.String())},
		recordErr: err,
	}
}

func (p *Pool) logOutcome(ctx context.Context, c job.Claimed, o outcome) {
	log := p.log.With(slog.String("job_id", c.ID.String()), slog.Int("attempt", c.Attempts))
	if o.recordErr != nil {
		log.ErrorContext(ctx, "record outcome", slog.String("intended_outcome", o.msg), slog.Any("error", o.recordErr))
		return
	}
	log.LogAttrs(ctx, o.level, o.msg, o.attrs...)
}

func (p *Pool) pipeline(ctx context.Context, c job.Claimed) (string, error) {
	r, err := p.analyze(ctx, c)
	if err != nil {
		return "", err
	}
	return p.deliver(ctx, c.ID, r)
}

func (p *Pool) analyze(ctx context.Context, c job.Claimed) (ai.Result, error) {
	if c.Result != nil {
		return *c.Result, nil
	}
	actx, cancel := context.WithTimeout(ctx, p.cfg.AnalyzeTimeout)
	defer cancel()
	r, err := p.provider.Analyze(actx, ai.Audio{Data: c.Audio, Filename: c.Filename, ContentType: c.ContentType})
	if err != nil {
		return ai.Result{}, err
	}
	if err := p.saveResult(ctx, c.Lease(), r); err != nil {
		return ai.Result{}, err
	}
	return r, nil
}

func (p *Pool) saveResult(ctx context.Context, l job.Lease, r ai.Result) error {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := p.store.SaveResult(dctx, l, r); err != nil {
		return fmt.Errorf("save result: %w", err)
	}
	return nil
}

func (p *Pool) deliver(ctx context.Context, id uuid.UUID, r ai.Result) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, p.cfg.DeliverTimeout)
	defer cancel()
	return p.sender.Send(dctx, id, r)
}

func waitFor(ctx context.Context, next <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-next:
		return true
	}
}

func permanent(err error) bool {
	var p permanentError
	return errors.As(err, &p) && p.Permanent()
}

func failure(err error) job.Failure {
	f := job.Failure{Message: err.Error()}
	var b responseBodyError
	if errors.As(err, &b) {
		f.ResponseBody = b.ResponseBody()
	}
	return f
}

func backoff(base time.Duration, attempt int) time.Duration {
	return min(base<<min(attempt-1, maxDoublings), maxBackoff)
}

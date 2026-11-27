package worker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/httpjson"
	"github.com/Ishoshot/debrief/internal/job"
	"github.com/Ishoshot/debrief/internal/worker"
)

var result = ai.Result{Transcript: "Visited Bergmann.", Report: ai.VisitReport{Summary: "Good visit.", Sentiment: ai.Positive}}

type fakeStore struct {
	mu          sync.Mutex
	queue       []job.Claimed
	events      []string
	done        chan struct{}
	polls       int
	reaps       int
	reaped      chan struct{}
	noDeadlines int
}

func newStore(jobs ...job.Claimed) *fakeStore {
	return &fakeStore{queue: jobs, done: make(chan struct{}), reaped: make(chan struct{}, 1)}
}

func (f *fakeStore) record(event string, final bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	if final {
		close(f.done)
	}
}

func (f *fakeStore) Claim(ctx context.Context, _ time.Duration) (job.Claimed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countNoDeadline(ctx)
	if err := ctx.Err(); err != nil {
		return job.Claimed{}, err
	}
	if len(f.queue) == 0 {
		f.polls++
		return job.Claimed{}, job.ErrNoJob
	}
	c := f.queue[0]
	f.queue = f.queue[1:]
	return c, nil
}

func (f *fakeStore) FailExpired(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countNoDeadline(ctx)
	f.reaps++
	select {
	case f.reaped <- struct{}{}:
	default:
	}
	return 0, nil
}

func (f *fakeStore) countNoDeadline(ctx context.Context) {
	if _, ok := ctx.Deadline(); !ok {
		f.noDeadlines++
	}
}

func (f *fakeStore) SaveResult(_ context.Context, l job.Lease, r ai.Result) error {
	f.record(fmt.Sprintf("save attempt %d: %s", l.Attempt, r.Transcript), false)
	return nil
}

func (f *fakeStore) Succeed(_ context.Context, _ job.Lease, d job.Delivery) error {
	f.record(fmt.Sprintf("succeed via %s ref %q", d.Destination, d.Reference), true)
	return nil
}

func (f *fakeStore) Retry(_ context.Context, _ job.Lease, fl job.Failure, delay time.Duration) (job.Status, error) {
	f.record(fmt.Sprintf("retry in %s: %s", delay, describe(fl)), true)
	return job.Queued, nil
}

func (f *fakeStore) Fail(_ context.Context, _ job.Lease, fl job.Failure) error {
	f.record("fail: "+describe(fl), true)
	return nil
}

func (f *fakeStore) Release(context.Context, job.Lease) error {
	f.record("release", true)
	return nil
}

func describe(fl job.Failure) string {
	if fl.ResponseBody == "" {
		return fl.Message
	}
	return fmt.Sprintf("%s [body %s]", fl.Message, fl.ResponseBody)
}

type providerFunc func(ctx context.Context, a ai.Audio) (ai.Result, error)

func (f providerFunc) Analyze(ctx context.Context, a ai.Audio) (ai.Result, error) { return f(ctx, a) }

type senderFunc func(ctx context.Context, id uuid.UUID, r ai.Result) (string, error)

func (f senderFunc) Send(ctx context.Context, id uuid.UUID, r ai.Result) (string, error) {
	return f(ctx, id, r)
}

func okSender(context.Context, uuid.UUID, ai.Result) (string, error) { return "note-1", nil }

func claimed(attempt int, saved *ai.Result) job.Claimed {
	return job.Claimed{
		Job:   job.Job{ID: uuid.New(), Status: job.Processing, Filename: "a.m4a", ContentType: "audio/m4a", Attempts: attempt, Result: saved},
		Audio: []byte("audio"),
	}
}

func config() worker.Config {
	return worker.Config{
		Workers: 2, Destination: "hubspot", PollInterval: 5 * time.Millisecond, Lease: time.Minute,
		AnalyzeTimeout: time.Second, DeliverTimeout: time.Second, Grace: time.Second, RetryBase: 10 * time.Second,
	}
}

func runUntilDone(t *testing.T, store *fakeStore, p ai.Provider, s worker.Sender, cfg worker.Config, log *slog.Logger) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		worker.New(store, p, s, cfg, log).Run(ctx)
		close(stopped)
	}()
	select {
	case <-store.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no outcome recorded")
	}
	cancel()
	<-stopped
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.events
}

func TestPipeline(t *testing.T) {
	saved := result
	tests := []struct {
		name    string
		job     job.Claimed
		analyze providerFunc
		send    senderFunc
		want    []string
	}{
		{
			name:    "analyzes, saves, delivers",
			job:     claimed(1, nil),
			analyze: func(context.Context, ai.Audio) (ai.Result, error) { return result, nil },
			send:    okSender,
			want:    []string{"save attempt 1: Visited Bergmann.", `succeed via hubspot ref "note-1"`},
		},
		{
			name: "saved result skips the model",
			job:  claimed(2, &saved),
			analyze: func(context.Context, ai.Audio) (ai.Result, error) {
				return ai.Result{}, errors.New("model called again")
			},
			send: okSender,
			want: []string{`succeed via hubspot ref "note-1"`},
		},
		{
			name: "transient model error retries with backoff",
			job:  claimed(3, nil),
			analyze: func(context.Context, ai.Audio) (ai.Result, error) {
				return ai.Result{}, fmt.Errorf("openai transcribe: %w", &httpjson.StatusError{Code: 503})
			},
			send: okSender,
			want: []string{"retry in 40s: openai transcribe: unexpected status 503 Service Unavailable"},
		},
		{
			name:    "delivery failure keeps the analysis for the retry",
			job:     claimed(1, nil),
			analyze: func(context.Context, ai.Audio) (ai.Result, error) { return result, nil },
			send: func(context.Context, uuid.UUID, ai.Result) (string, error) {
				return "", errors.New("hubspot: connection reset")
			},
			want: []string{"save attempt 1: Visited Bergmann.", "retry in 10s: hubspot: connection reset"},
		},
		{
			name: "permanent error fails without retry",
			job:  claimed(1, nil),
			analyze: func(context.Context, ai.Audio) (ai.Result, error) {
				return ai.Result{}, fmt.Errorf("gemini: %w", &httpjson.StatusError{Code: 401})
			},
			send: okSender,
			want: []string{"fail: gemini: unexpected status 401 Unauthorized"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runUntilDone(t, newStore(tt.job), tt.analyze, tt.send, config(), slog.New(slog.DiscardHandler))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("events (-want +got):\n%s", diff)
			}
		})
	}
}

func TestVendorReplyBodyIsStoredButNeverLogged(t *testing.T) {
	const reply = `{"error":{"status":"UNAVAILABLE","message":"Visited Bergmann today"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	vendor := providerFunc(func(ctx context.Context, _ ai.Audio) (ai.Result, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, http.NoBody)
		return ai.Result{}, fmt.Errorf("gemini: %w", httpjson.Do(srv.Client(), req, nil))
	})
	var logs bytes.Buffer

	got := runUntilDone(t, newStore(claimed(1, nil)), vendor, senderFunc(okSender), config(), slog.New(slog.NewJSONHandler(&logs, nil)))

	want := []string{"retry in 10s: gemini: unexpected status 503 Service Unavailable (UNAVAILABLE) [body " + reply + "]"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	if !strings.Contains(logs.String(), "(UNAVAILABLE)") || strings.Contains(logs.String(), "Bergmann") {
		t.Errorf("logs = %s; want the vendor code without the reply body", logs.String())
	}
}

func TestShutdownLetsInFlightJobFinish(t *testing.T) {
	store := newStore(claimed(1, nil))
	started := make(chan struct{})
	slow := providerFunc(func(context.Context, ai.Audio) (ai.Result, error) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		return result, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	worker.New(store, slow, senderFunc(okSender), config(), slog.New(slog.DiscardHandler)).Run(ctx)

	want := []string{"save attempt 1: Visited Bergmann.", `succeed via hubspot ref "note-1"`}
	if diff := cmp.Diff(want, store.events); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestShutdownReleasesJobAfterGrace(t *testing.T) {
	store := newStore(claimed(1, nil))
	started := make(chan struct{})
	stuck := providerFunc(func(ctx context.Context, _ ai.Audio) (ai.Result, error) {
		close(started)
		<-ctx.Done()
		return ai.Result{}, ctx.Err()
	})
	cfg := config()
	cfg.Grace = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	begin := time.Now()
	worker.New(store, stuck, senderFunc(okSender), cfg, slog.New(slog.DiscardHandler)).Run(ctx)

	if diff := cmp.Diff([]string{"release"}, store.events); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	if took := time.Since(begin); took > time.Second {
		t.Errorf("Run took %s after a %s grace period", took, cfg.Grace)
	}
}

func TestExpiredLeasesAreFailedOncePerLeaseNotPerPoll(t *testing.T) {
	store := newStore()
	cfg := config()
	cfg.PollInterval = time.Millisecond
	cfg.Lease = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	worker.New(store, nil, nil, cfg, slog.New(slog.DiscardHandler)).Run(ctx)

	if store.noDeadlines != 0 {
		t.Errorf("%d Claim or FailExpired calls had no deadline", store.noDeadlines)
	}
	if store.reaps != 1 || store.polls < 10 {
		t.Errorf("after 50ms with a 1ms poll and a 1h lease: reaps=%d polls=%d; want 1 reap and at least 10 polls", store.reaps, store.polls)
	}
}

func TestExpiredLeasesAreFailedAgainEachLease(t *testing.T) {
	store := newStore()
	cfg := config()
	cfg.Lease = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		worker.New(store, nil, nil, cfg, slog.New(slog.DiscardHandler)).Run(ctx)
		close(stopped)
	}()
	defer func() { cancel(); <-stopped }()

	for i := range 3 {
		select {
		case <-store.reaped:
		case <-time.After(5 * time.Second):
			t.Fatalf("reap %d of 3 did not happen with a 10ms lease", i+1)
		}
	}
}

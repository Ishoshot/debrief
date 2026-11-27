// Command server runs the recording API and the worker pool in one process.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/ai/gemini"
	"github.com/Ishoshot/debrief/internal/ai/openai"
	"github.com/Ishoshot/debrief/internal/config"
	"github.com/Ishoshot/debrief/internal/deliver/hubspot"
	"github.com/Ishoshot/debrief/internal/deliver/webhook"
	"github.com/Ishoshot/debrief/internal/httpapi"
	"github.com/Ishoshot/debrief/internal/httpjson"
	"github.com/Ishoshot/debrief/internal/job"
	"github.com/Ishoshot/debrief/internal/worker"
)

const (
	startupTimeout    = 30 * time.Second
	vendorTimeout     = 2 * time.Minute
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
	pollInterval      = time.Second
	leaseDuration     = 5 * time.Minute
	analyzeTimeout    = 3 * time.Minute
	deliverTimeout    = 30 * time.Second
	shutdownGrace     = 15 * time.Second
	retryBase         = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	log := slog.New(slog.NewJSONHandler(stdout, nil))
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("config:\n%w", err)
	}
	pool, err := connect(ctx, cfg.DatabaseURL, startupTimeout)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := job.NewStore(pool, cfg.MaxAttempts)

	httpClient := httpjson.NewClient(vendorTimeout)
	var (
		provider ai.Provider
		models   []string
	)
	switch cfg.AIProvider {
	case config.OpenAI:
		models = []string{cfg.OpenAITranscribeModel, cfg.OpenAIReportModel}
		provider = openai.New(cfg.OpenAIKey, cfg.OpenAITranscribeModel, cfg.OpenAIReportModel,
			cmp.Or(cfg.OpenAIBaseURL, openai.BaseURL), httpClient)
	case config.Gemini:
		models = []string{cfg.GeminiModel}
		provider = gemini.New(cfg.GeminiKey, cfg.GeminiModel, cmp.Or(cfg.GeminiBaseURL, gemini.BaseURL), httpClient)
	}
	var sender worker.Sender
	switch cfg.Destination {
	case config.Webhook:
		sender = webhook.New(cfg.WebhookURL, httpClient)
	case config.HubSpot:
		sender = hubspot.New(cfg.HubSpotToken, hubspot.BaseURL, httpClient)
	}

	workers := worker.New(store, provider, sender, workerConfig(cfg), log)
	srv := newServer(cfg.Port, store, log)
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info("server started", slog.String("addr", srv.Addr), slog.String("ai_provider", string(cfg.AIProvider)),
		slog.Any("models", models), slog.String("destination", string(cfg.Destination)), slog.Int("workers", cfg.Workers))

	err = serve(ctx, srv, ln, workers, log)
	log.Info("stopped")
	return err
}

func connect(ctx context.Context, databaseURL string, timeout time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := job.CreateSchema(sctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func workerConfig(cfg config.Config) worker.Config {
	return worker.Config{
		Workers:        cfg.Workers,
		Destination:    string(cfg.Destination),
		PollInterval:   pollInterval,
		Lease:          leaseDuration,
		AnalyzeTimeout: analyzeTimeout,
		DeliverTimeout: deliverTimeout,
		Grace:          shutdownGrace,
		RetryBase:      retryBase,
	}
}

func newServer(port string, store *job.Store, log *slog.Logger) *http.Server {
	gin.SetMode(gin.ReleaseMode)
	return &http.Server{
		Addr:              ":" + port,
		Handler:           httpapi.NewRouter(store, log),
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener, workers *worker.Pool, log *slog.Logger) error {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		workers.Run(gctx)
		log.Info("workers stopped")
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}

// Package httpapi accepts recordings, queues them as jobs and reports job status.
package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/job"
)

type (
	Store interface {
		Create(ctx context.Context, n job.New) (job.Job, bool, error)
		Get(ctx context.Context, id uuid.UUID) (job.Job, error)
		Ping(ctx context.Context) error
	}

	handler struct {
		store Store
		log   *slog.Logger
	}

	response struct {
		ID          uuid.UUID     `json:"id"`
		Status      job.Status    `json:"status"`
		Filename    string        `json:"filename"`
		Attempts    int           `json:"attempts"`
		MaxAttempts int           `json:"max_attempts"`
		LastError   string        `json:"last_error,omitempty"`
		Result      *ai.Result    `json:"result,omitempty"`
		Delivery    *job.Delivery `json:"delivery,omitempty"`
		CreatedAt   time.Time     `json:"created_at"`
		UpdatedAt   time.Time     `json:"updated_at"`
	}

	rejection struct {
		status int
		msg    string
	}
)

const (
	maxAudioBytes    = 25_000_000            // the OpenAI transcription upload limit
	maxBodyBytes     = maxAudioBytes + 1<<20 // Gin's MaxMultipartMemory caps memory use, not the body
	maxKeyBytes      = 255
	maxFilenameBytes = 100
	dbTimeout        = 10 * time.Second // an upload inserts up to 25 MB of audio
	healthTimeout    = 2 * time.Second
)

var (
	// The formats both OpenAI and Gemini accept, with the MIME type they expect.
	audioTypes = map[string]string{
		".flac": "audio/flac",
		".m4a":  "audio/m4a",
		".mp3":  "audio/mp3",
		".mpeg": "audio/mpeg",
		".mpga": "audio/mpeg",
		".ogg":  "audio/ogg",
		".wav":  "audio/wav",
		".webm": "audio/webm",
	}

	keyTooLong       = &rejection{http.StatusBadRequest, "Idempotency-Key must be at most 255 bytes"}
	audioTooLarge    = &rejection{http.StatusRequestEntityTooLarge, "audio must be at most 25 MB"}
	noAudioField     = &rejection{http.StatusBadRequest, "send the recording as multipart form field \"audio\""}
	unsupportedAudio = &rejection{http.StatusUnsupportedMediaType, "the audio file name must end in .flac, .m4a, .mp3, .mpeg, .mpga, .ogg, .wav or .webm"}
	unreadableAudio  = &rejection{http.StatusBadRequest, "could not read the audio upload"}
	emptyAudio       = &rejection{http.StatusBadRequest, "the audio file is empty"}
)

func NewRouter(store Store, log *slog.Logger) *gin.Engine {
	h := &handler{store: store, log: log}
	r := gin.New()
	r.Use(gin.Recovery(), requestLog(log))
	r.POST("/v1/recordings", h.upload)
	r.GET("/v1/recordings/:id", h.get)
	r.GET("/healthz", h.health)
	return r
}

func (h *handler) upload(c *gin.Context) {
	n, rejected := readUpload(c)
	if rejected != nil {
		fail(c, rejected.status, rejected.msg)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), dbTimeout)
	defer cancel()
	j, created, err := h.store.Create(ctx, n)
	if errors.Is(err, job.ErrKeyReused) {
		fail(c, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "create job", slog.Any("error", err))
		fail(c, http.StatusInternalServerError, "could not queue the recording")
		return
	}
	c.Header("Location", "/v1/recordings/"+j.ID.String())
	c.JSON(uploadStatus(created), toResponse(j))
}

func (h *handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		fail(c, http.StatusNotFound, "recording not found")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), dbTimeout)
	defer cancel()
	j, err := h.store.Get(ctx, id)
	if errors.Is(err, job.ErrNotFound) {
		fail(c, http.StatusNotFound, "recording not found")
		return
	}
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "get job", slog.Any("error", err))
		fail(c, http.StatusInternalServerError, "could not load the recording")
		return
	}
	c.JSON(http.StatusOK, toResponse(j))
}

func (h *handler) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), healthTimeout)
	defer cancel()
	if err := h.store.Ping(ctx); err != nil {
		h.log.WarnContext(ctx, "health check", slog.Any("error", err))
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "database unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func readUpload(c *gin.Context) (job.New, *rejection) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	key := c.GetHeader("Idempotency-Key")
	if len(key) > maxKeyBytes {
		return job.New{}, keyTooLong
	}
	fh, rejected := audioFile(c)
	if rejected != nil {
		return job.New{}, rejected
	}
	contentType, ok := audioTypes[strings.ToLower(filepath.Ext(fh.Filename))]
	if !ok {
		return job.New{}, unsupportedAudio
	}
	data, rejected := readAudio(fh)
	if rejected != nil {
		return job.New{}, rejected
	}
	return job.New{IdempotencyKey: key, Filename: cleanFilename(fh.Filename), ContentType: contentType, Audio: data}, nil
}

func audioFile(c *gin.Context) (*multipart.FileHeader, *rejection) {
	fh, err := c.FormFile("audio")
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return nil, audioTooLarge
	}
	if err != nil {
		return nil, noAudioField
	}
	if fh.Size > maxAudioBytes {
		return nil, audioTooLarge
	}
	return fh, nil
}

func readAudio(fh *multipart.FileHeader) ([]byte, *rejection) {
	data, err := readAll(fh)
	if err != nil {
		return nil, unreadableAudio
	}
	if len(data) == 0 {
		return nil, emptyAudio
	}
	return data, nil
}

func readAll(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

func cleanFilename(name string) string {
	clean := strings.Map(safeFilenameRune, path.Base(strings.ReplaceAll(name, `\`, "/")))
	if len(clean) > maxFilenameBytes {
		clean = clean[len(clean)-maxFilenameBytes:]
	}
	return clean
}

func safeFilenameRune(r rune) rune {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		return r
	}
	return '_'
}

func uploadStatus(created bool) int {
	if created {
		return http.StatusAccepted
	}
	return http.StatusOK
}

func toResponse(j job.Job) response {
	return response{
		ID:          j.ID,
		Status:      j.Status,
		Filename:    j.Filename,
		Attempts:    j.Attempts,
		MaxAttempts: j.MaxAttempts,
		LastError:   j.LastError,
		Result:      j.Result,
		Delivery:    j.Delivery,
		CreatedAt:   j.CreatedAt,
		UpdatedAt:   j.UpdatedAt,
	}
}

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

func requestLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.InfoContext(c.Request.Context(), "http request",
			slog.String("method", c.Request.Method),
			slog.String("route", c.FullPath()),
			slog.Int("status", c.Writer.Status()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	}
}

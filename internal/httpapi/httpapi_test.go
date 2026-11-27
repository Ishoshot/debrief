package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/httpapi"
	"github.com/Ishoshot/debrief/internal/job"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

var (
	fixedID   = uuid.MustParse("7d3c1b52-3f1e-4b3a-9d6f-0a1b2c3d4e5f")
	fixedTime = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	m4a       = []byte("m4a audio")
)

type fakeStore struct {
	created     []job.New
	existing    bool
	err         error
	jobs        map[uuid.UUID]job.Job
	pingErr     error
	noDeadlines int
}

func (f *fakeStore) Create(ctx context.Context, n job.New) (job.Job, bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		f.noDeadlines++
	}
	if f.err != nil {
		return job.Job{}, false, f.err
	}
	f.created = append(f.created, n)
	return job.Job{ID: fixedID, Status: job.Queued, Filename: n.Filename, MaxAttempts: 5, CreatedAt: fixedTime, UpdatedAt: fixedTime}, !f.existing, nil
}

func (f *fakeStore) Get(ctx context.Context, id uuid.UUID) (job.Job, error) {
	if _, ok := ctx.Deadline(); !ok {
		f.noDeadlines++
	}
	j, ok := f.jobs[id]
	if !ok {
		return job.Job{}, job.ErrNotFound
	}
	return j, nil
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func multipartBody(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func serve(store httpapi.Store, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	httpapi.NewRouter(store, slog.New(slog.DiscardHandler)).ServeHTTP(rec, req)
	return rec
}

func TestUploadQueuesJob(t *testing.T) {
	store := &fakeStore{}
	body, ct := multipartBody(t, "audio", "../Visit Bergmann (1).M4A", m4a)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/recordings", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Idempotency-Key", "visit-42")

	rec := serve(store, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("Location"), "/v1/recordings/"+fixedID.String(); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	wantBody := `{"id":"7d3c1b52-3f1e-4b3a-9d6f-0a1b2c3d4e5f","status":"queued","filename":"Visit_Bergmann__1_.M4A","attempts":0,"max_attempts":5,"created_at":"2026-11-25T10:00:00Z","updated_at":"2026-11-25T10:00:00Z"}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body:\n got %s\nwant %s", got, wantBody)
	}
	want := []job.New{{IdempotencyKey: "visit-42", Filename: "Visit_Bergmann__1_.M4A", ContentType: "audio/m4a", Audio: m4a}}
	if diff := cmp.Diff(want, store.created); diff != "" {
		t.Errorf("stored job mismatch (-want +got):\n%s", diff)
	}
	if store.noDeadlines != 0 {
		t.Error("Create got a context without a deadline")
	}
}

func TestUploadStatusCodes(t *testing.T) {
	tooBig := make([]byte, 25_000_001)
	pastBodyCap := make([]byte, 27<<20)
	tests := []struct {
		name     string
		field    string
		filename string
		data     []byte
		store    *fakeStore
		want     int
		wantBody string
	}{
		{"same key returns the existing job", "audio", "a.m4a", m4a, &fakeStore{existing: true}, http.StatusOK, ""},
		{"key reused with other audio", "audio", "a.m4a", m4a, &fakeStore{err: job.ErrKeyReused}, http.StatusUnprocessableEntity, `{"error":"idempotency key reused with different audio"}`},
		{"store down", "audio", "a.m4a", m4a, &fakeStore{err: errors.New("connection refused")}, http.StatusInternalServerError, `{"error":"could not queue the recording"}`},
		{"wrong field", "file", "a.m4a", m4a, &fakeStore{}, http.StatusBadRequest, `{"error":"send the recording as multipart form field \"audio\""}`},
		{"unsupported extension", "audio", "a.txt", m4a, &fakeStore{}, http.StatusUnsupportedMediaType, `{"error":"the audio file name must end in .flac, .m4a, .mp3, .mpeg, .mpga, .ogg, .wav or .webm"}`},
		{"extension not in the allowlist", "audio", "a.mp4", m4a, &fakeStore{}, http.StatusUnsupportedMediaType, `{"error":"the audio file name must end in .flac, .m4a, .mp3, .mpeg, .mpga, .ogg, .wav or .webm"}`},
		{"empty file", "audio", "a.mp3", nil, &fakeStore{}, http.StatusBadRequest, `{"error":"the audio file is empty"}`},
		{"over 25 MB", "audio", "a.wav", tooBig, &fakeStore{}, http.StatusRequestEntityTooLarge, `{"error":"audio must be at most 25 MB"}`},
		{"request body over the cap", "audio", "a.wav", pastBodyCap, &fakeStore{}, http.StatusRequestEntityTooLarge, `{"error":"audio must be at most 25 MB"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, ct := multipartBody(t, tt.field, tt.filename, tt.data)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/recordings", body)
			req.Header.Set("Content-Type", ct)
			rec := serve(tt.store, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %s, want %s", rec.Body, tt.wantBody)
			}
		})
	}
}

func TestUploadRejectsLongIdempotencyKey(t *testing.T) {
	body, ct := multipartBody(t, "audio", "a.m4a", m4a)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/recordings", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Idempotency-Key", strings.Repeat("k", 256))
	rec := serve(&fakeStore{}, req)

	want := `{"error":"Idempotency-Key must be at most 255 bytes"}`
	if rec.Code != http.StatusBadRequest || rec.Body.String() != want {
		t.Errorf("got %d %s, want 400 %s", rec.Code, rec.Body, want)
	}
}

func TestGet(t *testing.T) {
	done := job.Job{
		ID: fixedID, Status: job.Succeeded, Filename: "a.m4a", Attempts: 1, MaxAttempts: 5,
		Result: &ai.Result{Transcript: "Visited Bergmann.", Report: ai.VisitReport{
			Customer: "Bergmann Maschinenbau", Summary: "Good visit.", Sentiment: ai.Positive,
		}},
		Delivery:  &job.Delivery{Destination: "webhook", DeliveredAt: fixedTime},
		CreatedAt: fixedTime, UpdatedAt: fixedTime,
	}
	store := &fakeStore{jobs: map[uuid.UUID]job.Job{fixedID: done}}

	tests := []struct {
		path     string
		want     int
		wantBody string
	}{
		{"/v1/recordings/" + fixedID.String(), http.StatusOK, `{"id":"7d3c1b52-3f1e-4b3a-9d6f-0a1b2c3d4e5f","status":"succeeded","filename":"a.m4a","attempts":1,"max_attempts":5,"result":{"transcript":"Visited Bergmann.","report":{"customer":"Bergmann Maschinenbau","contacts":null,"summary":"Good visit.","key_points":null,"next_steps":null,"sentiment":"positive","products":null}},"delivery":{"destination":"webhook","delivered_at":"2026-11-25T10:00:00Z"},"created_at":"2026-11-25T10:00:00Z","updated_at":"2026-11-25T10:00:00Z"}`},
		{"/v1/recordings/" + uuid.NewString(), http.StatusNotFound, `{"error":"recording not found"}`},
		{"/v1/recordings/not-a-uuid", http.StatusNotFound, `{"error":"recording not found"}`},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := serve(store, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))
			if rec.Code != tt.want || rec.Body.String() != tt.wantBody {
				t.Errorf("got %d %s\nwant %d %s", rec.Code, rec.Body, tt.want, tt.wantBody)
			}
		})
	}
	if store.noDeadlines != 0 {
		t.Errorf("Get got a context without a deadline %d times", store.noDeadlines)
	}
}

func TestHealth(t *testing.T) {
	for _, tt := range []struct {
		pingErr  error
		want     int
		wantBody string
	}{
		{nil, http.StatusOK, `{"status":"ok"}`},
		{errors.New("down"), http.StatusServiceUnavailable, `{"status":"database unavailable"}`},
	} {
		rec := serve(&fakeStore{pingErr: tt.pingErr}, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
		if rec.Code != tt.want || rec.Body.String() != tt.wantBody {
			t.Errorf("ping err %v: got %d %s, want %d %s", tt.pingErr, rec.Code, rec.Body, tt.want, tt.wantBody)
		}
	}
}

package webhook_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/deliver/webhook"
)

func TestSend(t *testing.T) {
	id := uuid.MustParse("7d3c1b52-3f1e-4b3a-9d6f-0a1b2c3d4e5f")
	var gotKey, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotType = r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	result := ai.Result{Transcript: "Visited Bergmann.", Report: ai.VisitReport{
		Customer: "Bergmann Maschinenbau", Summary: "Good visit.", NextSteps: []string{"Send quote"}, Sentiment: ai.Positive,
	}}
	ref, err := webhook.New(srv.URL, srv.Client()).Send(context.Background(), id, result)
	if err != nil || ref != "" {
		t.Fatalf("Send = %q, %v; want \"\", nil", ref, err)
	}
	if gotKey != id.String() || gotType != "application/json" {
		t.Errorf("headers: Idempotency-Key=%q Content-Type=%q", gotKey, gotType)
	}
	want := `{"id":"7d3c1b52-3f1e-4b3a-9d6f-0a1b2c3d4e5f","transcript":"Visited Bergmann.","report":{"customer":"Bergmann Maschinenbau","contacts":null,"summary":"Good visit.","key_points":null,"next_steps":["Send quote"],"sentiment":"positive","products":null}}`
	if gotBody != want {
		t.Errorf("body:\n got %s\nwant %s", gotBody, want)
	}
}

func TestSendReportsReceiverFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := webhook.New(srv.URL, srv.Client()).Send(context.Background(), uuid.New(), ai.Result{})
	if err == nil || err.Error() != "webhook: unexpected status 502 Bad Gateway" {
		t.Errorf("err = %v, want webhook 502", err)
	}
}

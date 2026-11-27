// Package webhook delivers a visit report as a JSON POST.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/httpjson"
)

type (
	Sender struct {
		url  string
		http *http.Client
	}

	payload struct {
		ID         uuid.UUID      `json:"id"`
		Transcript string         `json:"transcript"`
		Report     ai.VisitReport `json:"report"`
	}
)

func New(url string, hc *http.Client) *Sender {
	return &Sender{url: url, http: hc}
}

// A retry after a lost response repeats the POST; the job ID in Idempotency-Key lets the receiver drop it.
func (s *Sender) Send(ctx context.Context, id uuid.UUID, r ai.Result) (string, error) {
	body, err := json.Marshal(payload{ID: id, Transcript: r.Transcript, Report: r.Report})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", id.String())
	if err := httpjson.Do(s.http, req, nil); err != nil {
		return "", fmt.Errorf("webhook: %w", err)
	}
	return "", nil
}

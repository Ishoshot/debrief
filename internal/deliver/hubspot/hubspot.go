// Package hubspot delivers a visit report as a HubSpot CRM note.
package hubspot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/httpjson"
)

type (
	Sender struct {
		token   string
		baseURL string
		http    *http.Client
	}

	object struct {
		ID string `json:"id"`
	}

	associationType struct {
		Category string `json:"associationCategory"`
		TypeID   int    `json:"associationTypeId"`
	}

	association struct {
		To    object            `json:"to"`
		Types []associationType `json:"types"`
	}

	note struct {
		Properties   map[string]string `json:"properties"`
		Associations []association     `json:"associations"`
	}
)

const (
	BaseURL         = "https://api.hubapi.com"
	maxNoteBody     = 65536 // HubSpot's limit is 65,536 characters; counting bytes is stricter
	noteToCompany   = 190
	transcriptOpen  = "<p><strong>Transcript</strong></p><p>"
	transcriptClose = "</p>"
	cutMarker       = " [...]"
)

func New(token, baseURL string, hc *http.Client) *Sender {
	return &Sender{token: token, baseURL: strings.TrimSuffix(baseURL, "/"), http: hc}
}

// HubSpot has no idempotency key, so a retry after a lost response can create a second note.
func (s *Sender) Send(ctx context.Context, id uuid.UUID, r ai.Result) (string, error) {
	r.Report.Customer = strings.TrimSpace(r.Report.Customer)
	associations, err := s.companyAssociations(ctx, r.Report.Customer)
	if err != nil {
		return "", fmt.Errorf("hubspot: %w", err)
	}
	noteID, err := s.createNote(ctx, noteBody(id, r), associations)
	if err != nil {
		return "", fmt.Errorf("hubspot: create note: %w", err)
	}
	return noteID, nil
}

func (s *Sender) companyAssociations(ctx context.Context, customer string) ([]association, error) {
	if customer == "" {
		return []association{}, nil
	}
	companyID, err := s.findOrCreateCompany(ctx, customer)
	if err != nil {
		return nil, err
	}
	return []association{{
		To:    object{ID: companyID},
		Types: []associationType{{Category: "HUBSPOT_DEFINED", TypeID: noteToCompany}},
	}}, nil
}

func (s *Sender) findOrCreateCompany(ctx context.Context, name string) (string, error) {
	id, err := s.findCompany(ctx, name)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return s.createCompany(ctx, name)
}

func (s *Sender) findCompany(ctx context.Context, name string) (string, error) {
	filter := map[string]string{"propertyName": "name", "operator": "EQ", "value": name}
	search := map[string]any{
		"filterGroups": []map[string]any{{"filters": []map[string]string{filter}}},
		"properties":   []string{"name"},
		"limit":        1,
	}
	var found struct {
		Results []object `json:"results"`
	}
	if err := s.post(ctx, "/crm/v3/objects/companies/search", search, &found); err != nil {
		return "", fmt.Errorf("search company: %w", err)
	}
	if len(found.Results) == 0 {
		return "", nil
	}
	return found.Results[0].ID, nil
}

func (s *Sender) createCompany(ctx context.Context, name string) (string, error) {
	var created object
	company := map[string]any{"properties": map[string]string{"name": name}}
	if err := s.post(ctx, "/crm/v3/objects/companies", company, &created); err != nil {
		return "", fmt.Errorf("create company: %w", err)
	}
	return created.ID, nil
}

func (s *Sender) createNote(ctx context.Context, body string, associations []association) (string, error) {
	var created object
	n := note{
		Properties: map[string]string{
			"hs_timestamp": time.Now().UTC().Format(time.RFC3339),
			"hs_note_body": body,
		},
		Associations: associations,
	}
	if err := s.post(ctx, "/crm/v3/objects/notes", n, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (s *Sender) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	return httpjson.Do(s.http, req, out)
}

func noteBody(id uuid.UUID, r ai.Result) string {
	return fitNote(reportHTML(id, r.Report), html.EscapeString(r.Transcript))
}

func fitNote(report, transcript string) string {
	room := maxNoteBody - len(report) - len(transcriptOpen) - len(transcriptClose)
	switch {
	case len(transcript) <= room:
		return report + transcriptOpen + transcript + transcriptClose
	case room >= len(cutMarker):
		return report + transcriptOpen + cutAtSpace(transcript, room-len(cutMarker)) + cutMarker + transcriptClose
	default:
		return cutAtSpace(report, maxNoteBody-len(cutMarker)) + cutMarker
	}
}

func reportHTML(id uuid.UUID, rep ai.VisitReport) string {
	var b strings.Builder
	heading(&b, rep.Customer)
	fmt.Fprintf(&b, "<p>%s</p>", html.EscapeString(rep.Summary))
	field(&b, "Contacts", strings.Join(rep.Contacts, ", "))
	list(&b, "Key points", rep.KeyPoints)
	list(&b, "Next steps", rep.NextSteps)
	field(&b, "Follow-up", rep.FollowUpDate)
	field(&b, "Sentiment", string(rep.Sentiment))
	field(&b, "Products", strings.Join(rep.Products, ", "))
	field(&b, "Debrief job", id.String())
	return b.String()
}

func heading(b *strings.Builder, customer string) {
	if customer == "" {
		b.WriteString("<h3>Visit report</h3>")
		return
	}
	fmt.Fprintf(b, "<h3>Visit report: %s</h3>", html.EscapeString(customer))
}

func field(b *strings.Builder, label, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(b, "<p><strong>%s:</strong> %s</p>", label, html.EscapeString(value))
}

func list(b *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "<p><strong>%s</strong></p><ul>", label)
	for _, it := range items {
		fmt.Fprintf(b, "<li>%s</li>", html.EscapeString(it))
	}
	b.WriteString("</ul>")
}

// Cutting at a space never splits an HTML entity such as &amp; or a tag.
func cutAtSpace(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	return s[:max(strings.LastIndexByte(s, ' '), 0)]
}

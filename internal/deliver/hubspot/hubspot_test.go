package hubspot_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/deliver/hubspot"
	"github.com/Ishoshot/debrief/internal/httpjson"
)

var id = uuid.MustParse("7d3c1b52-3f1e-4b3a-9d6f-0a1b2c3d4e5f")

type request struct {
	Properties   map[string]string `json:"properties"`
	Associations []any             `json:"associations"`
}

type call struct {
	Path string
	Body string
}

func fakeHubSpot(t *testing.T, found []string) (*httptest.Server, *[]call) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer pat-test" {
			t.Errorf("request %s %s with Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.URL.Path, string(body)})
		switch r.URL.Path {
		case "/crm/v3/objects/companies/search":
			results := make([]map[string]string, len(found))
			for i, id := range found {
				results[i] = map[string]string{"id": id}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total": len(found), "results": results})
		case "/crm/v3/objects/companies":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"501"}`)
		case "/crm/v3/objects/notes":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"98765","properties":{},"createdAt":"2026-11-25T10:00:00Z","archived":false}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func noteRequest(t *testing.T, calls []call) request {
	t.Helper()
	last := calls[len(calls)-1]
	if last.Path != "/crm/v3/objects/notes" {
		t.Fatalf("last call is %s, want the note", last.Path)
	}
	var got request
	if err := json.Unmarshal([]byte(last.Body), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSendAssociatesTheNoteWithTheCustomerCompany(t *testing.T) {
	const search = `{"filterGroups":[{"filters":[{"operator":"EQ","propertyName":"name","value":"Bergmann Maschinenbau"}]}],"limit":1,"properties":["name"]}`
	tests := []struct {
		name        string
		customer    string
		found       []string
		wantCalls   []call
		wantNote    string
		wantHeading string
	}{
		{
			name:     "existing company",
			customer: "Bergmann Maschinenbau",
			found:    []string{"301"},
			wantCalls: []call{
				{"/crm/v3/objects/companies/search", search},
				{"/crm/v3/objects/notes", ""},
			},
			wantNote:    `[{"to":{"id":"301"},"types":[{"associationCategory":"HUBSPOT_DEFINED","associationTypeId":190}]}]`,
			wantHeading: "<h3>Visit report: Bergmann Maschinenbau</h3>",
		},
		{
			name:     "new company",
			customer: " Bergmann Maschinenbau ",
			wantCalls: []call{
				{"/crm/v3/objects/companies/search", search},
				{"/crm/v3/objects/companies", `{"properties":{"name":"Bergmann Maschinenbau"}}`},
				{"/crm/v3/objects/notes", ""},
			},
			wantNote:    `[{"to":{"id":"501"},"types":[{"associationCategory":"HUBSPOT_DEFINED","associationTypeId":190}]}]`,
			wantHeading: "<h3>Visit report: Bergmann Maschinenbau</h3>",
		},
		{
			name:        "no customer",
			customer:    "",
			wantCalls:   []call{{"/crm/v3/objects/notes", ""}},
			wantNote:    `[]`,
			wantHeading: "<h3>Visit report</h3>",
		},
		{
			name:        "whitespace-only customer",
			customer:    " \t ",
			wantCalls:   []call{{"/crm/v3/objects/notes", ""}},
			wantNote:    `[]`,
			wantHeading: "<h3>Visit report</h3>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := fakeHubSpot(t, tt.found)
			result := ai.Result{Report: ai.VisitReport{Customer: tt.customer, Summary: "s", Sentiment: ai.Neutral}}

			ref, err := hubspot.New("pat-test", srv.URL, srv.Client()).Send(context.Background(), id, result)
			if err != nil || ref != "98765" {
				t.Fatalf("Send = %q, %v; want 98765, nil", ref, err)
			}
			got := noteRequest(t, *calls)
			gotCalls := slices.Clone(*calls)
			gotCalls[len(gotCalls)-1].Body = ""
			if diff := cmp.Diff(tt.wantCalls, gotCalls); diff != "" {
				t.Errorf("calls (-want +got):\n%s", diff)
			}
			associations, _ := json.Marshal(got.Associations)
			if string(associations) != tt.wantNote {
				t.Errorf("associations = %s, want %s", associations, tt.wantNote)
			}
			if body := got.Properties["hs_note_body"]; !strings.HasPrefix(body, tt.wantHeading+"<p>") {
				t.Errorf("hs_note_body starts %q, want heading %s", body[:min(len(body), 60)], tt.wantHeading)
			}
		})
	}
}

func TestSendFormatsTheNote(t *testing.T) {
	srv, calls := fakeHubSpot(t, []string{"301"})
	result := ai.Result{Transcript: "Anna said <maybe> 40 units.", Report: ai.VisitReport{
		Customer: "Bergmann & Söhne", Contacts: []string{"Anna Weber", "Jonas Klein"}, Summary: "Wants a quote.",
		KeyPoints: []string{"40 units"}, NextSteps: []string{"Send quote"}, FollowUpDate: "2026-12-03",
		Sentiment: ai.Positive,
	}}
	if _, err := hubspot.New("pat-test", srv.URL, srv.Client()).Send(context.Background(), id, result); err != nil {
		t.Fatal(err)
	}
	got := noteRequest(t, *calls)
	if _, err := time.Parse(time.RFC3339, got.Properties["hs_timestamp"]); err != nil {
		t.Errorf("hs_timestamp %q is not RFC 3339: %v", got.Properties["hs_timestamp"], err)
	}
	want := `<h3>Visit report: Bergmann &amp; Söhne</h3><p>Wants a quote.</p>` +
		`<p><strong>Contacts:</strong> Anna Weber, Jonas Klein</p>` +
		`<p><strong>Key points</strong></p><ul><li>40 units</li></ul>` +
		`<p><strong>Next steps</strong></p><ul><li>Send quote</li></ul>` +
		`<p><strong>Follow-up:</strong> 2026-12-03</p><p><strong>Sentiment:</strong> positive</p>` +
		`<p><strong>Debrief job:</strong> 7d3c1b52-3f1e-4b3a-9d6f-0a1b2c3d4e5f</p>` +
		`<p><strong>Transcript</strong></p><p>Anna said &lt;maybe&gt; 40 units.</p>`
	if body := got.Properties["hs_note_body"]; body != want {
		t.Errorf("hs_note_body:\n got %s\nwant %s", body, want)
	}
}

func TestSendTrimsTheNoteBodyToTheLimitOf65536Bytes(t *testing.T) {
	long := strings.Repeat("Tom & Jerry ", 10000)
	tests := []struct {
		name           string
		result         ai.Result
		wantSuffix     string
		wantTranscript bool
	}{
		{"long transcript", ai.Result{Transcript: long, Report: ai.VisitReport{Summary: "s", Sentiment: ai.Neutral}}, " [...]</p>", true},
		{"long report", ai.Result{Transcript: "t", Report: ai.VisitReport{Summary: long, Sentiment: ai.Neutral}}, " [...]", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := fakeHubSpot(t, nil)
			if _, err := hubspot.New("pat-test", srv.URL, srv.Client()).Send(context.Background(), id, tt.result); err != nil {
				t.Fatal(err)
			}
			body := noteRequest(t, *calls).Properties["hs_note_body"]
			kept, trimmed := strings.CutSuffix(body, tt.wantSuffix)
			lastWord := kept[strings.LastIndexByte(kept, ' ')+1:]
			if len(body) > 65536 || !trimmed || (lastWord != "Tom" && lastWord != "&amp;" && lastWord != "Jerry") {
				t.Errorf("note body is %d bytes ending %q; want at most 65536, cut after a whole word, ending %q", len(body), body[len(body)-30:], tt.wantSuffix)
			}
			if got := strings.Contains(body, "<strong>Transcript</strong>"); got != tt.wantTranscript {
				t.Errorf("transcript section present = %v, want %v", got, tt.wantTranscript)
			}
		})
	}
}

func TestSendClassifiesHubSpotErrors(t *testing.T) {
	tests := []struct {
		name          string
		failPath      string
		status        int
		customer      string
		wantErr       string
		wantPermanent bool
	}{
		{"bad token", "/crm/v3/objects/notes", http.StatusUnauthorized, "", "hubspot: create note: unexpected status 401 Unauthorized", true},
		{"missing companies scope", "/crm/v3/objects/companies/search", http.StatusForbidden, "Bergmann", "hubspot: search company: unexpected status 403 Forbidden", true},
		{"search rate limit", "/crm/v3/objects/companies/search", http.StatusTooManyRequests, "Bergmann", "hubspot: search company: unexpected status 429 Too Many Requests", false},
		{"company create outage", "/crm/v3/objects/companies", http.StatusServiceUnavailable, "Bergmann", "hubspot: create company: unexpected status 503 Service Unavailable", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tt.failPath:
					w.WriteHeader(tt.status)
				case "/crm/v3/objects/companies/search":
					_, _ = io.WriteString(w, `{"total":0,"results":[]}`)
				default:
					t.Errorf("unexpected call %s after %s", r.URL.Path, tt.failPath)
				}
			}))
			defer srv.Close()

			_, err := hubspot.New("pat-test", srv.URL, srv.Client()).Send(context.Background(), id,
				ai.Result{Report: ai.VisitReport{Customer: tt.customer}})
			var status *httpjson.StatusError
			if err == nil || err.Error() != tt.wantErr || !errors.As(err, &status) || status.Permanent() != tt.wantPermanent {
				t.Errorf("err = %v, want %q with Permanent() = %v", err, tt.wantErr, tt.wantPermanent)
			}
		})
	}
}

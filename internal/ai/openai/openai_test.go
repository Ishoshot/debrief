package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/ai/openai"
	"github.com/Ishoshot/debrief/internal/httpjson"
)

const reportJSON = `{"customer":"Bergmann Maschinenbau","contacts":["Anna Weber"],"summary":"Wants a quote.",` +
	`"key_points":["40 units"],"next_steps":["Send quote"],"follow_up_date":null,"sentiment":"positive","products":["X200"]}`

func chatReply(content, refusal, finish string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message":       map[string]any{"role": "assistant", "content": content, "refusal": refusal},
		"finish_reason": finish,
	}}})
	return string(b)
}

type captured struct {
	auth, model, filename, partType, audio string
	chat                                   map[string]any
}

func fakeOpenAI(t *testing.T, got *captured, transcriptStatus int, chat string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/audio/transcriptions":
			if transcriptStatus != http.StatusOK {
				w.WriteHeader(transcriptStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided: sk-te***"}}`)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
			}
			got.model = r.FormValue("model")
			f, fh, err := r.FormFile("file")
			if err != nil {
				t.Errorf("file part: %v", err)
				return
			}
			data, _ := io.ReadAll(f)
			got.filename, got.partType, got.audio = fh.Filename, fh.Header.Get("Content-Type"), string(data)
			_, _ = io.WriteString(w, `{"text":"Visited Bergmann Maschinenbau today."}`)
		case "/chat/completions":
			if err := json.NewDecoder(r.Body).Decode(&got.chat); err != nil {
				t.Errorf("decode chat request: %v", err)
			}
			_, _ = io.WriteString(w, chat)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

var audio = ai.Audio{Data: []byte("m4a-bytes"), Filename: "visit.m4a", ContentType: "audio/m4a"}

func TestAnalyze(t *testing.T) {
	var got captured
	srv := fakeOpenAI(t, &got, http.StatusOK, chatReply(reportJSON, "", "stop"))

	res, err := openai.New("sk-test", "transcribe-test", "report-test", srv.URL+"/", srv.Client()).Analyze(context.Background(), audio)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	want := ai.Result{
		Transcript: "Visited Bergmann Maschinenbau today.",
		Report: ai.VisitReport{
			Customer: "Bergmann Maschinenbau", Contacts: []string{"Anna Weber"}, Summary: "Wants a quote.",
			KeyPoints: []string{"40 units"}, NextSteps: []string{"Send quote"}, Sentiment: ai.Positive, Products: []string{"X200"},
		},
	}
	if diff := cmp.Diff(want, res); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
	if got.auth != "Bearer sk-test" || got.model != "transcribe-test" || got.filename != "visit.m4a" ||
		got.partType != "audio/m4a" || got.audio != "m4a-bytes" {
		t.Errorf("transcription request = %+v", got)
	}

	format := got.chat["response_format"].(map[string]any)
	schema := format["json_schema"].(map[string]any)
	messages := got.chat["messages"].([]any)
	user := messages[1].(map[string]any)
	if got.chat["model"] != "report-test" || format["type"] != "json_schema" || schema["strict"] != true ||
		user["content"] != "Visited Bergmann Maschinenbau today." {
		t.Errorf("chat request = %v", got.chat)
	}
}

func TestAnalyzeFailures(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		chat          string
		wantErr       string
		wantPermanent bool
	}{
		{"bad key", http.StatusUnauthorized, "", "openai transcribe: unexpected status 401 Unauthorized", true},
		{"rate limited", http.StatusTooManyRequests, "", "openai transcribe: unexpected status 429 Too Many Requests", false},
		{"refusal", http.StatusOK, chatReply("", "I can't help with that.", "stop"), "openai report: model refused the request", false},
		{"truncated", http.StatusOK, chatReply(`{"summary":`, "", "length"), `openai report: incomplete response: finish_reason "length"`, false},
		{"schema drift", http.StatusOK, chatReply(`{"summary":"s","sentiment":"meh"}`, "", "stop"), `openai report: decode visit report: unknown sentiment "meh"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeOpenAI(t, &captured{}, tt.status, tt.chat)
			_, err := openai.New("sk-test", "transcribe-test", "report-test", srv.URL, srv.Client()).Analyze(context.Background(), audio)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			var statusErr *httpjson.StatusError
			if permanent := errors.As(err, &statusErr) && statusErr.Permanent(); permanent != tt.wantPermanent {
				t.Errorf("permanent = %v, want %v", permanent, tt.wantPermanent)
			}
		})
	}
}

package gemini_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/ai/gemini"
)

const output = `{"transcript":"Visited Bergmann today.","report":{"customer":"Bergmann Maschinenbau","contacts":[],` +
	`"summary":"Needs a quote.","key_points":[],"next_steps":["Send quote"],"follow_up_date":"2026-12-03","sentiment":"neutral","products":[]}}`

func reply(text, finish string) string {
	b, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"role": "model", "parts": []any{
			map[string]any{"text": "thinking about the visit", "thought": true},
			map[string]any{"text": text},
		}},
		"finishReason": finish,
	}}})
	return string(b)
}

var audio = ai.Audio{Data: []byte("m4a-bytes"), Filename: "visit.m4a", ContentType: "audio/m4a"}

func TestAnalyze(t *testing.T) {
	var path, key string
	var body struct {
		Contents []struct {
			Parts []struct {
				InlineData struct {
					MIMEType string `json:"mimeType"`
					Data     []byte `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			ResponseMIMEType string `json:"responseMimeType"`
		} `json:"generationConfig"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("x-goog-api-key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, reply(output, "STOP"))
	}))
	defer srv.Close()

	res, err := gemini.New("g-key", "gemini-test", srv.URL, srv.Client()).Analyze(context.Background(), audio)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	want := ai.Result{Transcript: "Visited Bergmann today.", Report: ai.VisitReport{
		Customer: "Bergmann Maschinenbau", Contacts: []string{}, Summary: "Needs a quote.", KeyPoints: []string{},
		NextSteps: []string{"Send quote"}, FollowUpDate: "2026-12-03", Sentiment: ai.Neutral, Products: []string{},
	}}
	if diff := cmp.Diff(want, res); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
	inline := body.Contents[0].Parts[0].InlineData
	if path != "/models/gemini-test:generateContent" || key != "g-key" || inline.MIMEType != "audio/m4a" ||
		string(inline.Data) != "m4a-bytes" || body.GenerationConfig.ResponseMIMEType != "application/json" {
		t.Errorf("request: path=%s key=%s inline=%s/%q config=%+v", path, key, inline.MIMEType, inline.Data, body.GenerationConfig)
	}
}

func TestAnalyzeFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"server error", http.StatusServiceUnavailable, "", "gemini: unexpected status 503 Service Unavailable"},
		{"blocked prompt", http.StatusOK, `{"promptFeedback":{"blockReason":"SAFETY"}}`, "gemini: prompt blocked: SAFETY"},
		{"truncated", http.StatusOK, reply(`{"transcript":"Vis`, "MAX_TOKENS"), `gemini: incomplete response: finishReason "MAX_TOKENS"`},
		{"empty transcript", http.StatusOK, reply(`{"transcript":" ","report":{}}`, "STOP"), "gemini: empty transcript"},
		{"report breaks schema", http.StatusOK, reply(`{"transcript":"t","report":{"summary":"s"}}`, "STOP"), "gemini: visit report has no sentiment"},
		{"trailing object", http.StatusOK, reply(`{"transcript":"t","report":{"summary":"s","sentiment":"neutral"}} {"transcript":"x"}`, "STOP"), "gemini: decode response JSON: data after the JSON object"},
		{"trailing text", http.StatusOK, reply(`{"transcript":"t","report":{"summary":"s","sentiment":"neutral"}} Hope this helps`, "STOP"), "gemini: decode response JSON: data after the JSON object"},
		{"trailing brace", http.StatusOK, reply(`{"transcript":"t","report":{"summary":"s","sentiment":"neutral"}}}`, "STOP"), "gemini: decode response JSON: data after the JSON object"},
		{"trailing bracket", http.StatusOK, reply(`{"transcript":"t","report":{"summary":"s","sentiment":"neutral"}}]`, "STOP"), "gemini: decode response JSON: data after the JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			_, err := gemini.New("g-key", "gemini-test", srv.URL, srv.Client()).Analyze(context.Background(), audio)
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

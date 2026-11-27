// Package gemini analyzes a recording with one Gemini call that returns a transcript and visit report as JSON.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/httpjson"
)

type (
	Client struct {
		apiKey  string
		model   string
		baseURL string
		http    *http.Client
	}

	part struct {
		Text       string      `json:"text,omitempty"`
		InlineData *inlineData `json:"inlineData,omitempty"`
		Thought    bool        `json:"thought,omitempty"`
	}

	inlineData struct {
		MIMEType string `json:"mimeType"`
		Data     []byte `json:"data"`
	}

	content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}

	request struct {
		SystemInstruction content          `json:"systemInstruction"`
		Contents          []content        `json:"contents"`
		GenerationConfig  generationConfig `json:"generationConfig"`
	}

	generationConfig struct {
		ResponseMIMEType   string         `json:"responseMimeType"`
		ResponseJSONSchema map[string]any `json:"responseJsonSchema"`
	}

	response struct {
		Candidates []struct {
			Content      content `json:"content"`
			FinishReason string  `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}

	answer struct {
		Transcript string          `json:"transcript"`
		Report     json.RawMessage `json:"report"`
	}
)

const (
	BaseURL          = "https://generativelanguage.googleapis.com/v1beta"
	transcribePrompt = "Transcribe this recording word for word into transcript, then write the visit report."
)

var _ ai.Provider = (*Client)(nil)

func New(apiKey, model, baseURL string, hc *http.Client) *Client {
	return &Client{apiKey: apiKey, model: model, baseURL: strings.TrimSuffix(baseURL, "/"), http: hc}
}

func (c *Client) Analyze(ctx context.Context, a ai.Audio) (ai.Result, error) {
	res, err := c.analyze(ctx, a)
	if err != nil {
		return ai.Result{}, fmt.Errorf("gemini: %w", err)
	}
	return res, nil
}

func (c *Client) analyze(ctx context.Context, a ai.Audio) (ai.Result, error) {
	payload, err := json.Marshal(newRequest(a))
	if err != nil {
		return ai.Result{}, err
	}
	var out response
	if err := c.generateContent(ctx, payload, &out); err != nil {
		return ai.Result{}, err
	}
	text, err := answerText(out)
	if err != nil {
		return ai.Result{}, err
	}
	return parse([]byte(text))
}

func (c *Client) generateContent(ctx context.Context, payload []byte, out *response) error {
	url := c.baseURL + "/models/" + c.model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("x-goog-api-key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	return httpjson.Do(c.http, req, out)
}

func newRequest(a ai.Audio) request {
	return request{
		SystemInstruction: content{Parts: []part{{Text: ai.Instructions(time.Now())}}},
		Contents: []content{{Role: "user", Parts: []part{
			{InlineData: &inlineData{MIMEType: a.ContentType, Data: a.Data}},
			{Text: transcribePrompt},
		}}},
		GenerationConfig: generationConfig{
			ResponseMIMEType:   "application/json",
			ResponseJSONSchema: answerSchema(),
		},
	}
}

func answerSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"transcript": map[string]any{"type": "string"},
			"report":     ai.ReportSchema(),
		},
		"required":             []string{"transcript", "report"},
		"additionalProperties": false,
	}
}

func answerText(out response) (string, error) {
	if out.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("prompt blocked: %s", out.PromptFeedback.BlockReason)
	}
	if len(out.Candidates) == 0 {
		return "", errors.New("no candidates in response")
	}
	cand := out.Candidates[0]
	if cand.FinishReason != "STOP" {
		return "", fmt.Errorf("incomplete response: finishReason %q", cand.FinishReason)
	}
	return withoutThoughts(cand.Content.Parts), nil
}

func withoutThoughts(parts []part) string {
	var text strings.Builder
	for _, p := range parts {
		if !p.Thought {
			text.WriteString(p.Text)
		}
	}
	return text.String()
}

func parse(data []byte) (ai.Result, error) {
	ans, err := decodeAnswer(data)
	if err != nil {
		return ai.Result{}, err
	}
	if strings.TrimSpace(ans.Transcript) == "" {
		return ai.Result{}, errors.New("empty transcript")
	}
	report, err := ai.ParseReport(ans.Report)
	if err != nil {
		return ai.Result{}, err
	}
	return ai.Result{Transcript: ans.Transcript, Report: report}, nil
}

func decodeAnswer(data []byte) (answer, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var ans answer
	if err := dec.Decode(&ans); err != nil {
		return answer{}, fmt.Errorf("decode response JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return answer{}, errors.New("decode response JSON: data after the JSON object")
	}
	return ans, nil
}

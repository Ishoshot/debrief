// Package openai analyzes a recording with speech-to-text, then a chat completion held to the report schema.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/Ishoshot/debrief/internal/ai"
	"github.com/Ishoshot/debrief/internal/httpjson"
)

type (
	Client struct {
		apiKey          string
		transcribeModel string
		reportModel     string
		baseURL         string
		http            *http.Client
	}

	transcription struct {
		Text string `json:"text"`
	}

	message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	chatRequest struct {
		Model          string         `json:"model"`
		Messages       []message      `json:"messages"`
		ResponseFormat responseFormat `json:"response_format"`
	}

	responseFormat struct {
		Type       string     `json:"type"`
		JSONSchema jsonSchema `json:"json_schema"`
	}

	jsonSchema struct {
		Name   string         `json:"name"`
		Strict bool           `json:"strict"`
		Schema map[string]any `json:"schema"`
	}

	chatResponse struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
)

const BaseURL = "https://api.openai.com/v1"

var _ ai.Provider = (*Client)(nil)

func New(apiKey, transcribeModel, reportModel, baseURL string, hc *http.Client) *Client {
	return &Client{
		apiKey:          apiKey,
		transcribeModel: transcribeModel,
		reportModel:     reportModel,
		baseURL:         strings.TrimSuffix(baseURL, "/"),
		http:            hc,
	}
}

func (c *Client) Analyze(ctx context.Context, a ai.Audio) (ai.Result, error) {
	transcript, err := c.transcribe(ctx, a)
	if err != nil {
		return ai.Result{}, fmt.Errorf("openai transcribe: %w", err)
	}
	report, err := c.report(ctx, transcript)
	if err != nil {
		return ai.Result{}, fmt.Errorf("openai report: %w", err)
	}
	return ai.Result{Transcript: transcript, Report: report}, nil
}

func (c *Client) transcribe(ctx context.Context, a ai.Audio) (string, error) {
	form, contentType, err := c.transcriptionForm(a)
	if err != nil {
		return "", err
	}
	var out transcription
	if err := c.post(ctx, "/audio/transcriptions", contentType, form, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", errors.New("empty transcript")
	}
	return out.Text, nil
}

func (c *Client) report(ctx context.Context, transcript string) (ai.VisitReport, error) {
	payload, err := json.Marshal(c.reportRequest(transcript))
	if err != nil {
		return ai.VisitReport{}, err
	}
	var out chatResponse
	if err := c.post(ctx, "/chat/completions", "application/json", bytes.NewReader(payload), &out); err != nil {
		return ai.VisitReport{}, err
	}
	return parseReport(out)
}

func (c *Client) transcriptionForm(a ai.Audio) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("model", c.transcribeModel); err != nil {
		return nil, "", err
	}
	part, err := w.CreatePart(filePartHeader(a))
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(a.Data); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &body, w.FormDataContentType(), nil
}

func filePartHeader(a ai.Audio) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, a.Filename))
	h.Set("Content-Type", a.ContentType)
	return h
}

func (c *Client) reportRequest(transcript string) chatRequest {
	return chatRequest{
		Model: c.reportModel,
		Messages: []message{
			{Role: "developer", Content: ai.Instructions(time.Now())},
			{Role: "user", Content: transcript},
		},
		ResponseFormat: responseFormat{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: "visit_report", Strict: true, Schema: ai.ReportSchema()},
		},
	}
}

func parseReport(out chatResponse) (ai.VisitReport, error) {
	if len(out.Choices) == 0 {
		return ai.VisitReport{}, errors.New("no choices in response")
	}
	choice := out.Choices[0]
	if choice.Message.Refusal != "" {
		return ai.VisitReport{}, errors.New("model refused the request")
	}
	if choice.FinishReason != "stop" {
		return ai.VisitReport{}, fmt.Errorf("incomplete response: finish_reason %q", choice.FinishReason)
	}
	return ai.ParseReport([]byte(choice.Message.Content))
}

func (c *Client) post(ctx context.Context, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", contentType)
	return httpjson.Do(c.http, req, out)
}

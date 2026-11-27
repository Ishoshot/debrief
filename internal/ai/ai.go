// Package ai defines what the pipeline needs from a model: a transcript and a visit report per recording.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

type (
	Provider interface {
		Analyze(ctx context.Context, a Audio) (Result, error)
	}

	Audio struct {
		Data        []byte
		Filename    string
		ContentType string
	}

	Result struct {
		Transcript string      `json:"transcript"`
		Report     VisitReport `json:"report"`
	}

	VisitReport struct {
		Customer     string    `json:"customer"`
		Contacts     []string  `json:"contacts"`
		Summary      string    `json:"summary"`
		KeyPoints    []string  `json:"key_points"`
		NextSteps    []string  `json:"next_steps"`
		FollowUpDate string    `json:"follow_up_date,omitempty"`
		Sentiment    Sentiment `json:"sentiment"`
		Products     []string  `json:"products"`
	}

	Sentiment string
)

const (
	Positive Sentiment = "positive"
	Neutral  Sentiment = "neutral"
	Negative Sentiment = "negative"
)

var sentiments = []Sentiment{Positive, Neutral, Negative}

func (s *Sentiment) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if !slices.Contains(sentiments, Sentiment(v)) {
		return fmt.Errorf("unknown sentiment %q", v)
	}
	*s = Sentiment(v)
	return nil
}

func ParseReport(data []byte) (VisitReport, error) {
	r, err := decodeReport(data)
	if err != nil {
		return VisitReport{}, err
	}
	if err := validateReport(r); err != nil {
		return VisitReport{}, err
	}
	return r, nil
}

func decodeReport(data []byte) (VisitReport, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r VisitReport
	if err := dec.Decode(&r); err != nil {
		return VisitReport{}, fmt.Errorf("decode visit report: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return VisitReport{}, errors.New("decode visit report: data after the JSON object")
	}
	return r, nil
}

func validateReport(r VisitReport) error {
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("visit report has no summary")
	}
	if r.Sentiment == "" {
		return errors.New("visit report has no sentiment")
	}
	if r.FollowUpDate == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, r.FollowUpDate); err != nil {
		return fmt.Errorf("visit report follow_up_date %q is not YYYY-MM-DD", r.FollowUpDate)
	}
	return nil
}

// The date lets the model turn "next Tuesday" into a follow-up date.
func Instructions(today time.Time) string {
	return `You turn the audio recording of a field-sales visit into a CRM visit report.
The recording can be the rep's own spoken notes after the visit, or the conversation between the rep and the customer.
Today is ` + today.Format(time.DateOnly) + `.
Use only facts stated in the recording. Never invent names, numbers or dates.
customer: the customer company, or "" if not stated.
contacts: the customer's people who took part in the visit, with role if stated. Do not list the rep or the rep's colleagues.
summary: two or three sentences, written for a sales manager.
key_points: the important facts, one short sentence each.
next_steps: concrete action items for the rep.
follow_up_date: the agreed follow-up date as YYYY-MM-DD, or null if none.
sentiment: the customer's overall attitude: positive, neutral or negative.
products: products or topics discussed.
Write the report in the language of the recording.`
}

// Strict structured output needs every property in required, so optional values are nullable.
func ReportSchema() map[string]any {
	stringList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"customer":       map[string]any{"type": "string"},
			"contacts":       stringList,
			"summary":        map[string]any{"type": "string"},
			"key_points":     stringList,
			"next_steps":     stringList,
			"follow_up_date": map[string]any{"type": []string{"string", "null"}, "description": "YYYY-MM-DD"},
			"sentiment":      map[string]any{"type": "string", "enum": sentiments},
			"products":       stringList,
		},
		"required":             []string{"customer", "contacts", "summary", "key_points", "next_steps", "follow_up_date", "sentiment", "products"},
		"additionalProperties": false,
	}
}

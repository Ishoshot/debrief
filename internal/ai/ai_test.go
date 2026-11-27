package ai_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Ishoshot/debrief/internal/ai"
)

func TestParseReport(t *testing.T) {
	valid := `{"customer":"Bergmann Maschinenbau","contacts":["Anna Weber, head of purchasing"],
		"summary":"Positive visit.","key_points":["Wants 40 units"],"next_steps":["Send quote"],
		"follow_up_date":"2026-12-03","sentiment":"positive","products":["X200"]}`

	got, err := ai.ParseReport([]byte(valid))
	if err != nil {
		t.Fatalf("ParseReport(valid): %v", err)
	}
	want := ai.VisitReport{
		Customer:     "Bergmann Maschinenbau",
		Contacts:     []string{"Anna Weber, head of purchasing"},
		Summary:      "Positive visit.",
		KeyPoints:    []string{"Wants 40 units"},
		NextSteps:    []string{"Send quote"},
		FollowUpDate: "2026-12-03",
		Sentiment:    ai.Positive,
		Products:     []string{"X200"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseReport mismatch (-want +got):\n%s", diff)
	}

	nullDate, err := ai.ParseReport([]byte(`{"summary":"s","sentiment":"neutral","follow_up_date":null}`))
	if err != nil || nullDate.FollowUpDate != "" {
		t.Errorf("null follow_up_date: got %q, %v; want empty, nil", nullDate.FollowUpDate, err)
	}

	bad := []struct{ name, in, wantErr string }{
		{"unknown sentiment", `{"summary":"s","sentiment":"ecstatic"}`, `decode visit report: unknown sentiment "ecstatic"`},
		{"extra field", `{"summary":"s","sentiment":"neutral","budget":1}`, `decode visit report: json: unknown field "budget"`},
		{"no summary", `{"summary":"  ","sentiment":"neutral"}`, "visit report has no summary"},
		{"no sentiment", `{"summary":"s"}`, "visit report has no sentiment"},
		{"bad date", `{"summary":"s","sentiment":"neutral","follow_up_date":"next week"}`, `visit report follow_up_date "next week" is not YYYY-MM-DD`},
		{"not json", `Sure! Here is the report`, "decode visit report: invalid character 'S' looking for beginning of value"},
		{"trailing object", `{"summary":"s","sentiment":"neutral"} {"summary":"t"}`, "decode visit report: data after the JSON object"},
		{"trailing text", `{"summary":"s","sentiment":"neutral"} Hope this helps`, "decode visit report: data after the JSON object"},
		{"trailing brace", `{"summary":"s","sentiment":"neutral"}}`, "decode visit report: data after the JSON object"},
		{"trailing bracket", `{"summary":"s","sentiment":"neutral"}]`, "decode visit report: data after the JSON object"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ai.ParseReport([]byte(tt.in))
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("ParseReport error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// The schema is hand-written, so pin it to the Go type: a field added to one but
// not the other would let the model and the parser disagree.
func TestReportSchemaMatchesVisitReport(t *testing.T) {
	full, err := json.Marshal(ai.VisitReport{FollowUpDate: "2026-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(full, &fields); err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(maps.Keys(fields))

	schema := ai.ReportSchema()
	properties := slices.Sorted(maps.Keys(schema["properties"].(map[string]any)))
	required := slices.Sorted(slices.Values(schema["required"].([]string)))

	if diff := cmp.Diff(want, properties); diff != "" {
		t.Errorf("schema properties differ from VisitReport JSON fields (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want, required); diff != "" {
		t.Errorf("schema required differs from VisitReport JSON fields (-want +got):\n%s", diff)
	}
}

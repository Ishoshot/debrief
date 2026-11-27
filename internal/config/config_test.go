package config_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Ishoshot/debrief/internal/config"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	got, err := config.Load(env(map[string]string{
		"DATABASE_URL":   "postgres://db",
		"OPENAI_API_KEY": "sk-test",
		"WEBHOOK_URL":    "https://example.com/hook",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := config.Config{
		Port:                  "8080",
		DatabaseURL:           "postgres://db",
		Workers:               4,
		MaxAttempts:           5,
		AIProvider:            config.OpenAI,
		OpenAIKey:             "sk-test",
		OpenAITranscribeModel: "gpt-4o-transcribe",
		OpenAIReportModel:     "gpt-6-luna",
		Destination:           config.Webhook,
		WebhookURL:            "https://example.com/hook",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Load mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadGeminiDefaultModel(t *testing.T) {
	got, err := config.Load(env(map[string]string{
		"DATABASE_URL": "postgres://db", "AI_PROVIDER": "gemini", "GEMINI_API_KEY": "g", "WEBHOOK_URL": "https://example.com/hook",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GeminiModel != "gemini-3.5-flash" {
		t.Errorf("GeminiModel = %q, want gemini-3.5-flash", got.GeminiModel)
	}
}

func TestLoadGeminiWithAnotherAllowedModel(t *testing.T) {
	got, err := config.Load(env(map[string]string{
		"DATABASE_URL": "postgres://db", "AI_PROVIDER": "gemini", "GEMINI_API_KEY": "g", "GEMINI_MODEL": "gemini-3.6-flash",
		"OPENAI_REPORT_MODEL": "not-checked-for-gemini", "WEBHOOK_URL": "https://example.com/hook",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := config.Config{
		Port:        "8080",
		DatabaseURL: "postgres://db",
		Workers:     4,
		MaxAttempts: 5,
		AIProvider:  config.Gemini,
		GeminiKey:   "g",
		GeminiModel: "gemini-3.6-flash",
		Destination: config.Webhook,
		WebhookURL:  "https://example.com/hook",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Load mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "everything missing",
			env:  map[string]string{},
			want: "DATABASE_URL is required\nOPENAI_API_KEY is required\nWEBHOOK_URL is required: set it to your own test URL from https://webhook.site",
		},
		{
			name: "unknown provider and destination",
			env:  map[string]string{"DATABASE_URL": "x", "AI_PROVIDER": "llama", "DESTINATION": "fax"},
			want: "AI_PROVIDER \"llama\" is not supported: use openai or gemini\nDESTINATION \"fax\" is not supported: use webhook or hubspot",
		},
		{
			name: "gemini and hubspot need their own secrets",
			env:  map[string]string{"DATABASE_URL": "x", "AI_PROVIDER": "gemini", "OPENAI_API_KEY": "sk", "DESTINATION": "hubspot"},
			want: "GEMINI_API_KEY is required\nHUBSPOT_TOKEN is required",
		},
		{
			name: "openai models off the allowlist; gemini model not checked",
			env: map[string]string{"DATABASE_URL": "x", "OPENAI_API_KEY": "sk", "WEBHOOK_URL": "https://example.com/hook",
				"OPENAI_TRANSCRIBE_MODEL": "whisper-1", "OPENAI_REPORT_MODEL": "gpt-4o", "GEMINI_MODEL": "ignored-for-openai"},
			want: "OPENAI_TRANSCRIBE_MODEL \"whisper-1\" is not supported: use gpt-4o-transcribe, gpt-transcribe\nOPENAI_REPORT_MODEL \"gpt-4o\" is not supported: use gpt-6-luna, gpt-5.6-luna",
		},
		{
			name: "gemini model off the allowlist",
			env: map[string]string{"DATABASE_URL": "x", "AI_PROVIDER": "gemini", "GEMINI_API_KEY": "g", "WEBHOOK_URL": "https://example.com/hook",
				"GEMINI_MODEL": "gemini-1.0-pro"},
			want: "GEMINI_MODEL \"gemini-1.0-pro\" is not supported: use gemini-3.5-flash, gemini-3.6-flash",
		},
		{
			name: "bad numbers and relative webhook URL",
			env:  map[string]string{"DATABASE_URL": "x", "OPENAI_API_KEY": "sk", "WEBHOOK_URL": "/hook", "WORKERS": "0", "MAX_ATTEMPTS": "many"},
			want: "WORKERS must be a positive integer, got \"0\"\nMAX_ATTEMPTS must be a positive integer, got \"many\"\nWEBHOOK_URL must be an absolute http or https URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(env(tt.env))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			if got := err.Error(); got != tt.want {
				t.Errorf("error:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

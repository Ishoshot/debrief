// Package config reads the service settings from the environment once, at startup.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type (
	Provider    string
	Destination string

	Config struct {
		Port        string
		DatabaseURL string
		Workers     int
		MaxAttempts int

		AIProvider            Provider
		OpenAIKey             string
		GeminiKey             string
		OpenAITranscribeModel string
		OpenAIReportModel     string
		GeminiModel           string
		OpenAIBaseURL         string
		GeminiBaseURL         string

		Destination  Destination
		WebhookURL   string
		HubSpotToken string
	}
)

const (
	OpenAI Provider = "openai"
	Gemini Provider = "gemini"

	Webhook Destination = "webhook"
	HubSpot Destination = "hubspot"

	defaultWorkers     = 4
	defaultMaxAttempts = 5
)

// Only models tested with the prompt and the sample recording. The first one is the default.
var (
	openAITranscribeModels = []string{"gpt-4o-transcribe", "gpt-transcribe"}
	openAIReportModels     = []string{"gpt-6-luna", "gpt-5.6-luna"}
	geminiModels           = []string{"gemini-3.5-flash", "gemini-3.6-flash"}
)

func Load(getenv func(string) string) (Config, error) {
	var errs []error
	c := Config{
		Port:          cmp.Or(getenv("PORT"), "8080"),
		DatabaseURL:   getenv("DATABASE_URL"),
		AIProvider:    Provider(cmp.Or(getenv("AI_PROVIDER"), string(OpenAI))),
		OpenAIKey:     getenv("OPENAI_API_KEY"),
		GeminiKey:     getenv("GEMINI_API_KEY"),
		OpenAIBaseURL: getenv("OPENAI_BASE_URL"),
		GeminiBaseURL: getenv("GEMINI_BASE_URL"),
		Destination:   Destination(cmp.Or(getenv("DESTINATION"), string(Webhook))),
		WebhookURL:    getenv("WEBHOOK_URL"),
		HubSpotToken:  getenv("HUBSPOT_TOKEN"),
	}
	c.Workers = positiveInt(&errs, getenv, "WORKERS", defaultWorkers)
	c.MaxAttempts = positiveInt(&errs, getenv, "MAX_ATTEMPTS", defaultMaxAttempts)
	require(&errs, "DATABASE_URL", c.DatabaseURL)
	c.loadModels(&errs, getenv)
	c.checkDestination(&errs)
	return c, errors.Join(errs...)
}

func (c *Config) loadModels(errs *[]error, getenv func(string) string) {
	switch c.AIProvider {
	case OpenAI:
		require(errs, "OPENAI_API_KEY", c.OpenAIKey)
		c.OpenAITranscribeModel = oneOf(errs, getenv, "OPENAI_TRANSCRIBE_MODEL", openAITranscribeModels)
		c.OpenAIReportModel = oneOf(errs, getenv, "OPENAI_REPORT_MODEL", openAIReportModels)
	case Gemini:
		require(errs, "GEMINI_API_KEY", c.GeminiKey)
		c.GeminiModel = oneOf(errs, getenv, "GEMINI_MODEL", geminiModels)
	default:
		*errs = append(*errs, fmt.Errorf("AI_PROVIDER %q is not supported: use openai or gemini", c.AIProvider))
	}
}

func (c *Config) checkDestination(errs *[]error) {
	switch c.Destination {
	case Webhook:
		checkWebhookURL(errs, c.WebhookURL)
	case HubSpot:
		require(errs, "HUBSPOT_TOKEN", c.HubSpotToken)
	default:
		*errs = append(*errs, fmt.Errorf("DESTINATION %q is not supported: use webhook or hubspot", c.Destination))
	}
}

func checkWebhookURL(errs *[]error, raw string) {
	if raw == "" {
		*errs = append(*errs, errors.New("WEBHOOK_URL is required: set it to your own test URL from https://webhook.site"))
		return
	}
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		*errs = append(*errs, errors.New("WEBHOOK_URL must be an absolute http or https URL"))
	}
}

func require(errs *[]error, name, value string) {
	if value == "" {
		*errs = append(*errs, fmt.Errorf("%s is required", name))
	}
}

func oneOf(errs *[]error, getenv func(string) string, name string, allowed []string) string {
	v := cmp.Or(getenv(name), allowed[0])
	if !slices.Contains(allowed, v) {
		*errs = append(*errs, fmt.Errorf("%s %q is not supported: use %s", name, v, strings.Join(allowed, ", ")))
	}
	return v
}

func positiveInt(errs *[]error, getenv func(string) string, name string, fallback int) int {
	raw := getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		*errs = append(*errs, fmt.Errorf("%s must be a positive integer, got %q", name, raw))
		return fallback
	}
	return n
}

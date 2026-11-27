package httpjson_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ishoshot/debrief/internal/httpjson"
)

func TestDo(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantErr       string
		wantPermanent bool
	}{
		{"ok", http.StatusOK, `{"id":"n-1"}`, "", false},
		{"body text stays out of the error", http.StatusBadRequest, `{"message":"secret sk-live-123 echoed"}`, "unexpected status 400 Bad Request", true},
		{"unauthorized", http.StatusUnauthorized, "", "unexpected status 401 Unauthorized", true},
		{"request timeout", http.StatusRequestTimeout, "", "unexpected status 408 Request Timeout", false},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"Slow down.","type":"requests","code":"rate_limit_exceeded"}}`,
			"unexpected status 429 Too Many Requests (rate_limit_exceeded)", false},
		{"no credit", http.StatusTooManyRequests, `{"error":{"message":"You exceeded your quota.","type":"insufficient_quota","code":"insufficient_quota"}}`,
			"unexpected status 429 Too Many Requests (insufficient_quota)", true},
		{"quota code on a 5xx still retries", http.StatusServiceUnavailable, `{"error":{"code":"insufficient_quota"}}`,
			"unexpected status 503 Service Unavailable (insufficient_quota)", false},
		{"google overloaded", http.StatusServiceUnavailable, `{"error":{"code":503,"message":"High demand.","status":"UNAVAILABLE"}}`,
			"unexpected status 503 Service Unavailable (UNAVAILABLE)", false},
		{"hubspot category", http.StatusBadRequest, `{"status":"error","message":"Property x does not exist","category":"VALIDATION_ERROR"}`,
			"unexpected status 400 Bad Request (VALIDATION_ERROR)", true},
		{"free text in a code field", http.StatusBadGateway, `{"error":{"code":"Visited Bergmann today"}}`, "unexpected status 502 Bad Gateway", false},
		{"bad gateway", http.StatusBadGateway, "", "unexpected status 502 Bad Gateway", false},
		{"invalid json", http.StatusOK, `not json`, "decode response: invalid character 'o' in literal null (expecting 'u')", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
			var out struct {
				ID string `json:"id"`
			}
			err := httpjson.Do(srv.Client(), req, &out)

			if tt.wantErr == "" {
				if err != nil || out.ID != "n-1" {
					t.Fatalf("got id %q err %v, want n-1 nil", out.ID, err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			var statusErr *httpjson.StatusError
			if permanent := errors.As(err, &statusErr) && statusErr.Permanent(); permanent != tt.wantPermanent {
				t.Errorf("permanent = %v, want %v", permanent, tt.wantPermanent)
			}
			if statusErr != nil && statusErr.ResponseBody() != tt.body {
				t.Errorf("ResponseBody() = %q, want %q", statusErr.ResponseBody(), tt.body)
			}
		})
	}
}

func TestDoKeepsOnlyTheStartOfAnErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("z", 10_000))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	var statusErr *httpjson.StatusError
	if err := httpjson.Do(srv.Client(), req, nil); !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want a StatusError", err)
	}
	if got := len(statusErr.ResponseBody()); got != 8<<10 {
		t.Errorf("ResponseBody() has %d bytes, want 8192", got)
	}
}

func TestNewClientDoesNotFollowRedirects(t *testing.T) {
	tests := []struct{ location, wantErr string }{
		{"https://receiver.example/hooks/fictional-secret", "unexpected status 302 Found"},
		{"https://receiver.example/hooks/fictional-secret/%zz", "Post request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.location, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", tt.location)
				w.WriteHeader(http.StatusFound)
			}))
			defer srv.Close()

			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, http.NoBody)
			err := httpjson.Do(httpjson.NewClient(time.Second), req, nil)

			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDoNetworkErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/hooks/secret-token"
	srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, http.NoBody)
	err := httpjson.Do(http.DefaultClient, req, nil)

	if err == nil || !strings.HasPrefix(err.Error(), "Post request: ") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("err = %v, want a Post request error without the URL path", err)
	}
}

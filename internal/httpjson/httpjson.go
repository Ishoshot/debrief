// Package httpjson sends requests to JSON APIs and classifies their errors for every vendor client.
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

type (
	StatusError struct {
		Code       int
		VendorCode string
		body       string
	}

	errorReply struct {
		Error struct {
			Code   any    `json:"code"` // a string from OpenAI, a number from Google
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"error"`
		Category string `json:"category"`
	}
)

const (
	maxResponseBytes  = 4 << 20
	maxErrorBodyBytes = 8 << 10
)

var codeShape = regexp.MustCompile(`^[A-Za-z0-9_.]{1,64}$`)

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("unexpected status %d %s", e.Code, http.StatusText(e.Code))
	if e.VendorCode != "" {
		msg += " (" + e.VendorCode + ")"
	}
	return msg
}

// OpenAI sends 429 both for a rate limit and for an account without credit; only the rate limit clears on a retry.
func (e *StatusError) Permanent() bool {
	if e.Code == http.StatusTooManyRequests && e.VendorCode == "insufficient_quota" {
		return true
	}
	return e.Code < 500 && e.Code != http.StatusRequestTimeout && e.Code != http.StatusTooManyRequests
}

func (e *StatusError) ResponseBody() string { return e.body }

// Following a redirect could send a report to a host that the operator did not configure.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func Do(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req) //nolint:gosec // G704: URLs come from operator config, never from API callers.
	if err != nil {
		return errorWithoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newStatusError(resp)
	}
	return decode(resp.Body, out)
}

func errorWithoutURL(err error) error {
	ue, ok := errors.AsType[*url.Error](err)
	if !ok {
		return err
	}
	if opErr, ok := errors.AsType[*net.OpError](ue.Err); ok {
		return fmt.Errorf("%s request: %w", ue.Op, opErr)
	}
	if ue.Timeout() {
		return fmt.Errorf("%s request: timeout", ue.Op)
	}
	return fmt.Errorf("%s request failed", ue.Op)
}

func newStatusError(resp *http.Response) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return &StatusError{Code: resp.StatusCode, VendorCode: vendorCode(body), body: string(body)}
}

func decode(body io.Reader, out any) error {
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func vendorCode(body []byte) string {
	var reply errorReply
	if json.Unmarshal(body, &reply) != nil {
		return ""
	}
	code, _ := reply.Error.Code.(string)
	for _, c := range []string{code, reply.Error.Type, reply.Error.Status, reply.Category} {
		if codeShape.MatchString(c) {
			return c
		}
	}
	return ""
}

package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestRunRejectsBadConfigBeforeConnecting(t *testing.T) {
	env := map[string]string{"AI_PROVIDER": "gemini", "DESTINATION": "hubspot"}
	err := run(context.Background(), func(k string) string { return env[k] }, io.Discard)
	want := "config:\nDATABASE_URL is required\nGEMINI_API_KEY is required\nHUBSPOT_TOKEN is required"
	if err == nil || err.Error() != want {
		t.Errorf("run error = %v, want %q", err, want)
	}
}

func TestConnectGivesUpOnASilentDatabase(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := connect(t.Context(), "postgres://u:p@"+ln.Addr().String()+"/db?sslmode=disable", 200*time.Millisecond)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("connect succeeded against a silent database")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connect did not give up within its timeout")
	}
}

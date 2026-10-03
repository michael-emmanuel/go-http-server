package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func liveServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/live" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestHealthcheckSucceedsAgainstLiveInstance(t *testing.T) {
	if err := healthcheck(liveServer(t, http.StatusOK)); err != nil {
		t.Fatal(err)
	}
}

func TestHealthcheckFailsOnNon200(t *testing.T) {
	err := healthcheck(liveServer(t, http.StatusServiceUnavailable))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want a 503 failure", err)
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // the port is now known to be closed

	if err := healthcheck(addr); err == nil {
		t.Fatal("expected a connection error")
	}
}

func TestHealthcheckRejectsMalformedAddr(t *testing.T) {
	if err := healthcheck("not-an-address"); err == nil {
		t.Fatal("expected an address parse error")
	}
}

func TestHealthcheckNormalizesWildcardHost(t *testing.T) {
	// ":port" and "0.0.0.0:port" mean "all interfaces" for listening but are
	// not connectable targets; the probe must talk to loopback instead.
	addr := liveServer(t, http.StatusOK)
	_, port, _ := net.SplitHostPort(addr)
	for _, wildcard := range []string{":" + port, "0.0.0.0:" + port} {
		if err := healthcheck(wildcard); err != nil {
			t.Errorf("healthcheck(%q): %v", wildcard, err)
		}
	}
}

func TestRunRejectsUnknownSubcommand(t *testing.T) {
	err := run([]string{"frobnicate"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFailsFastOnInvalidConfig(t *testing.T) {
	t.Setenv("HTTP_READ_HEADER_TIMEOUT", "0s")
	err := run(nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP_READ_HEADER_TIMEOUT") {
		t.Fatalf("err = %v, want a config validation error naming the variable", err)
	}
}

func TestRunFailsFastOnUnparseableConfig(t *testing.T) {
	t.Setenv("HTTP_READ_TIMEOUT", "soon")
	if err := run(nil); err == nil {
		t.Fatal("expected a parse error")
	}
}

package unifi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCreateNetwork_ServerErrorIsNotRetried proves a non-idempotent write is
// attempted exactly once when the controller answers 5xx.
//
// retryablehttp's DefaultRetryPolicy retries every 5xx regardless of HTTP
// method, so a POST that partially succeeded before the controller errored was
// re-sent for the whole retry budget — observed against Network 10.6.101 as
// five POSTs to /rest/networkconf with ~1s/2s/4s/8s backoff, each of which can
// create a duplicate object.
func TestCreateNetwork_ServerErrorIsNotRetried(t *testing.T) {
	var posts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleNewStyleSetup(w, r) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/proxy/network/api/s/default/rest/networkconf" {
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"meta":{"rc":"error","msg":"api.err.ServerError"},"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// A short-but-real retry budget: the point is that it must not be spent on
	// a POST at all, not that it happens to be small.
	retryMax := 2
	c, err := New(context.Background(), &Config{BaseURL: srv.URL, APIKey: "test-key", RetryMax: &retryMax})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	name := "lab"
	net := &Network{Name: &name, Purpose: PurposeCorporate, Enabled: true}
	if _, err := c.CreateNetwork(context.Background(), "default", net); err == nil {
		t.Fatal("expected CreateNetwork to fail on HTTP 500")
	}

	if n := atomic.LoadInt32(&posts); n != 1 {
		t.Fatalf("POST must not be retried on 5xx (duplicate-create risk): want exactly 1 attempt, got %d", n)
	}
}

// TestListNetwork_ServerErrorIsRetriedForGET guards the other side of the fix:
// a transient 5xx on an idempotent read is still retried, so narrowing the
// policy does not make ordinary reads brittle.
func TestListNetwork_ServerErrorIsRetriedForGET(t *testing.T) {
	var gets int32
	const failedAttempts = 1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleNewStyleSetup(w, r) {
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/s/default/rest/networkconf" {
			if atomic.AddInt32(&gets, 1) <= failedAttempts {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	retryMax := 2
	c, err := New(context.Background(), &Config{BaseURL: srv.URL, APIKey: "test-key", RetryMax: &retryMax})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.ListNetwork(context.Background(), "default"); err != nil {
		t.Fatalf("GET should be retried through a transient 5xx, got: %v", err)
	}
	if n := atomic.LoadInt32(&gets); n != failedAttempts+1 {
		t.Fatalf("expected %d GET attempts, got %d", failedAttempts+1, n)
	}
}

// TestDeleteNetwork_ServerErrorIsRetried documents that DELETE (idempotent by
// HTTP semantics, and by-id on this API) keeps its retry budget.
func TestDeleteNetwork_ServerErrorIsRetried(t *testing.T) {
	var deletes int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleNewStyleSetup(w, r) {
			return
		}
		if r.Method == http.MethodDelete {
			if atomic.AddInt32(&deletes, 1) <= 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	retryMax := 2
	c, err := New(context.Background(), &Config{BaseURL: srv.URL, APIKey: "test-key", RetryMax: &retryMax})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.DeleteNetwork(context.Background(), "default", "abc", "lab"); err != nil {
		t.Fatalf("DELETE should be retried through a transient 5xx, got: %v", err)
	}
	if n := atomic.LoadInt32(&deletes); n != 2 {
		t.Fatalf("expected 2 DELETE attempts, got %d", n)
	}
}

// TestErrorHandler_SurfacesLastStatusAndBody proves the final error after a
// spent retry budget names the status code and a snippet of the controller's
// body, instead of the opaque "giving up after N attempt(s)" that made a
// reproducible 500 visible only under TF_LOG=DEBUG.
func TestErrorHandler_SurfacesLastStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleNewStyleSetup(w, r) {
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"meta":{"rc":"error","msg":"api.err.Invalid"}}`))
	}))
	defer srv.Close()

	retryMax := 1
	c, err := New(context.Background(), &Config{BaseURL: srv.URL, APIKey: "test-key", RetryMax: &retryMax})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.ListNetwork(context.Background(), "default")
	if err == nil {
		t.Fatal("expected an error after the retry budget is spent")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("error should name the last status code, got: %v", msg)
	}
	if !strings.Contains(msg, "api.err.Invalid") {
		t.Errorf("error should include a snippet of the last response body, got: %v", msg)
	}
}

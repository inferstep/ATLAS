package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEraseStopsWhenTheRequestIsCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Timeout: 2 * time.Second}

	start := time.Now()
	if eraseOneSlot(ctx, client, srv.URL, 0) {
		t.Error("a cancelled erase was reported clear")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancellation should stop the retries promptly, took %v", elapsed)
	}
}

// The classification, pinned status by status through eraseOneSlot. A
// permanent answer returns after exactly one request and without sleeping; a
// transient one is retried exactly twice more with the bounded pause and
// reported stale; a transient one that clears on the second try is reported
// clear. The matrix is the one eraseRetryable documents against the pinned
// llama-server's error types.
var (
	erasePermanent = []int{
		http.StatusBadRequest,       // "Invalid slot ID" / "Invalid action"
		http.StatusUnauthorized,     // api key refused
		http.StatusForbidden,        // permission
		http.StatusNotFound,         // no such route (every test fake here)
		http.StatusMethodNotAllowed, //
		http.StatusGone,             //
		http.StatusNotImplemented,   // slots endpoint or action not enabled
		http.StatusTeapot,           // an unlisted 4xx is still an answer
	}
	eraseTransient = []int{
		http.StatusRequestTimeout,      // 408
		http.StatusConflict,            // 409
		http.StatusTooEarly,            // 425
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503: loading, or no slot free
		http.StatusGatewayTimeout,      // 504
	}
)

func TestEraseDoesNotRetryADefinitiveRefusal(t *testing.T) {
	for _, status := range erasePermanent {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			http.Error(w, "no", status)
		}))
		client := &http.Client{Timeout: 2 * time.Second}

		start := time.Now()
		ok := eraseOneSlot(context.Background(), client, srv.URL, 0)
		elapsed := time.Since(start)
		srv.Close()

		if ok {
			t.Errorf("status %d: a refused erase was reported clear", status)
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Errorf("status %d: expected exactly one call, got %d", status, got)
		}
		if elapsed > 400*time.Millisecond {
			t.Errorf("status %d: a definitive refusal should not wait, took %v", status, elapsed)
		}
	}
}

// A transient answer is retried exactly as designed: three attempts, with
// 0.5s and then 1.0s between them, and no more. The bound is measured, not
// assumed, because the whole cost of this path is the sleeping.
func TestEraseRetriesATransientAnswerExactlyAsBounded(t *testing.T) {
	for _, status := range eraseTransient {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			http.Error(w, "not yet", status)
		}))
		client := &http.Client{Timeout: 2 * time.Second}

		start := time.Now()
		ok := eraseOneSlot(context.Background(), client, srv.URL, 0)
		elapsed := time.Since(start)
		srv.Close()

		if ok {
			t.Errorf("status %d: a slot that never cleared was reported clear", status)
		}
		if got := atomic.LoadInt32(&calls); got != 3 {
			t.Errorf("status %d: expected exactly three attempts, got %d", status, got)
		}
		if elapsed < 1400*time.Millisecond || elapsed > 2500*time.Millisecond {
			t.Errorf("status %d: retries should take ~1.5s of bounded pause, took %v", status, elapsed)
		}
	}
}

// A transient answer that clears is reported clear, on the attempt it clears.
// llama-server handles an erase on its main loop, so a slot that is
// mid-decode answers only once it frees up. Given up after one attempt, the
// slot still holds the previous session's KV: the cross-session bleed the
// erase exists to prevent.
func TestEraseSucceedsWhenATransientStateClears(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusConflict, http.StatusServiceUnavailable} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&calls, 1) == 1 {
				http.Error(w, "not yet", status)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		client := &http.Client{Timeout: 2 * time.Second}
		ok := eraseOneSlot(context.Background(), client, srv.URL, 0)
		srv.Close()

		if !ok {
			t.Errorf("status %d then 200: a slot that cleared was reported stale", status)
		}
		if got := atomic.LoadInt32(&calls); got != 2 {
			t.Errorf("status %d then 200: expected a single retry, got %d call(s)", status, got)
		}
	}
}

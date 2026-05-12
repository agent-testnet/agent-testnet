package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter_AllowUnderLimit(t *testing.T) {
	rl := &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    5,
		window:   time.Minute,
	}

	for i := range 5 {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
}

func TestRateLimiter_BlockOverLimit(t *testing.T) {
	rl := &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    3,
		window:   time.Minute,
	}

	for range 3 {
		rl.allow("1.2.3.4")
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("4th request should be blocked")
	}
}

func TestRateLimiter_DifferentIPs(t *testing.T) {
	rl := &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    2,
		window:   time.Minute,
	}

	rl.allow("1.1.1.1")
	rl.allow("1.1.1.1")
	if rl.allow("1.1.1.1") {
		t.Fatal("IP 1 should be blocked after 2 requests")
	}
	if !rl.allow("2.2.2.2") {
		t.Fatal("IP 2 should still be allowed")
	}
}

func TestRateLimiter_WindowExpiry(t *testing.T) {
	rl := &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    2,
		window:   50 * time.Millisecond,
	}

	rl.allow("1.2.3.4")
	rl.allow("1.2.3.4")
	if rl.allow("1.2.3.4") {
		t.Fatal("should be blocked at limit")
	}

	time.Sleep(60 * time.Millisecond)
	if !rl.allow("1.2.3.4") {
		t.Fatal("should be allowed after window expires")
	}
}

func TestRateLimiter_Wrap(t *testing.T) {
	rl := &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    1,
		window:   time.Minute,
	}

	handler := rl.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// First request should pass
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rr.Code)
	}

	// Second request should be rate limited
	rr2 := httptest.NewRecorder()
	handler(rr2, req)
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", rr2.Code)
	}
}

func TestRateLimiter_WrapNoPort(t *testing.T) {
	rl := &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    1,
		window:   time.Minute,
	}

	handler := rl.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "1.2.3.4" // no port
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

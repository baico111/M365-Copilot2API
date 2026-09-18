package web

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// TestNoGoroutineLeakOnRequests drives a batch of lightweight endpoints and
// asserts the handler set does not leak goroutines once responses complete.
// A whole-process goroutine delta is used because the process is otherwise
// quiescent during the test.
func TestNoGoroutineLeakOnRequests(t *testing.T) {
	s := newTestServerForAutoCleanup(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	// Warm up so any one-time goroutines (persist loop, cleanup) are started
	// before the baseline is captured.
	client := &http.Client{Timeout: 3 * time.Second}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/version", nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	for i := 0; i < 25; i++ {
		for _, p := range []string{"/api/version", "/v1/models", "/api/health", "/api/admin/session"} {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+p, nil)
			req.Header.Set("Authorization", "Bearer sk-test-leak")
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}

	var final int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		final = runtime.NumGoroutine()
		if final <= baseline+4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final > baseline+8 {
		t.Fatalf("goroutine leak: baseline=%d final=%d", baseline, final)
	}
}

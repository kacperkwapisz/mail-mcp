package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestRequireBearer(t *testing.T) {
	const token = "s3cret-token-value-long-enough"
	handler := RequireBearer(token, discardLogger(), okHandler())

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"correct token", "Bearer " + token, http.StatusOK},
		{"lowercase scheme", "bearer " + token, http.StatusOK},
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + token, http.StatusUnauthorized},
		{"token without scheme", token, http.StatusUnauthorized},
		{"prefix of token", "Bearer s3cret", http.StatusUnauthorized},
		{"token plus suffix", "Bearer " + token + "x", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestRequireBearerAdvertisesTheScheme(t *testing.T) {
	handler := RequireBearer("token-value-long-enough", discardLogger(), okHandler())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("401 should carry a WWW-Authenticate header")
	}
}

func TestOnlyPath(t *testing.T) {
	handler := OnlyPath("/mcp", okHandler())

	cases := map[string]int{
		"/mcp":         http.StatusOK,
		"/mcp/":        http.StatusOK,
		"/mcp/session": http.StatusOK,
		"/":            http.StatusNotFound,
		"/health":      http.StatusNotFound,
		"/.env":        http.StatusNotFound,
		"/admin":       http.StatusNotFound,
	}
	for path, want := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: status = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestOnlyPathRevealsNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	OnlyPath("/mcp", okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Body.Len() != 0 {
		t.Errorf("404 body should be empty, got %q", rec.Body.String())
	}
}

func TestRateLimiterBlocksAfterLimit(t *testing.T) {
	rl := NewRateLimiter(3, 3, false)
	handler := rl.Middleware(okHandler())

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || retryAfter < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", rec.Header().Get("Retry-After"))
	}
}

func TestRateLimiterSeparatesClients(t *testing.T) {
	rl := NewRateLimiter(1, 1, false)
	handler := rl.Middleware(okHandler())

	send := func(addr string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.RemoteAddr = addr
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send("10.0.0.1:1"); code != http.StatusOK {
		t.Fatalf("first client: %d", code)
	}
	if code := send("10.0.0.1:1"); code != http.StatusTooManyRequests {
		t.Fatalf("first client second request: %d, want 429", code)
	}
	// A different client must not inherit the first one's exhausted budget.
	if code := send("10.0.0.2:1"); code != http.StatusOK {
		t.Fatalf("second client: %d, want 200", code)
	}
}

func TestRateLimiterSeparatesMethods(t *testing.T) {
	// Polling GETs must not consume the budget real tool calls need.
	rl := NewRateLimiter(1, 5, false)
	handler := rl.Middleware(okHandler())

	send := func(method string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/mcp", nil)
		req.RemoteAddr = "10.0.0.1:1"
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	_ = send(http.MethodGet)
	if code := send(http.MethodGet); code != http.StatusTooManyRequests {
		t.Fatalf("GET should be exhausted, got %d", code)
	}
	if code := send(http.MethodPost); code != http.StatusOK {
		t.Fatalf("POST should have its own budget, got %d", code)
	}
}

func TestRateLimiterIgnoresSpoofedForwardedForByDefault(t *testing.T) {
	// Without a declared trusted proxy, honouring X-Forwarded-For would let
	// any caller mint a fresh budget per request.
	rl := NewRateLimiter(1, 1, false)
	handler := rl.Middleware(okHandler())

	send := func(xff string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.RemoteAddr = "10.0.0.1:1"
		req.Header.Set("X-Forwarded-For", xff)
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	_ = send("1.1.1.1")
	if code := send("2.2.2.2"); code != http.StatusTooManyRequests {
		t.Errorf("rotating X-Forwarded-For bypassed the limit: %d", code)
	}
}

func TestRateLimiterUsesForwardedForWhenTrusted(t *testing.T) {
	rl := NewRateLimiter(1, 1, true)
	handler := rl.Middleware(okHandler())

	send := func(xff string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.RemoteAddr = "10.0.0.1:1"
		req.Header.Set("X-Forwarded-For", xff)
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send("1.1.1.1"); code != http.StatusOK {
		t.Fatalf("first: %d", code)
	}
	// Distinct real clients behind the proxy get distinct budgets.
	if code := send("2.2.2.2"); code != http.StatusOK {
		t.Errorf("distinct forwarded client was limited: %d", code)
	}
	if code := send("1.1.1.1"); code != http.StatusTooManyRequests {
		t.Errorf("repeat forwarded client was not limited: %d", code)
	}
}

func TestRateLimiterTakesRightmostForwardedEntry(t *testing.T) {
	// A client can prepend entries; only the rightmost was appended by the
	// proxy we trust.
	rl := NewRateLimiter(1, 1, true)
	handler := rl.Middleware(okHandler())

	send := func(xff string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.RemoteAddr = "10.0.0.1:1"
		req.Header.Set("X-Forwarded-For", xff)
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	_ = send("spoofed-a, 9.9.9.9")
	if code := send("spoofed-b, 9.9.9.9"); code != http.StatusTooManyRequests {
		t.Errorf("prepending entries bypassed the limit: %d", code)
	}
}

func TestRateLimiterDisabledWhenLimitIsZero(t *testing.T) {
	rl := NewRateLimiter(0, 0, false)
	handler := rl.Middleware(okHandler())
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.RemoteAddr = "10.0.0.1:1"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d limited despite a zero limit", i+1)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	SecurityHeaders(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestLogRequestsPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	LogRequests(discardLogger(), okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("logging middleware altered the response: %d %q", rec.Code, rec.Body.String())
	}
}

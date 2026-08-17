// Package httpx wraps the MCP handler with the protections a mailbox server
// exposed to a network needs: mandatory authentication, rate limiting, and a
// closed surface area.
package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RequireBearer rejects any request without the exact bearer token.
//
// There is no unauthenticated mode: this server can read and send a person's
// mail, so an accidental deployment without a token would be a mailbox
// handed to the internet.
func RequireBearer(token string, logger *slog.Logger, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		// Compare unconditionally so a missing header and a wrong token take
		// the same amount of time.
		valid := subtle.ConstantTimeCompare([]byte(presented), expected) == 1
		if !ok || !valid {
			logger.Warn("rejected unauthenticated request",
				"remote", clientIP(r), "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="mail-mcp"`)
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// OnlyPath returns 404 for every request outside the given prefix.
//
// An empty body reveals nothing about what is running here, which keeps the
// endpoint uninteresting to opportunistic scanners.
func OnlyPath(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RateLimiter is a per-client sliding window limiter.
//
// GET and POST get separate budgets: polling should not be able to starve
// real tool calls, and a burst of tool calls should not look like abuse.
type RateLimiter struct {
	getPerMinute  int
	postPerMinute int
	window        time.Duration
	maxTracked    int
	trustedProxy  bool

	mu          sync.Mutex
	hits        map[string][]time.Time
	lastCleanup time.Time
}

// NewRateLimiter builds a limiter. Non-positive limits disable that bucket.
func NewRateLimiter(getPerMinute, postPerMinute int, trustProxyHeaders bool) *RateLimiter {
	return &RateLimiter{
		getPerMinute:  getPerMinute,
		postPerMinute: postPerMinute,
		window:        time.Minute,
		maxTracked:    4096,
		trustedProxy:  trustProxyHeaders,
		hits:          make(map[string][]time.Time),
		lastCleanup:   time.Now(),
	}
}

// Middleware wraps next with rate limiting.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := rl.getPerMinute
		if r.Method == http.MethodPost {
			limit = rl.postPerMinute
		}
		if limit <= 0 {
			next.ServeHTTP(w, r)
			return
		}

		key := rl.clientKey(r) + ":" + r.Method
		if retryAfter, limited := rl.allow(key, limit); limited {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeJSONError(w, http.StatusTooManyRequests, "rate_limited",
				"too many requests; retry in "+strconv.Itoa(retryAfter)+"s")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) allow(key string, limit int) (retryAfterSeconds int, limited bool) {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cleanupLocked(now)

	cutoff := now.Add(-rl.window)
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= limit {
		rl.hits[key] = kept
		wait := int(kept[0].Add(rl.window).Sub(now).Seconds()) + 1
		if wait < 1 {
			wait = 1
		}
		return wait, true
	}

	rl.hits[key] = append(kept, now)
	return 0, false
}

// cleanupLocked bounds memory: without it, a scanner rotating source
// addresses would grow the map without limit.
func (rl *RateLimiter) cleanupLocked(now time.Time) {
	if now.Sub(rl.lastCleanup) < rl.window {
		return
	}
	rl.lastCleanup = now
	cutoff := now.Add(-rl.window)

	for key, times := range rl.hits {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(rl.hits, key)
		}
	}

	if len(rl.hits) <= rl.maxTracked {
		return
	}
	keys := make([]string, 0, len(rl.hits))
	for k := range rl.hits {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return lastSeen(rl.hits[keys[i]]).Before(lastSeen(rl.hits[keys[j]]))
	})
	for _, k := range keys[:len(rl.hits)-rl.maxTracked] {
		delete(rl.hits, k)
	}
}

func lastSeen(times []time.Time) time.Time {
	if len(times) == 0 {
		return time.Time{}
	}
	return times[len(times)-1]
}

// clientKey identifies the caller for rate-limiting purposes.
//
// X-Forwarded-For is only consulted when the operator has declared that a
// trusted proxy sits in front; otherwise any client could spoof the header
// and sidestep the limit. Even then the rightmost entry is used, since that
// is the one the nearest proxy appended.
func (rl *RateLimiter) clientKey(r *http.Request) string {
	if rl.trustedProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return clientIP(r)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SecurityHeaders sets conservative defaults on every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// LogRequests records method, path, status, and duration.
func LogRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(started).Round(time.Millisecond),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader records the first status written and forwards it.
func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so streamed responses are not
// buffered by this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

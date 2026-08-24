package auth

import (
	"net/http"
	"sync"
	"time"
)

// Limiter is a tiny in-memory sliding-window rate limiter keyed by string
// (typically a client IP). It guards signup and tunnel-gate attempts. State
// is intentionally not persisted; a restart resets the counters, which is
// acceptable for abuse damping.
type Limiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	hits   map[string][]time.Time
	lastGC time.Time
}

func NewLimiter(window time.Duration, max int) *Limiter {
	return &Limiter{
		window: window,
		max:    max,
		hits:   make(map[string][]time.Time),
		lastGC: time.Now(),
	}
}

// Allow reports whether key may proceed and records the attempt.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	// Occasional full GC so abandoned keys don't leak.
	if now.Sub(l.lastGC) > time.Minute {
		for k, hits := range l.hits {
			kept := hits[:0]
			for _, h := range hits {
				if h.After(cutoff) {
					kept = append(kept, h)
				}
			}
			if len(kept) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = kept
			}
		}
		l.lastGC = now
	}

	hits := l.hits[key][:0]
	for _, h := range l.hits[key] {
		if h.After(cutoff) {
			hits = append(hits, h)
		}
	}
	if len(hits) >= l.max {
		l.hits[key] = hits
		return false
	}
	l.hits[key] = append(hits, now)
	return true
}

// ClientIP extracts the requesting IP, honoring Cloudflare/upstream
// proxy headers first (TLS terminates upstream in production).
func ClientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// First hop is the original client.
		for i := 0; i < len(v); i++ {
			if v[i] == ',' {
				return trimSpace(v[:i])
			}
		}
		return trimSpace(v)
	}
	host := r.RemoteAddr
	if i := len(host) - 1; i >= 0 && host[i] != ']' { // strip port unless IPv6 bracketed
		if j := lastIndexByte(host, ':'); j != -1 {
			host = host[:j]
		}
	}
	return host
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

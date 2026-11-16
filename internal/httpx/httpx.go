// Package httpx holds small HTTP helpers: client IP resolution behind
// trusted reverse proxies, an in-memory rate limiter and security headers.
package httpx

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DefaultTrustedProxies trusts only the local machine: a reverse proxy on the
// same host. Add your proxy's network (e.g. a Docker bridge) if it runs
// elsewhere.
var DefaultTrustedProxies = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}

// ParsePrefixes parses a comma/space separated list of CIDRs or IPs.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("invalid IP or CIDR %q", f)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// ClientIP returns the caller's IP. X-Forwarded-For is only honoured when
// the direct peer is a trusted proxy; it is read right to left, skipping
// trusted hops, so a client cannot spoof it.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !inAny(peer, trusted) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !inAny(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Limiter counts events per key in a sliding window. It is in memory: a
// restart resets it, which is acceptable for anti-abuse purposes.
type Limiter struct {
	Max    int
	Window time.Duration
	Now    func() time.Time

	mu     sync.Mutex
	events map[string][]time.Time
	sweep  time.Time
}

// NewLimiter allows max events per window and key.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, events: map[string][]time.Time{}}
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Limiter) prune(key string, now time.Time) []time.Time {
	ev := l.events[key]
	i := 0
	for i < len(ev) && now.Sub(ev[i]) >= l.Window {
		i++
	}
	ev = ev[i:]
	if len(ev) == 0 {
		delete(l.events, key)
	} else {
		l.events[key] = ev
	}
	// Occasionally drop idle keys so memory stays bounded.
	if now.Sub(l.sweep) > l.Window {
		l.sweep = now
		for k, e := range l.events {
			if len(e) == 0 || now.Sub(e[len(e)-1]) >= l.Window {
				delete(l.events, k)
			}
		}
	}
	return ev
}

// Allow records an event and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ev := l.prune(key, now)
	if len(ev) >= l.Max {
		return false
	}
	l.events[key] = append(ev, now)
	return true
}

// Blocked reports whether key has reached the limit, without recording.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, l.now())) >= l.Max
}

// Add records an event without checking (e.g. a failed login).
func (l *Limiter) Add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.events[key] = append(l.prune(key, now), now)
}

// Reset forgets a key (e.g. after a successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.events, key)
}

// SecureHTML sets headers for server-rendered HTML pages: no scripts at
// all, no framing, no referrer, no caching.
func SecureHTML(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
}

// CrossSite reports whether a browser flagged the request as cross-site
// (Fetch Metadata). Requests without the header (old browsers, curl) are
// not flagged; CSRF tokens cover those.
func CrossSite(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") == "cross-site"
}

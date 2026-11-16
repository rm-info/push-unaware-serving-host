package httpx

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	trusted, err := ParsePrefixes("127.0.0.0/8, ::1, 172.18.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ remote, xff, want string }{
		{"203.0.113.9:1234", "", "203.0.113.9"},
		{"203.0.113.9:1234", "1.2.3.4", "203.0.113.9"},              // untrusted peer: header ignored
		{"127.0.0.1:1234", "198.51.100.7", "198.51.100.7"},          // local proxy
		{"127.0.0.1:1234", "6.6.6.6, 198.51.100.7", "198.51.100.7"}, // spoofed left part ignored
		{"172.18.0.5:1234", "198.51.100.7, 127.0.0.1", "198.51.100.7"},
		{"127.0.0.1:1234", "", "127.0.0.1"},
		{"127.0.0.1:1234", "garbage", "127.0.0.1"},
		{"[::1]:1234", "2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientIP(r, trusted); got != c.want {
			t.Errorf("%s / %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	if _, err := ParsePrefixes("nope"); err == nil {
		t.Error("bad prefix accepted")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLimiter(2, time.Minute)
	l.Now = func() time.Time { return now }
	if !l.Allow("a") || !l.Allow("a") || l.Allow("a") {
		t.Fatal("limit not enforced")
	}
	if !l.Allow("b") {
		t.Fatal("keys not independent")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("a") {
		t.Fatal("window not sliding")
	}
	l.Add("c")
	l.Add("c")
	if !l.Blocked("c") {
		t.Fatal("Blocked")
	}
	l.Reset("c")
	if l.Blocked("c") {
		t.Fatal("Reset")
	}
}

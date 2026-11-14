// Package pushsend delivers Web Push messages (RFC 8030/8291/8292) and
// validates push endpoints.
package pushsend

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// DefaultPushHosts are the push services of mainstream browsers. Endpoints
// are restricted to these hosts (and their subdomains) by default so the
// server cannot be used to POST to arbitrary URLs.
var DefaultPushHosts = []string{
	"fcm.googleapis.com",                // Chrome, Edge (Android), Samsung Internet, Opera, Brave
	"android.googleapis.com",            // legacy GCM endpoints
	"updates.push.services.mozilla.com", // Firefox
	"push.services.mozilla.com",         // Firefox
	"push.apple.com",                    // Safari / iOS PWAs (web.push.apple.com)
	"notify.windows.com",                // Edge on Windows (WNS)
}

// ValidateEndpoint checks a push endpoint URL. allowedHosts nil or empty
// means any HTTPS host is accepted.
func ValidateEndpoint(endpoint string, allowedHosts []string) error {
	if len(endpoint) > 2048 {
		return errors.New("endpoint too long")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("endpoint must be an https URL")
	}
	if len(allowedHosts) == 0 {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range allowedHosts {
		h = strings.ToLower(strings.TrimPrefix(h, "."))
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("push service %q is not allowed", host)
}

// ValidateKeys checks the subscription keys decode to the expected sizes.
func ValidateKeys(p256dh, auth string) error {
	if b, err := decode(p256dh); err != nil || len(b) != 65 || b[0] != 4 {
		return errors.New("invalid keys.p256dh")
	}
	if b, err := decode(auth); err != nil || len(b) != 16 {
		return errors.New("invalid keys.auth")
	}
	return nil
}

func decode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// Message is one push to deliver.
type Message struct {
	Endpoint string
	P256dh   string
	Auth     string
	Payload  []byte
	Topic    string // optional collapse key (max 32 base64url chars)
}

// Result is the push service's answer.
type Result struct {
	Status     int           // HTTP status, 0 on transport error
	RetryAfter time.Duration // from Retry-After, if any
}

// Sender sends push messages.
type Sender interface {
	Send(ctx context.Context, m Message) (Result, error)
}

// WebPush sends real Web Push messages signed with the server's VAPID key.
type WebPush struct {
	PublicKey  string
	PrivateKey string
	Subject    string        // "mailto:..." or "https://..."
	TTL        time.Duration // how long the push service keeps an undelivered message
	Client     *http.Client
	// AllowedHosts is re-checked at send time (defense in depth).
	AllowedHosts []string
}

// Send implements Sender.
func (w *WebPush) Send(ctx context.Context, m Message) (Result, error) {
	if err := ValidateEndpoint(m.Endpoint, w.AllowedHosts); err != nil {
		return Result{}, err
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// webpush-go v1.4.0 prepends "mailto:" to anything that is not an
	// https URL, so strip our own prefix to avoid "mailto:mailto:".
	subject := strings.TrimPrefix(w.Subject, "mailto:")
	resp, err := webpush.SendNotificationWithContext(ctx, m.Payload, &webpush.Subscription{
		Endpoint: m.Endpoint,
		Keys:     webpush.Keys{P256dh: m.P256dh, Auth: m.Auth},
	}, &webpush.Options{
		HTTPClient:      client,
		Subscriber:      subject,
		Topic:           m.Topic,
		TTL:             int(w.TTL / time.Second),
		Urgency:         webpush.UrgencyHigh,
		VAPIDPublicKey:  w.PublicKey,
		VAPIDPrivateKey: w.PrivateKey,
	})
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	res := Result{Status: resp.StatusCode}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			res.RetryAfter = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			res.RetryAfter = time.Until(t)
		}
	}
	return res, nil
}

// Topic derives a stable collapse key from an opaque ID, so that a device
// that was offline gets at most one pending push per reminder.
func Topic(opaqueID string) string {
	h := sha256.Sum256([]byte(opaqueID))
	return base64.RawURLEncoding.EncodeToString(h[:24]) // 32 chars
}

package pushsend

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"golang.org/x/crypto/hkdf"
)

// TestSendEndToEnd plays the browser's push service: it checks the VAPID
// header and decrypts the RFC 8291 payload with the subscription's keys.
func TestSendEndToEnd(t *testing.T) {
	uaPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	rand.Read(authSecret)
	vPriv, vPub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}

	var gotPlain []byte
	var gotHeaders http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		gotPlain, err = decrypt(body, uaPriv, authSecret)
		if err != nil {
			t.Errorf("decrypt: %v", err)
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	w := &WebPush{PublicKey: vPub, PrivateKey: vPriv, Subject: "mailto:ops@push.test", TTL: time.Hour, Client: srv.Client()}
	res, err := w.Send(context.Background(), Message{
		Endpoint: srv.URL + "/push/abc",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaPriv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(authSecret),
		Payload:  []byte(`{"id":"r1","t":1}`),
		Topic:    Topic("r1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 201 || res.RetryAfter != 2*time.Minute {
		t.Fatalf("result %+v", res)
	}
	if string(gotPlain) != `{"id":"r1","t":1}` {
		t.Fatalf("plaintext %q", gotPlain)
	}
	if gotHeaders.Get("TTL") != "3600" || gotHeaders.Get("Urgency") != "high" || len(gotHeaders.Get("Topic")) != 32 {
		t.Fatalf("headers %v", gotHeaders)
	}
	// VAPID: "vapid t=<jwt>, k=<public key>", sub must be a single mailto:.
	auth := gotHeaders.Get("Authorization")
	if !strings.HasPrefix(auth, "vapid t=") || !strings.Contains(auth, ", k="+strings.TrimRight(vPub, "=")) {
		t.Fatalf("authorization %q", auth)
	}
	jwt := strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(auth, "vapid t="), ",", 2)[0], ",")
	claimsB64 := strings.Split(jwt, ".")[1]
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(claimsB64)
	var claims struct {
		Aud, Sub string
	}
	json.Unmarshal(claimsJSON, &claims)
	if claims.Sub != "mailto:ops@push.test" || claims.Aud != srv.URL {
		t.Fatalf("claims %s", claimsJSON)
	}
}

// decrypt implements the user-agent side of RFC 8291 (aes128gcm).
func decrypt(body []byte, uaPriv *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	idlen := int(body[20])
	asPubBytes := body[21 : 21+idlen]
	ciphertext := body[21+idlen:]
	_ = rs

	asPub, err := ecdh.P256().NewPublicKey(asPubBytes)
	if err != nil {
		return nil, err
	}
	shared, err := uaPriv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	info := append([]byte("WebPush: info\x00"), uaPriv.PublicKey().Bytes()...)
	info = append(info, asPubBytes...)
	ikm := make([]byte, 32)
	io.ReadFull(hkdf.New(sha256.New, shared, authSecret, info), ikm)
	cek := make([]byte, 16)
	io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: aes128gcm\x00")), cek)
	nonce := make([]byte, 12)
	io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: nonce\x00")), nonce)

	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	// Strip padding: content, 0x02 delimiter, zeros.
	i := len(plain) - 1
	for i >= 0 && plain[i] == 0 {
		i--
	}
	return plain[:i], nil
}

func TestValidateEndpoint(t *testing.T) {
	ok := []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://web.push.apple.com/QGx",
		"https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://wns2-par02p.notify.windows.com/w/?token=x",
	}
	for _, e := range ok {
		if err := ValidateEndpoint(e, DefaultPushHosts); err != nil {
			t.Errorf("%s: %v", e, err)
		}
	}
	bad := []string{
		"http://fcm.googleapis.com/fcm/send/abc",
		"https://evil.example/fcm.googleapis.com",
		"https://fcm.googleapis.com.evil.example/x",
		"https://notpush.apple.com.example/x",
		"https://user:pw@fcm.googleapis.com/x",
		"https://127.0.0.1/x",
		"ftp://web.push.apple.com/x",
		"not a url",
	}
	for _, e := range bad {
		if err := ValidateEndpoint(e, DefaultPushHosts); err == nil {
			t.Errorf("%s accepted", e)
		}
	}
	if err := ValidateEndpoint("https://my.own.push/x", nil); err != nil {
		t.Error(err)
	}
	if err := ValidateEndpoint("http://my.own.push/x", nil); err == nil {
		t.Error("http accepted")
	}
}

func TestValidateKeys(t *testing.T) {
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	p := base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
	a := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	if err := ValidateKeys(p, a); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKeys(p+"==", base64.URLEncoding.EncodeToString(make([]byte, 16))); err != nil {
		t.Fatal("padded keys rejected:", err)
	}
	if ValidateKeys("abc", a) == nil || ValidateKeys(p, "abc") == nil {
		t.Fatal("bad keys accepted")
	}
}

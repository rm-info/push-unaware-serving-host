// Package vapid loads or creates the server's single VAPID key pair.
//
// Every push subscription is bound to the public key it was created with:
// losing or changing this key kills every subscription of every app.
package vapid

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// FileName is the key file name inside the data directory.
const FileName = "vapid.json"

// Keys is the VAPID key pair (base64url, as produced by webpush-go).
type Keys struct {
	PublicKey  string    `json:"public_key"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
}

// LoadOrCreate reads dir/vapid.json, generating it on first run. created
// reports whether a new pair was just generated.
func LoadOrCreate(dir string) (k Keys, created bool, err error) {
	path := filepath.Join(dir, FileName)
	k, err = Load(dir)
	if err == nil {
		return k, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Keys{}, false, err
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return Keys{}, false, err
	}
	k = Keys{PublicKey: pub, PrivateKey: priv, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	b, _ := json.MarshalIndent(k, "", "  ")
	// O_EXCL: never overwrite an existing key, even in a race.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Keys{}, false, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return Keys{}, false, err
	}
	if err := f.Close(); err != nil {
		return Keys{}, false, err
	}
	return k, true, nil
}

// Load reads dir/vapid.json.
func Load(dir string) (Keys, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return Keys{}, err
	}
	var k Keys
	if err := json.Unmarshal(b, &k); err != nil {
		return Keys{}, fmt.Errorf("%s: %w", FileName, err)
	}
	if k.PublicKey == "" || k.PrivateKey == "" {
		return Keys{}, fmt.Errorf("%s: missing key", FileName)
	}
	return k, nil
}

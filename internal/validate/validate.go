// Package validate holds input rules shared by the CLI, the admin UI and the
// public key-request form.
package validate

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// AppName checks an application name: 2-63 chars, lowercase letters,
// digits and dashes, starting with a letter or digit.
func AppName(name string) error {
	if !appNameRe.MatchString(name) {
		return errors.New("name must be 2-63 characters: lowercase letters, digits and dashes, not starting with a dash")
	}
	return nil
}

// Origins parses a whitespace/comma separated list of browser origins
// ("https://host[:port]"). "*" means any origin. If requireOne is true the
// list must not be empty and "*" is refused.
func Origins(s string, requireOne bool) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, o := range fields {
		if o == "*" {
			if requireOne {
				return nil, errors.New(`"*" is not accepted here: list your origins`)
			}
			out = append(out, o)
			continue
		}
		o = strings.TrimSuffix(o, "/")
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("invalid origin %q (want scheme://host[:port], no path)", o)
		}
		if requireOne && u.Scheme != "https" {
			return nil, fmt.Errorf("origin %q must use https (Web Push requires it)", o)
		}
		o = strings.ToLower(u.Scheme + "://" + u.Host)
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if len(out) > 20 {
		return nil, errors.New("at most 20 origins")
	}
	if requireOne && len(out) == 0 {
		return nil, errors.New("at least one origin is required (where your PWA is served, e.g. https://app.example.org)")
	}
	return out, nil
}

// Contact checks an e-mail address used to reach an app's maintainer.
func Contact(s string) (string, error) {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return "", errors.New("a valid e-mail address is required")
	}
	return s, nil
}

// Text trims and bounds a free-text field.
func Text(s, field string, maxRunes int, required bool) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if required && s == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%s is not valid UTF-8", field)
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return "", fmt.Errorf("%s is too long (max %d characters)", field, maxRunes)
	}
	return s, nil
}

// Quota checks a quota value.
func Quota(n int, field string) error {
	if n < 1 || n > 10_000_000 {
		return fmt.Errorf("%s must be between 1 and 10000000", field)
	}
	return nil
}

// Password checks an admin password's strength (length only: long
// passphrases beat composition rules).
func Password(p string) error {
	if utf8.RuneCountInString(p) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	if len(p) > 72 {
		return errors.New("password must be at most 72 bytes (bcrypt limit)")
	}
	return nil
}

// Username checks an admin username.
func Username(u string) error {
	if !regexp.MustCompile(`^[a-zA-Z0-9._-]{2,32}$`).MatchString(u) {
		return errors.New("username must be 2-32 characters: letters, digits, dot, dash, underscore")
	}
	return nil
}

package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Two-step login with an authenticator app (RFC 6238, 30 s, 6 digits).

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret makes a secret and the otpauth:// link for authenticator apps.
func NewTOTPSecret(username string) (secret, link string) {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	secret = b32.EncodeToString(b)
	link = fmt.Sprintf("otpauth://totp/ZeroOS:%s?secret=%s&issuer=ZeroOS", url.PathEscape(username), secret)
	return
}

func totpAt(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(t.Unix()/30))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1000000), nil
}

// CheckTOTP accepts the current code and one step either side for clock drift.
func CheckTOTP(secret, code string) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return false
	}
	now := time.Now()
	for _, d := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		if c, err := totpAt(secret, now.Add(d)); err == nil && hmac.Equal([]byte(c), []byte(code)) {
			return true
		}
	}
	return false
}

// EnableTOTP stores the secret after the user proved it works with a code.
func (a *Auth) EnableTOTP(userID int64, secret, code string) error {
	if !CheckTOTP(secret, code) {
		return errors.New("That code didn't match. Check the time on your phone and try again.")
	}
	enc, err := a.c.Encrypt(secret)
	if err != nil {
		return err
	}
	_, err = a.c.DB.Exec(`UPDATE users SET totp_secret=? WHERE id=?`, enc, userID)
	return err
}

// DisableTOTP turns two-step login off.
func (a *Auth) DisableTOTP(userID int64) error {
	_, err := a.c.DB.Exec(`UPDATE users SET totp_secret='' WHERE id=?`, userID)
	return err
}

func (a *Auth) totpSecret(userID int64) string {
	var enc string
	_ = a.c.DB.QueryRow(`SELECT totp_secret FROM users WHERE id=?`, userID).Scan(&enc)
	s, _ := a.c.Decrypt(enc)
	return s
}

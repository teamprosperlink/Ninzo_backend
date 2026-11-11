package console

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP is defined on HMAC-SHA1, which authenticator apps use.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Password hashing: PBKDF2-SHA256, from the standard library, with OWASP's count for it.
const (
	hashIterations = 600_000
	saltBytes      = 16
	keyBytes       = 32
	totpStep       = 30 * time.Second
	totpDigits     = 6
)

// HashPassword returns "pbkdf2-sha256$iterations$salt$key", all base64.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, keyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", hashIterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword compares in constant time. A malformed hash never matches.
func CheckPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 100_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, n, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked against for an unknown ID, so a wrong ID takes as long as a wrong password.
var dummyHash, _ = HashPassword("not-a-real-password")

// TemporaryPassword is 20 random characters from an alphabet with no look-alikes.
func TemporaryPassword() (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

// Sealer encrypts TOTP secrets at rest, with a key derived from the service's signing key.
type Sealer struct{ key []byte }

// NewSealer derives the TOTP key from the signing key.
func NewSealer(signingKey []byte) (Sealer, error) {
	key, err := hkdf.Key(sha256.New, signingKey, nil, "ninzo console totp v1", 32)
	return Sealer{key: key}, err
}

func (s Sealer) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts a secret, bound to the user it belongs to.
func (s Sealer) Seal(secret []byte, user string) ([]byte, error) {
	a, err := s.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, secret, []byte(user)), nil
}

// Open decrypts a secret sealed for this user.
func (s Sealer) Open(sealed []byte, user string) ([]byte, error) {
	a, err := s.aead()
	if err != nil {
		return nil, err
	}
	if len(sealed) < a.NonceSize() {
		return nil, errors.New("console: sealed secret too short")
	}
	return a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], []byte(user))
}

// NewTOTPSecret is 20 random bytes, as RFC 4226 recommends.
func NewTOTPSecret() ([]byte, error) {
	secret := make([]byte, 20)
	_, err := rand.Read(secret)
	return secret, err
}

// EncodeSecret is the base32 form authenticator apps take.
func EncodeSecret(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

// TOTPCode is the code for one time step (RFC 6238, HMAC-SHA1, 6 digits).
func TOTPCode(secret []byte, step int64) string {
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%0*d", totpDigits, n)
}

// StepOf is the time step a moment falls in.
func StepOf(t time.Time) int64 { return t.Unix() / int64(totpStep/time.Second) }

// MatchTOTP finds the step a code belongs to, within one step either side of now, and only after
// the last step used, so a code works once.
func MatchTOTP(secret []byte, code string, now time.Time, last int64) (int64, bool) {
	step := StepOf(now)
	for _, s := range []int64{step - 1, step, step + 1} {
		if s > last && subtle.ConstantTimeCompare([]byte(TOTPCode(secret, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

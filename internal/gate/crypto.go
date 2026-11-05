package gate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/argon2"
)

type vault struct {
	aead      cipher.AEAD
	lookupKey []byte
}

func newVault(master []byte) (*vault, error) {
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, master)
		m.Write([]byte(label))
		return m.Sum(nil)
	}
	b, err := aes.NewCipher(derive("tabsgate/encryption/v1"))
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &vault{aead: a, lookupKey: derive("tabsgate/lookup/v1")}, nil
}

func (v *vault) fingerprint(provider, key string) string {
	m := hmac.New(sha256.New, v.lookupKey)
	m.Write([]byte(provider + "\x00" + key))
	return hex.EncodeToString(m.Sum(nil))
}

func (v *vault) encrypt(id, provider, key string) []byte {
	n := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		panic(err)
	}
	return v.aead.Seal(n, n, []byte(key), []byte(id+"\x00"+provider))
}

func (v *vault) decrypt(c connection) (string, error) {
	n := v.aead.NonceSize()
	if len(c.Secret) < n {
		return "", errors.New("invalid ciphertext")
	}
	b, err := v.aead.Open(nil, c.Secret[:n], c.Secret[n:], []byte(c.ID+"\x00"+c.Provider))
	return string(b), err
}

func randomToken(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(token string) string {
	b := sha256.Sum256([]byte(token))
	return hex.EncodeToString(b[:])
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func hashWord(word string) (salt, hash []byte) {
	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	return salt, argon2.IDKey([]byte(word), salt, 2, 32*1024, 1, 32)
}
func checkWord(word string, salt, hash []byte) bool {
	if len(salt) != 16 || len(hash) != 32 {
		return false
	}
	return subtle.ConstantTimeCompare(argon2.IDKey([]byte(word), salt, 2, 32*1024, 1, 32), hash) == 1
}
func validSecret(key string) bool {
	if len(key) < 1 || len(key) > 4096 {
		return false
	}
	for _, r := range key {
		if r <= 32 || r > 126 {
			return false
		}
	}
	return true
}
func validWord(word string) bool {
	return len(word) > 0 && len(word) <= 256 && strings.TrimSpace(word) != ""
}

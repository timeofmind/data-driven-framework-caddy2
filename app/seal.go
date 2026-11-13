package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// A secret an admin supplies on the Service Configuration page (a payment
// provider's secret, a bucket's key) is kept sealed in the app's database:
// AES-GCM under a key derived from the server's token key, so a dump of
// the database does not give the keys away, and every node of a cluster
// (sharing the token key file) can open them.

const sealedPrefix = "sealed:v1:"

// sealKey derives the sealing key from the token key.
func sealKey(tokenKey []byte) []byte {
	k := sha256.Sum256(append([]byte("dd-service-settings:"), tokenKey...))
	return k[:]
}

// seal encrypts a value for storage.
func seal(tokenKey []byte, plain string) (string, error) {
	if len(tokenKey) == 0 {
		return "", errors.New("no token key to seal with")
	}
	block, err := aes.NewCipher(sealKey(tokenKey))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return sealedPrefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// unseal decrypts a stored value; a value that is not sealed is itself.
func unseal(tokenKey []byte, stored string) (string, error) {
	if !strings.HasPrefix(stored, sealedPrefix) {
		return stored, nil
	}
	if len(tokenKey) == 0 {
		return "", errors.New("no token key to unseal with")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(stored, sealedPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(sealKey(tokenKey))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed value is too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("the sealed value does not open with this server's token key")
	}
	return string(plain), nil
}

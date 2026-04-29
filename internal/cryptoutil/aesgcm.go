package cryptoutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

var (
	ErrEmptyKey      = errors.New("empty key")
	ErrInvalidKeyLen = errors.New("invalid key length (must be 32 bytes after base64 decode)")
	ErrInvalidCipher = errors.New("invalid ciphertext")
)

// NewAESGCMFromBase64Key creates an AES-256-GCM instance from a base64-encoded 32-byte key.
// Accepts both padded (StdEncoding) and unpadded (RawStdEncoding) base64 strings.
func NewAESGCMFromBase64Key(keyB64 string) (cipher.AEAD, error) {
	if keyB64 == "" {
		return nil, ErrEmptyKey
	}

	var key []byte
	if b, err := base64.RawStdEncoding.DecodeString(keyB64); err == nil {
		key = b
	} else if b, err2 := base64.StdEncoding.DecodeString(keyB64); err2 == nil {
		key = b
	} else {
		return nil, err
	}

	if len(key) != 32 {
		return nil, ErrInvalidKeyLen
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptString encrypts plaintext and returns base64(nonce||ciphertext).
func EncryptString(aead cipher.AEAD, plaintext string) (string, error) {
	if aead == nil {
		return "", errors.New("nil aead")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nil, nonce, []byte(plaintext), nil)
	out := append(nonce, ct...)
	return base64.RawStdEncoding.EncodeToString(out), nil
}

// DecryptString decrypts base64(nonce||ciphertext) and returns plaintext.
func DecryptString(aead cipher.AEAD, cipherB64 string) (string, error) {
	if aead == nil {
		return "", errors.New("nil aead")
	}
	raw, err := base64.RawStdEncoding.DecodeString(cipherB64)
	if err != nil {
		// Allow padded input in case we ever change encoders.
		raw, err = base64.StdEncoding.DecodeString(cipherB64)
		if err != nil {
			return "", err
		}
	}
	if len(raw) < aead.NonceSize()+1 {
		return "", ErrInvalidCipher
	}
	nonce := raw[:aead.NonceSize()]
	ct := raw[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}


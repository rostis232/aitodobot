package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

type AESGCM struct {
	gcm cipher.AEAD
}

func NewAESGCMFromBase64(masterKeyB64 string) (*AESGCM, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("MASTER_KEY must decode to 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AESGCM{gcm: gcm}, nil
}

func (a *AESGCM) EncryptToBase64(plaintext []byte) (string, error) {
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := a.gcm.Seal(nil, nonce, plaintext, nil)
	out := append(nonce, ciphertext...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (a *AESGCM) DecryptFromBase64(ciphertextB64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, err
	}
	ns := a.gcm.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("ciphertext too short")
	}
	nonce := raw[:ns]
	ct := raw[ns:]
	return a.gcm.Open(nil, nonce, ct, nil)
}

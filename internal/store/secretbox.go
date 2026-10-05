// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// SettingsKeySize is the length of the key that encrypts stored secrets.
const SettingsKeySize = 32

// ErrSecretUnreadable means a stored secret didn't decrypt: it was written
// with another settings key, or it was changed.
var ErrSecretUnreadable = errors.New("store: stored secret can't be decrypted with this settings key")

// SecretBox encrypts the secrets core stores (AES-256-GCM, a random nonce
// per value). The settings name, passed as additional data, binds each
// ciphertext to its column.
type SecretBox struct{ aead cipher.AEAD }

// NewSecretBox returns a SecretBox for a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != SettingsKeySize {
		return nil, fmt.Errorf("store: settings key must be %d bytes, got %d", SettingsKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

func (b *SecretBox) seal(name, plaintext string) ([]byte, error) {
	if plaintext == "" {
		return []byte{}, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(name)), nil
}

func (b *SecretBox) open(name string, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return "", ErrSecretUnreadable
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], []byte(name))
	if err != nil {
		return "", ErrSecretUnreadable
	}
	return string(plain), nil
}

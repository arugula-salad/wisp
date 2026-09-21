package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Client-side encryption is optional and off by default: the bucket here is the
// operator's own Garage, reached over WireGuard. When it is on, a chunk's ID is
// HMAC-SHA256(key, plaintext) and its body is AES-256-GCM sealed under a nonce
// derived from that ID.
//
// That is convergent encryption, and the trade-off is deliberate: identical
// plaintext produces identical ciphertext, so dedup still works, at the cost of
// letting someone who can read the bucket tell that two chunks are equal (and
// confirm a guessed chunk). Without it, an encrypted backup of a 20 GB disk would
// re-upload the whole disk every time.
//
// Everything else a backup writes (manifests, the latest pointer, tombstones) is
// sealed too, under a random nonce: a manifest carries the sprite's record, and
// that holds its environment and network policy. Only repository.json and the
// prune marker stay readable, and neither says anything about a sprite.
//
// The key never leaves this host. A key kept only on the machine the backup is
// protecting against losing is not a backup; README says so.

const keyCheckMessage = "mini-sprites-backup-key-check"

type crypter struct {
	idKey []byte // keys the chunk IDs
	aead  cipher.AEAD
}

// subkey derives an independent key for one purpose, so that the key that names
// chunks is never also the key that seals them.
func subkey(key []byte, purpose string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("mini-sprites backup: " + purpose))
	return h.Sum(nil)
}

// loadKey reads a 32-byte key from a file, as 64 hex characters (what
// `openssl rand -hex 32` writes) or as 32 raw bytes.
func loadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if trimmed := strings.TrimSpace(string(b)); len(trimmed) == 2*32 {
		key, err := hex.DecodeString(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%s: not 64 hex characters: %w", path, err)
		}
		return key, nil
	}
	if len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("%s: need a 32-byte key, as 64 hex characters or 32 raw bytes (openssl rand -hex 32 > %s)", path, path)
}

func newCrypter(key []byte) (*crypter, error) {
	if len(key) != 32 {
		return nil, errors.New("backup: key must be 32 bytes")
	}
	block, err := aes.NewCipher(subkey(key, "seal"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &crypter{idKey: subkey(key, "chunk id"), aead: aead}, nil
}

// id is the chunk's content address. A nil crypter (no encryption) addresses by
// plain SHA-256, so an unencrypted bucket is inspectable with any S3 tool.
func (c *crypter) id(plain []byte) string {
	if c == nil {
		sum := sha256.Sum256(plain)
		return hex.EncodeToString(sum[:])
	}
	h := hmac.New(sha256.New, c.idKey)
	h.Write(plain)
	return hex.EncodeToString(h.Sum(nil))
}

// keyCheck is stored in the repository so that a second machine pointed at the
// same bucket with the wrong key is told so, rather than writing chunks nobody
// can read.
func (c *crypter) keyCheck() string {
	if c == nil {
		return ""
	}
	return c.id([]byte(keyCheckMessage))
}

// seal encrypts a chunk. The nonce comes from the ID, which is itself a keyed
// hash of the plaintext, so it never repeats for different content.
func (c *crypter) seal(id string, plain []byte) ([]byte, error) {
	if c == nil {
		return plain, nil
	}
	nonce, err := c.nonce(id)
	if err != nil {
		return nil, err
	}
	return c.aead.Seal(nil, nonce, plain, nil), nil
}

// open decrypts a chunk and checks that it is the chunk that was asked for.
func (c *crypter) open(id string, body []byte) ([]byte, error) {
	if c == nil {
		if got := c.id(body); got != id {
			return nil, fmt.Errorf("chunk %s is corrupt: content hashes to %s", id, got)
		}
		return body, nil
	}
	nonce, err := c.nonce(id)
	if err != nil {
		return nil, err
	}
	plain, err := c.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("chunk %s does not decrypt with this key: %w", id, err)
	}
	if got := c.id(plain); got != id {
		return nil, fmt.Errorf("chunk %s is corrupt: content hashes to %s", id, got)
	}
	return plain, nil
}

// sealBlob encrypts a metadata object. Unlike a chunk it is not content-addressed
// and is rewritten in place, so the nonce is random and travels in front of the
// ciphertext. The object's key is bound in as associated data: a manifest copied
// over another sprite's does not open.
func (c *crypter) sealBlob(key string, plain []byte) ([]byte, error) {
	if c == nil {
		return plain, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, []byte(key)), nil
}

func (c *crypter) openBlob(key string, body []byte) ([]byte, error) {
	if c == nil {
		return body, nil
	}
	n := c.aead.NonceSize()
	if len(body) < n {
		return nil, fmt.Errorf("%s is too short to be sealed", key)
	}
	plain, err := c.aead.Open(nil, body[:n], body[n:], []byte(key))
	if err != nil {
		return nil, fmt.Errorf("%s does not decrypt with this key: %w", key, err)
	}
	return plain, nil
}

func (c *crypter) nonce(id string) ([]byte, error) {
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) < c.aead.NonceSize() {
		return nil, fmt.Errorf("chunk id %q is not a %d-byte hash", id, sha256.Size)
	}
	return raw[:c.aead.NonceSize()], nil
}

// sameKey reports whether a repository written with check was written with this
// crypter's key. Both sides being empty means neither uses encryption.
func (c *crypter) sameKey(check string) bool {
	return hmac.Equal([]byte(c.keyCheck()), []byte(check))
}

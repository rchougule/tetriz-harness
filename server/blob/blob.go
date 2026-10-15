// Package blob stores artifact bodies keyed by their sha256 hex content hash.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

var (
	// ErrNotFound is returned by Get for a hash with no stored body.
	ErrNotFound = errors.New("blob not found")
	// ErrInvalidHash is returned for a key that is not a lowercase sha256 hex digest.
	ErrInvalidHash = errors.New("content hash must be 64 lowercase hex characters")
	// ErrHashMismatch is returned by Put when the body does not hash to its key.
	ErrHashMismatch = errors.New("body does not match its content hash")
)

// Store holds content-addressed bodies. Put is idempotent: the key is the
// body's hash, so re-putting identical bytes changes nothing.
type Store interface {
	Put(ctx context.Context, hash string, body []byte) error
	Get(ctx context.Context, hash string) ([]byte, error)
	Has(ctx context.Context, hash string) (bool, error)
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateHash rejects anything but a lowercase sha256 hex digest, which also
// keeps a key from naming a path outside the store.
func ValidateHash(hash string) error {
	if !hashPattern.MatchString(hash) {
		return ErrInvalidHash
	}
	return nil
}

// Hash returns the sha256 hex digest of body.
func Hash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func checkPut(hash string, body []byte) error {
	if err := ValidateHash(hash); err != nil {
		return err
	}
	if Hash(body) != hash {
		return fmt.Errorf("putting %s: %w", hash, ErrHashMismatch)
	}
	return nil
}

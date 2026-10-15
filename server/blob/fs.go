package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FS stores bodies as files under a root directory, fanned out by the first
// two hex characters of the hash.
type FS struct {
	root string
}

// NewFS returns a store rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("blob directory is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating blob directory: %w", err)
	}
	return &FS{root: dir}, nil
}

func (s *FS) path(hash string) string {
	return filepath.Join(s.root, hash[:2], hash)
}

// Put writes body atomically: a reader never sees a partial file.
func (s *FS) Put(_ context.Context, hash string, body []byte) error {
	if err := checkPut(hash, body); err != nil {
		return err
	}
	dst := s.path(hash)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("creating blob shard: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+hash+"-*")
	if err != nil {
		return fmt.Errorf("creating temp blob: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp blob: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("publishing blob: %w", err)
	}
	return nil
}

// Get reads a body.
func (s *FS) Get(_ context.Context, hash string) ([]byte, error) {
	if err := ValidateHash(hash); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading blob: %w", err)
	}
	return b, nil
}

// Has reports whether a body is stored.
func (s *FS) Has(_ context.Context, hash string) (bool, error) {
	if err := ValidateHash(hash); err != nil {
		return false, err
	}
	_, err := os.Stat(s.path(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking blob: %w", err)
	}
	return true, nil
}

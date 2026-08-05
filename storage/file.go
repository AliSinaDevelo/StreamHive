package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileStore stores blobs as files named by hex-encoded keys.
type FileStore struct {
	dir string
}

// NewFileStore opens or creates a directory-backed BlobStore.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("storage: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) pathFor(key []byte) (string, error) {
	if len(key) == 0 {
		return "", ErrKeyEmpty
	}
	return filepath.Join(s.dir, hex.EncodeToString(key)), nil
}

// Put stores data under key, replacing any existing value atomically.
func (s *FileStore) Put(ctx context.Context, key []byte, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.pathFor(key)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, ".streamhive-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

// Get returns a copy of the value for key.
func (s *FileStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := VerifySHA256Key(key, data); err != nil && !errors.Is(err, ErrInvalidSHA256Key) {
		return nil, err
	}
	return data, nil
}

// Has reports whether key exists.
func (s *FileStore) Has(ctx context.Context, key []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path, err := s.pathFor(key)
	if err != nil {
		return false, err
	}
	if len(key) == SHA256KeyBytes {
		data, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
		return VerifySHA256Key(key, data) == nil, nil
	}
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes key. Missing keys are not an error.
func (s *FileStore) Delete(ctx context.Context, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.pathFor(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListKeys returns all known keys in deterministic bytewise order.
func (s *FileStore) ListKeys(ctx context.Context) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	keys := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".streamhive-") {
			continue
		}
		key, err := hex.DecodeString(entry.Name())
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return string(keys[i]) < string(keys[j])
	})
	return keys, nil
}

var _ BlobStore = (*FileStore)(nil)
var _ BlobKeyLister = (*FileStore)(nil)
var _ BlobKeyPager = (*FileStore)(nil)

// ListKeyPage returns the smallest keys strictly after after, bounded by limit.
// Directory entries are scanned in chunks so the process does not materialize
// the complete inventory just to send one bounded replication page.
func (s *FileStore) ListKeyPage(ctx context.Context, after []byte, limit int) ([][]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	limit = normalizeKeyPageLimit(limit)
	dir, err := os.Open(s.dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_ = dir.Close()
	}()

	var page [][]byte
	for {
		entries, readErr := dir.Readdir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".streamhive-") {
				continue
			}
			key, err := hex.DecodeString(entry.Name())
			if err != nil {
				return nil, nil, err
			}
			page = insertKeyPage(page, key, after, limit)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, nil, readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(page) == 0 {
		return nil, nil, nil
	}
	next := append([]byte(nil), page[len(page)-1]...)
	return page, next, nil
}

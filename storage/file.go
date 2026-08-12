package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/btree"
)

// FileStore stores blobs as files named by hex-encoded keys.
type FileStore struct {
	mu           sync.RWMutex
	dir          string
	keys         *btree.BTreeG[string]
	indexModTime time.Time
}

var (
	// ErrInvalidKeyFilename is returned when a regular FileStore entry is not a hex key.
	ErrInvalidKeyFilename = errors.New("storage: invalid key filename")
	// ErrNonRegularEntry is returned when a FileStore key resolves to a non-regular entry.
	ErrNonRegularEntry = errors.New("storage: non-regular blob entry")
)

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

func (s *FileStore) ensureKeyIndex(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	dirInfo, err := os.Stat(s.dir)
	if err != nil {
		return err
	}
	if s.keys != nil && dirInfo.ModTime().Equal(s.indexModTime) {
		return nil
	}

	for {
		index, err := s.readKeyIndex(ctx)
		if err != nil {
			return err
		}
		latestInfo, err := os.Stat(s.dir)
		if err != nil {
			return err
		}
		if latestInfo.ModTime().Equal(dirInfo.ModTime()) {
			s.keys = index
			s.indexModTime = latestInfo.ModTime()
			return nil
		}
		dirInfo = latestInfo
	}
}

func (s *FileStore) readKeyIndex(ctx context.Context) (*btree.BTreeG[string], error) {
	dir, err := os.Open(s.dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = dir.Close()
	}()

	index := btree.NewOrderedG[string](32)
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			regular, err := isRegularBlobEntry(entry)
			if err != nil {
				return nil, err
			}
			if !regular {
				continue
			}
			key, err := hex.DecodeString(entry.Name())
			if err != nil {
				return nil, fmt.Errorf("%w: %q", ErrInvalidKeyFilename, entry.Name())
			}
			index.ReplaceOrInsert(string(key))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return index, nil
}

func isRegularBlobEntry(entry os.DirEntry) (bool, error) {
	if entry.IsDir() || strings.HasPrefix(entry.Name(), ".streamhive-") {
		return false, nil
	}
	info, err := entry.Info()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

func validateRegularBlobFile(pathInfo, fileInfo os.FileInfo) error {
	if !pathInfo.Mode().IsRegular() || !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		return ErrNonRegularEntry
	}
	return nil
}

func regularBlobFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNonRegularEntry
	}
	return info, nil
}

func openRegularBlobFile(path string) (*os.File, error) {
	pathInfo, err := regularBlobFileInfo(path)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	fileInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateRegularBlobFile(pathInfo, fileInfo); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	renameErr := os.Rename(tmpName, path)
	if renameErr == nil && s.keys != nil {
		s.keys.ReplaceOrInsert(string(key))
		s.refreshIndexModTimeLocked()
	}
	if renameErr != nil {
		return renameErr
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
	file, err := openRegularBlobFile(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
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
	file, err := openRegularBlobFile(path)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNonRegularEntry) {
			return false, nil
		}
		return false, err
	}
	if len(key) == SHA256KeyBytes {
		data, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil {
			return false, readErr
		}
		if closeErr != nil {
			return false, closeErr
		}
		return VerifySHA256Key(key, data) == nil, nil
	}
	if err := file.Close(); err != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, infoErr := regularBlobFileInfo(path); infoErr != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(infoErr, ErrNotFound) {
			if s.keys != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				s.keys.Delete(string(key))
				s.refreshIndexModTimeLocked()
			}
			return nil
		}
		return infoErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	removeErr := os.Remove(path)
	if (removeErr == nil || os.IsNotExist(removeErr)) && s.keys != nil {
		s.keys.Delete(string(key))
		s.refreshIndexModTimeLocked()
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}

func (s *FileStore) refreshIndexModTimeLocked() {
	info, err := os.Stat(s.dir)
	if err != nil {
		s.indexModTime = time.Time{}
		return
	}
	s.indexModTime = info.ModTime()
}

// ListKeys returns all known keys in deterministic bytewise order.
func (s *FileStore) ListKeys(ctx context.Context) ([][]byte, error) {
	if err := s.ensureKeyIndex(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([][]byte, 0, s.keys.Len())
	var ctxErr error
	s.keys.Ascend(func(key string) bool {
		if err := ctx.Err(); err != nil {
			ctxErr = err
			return false
		}
		keys = append(keys, []byte(key))
		return true
	})
	if ctxErr != nil {
		return nil, ctxErr
	}
	return keys, nil
}

var _ BlobStore = (*FileStore)(nil)
var _ BlobKeyLister = (*FileStore)(nil)
var _ BlobKeyPager = (*FileStore)(nil)

// ListKeyPage returns the smallest keys strictly after after, bounded by limit.
// The process-local ordered index is rebuilt from the durable file names on first
// enumeration and then maintained after successful store mutations.
func (s *FileStore) ListKeyPage(ctx context.Context, after []byte, limit int) ([][]byte, []byte, error) {
	if err := s.ensureKeyIndex(ctx); err != nil {
		return nil, nil, err
	}
	limit = normalizeKeyPageLimit(limit)
	s.mu.RLock()
	defer s.mu.RUnlock()
	capacity := limit
	if capacity > s.keys.Len() {
		capacity = s.keys.Len()
	}
	page := make([][]byte, 0, capacity)
	var ctxErr error
	pivot := string(after)
	s.keys.AscendGreaterOrEqual(pivot, func(key string) bool {
		if err := ctx.Err(); err != nil {
			ctxErr = err
			return false
		}
		if len(after) > 0 && key == pivot {
			return true
		}
		page = append(page, []byte(key))
		return len(page) < limit
	})
	if ctxErr != nil {
		return nil, nil, ctxErr
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

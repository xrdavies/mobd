// Package artifact stores binary files (tool screenshots, uploads) outside
// the conversation snapshot, addressed by random immutable IDs.
package artifact

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	ErrNotFound    = errors.New("artifact not found")
	ErrUnsupported = errors.New("unsupported artifact type")
)

var extByMime = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

var mimeByExt = map[string]string{"png": "image/png", "jpg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}

var validID = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|gif|webp)$`)

// Store keeps artifacts as flat files in dir, created on first save.
type Store struct {
	dir string
}

func New(dir string) *Store {
	return &Store{dir: dir}
}

// Supported reports whether mime can be stored.
func Supported(mime string) bool {
	_, ok := extByMime[mime]
	return ok
}

// Save writes data atomically and returns its ID.
func (s *Store) Save(mime string, data []byte) (string, error) {
	ext, ok := extByMime[strings.ToLower(mime)]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnsupported, mime)
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return "", err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:]) + "." + ext
	f, err := os.CreateTemp(s.dir, ".artifact-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.dir, id)); err != nil {
		return "", err
	}
	return id, nil
}

// Path resolves an ID to its file and MIME type.
func (s *Store) Path(id string) (string, string, error) {
	if !validID.MatchString(id) {
		return "", "", ErrNotFound
	}
	path := filepath.Join(s.dir, id)
	if _, err := os.Stat(path); err != nil {
		return "", "", ErrNotFound
	}
	return path, mimeByExt[id[strings.LastIndexByte(id, '.')+1:]], nil
}

// Delete removes artifacts, ignoring unknown IDs.
func (s *Store) Delete(ids ...string) {
	for _, id := range ids {
		if validID.MatchString(id) {
			_ = os.Remove(filepath.Join(s.dir, id))
		}
	}
}

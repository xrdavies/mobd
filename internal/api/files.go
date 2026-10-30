package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tus "github.com/tus/tusd/v2/pkg/handler"
)

const maxFileSize int64 = 20 << 30

var uploadID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var fileDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type sharedFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type fileReceipt struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type fileService struct {
	dir     string
	store   filestore.FileStore
	locker  filelocker.FileLocker
	handler http.Handler
	err     error
	mu      sync.Mutex
	digests map[string]cachedDigest
}

type cachedDigest struct {
	info os.FileInfo
	sum  string
}

// uploadsDir holds unfinished uploads inside the shared folder: the same file system, so
// publishing is an atomic link, and hidden from the listing like any dot name.
const uploadsDir = ".uploads"

// newFileService serves dir, the folder shared with paired devices.
func newFileService(dir string) *fileService {
	f := &fileService{dir: dir, digests: make(map[string]cachedDigest)}
	for _, path := range []string{dir, filepath.Join(dir, uploadsDir)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			f.err = err
			return f
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			f.err = fmt.Errorf("%s must be a directory, not a symbolic link", path)
			return f
		}
	}
	f.store = filestore.New(filepath.Join(dir, uploadsDir))
	f.store.FileModePerm, f.store.DirModePerm = 0600, 0700
	f.locker = filelocker.New(f.store.Path)
	composer := tus.NewStoreComposer()
	composer.UseCore(f.store)
	composer.UseTerminater(f.store)
	f.locker.UseIn(composer)
	f.handler, f.err = tus.NewHandler(tus.Config{
		BasePath: "/file-uploads/", StoreComposer: composer, MaxSize: maxFileSize,
		DisableDownload: true, DisableConcatenation: true, Cors: &tus.CorsConfig{Disable: true},
		PreUploadCreateCallback: func(e tus.HookEvent) (tus.HTTPResponse, tus.FileInfoChanges, error) {
			m := e.Upload.MetaData
			if !validFileName(m["filename"]) || len(m["filename"]) > 200 || !fileDigest.MatchString(m["sha256"]) || e.Upload.SizeIsDeferred {
				return tus.HTTPResponse{}, tus.FileInfoChanges{}, tus.NewError("INVALID_FILE", "A safe filename, SHA-256 and known size are required", 400)
			}
			return tus.HTTPResponse{}, tus.FileInfoChanges{MetaData: tus.MetaData{"filename": m["filename"], "sha256": m["sha256"]}}, nil
		},
	})
	return f
}

func validFileName(name string) bool {
	// Dot names are hidden: .uploads, and what Finder or editors leave behind (.DS_Store).
	if name == "" || strings.HasPrefix(name, ".") || !utf8.ValidString(name) || len(name) > 240 || strings.TrimRight(name, ". ") != name {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) || strings.ContainsRune(`/\<>:"|?*`, c) {
			return false
		}
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" {
		return false
	}
	return !(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9')
}

func (f *fileService) ready(w http.ResponseWriter) bool {
	if f.err != nil {
		writeErr(w, 503, "File storage unavailable: "+f.err.Error())
		return false
	}
	return true
}

func (f *fileService) options(w http.ResponseWriter) {
	w.Header().Set("Tus-Version", "1.0.0")
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.Header().Set("Tus-Extension", "creation,termination")
	w.Header().Set("Tus-Max-Size", fmt.Sprint(maxFileSize))
	w.WriteHeader(http.StatusNoContent)
}

// Relative locations work through HTTPS gateways without trusting forwarded/Host headers.
type uploadResponse struct{ http.ResponseWriter }

func (w uploadResponse) WriteHeader(status int) {
	if location := w.Header().Get("Location"); location != "" {
		if u, err := url.Parse(location); err == nil {
			w.Header().Set("Location", u.EscapedPath())
		}
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w uploadResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (f *fileService) openFiles() (*os.Root, error) {
	info, err := os.Lstat(f.dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("File directory must not be a symbolic link")
	}
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("File directory changed; retry")
	}
	return root, nil
}

func (f *fileService) upload(w http.ResponseWriter, r *http.Request) {
	if !f.ready(w) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/file-uploads/")
	override := r.Header.Get("X-HTTP-Method-Override")
	if (id != "" && !uploadID.MatchString(id)) || (override != "" && !(id != "" && r.Method == "POST" && override == "PATCH")) {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/file-uploads/", f.handler).ServeHTTP(uploadResponse{w}, r)
}

func (f *fileService) list(w http.ResponseWriter, r *http.Request) {
	if !f.ready(w) {
		return
	}
	root, err := f.openFiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	files := []sharedFile{}
	for _, e := range entries {
		if !validFileName(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err == nil && info.Mode().IsRegular() {
			files = append(files, sharedFile{e.Name(), info.Size(), info.ModTime().UTC()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	writeJSON(w, 200, map[string]any{"directory": f.dir, "maxUploadSize": maxFileSize, "files": files})
}

func (f *fileService) complete(w http.ResponseWriter, r *http.Request) {
	if !f.ready(w) {
		return
	}
	id := r.PathValue("id")
	if !uploadID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	lock, err := f.locker.NewLock(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err = lock.Lock(ctx, cancel); err != nil {
		writeErr(w, 409, "Upload is busy; retry completion")
		return
	}
	defer lock.Unlock()
	receiptPath := filepath.Join(f.store.Path, id+".published")
	var receipt fileReceipt
	if data, err := os.ReadFile(receiptPath); err == nil {
		if json.Unmarshal(data, &receipt) != nil {
			writeErr(w, 500, "Invalid completion receipt")
			return
		}
		writeJSON(w, 200, receipt)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeErr(w, 500, err.Error())
		return
	}
	upload, err := f.store.GetUpload(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	info, err := upload.GetInfo(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if info.Offset != info.Size {
		writeErr(w, 409, "Upload is incomplete")
		return
	}
	reader, err := upload.GetReader(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sum, err := hashFile(ctx, reader)
	reader.Close()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if sum != info.MetaData["sha256"] {
		writeErr(w, 422, "Checksum mismatch; cancel this upload and upload the file again")
		return
	}
	// Link is atomic and refuses to replace an existing name, including a symlink.
	root, err := f.openFiles()
	if err != nil {
		writeErr(w, 500, "File directory is unavailable or is a symbolic link")
		return
	}
	defer root.Close()
	name := info.MetaData["filename"]
	if !validFileName(name) {
		writeErr(w, 400, "Invalid filename")
		return
	}
	source := uploadsDir + "/" + id
	for attempt := 0; attempt < 2; attempt++ {
		err = root.Link(source, name)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			writeErr(w, 500, err.Error())
			return
		}
		a, _ := root.Stat(source)
		b, _ := root.Lstat(name)
		if a != nil && b != nil && os.SameFile(a, b) {
			err = nil
			break
		} // retry after publication but before receipt
		ext := filepath.Ext(info.MetaData["filename"])
		stem := strings.TrimSuffix(info.MetaData["filename"], ext)
		name = stem + " (" + id + ")" + ext
	}
	if err != nil {
		writeErr(w, 409, "Destination exists; cancel and upload with another name")
		return
	}
	receipt = fileReceipt{Name: name, Size: info.Size, SHA256: sum}
	data, _ := json.Marshal(receipt)
	if err = os.WriteFile(receiptPath+".tmp", data, 0600); err == nil {
		err = os.Rename(receiptPath+".tmp", receiptPath)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Retain the small receipt for idempotency, remove tus's staging links.
	if err = f.store.AsTerminatableUpload(upload).Terminate(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, receipt)
}

func hashFile(ctx context.Context, reader io.Reader) (string, error) {
	h := sha256.New()
	buf := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := reader.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

func (f *fileService) download(w http.ResponseWriter, r *http.Request) {
	if !f.ready(w) {
		return
	}
	name := r.PathValue("name")
	if !validFileName(name) {
		http.NotFound(w, r)
		return
	}
	root, err := f.openFiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	file, err := root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		writeErr(w, 409, "File changed; refresh and retry")
		return
	}
	f.mu.Lock()
	cached := f.digests[name]
	f.mu.Unlock()
	sum := cached.sum
	if cached.info == nil || !os.SameFile(info, cached.info) || info.Size() != cached.info.Size() || !info.ModTime().Equal(cached.info.ModTime()) {
		sum, err = hashFile(r.Context(), file)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		after, err := file.Stat()
		if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			writeErr(w, 409, "File changed; refresh and retry")
			return
		}
		f.mu.Lock()
		if len(f.digests) > 1024 {
			clear(f.digests)
		}
		f.digests[name] = cachedDigest{info, sum}
		f.mu.Unlock()
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"sha256-`+sum+`"`)
	w.Header().Set("X-Content-SHA256", sum)
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (f *fileService) rename(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validFileName(name) {
		writeErr(w, 400, "Invalid filename")
		return
	}
	var req struct {
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "Invalid JSON")
		return
	}
	if !validFileName(req.NewName) {
		writeErr(w, 400, "Invalid new filename")
		return
	}
	if strings.HasPrefix(req.NewName, ".") {
		writeErr(w, 400, "Filename cannot start with a dot")
		return
	}
	oldPath := filepath.Join(f.dir, name)
	newPath := filepath.Join(f.dir, req.NewName)
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		writeErr(w, 404, "File not found")
		return
	}
	if _, err := os.Stat(newPath); err == nil {
		writeErr(w, 409, "A file with that name already exists")
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (f *fileService) delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validFileName(name) {
		writeErr(w, 400, "Invalid filename")
		return
	}
	path := filepath.Join(f.dir, name)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			writeErr(w, 404, "File not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

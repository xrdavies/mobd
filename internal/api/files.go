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

type sharedFolder struct {
	Name     string    `json:"name"`
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
			kept := tus.MetaData{"filename": m["filename"], "sha256": m["sha256"]}
			if rel, ok := m["relativePath"]; ok {
				if !validRelativePath(rel) {
					return tus.HTTPResponse{}, tus.FileInfoChanges{}, tus.NewError("INVALID_FILE", "A safe relative path is required", 400)
				}
				kept["relativePath"] = rel
			}
			return tus.HTTPResponse{}, tus.FileInfoChanges{MetaData: kept}, nil
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

// validRelativePath accepts a slash-separated path inside the shared folder. Every segment must be
// a valid file name, which also refuses "", ".", ".." and hidden names such as .uploads.
func validRelativePath(rel string) bool {
	if len(rel) > 1024 {
		return false
	}
	for _, segment := range strings.Split(rel, "/") {
		if !validFileName(segment) {
			return false
		}
	}
	return true
}

// lstatInside returns rel's own info after checking no folder on the way is a symbolic link: the
// root keeps links from leaving the shared folder, this keeps paths from being aliased inside it.
func lstatInside(root *os.Root, rel string) (os.FileInfo, error) {
	segments := strings.Split(rel, "/")
	for i := 1; i < len(segments); i++ {
		info, err := root.Lstat(strings.Join(segments[:i], "/"))
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, os.ErrNotExist
		}
	}
	return root.Lstat(rel)
}

// makeFolders creates rel and its parents; an existing file or link on the way is an error.
func makeFolders(root *os.Root, rel string) error {
	segments := strings.Split(rel, "/")
	for i := 1; i <= len(segments); i++ {
		path := strings.Join(segments[:i], "/")
		if err := root.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a folder", path)
		}
	}
	return nil
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
	rel := r.URL.Query().Get("path")
	opened := "."
	if rel != "" {
		if !validRelativePath(rel) {
			writeErr(w, 400, "Invalid path")
			return
		}
		if info, err := lstatInside(root, rel); err != nil || !info.IsDir() {
			http.NotFound(w, r)
			return
		}
		opened = rel
	}
	dir, err := root.Open(opened)
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
	files, folders := []sharedFile{}, []sharedFolder{}
	for _, e := range entries {
		if !validFileName(e.Name()) {
			continue
		}
		if e.IsDir() {
			if info, err := e.Info(); err == nil {
				folders = append(folders, sharedFolder{e.Name(), info.ModTime().UTC()})
			}
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err == nil && info.Mode().IsRegular() {
			files = append(files, sharedFile{e.Name(), info.Size(), info.ModTime().UTC()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	sort.Slice(folders, func(i, j int) bool { return folders[i].Name < folders[j].Name })
	writeJSON(w, 200, map[string]any{"directory": f.dir, "path": rel, "maxUploadSize": maxFileSize, "files": files, "folders": folders})
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

	// A folder upload names its place in the shared folder; the checksum has passed, so a failed
	// upload leaves no folders behind.
	name := info.MetaData["filename"]
	if rel, ok := info.MetaData["relativePath"]; ok {
		if !validRelativePath(rel) {
			writeErr(w, 400, "Invalid path")
			return
		}
		name = rel
	} else if !validFileName(name) {
		writeErr(w, 400, "Invalid filename")
		return
	}
	folder, base := "", name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		folder, base = name[:i+1], name[i+1:]
		if err := makeFolders(root, name[:i]); err != nil {
			writeErr(w, 409, "Cannot create the folder: a file with that name exists")
			return
		}
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
		ext := filepath.Ext(base)
		name = folder + strings.TrimSuffix(base, ext) + " (" + id + ")" + ext
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
	name := r.PathValue("path")
	if !validRelativePath(name) {
		http.NotFound(w, r)
		return
	}
	root, err := f.openFiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	info, err := lstatInside(root, name)
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
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(name)}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"sha256-`+sum+`"`)
	w.Header().Set("X-Content-SHA256", sum)
	http.ServeContent(w, r, filepath.Base(name), info.ModTime(), file)
}

// rename handles POST /files/{path}/rename: a new name in the same folder, for a file or a folder.
func (f *fileService) rename(w http.ResponseWriter, r *http.Request) {
	if !f.ready(w) {
		return
	}
	rel, ok := strings.CutSuffix(r.PathValue("path"), "/rename")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !validRelativePath(rel) {
		writeErr(w, 400, "Invalid path")
		return
	}
	var req struct {
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, 400, "Invalid JSON")
		return
	}
	if !validFileName(req.NewName) || len(req.NewName) > 200 {
		writeErr(w, 400, "Invalid new name")
		return
	}
	root, err := f.openFiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	info, err := lstatInside(root, rel)
	if err != nil || !(info.Mode().IsRegular() || info.IsDir()) {
		http.NotFound(w, r)
		return
	}
	target := req.NewName
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		target = rel[:i+1] + req.NewName
	}
	if _, err := root.Lstat(target); err == nil {
		writeErr(w, 409, "A file or folder with that name already exists")
		return
	}
	if info.IsDir() {
		err = root.Rename(rel, target)
	} else if err = root.Link(rel, target); err == nil { // Link refuses to replace, unlike Rename
		err = root.Remove(rel)
	}
	if errors.Is(err, os.ErrExist) {
		writeErr(w, 409, "A file or folder with that name already exists")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// remove handles DELETE /files/{path}: a file, or a folder with everything in it. A symbolic link
// is not shared, so it stays. Clients confirm with the user first.
func (f *fileService) remove(w http.ResponseWriter, r *http.Request) {
	if !f.ready(w) {
		return
	}
	rel := r.PathValue("path")
	if !validRelativePath(rel) {
		writeErr(w, 400, "Invalid path")
		return
	}
	root, err := f.openFiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	info, err := lstatInside(root, rel)
	if err != nil || !(info.Mode().IsRegular() || info.IsDir()) {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		err = root.RemoveAll(rel)
	} else {
		err = root.Remove(rel)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// createFolder handles POST /file-folders: empty folders of a folder upload have no file to carry
// their path. Creating one that exists succeeds, so a retried upload does not fail.
func (f *fileService) createFolder(w http.ResponseWriter, r *http.Request) {
	if !f.ready(w) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || !validRelativePath(req.Path) {
		writeErr(w, 400, "Invalid path")
		return
	}
	root, err := f.openFiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	if err := makeFolders(root, req.Path); err != nil {
		writeErr(w, 409, "Cannot create the folder: a file with that name exists")
		return
	}
	w.WriteHeader(204)
}

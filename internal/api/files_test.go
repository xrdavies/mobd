package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileRequest(t *testing.T, h http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Tus-Resumable", "1.0.0")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func createFileUpload(t *testing.T, h http.Handler, name string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	w := fileRequest(t, h, "POST", "/file-uploads/", nil, map[string]string{
		"Upload-Length":   fmt.Sprint(len(data)),
		"Upload-Metadata": "filename " + base64.StdEncoding.EncodeToString([]byte(name)) + ",sha256 " + base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(sum[:]))),
	})
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, "/file-uploads/") {
		t.Fatalf("unsafe location %q", location)
	}
	return location
}

func appendFileUpload(t *testing.T, h http.Handler, location string, offset int, data []byte) {
	t.Helper()
	w := fileRequest(t, h, "PATCH", location, data, map[string]string{"Upload-Offset": fmt.Sprint(offset), "Content-Type": "application/offset+octet-stream"})
	if w.Code != 204 || w.Header().Get("Upload-Offset") != fmt.Sprint(offset+len(data)) {
		t.Fatalf("append: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

func finishFileUpload(t *testing.T, h http.Handler, location string) fileReceipt {
	t.Helper()
	w := fileRequest(t, h, "POST", location+"/complete", nil, nil)
	if w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	var receipt fileReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestFileTransferResumePublishAndDownload(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	data := bytes.Repeat([]byte("resumable binary\x00\xff"), 20000)
	location := createFileUpload(t, h, "报告 1.bin", data)
	appendFileUpload(t, h, location, 0, data[:10000])
	if w := fileRequest(t, h, "POST", location+"/complete", nil, nil); w.Code != 409 {
		t.Fatalf("partial published: %d", w.Code)
	}
	listing := fileRequest(t, h, "GET", "/files", nil, nil)
	if !strings.Contains(listing.Body.String(), `"files":[]`) {
		t.Fatalf("partial visible: %s", listing.Body.String())
	}
	// Reconstruct the file service to lose every in-memory upload/offset cache.
	s.files = newFileService(filesDir(s.cfg))
	h = authenticatedHandler(t, s)
	if w := fileRequest(t, h, "HEAD", location, nil, nil); w.Code != 200 || w.Header().Get("Upload-Offset") != "10000" {
		t.Fatalf("resume: %d %v", w.Code, w.Header())
	}
	if w := fileRequest(t, h, "PATCH", location, []byte("wrong"), map[string]string{"Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"}); w.Code != 409 {
		t.Fatalf("stale offset accepted: %d", w.Code)
	}
	appendFileUpload(t, h, location, 10000, data[10000:])
	receipt := finishFileUpload(t, h, location)
	if receipt.Name != "报告 1.bin" || receipt.Size != int64(len(data)) {
		t.Fatalf("receipt: %+v", receipt)
	}
	if again := finishFileUpload(t, h, location); again != receipt {
		t.Fatalf("not idempotent: %+v", again)
	}
	path := "/files/" + url.PathEscape(receipt.Name)
	head := fileRequest(t, h, "HEAD", path, nil, nil)
	if head.Code != 200 || head.Header().Get("X-Content-SHA256") != receipt.SHA256 || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %v", head.Code, head.Header())
	}
	etag := head.Header().Get("ETag")
	first := fileRequest(t, h, "GET", path, nil, map[string]string{"Range": "bytes=0-9999", "If-Match": etag, "Accept-Encoding": "gzip"})
	second := fileRequest(t, h, "GET", path, nil, map[string]string{"Range": "bytes=10000-", "If-Match": etag})
	if first.Code != 206 || second.Code != 206 || first.Header().Get("Content-Encoding") != "" {
		t.Fatalf("range: %d %d %v", first.Code, second.Code, first.Header())
	}
	if !bytes.Equal(append(first.Body.Bytes(), second.Body.Bytes()...), data) {
		t.Fatal("download differs")
	}
	if first.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(first.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("missing attachment headers")
	}
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "files", receipt.Name), []byte("new version"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := fileRequest(t, h, "GET", path, nil, map[string]string{"Range": "bytes=1-", "If-Match": etag}); w.Code != 412 {
		t.Fatalf("mixed versions: %d", w.Code)
	}
}

func TestFileTransferDuplicatesEmptyAndCancel(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	first := createFileUpload(t, h, "empty.txt", nil)
	if got := finishFileUpload(t, h, first); got.Name != "empty.txt" {
		t.Fatal(got)
	}
	second := createFileUpload(t, h, "empty.txt", nil)
	got := finishFileUpload(t, h, second)
	if got.Name == "empty.txt" || !strings.HasSuffix(got.Name, ".txt") {
		t.Fatal(got)
	}
	if w := fileRequest(t, h, "GET", "/files/"+url.PathEscape(got.Name), nil, nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("empty download: %d", w.Code)
	}
	third := createFileUpload(t, h, "cancel.bin", []byte("test"))
	appendFileUpload(t, h, third, 0, []byte("te"))
	if w := fileRequest(t, h, "DELETE", third, nil, nil); w.Code != 204 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if w := fileRequest(t, h, "HEAD", third, nil, nil); w.Code != 404 {
		t.Fatalf("canceled upload survived: %d", w.Code)
	}
}

func TestFileTransferValidationAuthenticationAndCORS(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	for _, path := range []string{"/files", "/files/private", "/file-uploads/", "/file-uploads/00000000000000000000000000000000/complete"} {
		if w := fileRequest(t, s.Handler(), "GET", path, nil, nil); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	for _, name := range []string{"../outside", "/absolute", "a\\b", "a\n", ".", "NUL", "COM1.txt"} {
		w := fileRequest(t, h, "POST", "/file-uploads/", nil, map[string]string{"Upload-Length": "1", "Upload-Metadata": "filename " + base64.StdEncoding.EncodeToString([]byte(name)) + ",sha256 " + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 64)))})
		if w.Code != 400 {
			t.Fatalf("unsafe name %q: %d", name, w.Code)
		}
	}
	if err := os.Symlink(filepath.Join(s.cfg.DataDir, "settings.json"), filepath.Join(s.cfg.DataDir, "files", "secret.json")); err != nil {
		t.Fatal(err)
	}
	if w := fileRequest(t, h, "GET", "/files/secret.json", nil, nil); w.Code != 404 {
		t.Fatalf("symlink exposed: %d", w.Code)
	}
	if w := fileRequest(t, h, "GET", "/files", nil, nil); strings.Contains(w.Body.String(), "secret.json") {
		t.Fatal("symlink listed")
	}
	location := createFileUpload(t, h, "wrong.bin", []byte("good"))
	appendFileUpload(t, h, location, 0, []byte("evil"))
	if w := fileRequest(t, h, "POST", location+"/complete", nil, nil); w.Code != 422 {
		t.Fatalf("checksum ignored: %d", w.Code)
	}
	w := fileRequest(t, s.Handler(), "OPTIONS", "/file-uploads/", nil, map[string]string{"Origin": "http://localhost:1420", "Access-Control-Request-Method": "PATCH"})
	if w.Code != 204 || w.Header().Get("Tus-Version") != "1.0.0" || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Upload-Offset") {
		t.Fatalf("CORS: %d %v", w.Code, w.Header())
	}
	if w := fileRequest(t, s.Handler(), "OPTIONS", "/file-uploads/", nil, map[string]string{"Origin": "https://untrusted.invalid"}); w.Code != 403 {
		t.Fatalf("CORS origin allowed: %d", w.Code)
	}
}

func TestSharedFolderHidesDotNamesAndKeepsPartialsInUploads(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	dir := filesDir(s.cfg)
	// Finder leaves .DS_Store in a visible folder; dot names are never listed or accepted.
	if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("finder"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".DS_Store", ".uploads", ".hidden.txt", "."} {
		if validFileName(name) {
			t.Fatalf("accepted %q", name)
		}
	}
	data := []byte("visible")
	location := createFileUpload(t, h, "notes.txt", data)
	appendFileUpload(t, h, location, 0, data[:3])
	id := strings.TrimPrefix(location, "/file-uploads/")
	if _, err := os.Stat(filepath.Join(dir, uploadsDir, id)); err != nil {
		t.Fatalf("partial not in %s: %v", uploadsDir, err)
	}
	appendFileUpload(t, h, location, 3, data[3:])
	finishFileUpload(t, h, location)
	var listing struct {
		Directory string `json:"directory"`
		Files     []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	w := fileRequest(t, h, "GET", "/files", nil, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Directory != dir || len(listing.Files) != 1 || listing.Files[0].Name != "notes.txt" {
		t.Fatalf("listing: %s", w.Body.String())
	}
	if w := fileRequest(t, h, "GET", "/files/.DS_Store", nil, nil); w.Code != 404 {
		t.Fatalf("dot file served: %d", w.Code)
	}
}

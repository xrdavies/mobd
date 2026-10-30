package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func escapedPath(rel string) string {
	segments := strings.Split(rel, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

func renameFile(t *testing.T, h http.Handler, rel, newName string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"newName": newName})
	return fileRequest(t, h, "POST", "/files/"+escapedPath(rel)+"/rename", body, nil).Code
}

func deleteFile(t *testing.T, h http.Handler, rel string) int {
	t.Helper()
	return fileRequest(t, h, "DELETE", "/files/"+escapedPath(rel), nil, nil).Code
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// createInFolder starts a folder upload of rel; the status is what mobd answered.
func createInFolder(t *testing.T, h http.Handler, rel string, data []byte) (string, int) {
	t.Helper()
	sum := sha256.Sum256(data)
	b := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	w := fileRequest(t, h, "POST", "/file-uploads/", nil, map[string]string{
		"Upload-Length":   fmt.Sprint(len(data)),
		"Upload-Metadata": "filename " + b(filepath.Base(rel)) + ",sha256 " + b(hex.EncodeToString(sum[:])) + ",relativePath " + b(rel),
	})
	return w.Header().Get("Location"), w.Code
}

func uploadInFolder(t *testing.T, h http.Handler, rel, text string) *httpResult {
	t.Helper()
	location, code := createInFolder(t, h, rel, []byte(text))
	if code != 201 {
		t.Fatalf("create %s: %d", rel, code)
	}
	appendFileUpload(t, h, location, 0, []byte(text))
	w := fileRequest(t, h, "POST", location+"/complete", nil, nil)
	var receipt fileReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	return &httpResult{w.Code, receipt}
}

type httpResult struct {
	code    int
	receipt fileReceipt
}

type listing struct {
	Path    string `json:"path"`
	Files   []struct{ Name string }
	Folders []struct{ Name string }
}

func listFolder(t *testing.T, h http.Handler, rel string) (listing, int) {
	t.Helper()
	w := fileRequest(t, h, "GET", "/files?path="+url.QueryEscape(rel), nil, nil)
	var l listing
	_ = json.Unmarshal(w.Body.Bytes(), &l)
	return l, w.Code
}

func TestFolderUploadCreatesFoldersAndIsReachable(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	dir := filesDir(s.cfg)
	got := uploadInFolder(t, h, "reports 报告/2024/q1.pdf", "first")
	if got.code != 200 || got.receipt.Name != "reports 报告/2024/q1.pdf" {
		t.Fatalf("complete: %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "reports 报告/2024/q1.pdf")); string(b) != "first" {
		t.Fatalf("on disk: %q", b)
	}
	if st, _ := os.Stat(filepath.Join(dir, "reports 报告")); st == nil || st.Mode().Perm() != 0700 {
		t.Fatalf("folder mode: %v", st)
	}
	top, code := listFolder(t, h, "")
	if code != 200 || len(top.Folders) != 1 || top.Folders[0].Name != "reports 报告" || len(top.Files) != 0 {
		t.Fatalf("top: %d %+v", code, top)
	}
	inner, code := listFolder(t, h, "reports 报告/2024")
	if code != 200 || inner.Path != "reports 报告/2024" || len(inner.Files) != 1 || inner.Files[0].Name != "q1.pdf" {
		t.Fatalf("inner: %d %+v", code, inner)
	}
	w := fileRequest(t, h, "GET", "/files/"+escapedPath("reports 报告/2024/q1.pdf"), nil, nil)
	if w.Code != 200 || w.Body.String() != "first" || !strings.Contains(w.Header().Get("Content-Disposition"), "q1.pdf") || strings.Contains(w.Header().Get("Content-Disposition"), "2024") {
		t.Fatalf("download: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	again := uploadInFolder(t, h, "reports 报告/2024/q1.pdf", "second")
	if again.code != 200 || !strings.HasPrefix(again.receipt.Name, "reports 报告/2024/q1 (") || !strings.HasSuffix(again.receipt.Name, ").pdf") {
		t.Fatalf("duplicate: %+v", again)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "reports 报告/2024/q1.pdf")); string(b) != "first" {
		t.Fatalf("existing file overwritten: %q", b)
	}
	// Dots inside a name are fine; only whole segments starting with a dot are not.
	if got := uploadInFolder(t, h, "v1..2/a..b.txt", "dots"); got.code != 200 {
		t.Fatalf("dots: %+v", got)
	}
}

func TestFolderUploadRefusesUnsafePathsAndLinks(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	dir := filesDir(s.cfg)
	for _, rel := range []string{"../evil.txt", "/abs/evil.txt", "a/../../evil.txt", "a/.hidden/x.txt", ".uploads/x.txt", "a//b.txt", "a/", strings.Repeat("a/", 600) + "b"} {
		if _, code := createInFolder(t, h, rel, []byte("x")); code != 400 {
			t.Errorf("%.40q: %d", rel, code)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "esc")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"esc/out.txt", "esc/made/out.txt"} {
		if got := uploadInFolder(t, h, rel, "escape"); got.code != 409 {
			t.Errorf("%s through a link: %+v", rel, got)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the shared folder: %v", entries)
	}
	write(t, filepath.Join(outside, "secret.txt"), "s")
	if w := fileRequest(t, h, "GET", "/files/esc/secret.txt", nil, nil); w.Code != 404 {
		t.Errorf("download through a link: %d", w.Code)
	}
	if _, code := listFolder(t, h, "esc"); code != 404 {
		t.Errorf("list through a link: %d", code)
	}
	if _, code := listFolder(t, h, "../"); code != 400 {
		t.Errorf("list outside: %d", code)
	}
	write(t, filepath.Join(dir, "plain"), "file")
	if got := uploadInFolder(t, h, "plain/x.txt", "y"); got.code != 409 {
		t.Errorf("file in the way: %+v", got)
	}
	// A wrong checksum publishes nothing and creates no folders.
	location, _ := createInFolder(t, h, "nofolder/x.txt", []byte("good"))
	appendFileUpload(t, h, location, 0, []byte("evil"))
	if w := fileRequest(t, h, "POST", location+"/complete", nil, nil); w.Code != 422 || exists(filepath.Join(dir, "nofolder")) {
		t.Errorf("checksum: %d, folder exists %v", w.Code, exists(filepath.Join(dir, "nofolder")))
	}
}

func TestCreateFolder(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	dir := filesDir(s.cfg)
	create := func(path string) int {
		body, _ := json.Marshal(map[string]string{"path": path})
		return fileRequest(t, h, "POST", "/file-folders", body, nil).Code
	}
	if create("empty/inner") != 204 || create("empty/inner") != 204 || !exists(filepath.Join(dir, "empty/inner")) {
		t.Fatal("create or repeat failed")
	}
	for _, path := range []string{"", "../x", ".hidden", "a/.b"} {
		if c := create(path); c != 400 {
			t.Errorf("%q: %d", path, c)
		}
	}
	if w := fileRequest(t, s.Handler(), "POST", "/file-folders", []byte(`{"path":"x"}`), nil); w.Code != 401 {
		t.Errorf("without credential: %d", w.Code)
	}
}

func TestRenameFilesAndFolders(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	dir := filesDir(s.cfg)
	write(t, filepath.Join(dir, "a.txt"), "A")
	if c := renameFile(t, h, "a.txt", "b 报告.txt"); c != 204 || exists(filepath.Join(dir, "a.txt")) {
		t.Fatalf("rename: %d", c)
	}
	if w := fileRequest(t, h, "GET", "/files/"+escapedPath("b 报告.txt"), nil, nil); w.Code != 200 || w.Body.String() != "A" {
		t.Fatalf("download: %d", w.Code)
	}
	write(t, filepath.Join(dir, "f/deep/x.txt"), "X")
	if c := renameFile(t, h, "f/deep/x.txt", "y.txt"); c != 204 || !exists(filepath.Join(dir, "f/deep/y.txt")) {
		t.Fatalf("nested: %d", c)
	}
	if c := renameFile(t, h, "f/deep", "deeper"); c != 204 || !exists(filepath.Join(dir, "f/deeper/y.txt")) {
		t.Fatalf("folder: %d", c)
	}
	// A file may be called rename.
	write(t, filepath.Join(dir, "g/rename"), "R")
	if c := renameFile(t, h, "g/rename", "other"); c != 204 {
		t.Fatalf("file called rename: %d", c)
	}
	if w := fileRequest(t, h, "POST", "/files/g/other", []byte(`{"newName":"z"}`), nil); w.Code != 404 {
		t.Fatalf("POST without /rename: %d", w.Code)
	}
	write(t, filepath.Join(dir, "c1.txt"), "one")
	write(t, filepath.Join(dir, "c2.txt"), "two")
	if c := renameFile(t, h, "c1.txt", "c2.txt"); c != 409 {
		t.Errorf("conflict: %d", c)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "c2.txt")); string(b) != "two" || !exists(filepath.Join(dir, "c1.txt")) {
		t.Errorf("conflict changed files: %q", b)
	}
	if c := renameFile(t, h, "nope.txt", "x.txt"); c != 404 {
		t.Errorf("missing: %d", c)
	}
	for _, n := range []string{"", "../evil.txt", "a/b.txt", ".hidden", ".uploads", "NUL", "a\\b"} {
		if c := renameFile(t, h, "c1.txt", n); c != 400 {
			t.Errorf("new name %q: %d", n, c)
		}
	}
	for _, n := range []string{".uploads", "../auth.json"} {
		if c := renameFile(t, h, n, "x.txt"); c == 204 {
			t.Errorf("source %q renamed", n)
		}
	}
	if w := fileRequest(t, h, "POST", "/files/c1.txt/rename", []byte("{"), nil); w.Code != 400 {
		t.Errorf("bad JSON: %d", w.Code)
	}
	// Symbolic links are not shared: neither moved, nor replaced by a rename onto them.
	secret := filepath.Join(t.TempDir(), "secret")
	write(t, secret, "s")
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if c := renameFile(t, h, "link", "link2"); c != 404 {
		t.Errorf("link: %d", c)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if c := renameFile(t, h, "c1.txt", "dangling"); c != 409 {
		t.Errorf("onto a dangling link: %d", c)
	}
	if w := fileRequest(t, s.Handler(), "POST", "/files/c1.txt/rename", []byte(`{"newName":"z.txt"}`), nil); w.Code != 401 {
		t.Errorf("without credential: %d", w.Code)
	}
}

func TestDeleteFilesAndFolders(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	dir := filesDir(s.cfg)
	write(t, filepath.Join(dir, "x.txt"), "x")
	if c := deleteFile(t, h, "x.txt"); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if c := deleteFile(t, h, "x.txt"); c != 404 {
		t.Errorf("again: %d", c)
	}
	write(t, filepath.Join(dir, "f/deep/y.txt"), "y")
	if c := deleteFile(t, h, "f/deep/y.txt"); c != 204 || !exists(filepath.Join(dir, "f/deep")) {
		t.Fatalf("nested: %d", c)
	}
	write(t, filepath.Join(dir, "f/deep/z.txt"), "z")
	// Deleting a folder takes its contents, but never what a link inside points at.
	kept := filepath.Join(t.TempDir(), "kept")
	write(t, kept, "k")
	if err := os.Symlink(kept, filepath.Join(dir, "f/link")); err != nil {
		t.Fatal(err)
	}
	if c := deleteFile(t, h, "f"); c != 204 || exists(filepath.Join(dir, "f")) || !exists(kept) {
		t.Fatalf("folder: %d, folder left %v, target kept %v", c, exists(filepath.Join(dir, "f")), exists(kept))
	}
	write(t, filepath.Join(dir, ".DS_Store"), "f")
	for _, n := range []string{".DS_Store", ".uploads", "../auth.json"} {
		if c := deleteFile(t, h, n); c == 204 {
			t.Errorf("%q deleted", n)
		}
	}
	if !exists(filepath.Join(dir, ".DS_Store")) || !exists(filepath.Join(dir, uploadsDir)) {
		t.Error("hidden entries removed")
	}
	if err := os.Symlink(kept, filepath.Join(dir, "lnk")); err != nil {
		t.Fatal(err)
	}
	if c := deleteFile(t, h, "lnk"); c != 404 || !exists(filepath.Join(dir, "lnk")) {
		t.Errorf("link: %d", c)
	}
	if w := fileRequest(t, s.Handler(), "DELETE", "/files/lnk", nil, nil); w.Code != 401 {
		t.Errorf("without credential: %d", w.Code)
	}
	w := fileRequest(t, s.Handler(), "OPTIONS", "/files/a/b", nil, map[string]string{"Origin": "http://localhost:1420", "Access-Control-Request-Method": "DELETE"})
	if w.Code != 204 || !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "DELETE") {
		t.Errorf("preflight: %d", w.Code)
	}
}

package maintenance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
)

// archive builds a release tarball; files maps names to contents (a "mobd" script by default).
func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func script(version string) string { return "#!/bin/sh\necho 'mobd " + version + "'\n" }

// releaseServer serves latest.json, release.json and the archive for this platform.
func releaseServer(t *testing.T, version string, tarball []byte, sum string) *httptest.Server {
	t.Helper()
	key, arch, err := platform()
	if err != nil {
		t.Skip(err)
	}
	if sum == "" {
		h := sha256.Sum256(tarball)
		sum = hex.EncodeToString(h[:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mobd/latest.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q,"notes":"fixes"}`, version)
	})
	mux.HandleFunc("/mobd/v"+version+"/release.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q,"platforms":{%q:{"sha256":%q}}}`, version, key, sum)
	})
	mux.HandleFunc(fmt.Sprintf("/mobd/v%s/mobd_%s_%s.tar.gz", version, version, arch), func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func upgraderFor(t *testing.T, base string) Upgrader {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "mobd")
	os.MkdirAll(filepath.Dir(exe), 0o700)
	os.WriteFile(exe, []byte(script("0.5.2")), 0o755)
	return Upgrader{Base: base, Exe: exe, Current: "0.5.2",
		Cache: filepath.Join(dir, "cache"), Verify: func(string) error { return nil }}
}

func TestCheckReportsNewerRelease(t *testing.T) {
	srv := releaseServer(t, "0.6.0", archive(t, map[string]string{"LICENSE": "", "README.md": "", "mobd": script("0.6.0")}), "")
	u := upgraderFor(t, srv.URL)
	rel, err := u.Check(context.Background())
	if err != nil || rel.Latest != "0.6.0" || !rel.Newer || !rel.CanUpgrade || rel.Notes != "fixes" {
		t.Fatalf("%+v %v", rel, err)
	}
	if Newer("0.5.2", "0.5.10") || !Newer("0.10.0", "0.9.9") {
		t.Fatal("versions compare as strings")
	}
	u.Base = "http://example.com"
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain HTTP accepted: %v", err)
	}
}

func TestPrepareVerifiesAndKeepsTheDownload(t *testing.T) {
	good := archive(t, map[string]string{"LICENSE": "", "README.md": "", "mobd": script("0.6.0")})
	srv := releaseServer(t, "0.6.0", good, "")
	u := upgraderFor(t, srv.URL)
	bin, v, err := u.Prepare(context.Background(), "")
	if err != nil || v != "0.6.0" {
		t.Fatalf("%v %s", err, v)
	}
	if out, _ := os.ReadFile(bin); string(out) != script("0.6.0") {
		t.Fatal("wrong binary")
	}
	srv.Close() // a kept, verified download needs no network
	if again, _, err := u.Prepare(context.Background(), "0.6.0"); err != nil || again != bin {
		t.Fatalf("cached download not reused: %v", err)
	}
	if exe, _ := os.ReadFile(u.Exe); string(exe) != script("0.5.2") {
		t.Fatal("prepare touched the installed binary")
	}
}

func TestPrepareRefusesBadReleases(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		sum   string
		want  string
	}{
		"checksum": {map[string]string{"LICENSE": "", "README.md": "", "mobd": script("0.6.0")}, strings.Repeat("0", 64), "checksum mismatch"},
		"contents": {map[string]string{"LICENSE": "", "README.md": "", "mobd": script("0.6.0"), "extra": ""}, "", "unexpected archive contents"},
		"version":  {map[string]string{"LICENSE": "", "README.md": "", "mobd": script("0.5.9")}, "", "version mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := releaseServer(t, "0.6.0", archive(t, tc.files), tc.sum)
			u := upgraderFor(t, srv.URL)
			if _, _, err := u.Prepare(context.Background(), ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if _, err := os.Stat(filepath.Join(u.Cache, "0.6.0", "verified")); !os.IsNotExist(err) {
				t.Fatal("a refused download is kept as verified")
			}
		})
	}
	u := upgraderFor(t, "http://localhost:1")
	if _, _, err := u.Prepare(context.Background(), "../1.0.0"); err == nil {
		t.Fatal("invalid version accepted")
	}
}

func TestUpgradeSwapsAndRollsBack(t *testing.T) {
	dir, args, _ := setup(t, "")
	st := Store{Dir: dir}
	exe := filepath.Join(dir, "mobd-installed")
	newBin := filepath.Join(dir, "mobd-new")
	os.WriteFile(exe, []byte("old"), 0o755)
	os.WriteFile(newBin, []byte("new"), 0o755)

	svc := &fakeService{}
	if state, err := Upgrade(st, svc, args, false, exe, newBin, nil); err != nil || state != StateDone {
		t.Fatalf("%s %v", state, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatal("not swapped")
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != "old" {
		t.Fatal("previous binary not kept")
	}

	// The next upgrade's binary does not come up: the previous one runs again.
	os.WriteFile(newBin, []byte("broken"), 0o755)
	started := 0
	svc = &fakeService{fail: func(config.Config) error {
		started++
		if b, _ := os.ReadFile(exe); string(b) == "broken" {
			return errors.New("did not answer")
		}
		return nil
	}}
	state, err := Upgrade(st, svc, args, false, exe, newBin, nil)
	if state != StateRolledBack || err == nil || started != 2 {
		t.Fatalf("%s %v starts=%d", state, err, started)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("not rolled back: %s", b)
	}
}

package maintenance

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBase is where releases are published (latest.json, v<version>/release.json and archives).
const DefaultBase = "https://dl.mymob.dev"

// Release is what `mobd upgrade --check` reports.
type Release struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Newer   bool   `json:"newer"`
	Notes   string `json:"notes,omitempty"`
	// CanUpgrade is false when this install cannot replace itself; Reason says why and what to do.
	CanUpgrade bool   `json:"canUpgrade"`
	Reason     string `json:"reason,omitempty"`
}

// Upgrader downloads, verifies and stages a release for one installed binary (Exe). The checks are
// install.sh's: manifest, SHA-256, archive contents, macOS Developer ID signature, --version.
type Upgrader struct {
	Base    string // DefaultBase unless overridden (MOBD_UPDATE_BASE, tests)
	Exe     string // the installed binary to replace
	Current string // its version
	Cache   string // where verified downloads are kept (~/.mobd/upgrade)
	Client  *http.Client
	// Verify checks the extracted binary's signature; nil uses the platform check.
	Verify func(path string) error
}

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// platform is the release.json key and archive suffix for this system, as install.sh picks them.
func platform() (string, string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "darwin-aarch64", "darwin_arm64", nil
	case "linux/amd64":
		return "linux-x86_64", "linux_amd64", nil
	case "linux/arm64":
		return "linux-aarch64", "linux_arm64", nil
	}
	return "", "", fmt.Errorf("no mobd releases for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// base checks the download origin: HTTPS, or plain HTTP to this computer only (tests).
func (u Upgrader) base() (string, error) {
	b := strings.TrimRight(u.Base, "/")
	if b == "" {
		b = DefaultBase
	}
	p, err := url.Parse(b)
	if err != nil {
		return "", err
	}
	if p.Scheme == "https" || (p.Scheme == "http" && (p.Hostname() == "127.0.0.1" || p.Hostname() == "localhost")) {
		return b, nil
	}
	return "", fmt.Errorf("download address %s must use HTTPS", b)
}

func (u Upgrader) get(ctx context.Context, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := u.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("%s is larger than expected", url)
	}
	return err
}

type manifest struct {
	Version   string `json:"version"`
	Notes     string `json:"notes"`
	Platforms map[string]struct {
		SHA256 string `json:"sha256"`
	} `json:"platforms"`
}

func (u Upgrader) manifest(ctx context.Context, path string) (manifest, error) {
	var m manifest
	b, err := u.base()
	if err != nil {
		return m, err
	}
	var buf strings.Builder
	if err := u.get(ctx, b+"/mobd/"+path, &buf, 1<<20); err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(buf.String()), &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	if !stableVersion.MatchString(m.Version) {
		return m, fmt.Errorf("%s: invalid version %q", path, m.Version)
	}
	return m, nil
}

// Blocked says why this install cannot replace itself, or "" when it can.
func (u Upgrader) Blocked() string {
	exe := u.Exe
	if runtime.GOOS == "linux" && (strings.HasPrefix(exe, "/usr/") || strings.HasPrefix(exe, "/bin/")) {
		return "mobd was installed with a package (.deb); upgrade it with apt"
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".mobd-write-check-")
	if err != nil {
		return fmt.Sprintf("cannot write to %s; reinstall mobd with install.sh", filepath.Dir(exe))
	}
	f.Close()
	os.Remove(f.Name())
	if runtime.GOOS == "darwin" && u.Verify == nil {
		if _, err := teamID(exe); err != nil {
			return "this mobd is not a signed release (a development build); install a release with install.sh"
		}
	}
	return ""
}

// Check reads the latest stable release (`mobd upgrade --check`). It changes nothing.
func (u Upgrader) Check(ctx context.Context) (Release, error) {
	r := Release{Current: u.Current}
	m, err := u.manifest(ctx, "latest.json")
	if err != nil {
		return r, err
	}
	r.Latest, r.Notes, r.Newer = m.Version, m.Notes, Newer(m.Version, u.Current)
	r.Reason = u.Blocked()
	r.CanUpgrade = r.Reason == ""
	return r, nil
}

// Newer reports whether version a is newer than b (X.Y.Z; an unparsable b counts as older).
func Newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		var y int
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// Prepare downloads and verifies version ("" = latest) while mobd keeps serving, and returns the
// verified binary. A verified download is kept and reused.
func (u Upgrader) Prepare(ctx context.Context, version string) (string, string, error) {
	if reason := u.Blocked(); reason != "" {
		return "", "", errors.New(reason)
	}
	key, arch, err := platform()
	if err != nil {
		return "", "", err
	}
	if version == "" {
		m, err := u.manifest(ctx, "latest.json")
		if err != nil {
			return "", "", err
		}
		version = m.Version
	}
	if !stableVersion.MatchString(version) {
		return "", "", fmt.Errorf("invalid version %q; expected X.Y.Z", version)
	}
	dir := filepath.Join(u.Cache, version)
	bin := filepath.Join(dir, "mobd")
	if _, err := os.Stat(filepath.Join(dir, "verified")); err == nil && u.versionIs(bin, version) == nil {
		return bin, version, nil
	}
	rel, err := u.manifest(ctx, "v"+version+"/release.json")
	if err != nil {
		return "", "", err
	}
	if rel.Version != version {
		return "", "", errors.New("release version mismatch")
	}
	want := strings.ToLower(rel.Platforms[key].SHA256)
	if len(want) != 64 {
		return "", "", fmt.Errorf("mobd %s has no build for this system", version)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	b, _ := u.base()
	archive := filepath.Join(dir, "mobd.tar.gz")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	err = u.get(ctx, fmt.Sprintf("%s/mobd/v%s/mobd_%s_%s.tar.gz", b, version, version, arch), io.MultiWriter(f, h), 512<<20)
	f.Close()
	if err != nil {
		return "", "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		os.RemoveAll(dir)
		return "", "", errors.New("checksum mismatch; nothing was changed")
	}
	if err := extract(archive, bin); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	verify := u.Verify
	if verify == nil {
		verify = u.verifySignature
	}
	if err := verify(bin); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("signature check: %w", err)
	}
	if err := u.versionIs(bin, version); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	os.Remove(archive)
	return bin, version, os.WriteFile(filepath.Join(dir, "verified"), nil, 0o600)
}

// extract writes the archive's mobd to bin; the archive may hold only the regular files LICENSE,
// README.md and mobd.
func extract(archive, bin string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var names []string
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag != tar.TypeReg {
			return errors.New("archive contains links or special files")
		}
		if hdr.Name != "mobd" {
			continue
		}
		out, err := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, 512<<20))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		found = true
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "LICENSE,README.md,mobd" || !found {
		return errors.New("unexpected archive contents")
	}
	return nil
}

func (u Upgrader) versionIs(bin, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("the new mobd does not run: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != "mobd "+version {
		return fmt.Errorf("binary version mismatch: %q", got)
	}
	return nil
}

// verifySignature: on macOS the new binary must carry the same Developer ID team as the installed
// one (install.sh pins the team the same way); Linux releases are checked by SHA-256 only.
func (u Upgrader) verifySignature(bin string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	team, err := teamID(u.Exe)
	if err != nil {
		return err
	}
	req := fmt.Sprintf(`=anchor apple generic and identifier "com.xrdavies.mobd" and certificate leaf[subject.OU] = "%s" and certificate leaf[field.1.2.840.113635.100.6.1.13] exists`, team)
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--strict", "-R", req, bin).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func teamID(exe string) (string, error) {
	out, _ := exec.Command("/usr/bin/codesign", "-dv", "--verbose=2", exe).CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "TeamIdentifier="); ok && v != "not set" && v != "" {
			return v, nil
		}
	}
	return "", errors.New("not signed with a Developer ID")
}

// Swap puts the verified binary in place of Exe, keeping the old one as Exe+".previous".
func Swap(exe, bin string) error {
	staged := exe + ".new"
	in, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(staged)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(staged)
		return err
	}
	if err := copyFile(exe, exe+".previous"); err != nil {
		os.Remove(staged)
		return err
	}
	return os.Rename(staged, exe)
}

// Unswap restores the binary Swap kept.
func Unswap(exe string) error { return os.Rename(exe+".previous", exe) }

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	tmp := to + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

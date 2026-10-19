//go:build linux

package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultRoot is where cgroup v2 is mounted.
const DefaultRoot = "/sys/fs/cgroup"

// ErrNoController: the memory controller is not enabled for the cgroup (not delegated to the
// user manager), so memory.current is missing.
var ErrNoController = errors.New("cgroup: memory controller not enabled")

// FS is the cgroup v2 hierarchy. Directories are absolute paths as returned by Path.
type FS interface {
	// Path resolves a unit's ControlGroup ("/user.slice/...") to a directory under the root.
	Path(controlGroup string) string
	Mkdir(dir string) error                     // child cgroup (session, apps, turn); exists is fine
	Rmdir(dir string) error                     // fails while populated; missing is fine
	Move(dir string, pid int) error             // write cgroup.procs
	Open(dir string) (*os.File, error)          // O_DIRECTORY fd for CLONE_INTO_CGROUP
	CPUUsage(dir string) (time.Duration, error) // cpu.stat usage_usec (always present on v2)
	Memory(dir string) (int64, error)           // memory.current; ErrNoController if absent
	Populated(dir string) (bool, error)         // cgroup.events populated
	Frozen(dir string) (bool, error)            // cgroup.events frozen
	FreezeDirect(dir string, on bool) error     // cgroup.freeze, fallback when FreezeUnit fails
}

// New returns the hierarchy mounted at root (DefaultRoot, or a temp directory in tests).
func New(root string) FS { return fsys{root: root} }

type fsys struct{ root string }

func (f fsys) Path(controlGroup string) string {
	return filepath.Join(f.root, filepath.Clean("/"+controlGroup))
}

func (fsys) Mkdir(dir string) error {
	if err := os.Mkdir(dir, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func (fsys) Rmdir(dir string) error {
	if err := syscall.Rmdir(dir); err != nil && !errors.Is(err, syscall.ENOENT) {
		return &os.PathError{Op: "rmdir", Path: dir, Err: err}
	}
	return nil
}

func (fsys) Move(dir string, pid int) error {
	return write(filepath.Join(dir, "cgroup.procs"), strconv.Itoa(pid))
}

func (fsys) Open(dir string) (*os.File, error) {
	return os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
}

func (fsys) CPUUsage(dir string) (time.Duration, error) {
	v, err := keyed(filepath.Join(dir, "cpu.stat"), "usage_usec")
	return time.Duration(v) * time.Microsecond, err
}

func (fsys) Memory(dir string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if errors.Is(err, os.ErrNotExist) {
		if _, serr := os.Stat(dir); serr != nil {
			return 0, serr
		}
		return 0, ErrNoController
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func (fsys) Populated(dir string) (bool, error) {
	v, err := keyed(filepath.Join(dir, "cgroup.events"), "populated")
	return v == 1, err
}

func (fsys) Frozen(dir string) (bool, error) {
	v, err := keyed(filepath.Join(dir, "cgroup.events"), "frozen")
	return v == 1, err
}

func (fsys) FreezeDirect(dir string, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return write(filepath.Join(dir, "cgroup.freeze"), v)
}

// write writes an interface file in one write(2), as the kernel expects.
func write(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// keyed reads one "key value" line of a flat-keyed file (cpu.stat, cgroup.events).
func keyed(path, key string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%s: %s: %w", path, key, err)
			}
			return n, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s: no %s", path, key)
}

//go:build linux

package cgroup

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Fake is an FS over a temp directory that also does the kernel's part: Mkdir creates the
// interface files, and Move, Exit and FreezeDirect keep cgroup.events up to date. Reads go
// through the real code. Tests set usage with SetCPU and SetMemory.
type Fake struct {
	FS
	root     string
	NoMemory bool // new cgroups get no memory.current, as without the memory controller

	mu sync.Mutex
}

func NewFake(root string) *Fake { return &Fake{FS: New(root), root: root} }

// Create makes dir and any missing parents as cgroups, like systemd creating a scope.
func (f *Fake) Create(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel, err := filepath.Rel(f.root, dir)
	if err != nil {
		return err
	}
	p, parts := f.root, []string{"."}
	if rel != "." {
		parts = append(parts, strings.Split(rel, string(filepath.Separator))...)
	}
	for _, part := range parts {
		p = filepath.Join(p, part)
		if err := f.mkdir(p); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func (f *Fake) Mkdir(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.mkdir(dir); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func (f *Fake) mkdir(dir string) error {
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	files := map[string]string{
		"cgroup.procs":  "",
		"cgroup.freeze": "0",
		"cpu.stat":      "usage_usec 0\nuser_usec 0\nsystem_usec 0\n",
	}
	if !f.NoMemory {
		files["memory.current"] = "0\n"
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0644); err != nil {
			return err
		}
	}
	return f.refresh()
}

func (f *Fake) Rmdir(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pop, err := f.FS.Populated(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	} else if pop {
		return &os.PathError{Op: "rmdir", Path: dir, Err: syscall.EBUSY}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			return &os.PathError{Op: "rmdir", Path: dir, Err: syscall.EBUSY}
		}
	}
	return os.RemoveAll(dir)
}

// Move takes pid out of whatever cgroup holds it, as the kernel does.
func (f *Fake) Move(dir string, pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "cgroup.procs")); err != nil {
		return err
	}
	if err := f.remove(pid); err != nil {
		return err
	}
	pids, err := procs(dir)
	if err != nil {
		return err
	}
	if err := writeProcs(dir, append(pids, pid)); err != nil {
		return err
	}
	return f.refresh()
}

// Exit removes pid from its cgroup, as when the process dies.
func (f *Fake) Exit(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.remove(pid); err != nil {
		return err
	}
	return f.refresh()
}

func (f *Fake) FreezeDirect(dir string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.FS.FreezeDirect(dir, on); err != nil {
		return err
	}
	return f.refresh()
}

func (f *Fake) SetCPU(dir string, usage time.Duration) error {
	return os.WriteFile(filepath.Join(dir, "cpu.stat"),
		[]byte("usage_usec "+strconv.FormatInt(usage.Microseconds(), 10)+"\n"), 0644)
}

func (f *Fake) SetMemory(dir string, bytes int64) error {
	return os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatInt(bytes, 10)+"\n"), 0644)
}

// cgroups lists every fake cgroup directory under the root.
func (f *Fake) cgroups() ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "cgroup.procs" {
			dirs = append(dirs, filepath.Dir(p))
		}
		return err
	})
	return dirs, err
}

func (f *Fake) remove(pid int) error {
	dirs, err := f.cgroups()
	if err != nil {
		return err
	}
	for _, d := range dirs {
		pids, err := procs(d)
		if err != nil {
			return err
		}
		if i := slices.Index(pids, pid); i >= 0 {
			if err := writeProcs(d, slices.Delete(pids, i, i+1)); err != nil {
				return err
			}
		}
	}
	return nil
}

// refresh rewrites every cgroup.events: populated if the subtree has a process, frozen if the
// cgroup or an ancestor has cgroup.freeze set.
func (f *Fake) refresh() error {
	dirs, err := f.cgroups()
	if err != nil {
		return err
	}
	hasProcs, freeze := map[string]bool{}, map[string]bool{}
	for _, d := range dirs {
		pids, err := procs(d)
		if err != nil {
			return err
		}
		hasProcs[d] = len(pids) > 0
		b, err := os.ReadFile(filepath.Join(d, "cgroup.freeze"))
		if err != nil {
			return err
		}
		freeze[d] = strings.TrimSpace(string(b)) == "1"
	}
	under := func(d, parent string) bool {
		return d == parent || strings.HasPrefix(d, parent+string(filepath.Separator))
	}
	for _, d := range dirs {
		populated, frozen := 0, 0
		for _, o := range dirs {
			if hasProcs[o] && under(o, d) {
				populated = 1
			}
			if freeze[o] && under(d, o) {
				frozen = 1
			}
		}
		events := "populated " + strconv.Itoa(populated) + "\nfrozen " + strconv.Itoa(frozen) + "\n"
		if err := os.WriteFile(filepath.Join(d, "cgroup.events"), []byte(events), 0644); err != nil {
			return err
		}
	}
	return nil
}

func procs(dir string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, s := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func writeProcs(dir string, pids []int) error {
	var b strings.Builder
	for _, pid := range pids {
		b.WriteString(strconv.Itoa(pid) + "\n")
	}
	return os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(b.String()), 0644)
}

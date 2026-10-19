//go:build linux

package manager

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxLog is the size at which a component log is rotated to <name>.log.1.
const maxLog = 1 << 20

// logFile is one component's log (xvfb, dbus, openbox, apps) in <desktop>/logs. Processes
// hold it O_APPEND, so rotation copies and truncates instead of renaming.
type logFile struct {
	*os.File
	start int64 // size when opened: what this start of the desktop wrote comes after it
}

func openLog(dir, name string) (*logFile, error) {
	path := filepath.Join(dir, name+".log")
	if err := rotate(path, maxLog); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &logFile{File: f, start: fi.Size()}, nil
}

// reason picks the line that says why a process failed, from what was written since the log
// was opened: X's first "(EE)" error that is more than the "Fatal server error:" heading (xkbcomp
// warnings come first), else the first non-empty line.
func (l *logFile) reason() string {
	f, err := os.Open(l.Name())
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(l.start, io.SeekStart); err != nil {
		return ""
	}
	first := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "(EE)"); ok {
			if rest = strings.TrimSpace(rest); rest != "" && rest != "Fatal server error:" {
				return clip(rest)
			}
			continue
		}
		if first == "" {
			first = line
		}
	}
	return clip(first)
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// contains reports whether the log written since opening mentions s.
func (l *logFile) contains(s string) bool {
	b, err := os.ReadFile(l.Name())
	if err != nil || int64(len(b)) < l.start {
		return false
	}
	return strings.Contains(string(b[l.start:]), s)
}

// rotate copies path to path.1 and truncates it once it is larger than max.
func rotate(path string, max int64) error {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || fi.Size() <= max {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(path+".1", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

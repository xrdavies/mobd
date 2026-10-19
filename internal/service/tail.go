package service

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"time"
)

// Follow prints the last n lines of path, then what is appended, like tail -n n -F: it
// reopens the file when it is replaced or truncated. It returns only on error.
func Follow(path string, n int, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()
	if err := lastLines(f, n, w); err != nil {
		return err
	}
	for {
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		info, statErr := os.Stat(path)
		cur, err := f.Stat()
		if err != nil {
			return err
		}
		offset, _ := f.Seek(0, io.SeekCurrent)
		if statErr == nil && (!os.SameFile(info, cur) || info.Size() < offset) {
			next, err := os.Open(path)
			if err != nil {
				continue
			}
			f.Close()
			f = next
		}
	}
}

// Tail prints the last n lines of path.
func Tail(path string, n int, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return lastLines(f, n, w)
}

// lastLines writes the last n lines of f and leaves f at its end.
func lastLines(f *os.File, n int, w io.Writer) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	const chunk = 64 << 10
	size := info.Size()
	start := max(size-chunk, 0)
	var data []byte
	// Read backwards until n newlines are found or the file is exhausted.
	for {
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return err
		}
		data = buf
		if start == 0 || bytes.Count(data, []byte("\n")) > n {
			break
		}
		start = max(start-chunk, 0)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first piece may be a partial line
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	bw := bufio.NewWriter(w)
	for _, l := range lines {
		bw.Write(l)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	_, err = f.Seek(size, io.SeekStart)
	return err
}

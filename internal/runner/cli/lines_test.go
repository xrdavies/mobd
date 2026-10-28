package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func lines(t *testing.T, input string) []string {
	t.Helper()
	var got []string
	if err := Lines(strings.NewReader(input), "test", func(l string) error { got = append(got, l); return nil }); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLinesSplitsLikeAScanner(t *testing.T) {
	got := lines(t, "a\r\nb\n\nc")
	if strings.Join(got, "|") != "a|b||c" {
		t.Fatalf("%q", got)
	}
	if got := lines(t, ""); len(got) != 0 {
		t.Fatalf("empty input: %q", got)
	}
}

// Codex answers thread/resume with a long conversation's stored turns in one line; the old
// 8 MB limit failed such a turn with "bufio.Scanner: token too long".
func TestLinesReadsALineOverEightMB(t *testing.T) {
	big := strings.Repeat("x", 20<<20)
	got := lines(t, `{"id":1}`+"\n"+big+"\nafter\n")
	if len(got) != 3 || got[0] != `{"id":1}` || len(got[1]) != len(big) || got[2] != "after" {
		t.Fatalf("got %d lines, middle %d bytes", len(got), len(got[1]))
	}
}

// A line past MaxLine fails with an error that says what to do, without reading it all.
func TestLinesRefusesALineOverTheLimit(t *testing.T) {
	defer func(old int) { maxLine = old }(maxLine)
	maxLine = 2 << 20
	r := io.MultiReader(strings.NewReader("ok\n"), &endless{})
	var seen []string
	err := Lines(r, "codex", func(l string) error { seen = append(seen, l); return nil })
	if err == nil || !strings.Contains(err.Error(), "codex printed a line over 2 MB") || len(seen) != 1 {
		t.Fatalf("err %v, seen %q", err, seen)
	}
}

type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestLinesStopsAtTheConsumersError(t *testing.T) {
	stop := errors.New("stop")
	n := 0
	err := Lines(strings.NewReader("a\nb\nc\n"), "test", func(string) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err %v after %d lines", err, n)
	}
}

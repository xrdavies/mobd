package artifact

import (
	"errors"
	"os"
	"testing"
)

func TestSaveResolveDelete(t *testing.T) {
	s := New(t.TempDir() + "/artifacts")
	id, err := s.Save("image/png", []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	path, mime, err := s.Path(id)
	if err != nil || mime != "image/png" {
		t.Fatalf("path=%s mime=%s err=%v", path, mime, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "png" {
		t.Fatalf("data %q", data)
	}
	s.Delete(id)
	if _, _, err := s.Path(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted artifact resolved: %v", err)
	}
	if _, err := s.Save("text/html", nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("html accepted: %v", err)
	}
	for _, bad := range []string{"../state.json", "x.png", id + "/.."} {
		if _, _, err := s.Path(bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q resolved", bad)
		}
	}
}

package service

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentPlist(t *testing.T) {
	data := agentPlist("/a & b/mobd", "/tmp/<config>.json", "/work space", "/logs", "/bin:/a&b")
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	var values []string
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if el, ok := token.(xml.StartElement); ok && el.Name.Local == "string" {
			var value string
			if err := dec.DecodeElement(&value, &el); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
	}
	joined := strings.Join(values, "|")
	if !strings.Contains(joined, "/a & b/mobd|serve|--config|/tmp/<config>.json") {
		t.Fatal(joined)
	}
	if strings.Contains(string(data), "SharedSecret") {
		t.Fatal("secrets must remain in private config")
	}
}

func TestStopFailurePreservesLogin(t *testing.T) {
	dir := t.TempDir()
	plist := filepath.Join(dir, "mobd.plist")
	if err := writePrivate(plist, []byte("config")); err != nil {
		t.Fatal(err)
	}
	m := Manager{Dir: dir, Plist: plist, Target: "gui/123/test", run: func(args ...string) ([]byte, error) {
		if args[0] == "print" {
			return nil, nil
		}
		return []byte("permission denied"), errors.New("failed")
	}}
	if err := m.Stop(); err == nil {
		t.Fatal("bootout failure ignored")
	}
	if _, err := os.Stat(plist); err != nil {
		t.Fatal("login config deleted after failed stop")
	}
	m.run = func(...string) ([]byte, error) { return nil, nil }
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatal("login config not removed")
	}
}

func TestLocalURL(t *testing.T) {
	for addr, want := range map[string]string{":8080": "http://127.0.0.1:8080", "0.0.0.0:8000": "http://127.0.0.1:8000", "[::]:8080": "http://[::1]:8080", "192.168.1.2:9000": "http://192.168.1.2:9000"} {
		if got := LocalURL(addr); got != want {
			t.Errorf("%s: %s", addr, got)
		}
	}
}

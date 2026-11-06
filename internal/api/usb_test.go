//go:build unix

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/xrdavies/mobd/internal/usb"
	"github.com/xrdavies/mobd/internal/usbip"
)

// The whole API: create a session, open its tunnel as Mob Desktop would and export an emulated
// device over it, see it attached, release it. Another paired client cannot see or touch it.
func TestUSBSessionOverTheAPI(t *testing.T) {
	s := testServer(t, "")
	imp := &usb.Fake{}
	s.usb = usb.New(imp, nil)
	alice, bob := pairedClient(t, s), pairedClient(t, s)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call := func(token, method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var v map[string]any
		_ = json.NewDecoder(res.Body).Decode(&v)
		return res.StatusCode, v
	}

	code, health := call(alice.Token, "GET", "/health", "")
	features, _ := health["features"].([]any)
	if code != 200 || !slices.Contains(features, any("usbRedirection")) || health["usb"].(map[string]any)["ready"] != true {
		t.Fatalf("health %d %v %v", code, features, health["usb"])
	}

	code, created := call(alice.Token, "POST", "/usb/sessions", `{"busId":"1-1","name":"Loopback"}`)
	if code != http.StatusCreated || created["state"] != "connecting" {
		t.Fatalf("create %d %v", code, created)
	}
	id := created["id"].(string)
	if code, _ := call(bob.Token, "GET", "/usb/sessions/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("bob sees alice's session: %d", code)
	}

	// Bob cannot open alice's tunnel.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/usb/sessions/" + id + "/tunnel"
	if _, res, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + bob.Token}}}); err == nil || res == nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("bob opened alice's tunnel: %v", err)
	}

	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + alice.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	// The exporter: the emulated device behind an in-memory connection, bridged to the WebSocket.
	dev, exp := net.Pipe()
	go usbip.Serve(exp, &usbip.Loopback{BusID: "1-1"})
	states := make(chan string, 8)
	go func() {
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				dev.Close()
				return
			}
			if typ == websocket.MessageText {
				var m struct{ T, State string }
				_ = json.Unmarshal(b, &m)
				states <- m.State
				continue
			}
			if _, err := dev.Write(b); err != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := dev.Read(buf)
			if err != nil {
				return
			}
			if ws.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
		}
	}()

	select {
	case st := <-states:
		if st != "connected" {
			t.Fatalf("first state %q", st)
		}
	case <-ctx.Done():
		t.Fatal("never connected")
	}
	if d := imp.Descriptor(0); len(d) != 18 {
		t.Fatalf("no device descriptor crossed the tunnel: %x", d)
	}
	_, list := call(alice.Token, "GET", "/usb", "")
	if ss := list["sessions"].([]any); len(ss) != 1 || ss[0].(map[string]any)["localBusId"] != "9-1" {
		t.Fatalf("list %v", list)
	}
	if code, _ := call(bob.Token, "DELETE", "/usb/sessions/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("bob released alice's session: %d", code)
	}
	code, released := call(alice.Token, "DELETE", "/usb/sessions/"+id, "")
	if code != 200 || released["state"] != "disconnected" || len(imp.Detached) != 1 {
		t.Fatalf("release %d %v detached %v", code, released, imp.Detached)
	}
}

func TestUSBCreateNeedsAReadyImporter(t *testing.T) {
	s := testServer(t, "")
	s.usb = usb.New(&usb.Fake{NotReady: "run `sudo mobd usb setup`"}, nil)
	rr := doJSON(t, s, "POST", "/usb/sessions", `{"busId":"1-1"}`)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "usb_not_ready") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, s, "POST", "/usb/sessions", `{"busId":""}`); rr.Code != http.StatusBadRequest && rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty bus %d", rr.Code)
	}
}

package pairing

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTest(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func invitation(t *testing.T, s *Store) string {
	t.Helper()
	i, err := s.Invite("https://office.example")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(i.URL)
	if err != nil || u.Path != "/pair" || u.RawQuery != "" {
		t.Fatalf("invalid invitation: %v %v", i, err)
	}
	f, _ := url.ParseQuery(u.Fragment)
	if f.Get("v") != "1" || f.Get("serverId") != i.ServerID || len(f.Get("invite")) != 64 {
		t.Fatal("invalid fragment")
	}
	return f.Get("invite")
}

func assertAuthorized(t *testing.T, s *Store, token string, want bool) {
	t.Helper()
	_, finish, err := s.Begin(context.Background(), token, "GET", "/events")
	if finish != nil {
		finish()
	}
	if (err == nil) != want {
		t.Fatalf("authorized=%t want %t: %v", err == nil, want, err)
	}
}

func TestCredentialSurvivesInvitationExpiryAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s := openTest(t, path)
	now := time.Now()
	s.now = func() time.Time { return now }
	secret := invitation(t, s)
	c, err := s.Redeem(secret, "phone")
	if err != nil {
		t.Fatal(err)
	}
	expired := invitation(t, s)
	now = now.Add(InvitationTTL)
	if _, err := s.Redeem(expired, "desktop"); !errors.Is(err, ErrInvitation) {
		t.Fatal("expired invite accepted")
	}
	if _, err := s.Redeem(secret, "desktop"); !errors.Is(err, ErrInvitation) {
		t.Fatal("consumed invite accepted")
	}
	now = now.AddDate(20, 0, 0)
	assertAuthorized(t, s, c.Token, true)
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), c.Token) || strings.Contains(string(data), secret) {
		t.Fatal("plaintext secret persisted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("auth file is not private")
	}
	s.Close()
	s = openTest(t, path)
	assertAuthorized(t, s, c.Token, true)
	if s.state.ServerID != c.ServerID || s.Clients()[0].LastSeenAt == nil {
		t.Fatal("identity or activity lost")
	}
	if err := s.Revoke(c.ClientID, false); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openTest(t, path)
	assertAuthorized(t, s, c.Token, false)
	if s.Clients()[0].RevokedAt == nil {
		t.Fatal("revocation lost on restart")
	}
	if err := s.Revoke(c.ClientID, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openTest(t, path)
	assertAuthorized(t, s, c.Token, false)
	if len(s.Clients()) != 0 {
		t.Fatal("deleted record restored")
	}
}

func TestConcurrentRedemptionIsSingleUse(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "auth.json"))
	secret := invitation(t, s)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 32 {
		wg.Go(func() {
			if _, err := s.Redeem(secret, "phone"); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrInvitation) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 || len(s.Clients()) != 1 {
		t.Fatal("multiple credentials issued")
	}
}

func TestPersistenceFailureDoesNotIssueCredentialOrAcknowledgeRevocation(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "auth.json"))
	secret := invitation(t, s)
	path := s.path
	s.path = filepath.Join(t.TempDir(), "missing", "auth.json")
	if c, err := s.Redeem(secret, "phone"); err == nil || c.Token != "" || len(s.Clients()) != 0 {
		t.Fatal("issued unpersisted credential")
	}
	s.path = path
	c, err := s.Redeem(secret, "phone")
	if err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(t.TempDir(), "missing", "auth.json")
	if _, _, err := s.Begin(context.Background(), c.Token, "GET", "/events"); err == nil {
		t.Fatal("activity write failure ignored")
	}
	if err := s.Revoke(c.ClientID, false); err == nil {
		t.Fatal("acknowledged unpersisted revocation")
	}
	if err := s.Reset(); err == nil {
		t.Fatal("acknowledged unpersisted reset")
	}
	s.path = path
	assertAuthorized(t, s, c.Token, true)
}

func TestCloseInvitationsAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s := openTest(t, path)
	c, _ := s.Redeem(invitation(t, s), "phone")
	secret := invitation(t, s)
	s.CloseInvitations()
	if _, err := s.Redeem(secret, "desktop"); !errors.Is(err, ErrInvitation) {
		t.Fatal("closed invitation accepted")
	}
	assertAuthorized(t, s, c.Token, true)
	ctx, finish, err := s.Begin(context.Background(), c.Token, "GET", "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	secret = invitation(t, s)
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("reset left connection open")
	}
	if s.state.ServerID == c.ServerID {
		t.Fatal("reset retained identity")
	}
	assertAuthorized(t, s, c.Token, false)
	if _, err := s.Redeem(secret, "desktop"); !errors.Is(err, ErrInvitation) {
		t.Fatal("reset left invitation open")
	}
	s.Close()
	s = openTest(t, path)
	assertAuthorized(t, s, c.Token, false)
}

func TestOpenFailsClosedAndLocksState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s := openTest(t, path)
	if other, err := Open(path); err == nil {
		other.Close()
		t.Fatal("two writers accepted")
	}
	s.Close()
	for _, data := range []string{`{`, `null`, `{}`, `{"version":2}`, `{"version":1,"serverId":"x","clients":{}}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(path); err == nil {
			s.Close()
			t.Fatalf("corrupt state accepted: %s", data)
		}
	}
}

func TestInviteAndClientValidation(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "auth.json"))
	for _, address := range []string{"", "http://office.example", "https://x/a", "https://x/?token=x", "https://x/#secret", "https://user:pass@x", "javascript:foo", "https://"} {
		if _, err := s.Invite(address); err == nil {
			t.Errorf("accepted URL %s", address)
		}
	}
	for _, address := range []string{"https://office.example", "http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080"} {
		if _, err := s.Invite(address); err != nil {
			t.Error(err)
		}
	}
	secret := invitation(t, s)
	for _, name := range []string{"", "  ", "phone\x1b[31m", strings.Repeat("x", 129)} {
		if _, err := s.Redeem(secret, name); err == nil {
			t.Errorf("accepted name %q", name)
		}
	}
	if _, err := s.Redeem(secret, "手机"); err != nil {
		t.Fatal(err)
	}
	assertAuthorized(t, s, "", false)
	assertAuthorized(t, s, strings.Repeat("x", 64), false)
}

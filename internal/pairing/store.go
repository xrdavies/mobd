// Package pairing owns invitations, persistent client credentials and live requests.
package pairing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

var (
	ErrUnauthorized = errors.New("pairing required or credential revoked")
	ErrInvitation   = errors.New("invitation invalid, expired or already used")
	ErrNotFound     = errors.New("client not found")
	ErrName         = errors.New("client name must be 1-128 bytes without control characters")
)

const InvitationTTL = 10 * time.Minute

type Client struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

type record struct {
	Client
	TokenHash string `json:"tokenHash"`
}

type state struct {
	Version  int               `json:"version"`
	ServerID string            `json:"serverId"`
	Clients  map[string]record `json:"clients"`
}

type Invitation struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	ServerID  string    `json:"serverId"`
}

type Credential struct {
	ServerID string `json:"serverId"`
	ClientID string `json:"clientId"`
	Token    string `json:"token"`
}

type Connection struct {
	ID        string    `json:"id"`
	ClientID  string    `json:"clientId"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	StartedAt time.Time `json:"startedAt"`
}

type active struct {
	Connection
	cancel context.CancelFunc
}

type Store struct {
	mu      sync.Mutex
	path    string
	lock    *os.File
	state   state
	invites map[string]time.Time
	active  map[string]active
	now     func() time.Time
	closed  bool
}

func random() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Open locks this authentication file for one daemon; corrupt state is fatal.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("authentication state already in use: %w", err)
	}
	s := &Store{path: path, lock: lock, invites: make(map[string]time.Time), active: make(map[string]active), now: time.Now}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.state = state{Version: 1, ServerID: random(), Clients: make(map[string]record)}
		err = s.save(s.state)
	} else if err == nil {
		err = json.Unmarshal(data, &s.state)
		if err == nil {
			err = s.validate()
		}
		if err == nil {
			err = os.Chmod(path, 0600)
		}
	}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("authentication state: %w", err)
	}
	return s, nil
}

func (s *Store) validate() error {
	validHash := func(v string) bool { b, err := hex.DecodeString(v); return err == nil && len(b) == 32 }
	if s.state.Version != 1 || !validHash(s.state.ServerID) || s.state.Clients == nil {
		return errors.New("invalid schema")
	}
	seen := make(map[string]bool)
	for id, c := range s.state.Clients {
		if id != c.ID || !validHash(id) || !validHash(c.TokenHash) || seen[c.TokenHash] || !validName(c.Name) || c.CreatedAt.IsZero() {
			return errors.New("invalid client record")
		}
		seen[c.TokenHash] = true
	}
	return nil
}

func validName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 128 && strings.IndexFunc(name, unicode.IsControl) == -1
}

func (s *Store) save(next state) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".pairing-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

func (s *Store) copyState() state {
	next := state{Version: 1, ServerID: s.state.ServerID, Clients: make(map[string]record, len(s.state.Clients))}
	for id, c := range s.state.Clients {
		next.Clients[id] = c
	}
	return next
}

// Invite accepts a root HTTPS origin, or loopback HTTP for local SSH tunnels.
func (s *Store) Invite(address string) (Invitation, error) {
	u, err := url.Parse(address)
	if err != nil {
		return Invitation{}, errors.New("invalid server URL")
	}
	ip := net.ParseIP(u.Hostname())
	local := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
	if len(address) > 1024 || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && local)) {
		return Invitation{}, errors.New("URL must be an HTTPS origin without credentials, path, query or fragment (HTTP is allowed only on loopback)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Invitation{}, errors.New("pairing store closed")
	}
	now := s.now().UTC()
	for key, expiry := range s.invites {
		if !now.Before(expiry) {
			delete(s.invites, key)
		}
	}
	if len(s.invites) >= 32 {
		return Invitation{}, errors.New("too many pending invitations; use mobd link --close")
	}
	secret := random()
	expires := now.Add(InvitationTTL)
	s.invites[hash(secret)] = expires
	u.Path = "/pair"
	u.Fragment = url.Values{"v": {"1"}, "serverId": {s.state.ServerID}, "invite": {secret}}.Encode()
	return Invitation{URL: u.String(), ExpiresAt: expires, ServerID: s.state.ServerID}, nil
}

func (s *Store) CloseInvitations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.invites)
}

func (s *Store) Redeem(secret, name string) (Credential, error) {
	name = strings.TrimSpace(name)
	if !validName(name) {
		return Credential{}, ErrName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, now := hash(secret), s.now().UTC()
	expires, ok := s.invites[key]
	if s.closed || !ok || !now.Before(expires) {
		delete(s.invites, key)
		return Credential{}, ErrInvitation
	}
	id, token := random(), random()
	next := s.copyState()
	next.Clients[id] = record{Client: Client{ID: id, Name: name, CreatedAt: now}, TokenHash: hash(token)}
	if err := s.save(next); err != nil {
		return Credential{}, fmt.Errorf("save client: %w", err)
	}
	s.state = next
	delete(s.invites, key)
	return Credential{ServerID: s.state.ServerID, ClientID: id, Token: token}, nil
}

// Begin orders authentication and registration against revocation under one lock.
func (s *Store) Begin(parent context.Context, token, method, path string) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(token) != 64 {
		return nil, nil, ErrUnauthorized
	}
	var found record
	want := hash(token)
	for _, c := range s.state.Clients {
		if c.TokenHash == want && c.RevokedAt == nil {
			found = c
			break
		}
	}
	if found.ID == "" {
		return nil, nil, ErrUnauthorized
	}
	now := s.now().UTC()
	// Checkpoint recent activity at most once per minute per client.
	if found.LastSeenAt == nil || now.Sub(*found.LastSeenAt) >= time.Minute {
		next := s.copyState()
		found.LastSeenAt = &now
		next.Clients[found.ID] = found
		if err := s.save(next); err != nil {
			return nil, nil, err
		}
		s.state = next
	}
	ctx, cancel := context.WithCancel(parent)
	id := random()
	s.active[id] = active{Connection: Connection{ID: id, ClientID: found.ID, Method: method, Path: path, StartedAt: now}, cancel: cancel}
	return ctx, func() {
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.active, id)
	}, nil
}

func (s *Store) Clients() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	clients := make([]Client, 0, len(s.state.Clients))
	for _, c := range s.state.Clients {
		clients = append(clients, c.Client)
	}
	slices.SortFunc(clients, func(a, b Client) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return clients
}

func (s *Store) Connections() []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	connections := make([]Connection, 0, len(s.active))
	for _, c := range s.active {
		connections = append(connections, c.Connection)
	}
	slices.SortFunc(connections, func(a, b Connection) int { return a.StartedAt.Compare(b.StartedAt) })
	return connections
}

func (s *Store) Revoke(id string, remove bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("pairing store closed")
	}
	c, ok := s.state.Clients[id]
	if !ok {
		return ErrNotFound
	}
	next := s.copyState()
	if remove {
		delete(next.Clients, id)
	} else {
		now := s.now().UTC()
		if c.RevokedAt == nil {
			c.RevokedAt = &now
		}
		next.Clients[id] = c
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.state = next
	for _, c := range s.active {
		if c.ClientID == id {
			c.cancel()
		}
	}
	return nil
}

func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("pairing store closed")
	}
	next := state{Version: 1, ServerID: random(), Clients: make(map[string]record)}
	if err := s.save(next); err != nil {
		return err
	}
	s.state = next
	clear(s.invites)
	for _, c := range s.active {
		c.cancel()
	}
	return nil
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	clear(s.invites)
	for _, c := range s.active {
		c.cancel()
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
	}
}

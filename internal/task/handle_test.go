package task

import "testing"

func TestHandleRules(t *testing.T) {
	for h, ok := range map[string]bool{
		"api": true, "order-service": true, "ui2": true, "x1": true, "a1": false,
		"": false, "a": false, "Api": false, "2api": false, "my api": false, "api@m4002": false,
		"abc123": false, "deadbeef": false, // all hex: reads as a short ID
		"all": false, "mobd": false, "user": false,
		"a234567890123456789012345678901x": true, "a2345678901234567890123456789012x": false,
	} {
		if ValidHandle(h) != ok {
			t.Errorf("ValidHandle(%q) = %v", h, !ok)
		}
	}
}

func TestHandlesAreUniqueKeptWhenArchivedAndMovable(t *testing.T) {
	s := NewStore()
	conv := func(prompt string) string {
		turn, err := s.CreateTurnWith("", prompt, TurnOptions{Runner: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = s.UpdateStatus(turn.ID, StatusSucceeded, "")
		return turn.SessionID
	}
	str := func(v string) *string { return &v }
	yes := true
	a, b := conv("first"), conv("second")

	if got, err := s.UpdateSession(a, SessionPatch{Handle: str("api")}); err != nil || got.Handle != "api" {
		t.Fatalf("set: %+v %v", got, err)
	}
	if _, err := s.UpdateSession(b, SessionPatch{Handle: str("api")}); err != ErrHandleTaken {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.UpdateSession(b, SessionPatch{Handle: str("Bad Name")}); err != ErrInvalidHandle {
		t.Fatalf("invalid: %v", err)
	}
	// Archive keeps the handle.
	if got, _ := s.UpdateSession(a, SessionPatch{Archived: &yes}); got.Handle != "api" {
		t.Fatalf("archived lost its handle: %+v", got)
	}
	if owner, ok := s.SessionByHandle("api"); !ok || owner.ID != a || !owner.Archived {
		t.Fatalf("lookup: %+v %v", owner, ok)
	}
	// Move: b takes it in one commit, a is left without one.
	if got, err := s.UpdateSession(b, SessionPatch{Handle: str("api"), TakeHandle: true}); err != nil || got.Handle != "api" {
		t.Fatalf("take: %+v %v", got, err)
	}
	if old, _, _, _ := s.Session(a); old.Handle != "" {
		t.Fatalf("old owner kept %q", old.Handle)
	}
	// Clearing frees it; deleting the owner frees it too.
	if got, _ := s.UpdateSession(b, SessionPatch{Handle: str("")}); got.Handle != "" {
		t.Fatalf("clear: %+v", got)
	}
	if _, err := s.UpdateSession(a, SessionPatch{Handle: str("api")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(a); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.SessionByHandle("api"); ok {
		t.Fatal("deleted conversation kept its handle")
	}
	if _, err := s.UpdateSession(b, SessionPatch{Handle: str("api")}); err != nil {
		t.Fatalf("reuse after delete: %v", err)
	}
}

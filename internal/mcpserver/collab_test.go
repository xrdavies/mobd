package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectCollab(t *testing.T, c *collabClient) *mcp.ClientSession {
	t.Helper()
	srv := New(&fakeDesktop{}, "test", nil)
	registerCollabTools(srv, c)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestCollabToolsFollowTheTurnsRoles(t *testing.T) {
	for _, c := range []struct {
		roles     []string
		want, not []string
	}{
		{[]string{"send"}, []string{"list_collaborators", "send_request"}, []string{"complete_request", "answer_clarification"}},
		{[]string{"handle"}, []string{"ask_clarification", "complete_request"}, []string{"send_request", "answer_clarification"}},
		{[]string{"answer"}, []string{"answer_clarification"}, []string{"send_request", "complete_request"}},
	} {
		names := toolNames(t, connectCollab(t, &collabClient{api: "http://127.0.0.1:1", taskID: "t1", token: "tok", roles: c.roles}))
		for _, n := range c.want {
			if !slices.Contains(names, n) {
				t.Errorf("%v: missing %s in %v", c.roles, n, names)
			}
		}
		for _, n := range c.not {
			if slices.Contains(names, n) {
				t.Errorf("%v: offered %s", c.roles, n)
			}
		}
	}
	t.Setenv("MOB_COLLAB", "")
	if collabFromEnv() != nil {
		t.Error("collab tools without MOB_COLLAB")
	}
}

func TestSendRequestCallsMobdWithTheTurnToken(t *testing.T) {
	var gotPath, gotToken string
	var gotBody map[string]string
	mobd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken = r.URL.Path, r.Header.Get("X-Mob-Task-Token")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if gotBody["to"] == "nobody" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"this turn may not do that: \"nobody\" is not an agent the user mentioned"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"r-1","toName":"@api"}`))
	}))
	defer mobd.Close()
	cs := connectCollab(t, &collabClient{api: mobd.URL, taskID: "t1", token: "tok", roles: []string{"send"}})
	res := call(t, cs, "send_request", map[string]any{"to": "@api", "request": "Which fields?"})
	if gotPath != "/internal/tasks/t1/collab/send" || gotToken != "tok" || gotBody["to"] != "@api" || gotBody["text"] != "Which fields?" {
		t.Fatalf("%s %s %v", gotPath, gotToken, gotBody)
	}
	if text := resultText(res); !strings.Contains(text, "r-1") || !strings.Contains(text, "end your turn") {
		t.Fatalf("result %q", text)
	}
	res = call(t, cs, "send_request", map[string]any{"to": "nobody", "request": "x"})
	if !res.IsError || !strings.Contains(resultText(res), "not an agent the user mentioned") {
		t.Fatalf("refusal not reported: %+v", res)
	}
}

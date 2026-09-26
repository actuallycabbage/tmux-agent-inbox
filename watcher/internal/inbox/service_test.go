package inbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceDiscovery(t *testing.T) {
	base := t.TempDir()
	password := "test-secret"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		if !ok || user != "opencode" || secret != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(serverInfo{PID: 42, Version: "2.0.11", URLs: []string{server.URL}})
	}))
	defer server.Close()
	registered := registration{URL: server.URL, PID: 42, Version: "2.0.11", Password: &password}
	path := filepath.Join(base, "opencode", "service.json")
	if err := writeJSON(path, registered); err != nil {
		t.Fatal(err)
	}
	service, info, err := discover(context.Background(), base, newHTTPClient())
	if err != nil || service == nil || info.PID != 42 {
		t.Fatalf("authenticated discovery failed: %+v, %v", info, err)
	}
	registered.PID = 43
	if err := writeJSON(path, registered); err != nil {
		t.Fatal(err)
	}
	if _, _, err := discover(context.Background(), base, newHTTPClient()); err == nil {
		t.Fatal("a stale registration must not accept an unrelated server on the same port")
	}
	registered.PID, registered.Password = 42, nil
	if err := writeJSON(path, registered); err != nil {
		t.Fatal(err)
	}
	if _, _, err := discover(context.Background(), base, newHTTPClient()); err == nil {
		t.Fatal("authentication failure must not look like an empty server")
	}
}

func TestSnapshotRecovery(t *testing.T) {
	seenSpace := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch r.URL.Path {
		case "/api/session/active":
			result = map[string]any{"data": map[string]any{"ses_running": map[string]string{"type": "running"}}}
		case "/api/debug/location":
			result = []location{{Directory: "/one space"}, {Directory: "/two"}}
		case "/api/form":
			forms := []request{}
			if r.URL.Query().Get("location[directory]") == "/two" {
				forms = append(forms, request{ID: "frm_1", SessionID: "ses_child"})
			} else if r.URL.Query().Get("location[directory]") == "/one space" {
				seenSpace = true
			}
			result = envelope[[]request]{Data: forms}
		case "/api/permission/request":
			result = envelope[[]request]{Data: []request{}}
		case "/api/shell":
			commands := []shell{}
			if r.URL.Query().Get("location[directory]") == "/two" {
				command := shell{Status: "running"}
				command.Metadata.SessionID, command.Time.Started = "ses_shell", 123
				exited, unowned := command, command
				exited.Status, exited.Metadata.SessionID = "exited", "ses_exited"
				unowned.Metadata.SessionID = ""
				commands = append(commands, command, exited, unowned)
			}
			result = envelope[[]shell]{Data: commands}
		case "/api/session/ses_deleted":
			w.WriteHeader(http.StatusNotFound)
			return
		default:
			id := strings.TrimPrefix(r.URL.Path, "/api/session/")
			if id == "" || id == "ses_exited" {
				t.Errorf("unowned/exited commands must not cause session lookups: %q", id)
			}
			item := testSession(id, 1)
			if id == "ses_child" {
				item.ParentID = "ses_root"
			}
			result = envelope[session]{Data: item}
		}
		json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	service := &api{registration: registration{URL: server.URL}, client: newHTTPClient()}
	bridges := []bridge{{Token: "bridge", Pane: "%1", Sessions: []string{"ses_root"}}}
	rows, err := service.snapshot(context.Background(), bridges, []row{{ID: "ses_deleted"}})
	if err != nil {
		t.Fatal(err)
	}
	if !seenSpace || len(rows) != 4 || rows[0].ID != "ses_child" || rows[0].Status != "QUESTION" || rows[0].Pane != "%1" {
		t.Fatalf("pre-existing pending request/parent mapping was not recovered: %+v", rows)
	}
	if rows[2].ID != "ses_shell" || rows[2].Status != "SHELL" || rows[2].ShellCount != 1 || rows[2].ShellStart != 123 {
		t.Fatalf("background commands must recover sessions absent from the active list and pane registrations: %+v", rows)
	}
	server.Close()
	if _, err := service.snapshot(context.Background(), bridges, rows); err == nil {
		t.Fatal("disconnections must not silently clear the attention list")
	}
}

func TestServiceDoesNotFollowRedirects(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()
	password := "secret"
	service := &api{registration: registration{URL: server.URL, Password: &password}, client: newHTTPClient()}
	var info serverInfo
	if err := service.get(context.Background(), "/api/info", &info); err == nil || forwarded {
		t.Fatal("service discovery must not forward credentials to redirects")
	}
}

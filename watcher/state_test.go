package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testSession(id string, updated int64) session {
	item := session{ID: id, Title: id, Location: location{Directory: "/same/repo"}}
	item.Time.Updated = updated
	return item
}

func rowIDs(rows []row) []string {
	ids := make([]string, len(rows))
	for i, item := range rows {
		ids[i] = item.ID
	}
	return ids
}

func TestAttentionAndRecency(t *testing.T) {
	const now = int64(1_000_000)
	sessions := map[string]session{
		"ses_permission": testSession("ses_permission", 1),
		"ses_question":   testSession("ses_question", 2),
		"ses_new":        testSession("ses_new", 100),
		"ses_old":        testSession("ses_old", 10),
		"ses_idle":       testSession("ses_idle", 200),
	}
	active := map[string]json.RawMessage{"ses_permission": nil, "ses_question": nil, "ses_old": nil, "ses_new": nil}
	forms := []request{{ID: "frm_1", SessionID: "ses_question"}}
	permissions := []request{{ID: "per_1", SessionID: "ses_permission"}}
	rows := makeRows(sessions, active, forms, permissions, nil, nil, now)
	want := []string{"ses_permission", "ses_question", "ses_new", "ses_old"}
	if !reflect.DeepEqual(rowIDs(rows), want) {
		t.Fatalf("state groups/newest-first: got %v, want %v", rowIDs(rows), want)
	}
	if summary(rows) != "OC: 2 waiting · 2 running" || len(newAlerts(rows, nil)) != 2 {
		t.Fatalf("incorrect attention state: %v", rows)
	}
	later := makeRows(sessions, active, forms, permissions, nil, rows, now+5000)
	if len(newAlerts(later, rows)) != 0 || later[0].Since != rows[0].Since {
		t.Fatal("unchanged requests must not notify again or reset waiting time")
	}
	permissions[0].ID = "per_2"
	another := makeRows(sessions, active, forms, permissions, nil, rows, now+10000)
	if !reflect.DeepEqual(rowIDs(newAlerts(another, rows)), []string{"ses_permission"}) || another[0].Since != now+10000 {
		t.Fatal("a new request in the same session must alert again")
	}
	answered := makeRows(sessions, active, nil, nil, nil, rows, now+15000)
	if summary(answered) != "OC: 4 running" {
		t.Fatal("answering must clear waiting status even while the drain remains active")
	}
}

func TestPaneOwnership(t *testing.T) {
	sessions := map[string]session{"ses_a": testSession("ses_a", 1), "ses_b": testSession("ses_b", 1)}
	child := testSession("ses_child", 1)
	child.ParentID = "ses_a"
	sessions[child.ID] = child
	bridges := []bridge{
		{Pane: "%1", Sessions: []string{"ses_a", "ses_b"}, Current: "ses_b"},
		{Pane: "%2", Sessions: []string{"ses_a"}, Current: "ses_a"},
	}
	for id, pane := range map[string]string{"ses_a": "%2", "ses_b": "%1", "ses_child": "%2"} {
		owner := ownerFor(sessions[id], sessions, bridges)
		if owner == nil || owner.Pane != pane {
			t.Fatalf("%s should belong to %s, got %+v", id, pane, owner)
		}
	}
	if ownerFor(testSession("ses_unknown", 1), sessions, bridges) != nil {
		t.Fatal("shared directories must not be used to guess pane ownership")
	}
	if stateDirectory("/state", "/one/socket") == stateDirectory("/state", "/two/socket") {
		t.Fatal("different tmux sockets need separate state")
	}
}

func TestReviewAndEmptySummary(t *testing.T) {
	const now = int64(100_000_000)
	done := testSession("ses_done", now)
	done.Time.Idle = now - 100
	failed := testSession("ses_failed", now)
	failed.Outcome, failed.Time.Idle = "failed", now-200
	old := testSession("ses_old", 1)
	old.Time.Idle = now - (24 * time.Hour).Milliseconds() - 1
	sessions := map[string]session{done.ID: done, failed.ID: failed, old.ID: old}
	rows := makeRows(sessions, nil, nil, nil, nil, nil, now)
	if !reflect.DeepEqual(rowIDs(rows), []string{"ses_failed", "ses_done"}) || summary(rows) != "OC: 2 review" {
		t.Fatal("only recent, unviewed root completions should need review")
	}
	done.Time.Viewed = done.Time.Idle
	rows = makeRows(map[string]session{done.ID: done}, nil, nil, nil, nil, nil, now)
	if len(rows) != 0 || summary(rows) != "" || summary([]row{{Status: "IDLE"}}) != "" {
		t.Fatal("viewing clears review and zero-count indicators must disappear")
	}
}

func TestBridgeWireCompatibility(t *testing.T) {
	info := serverInfo{PID: 42, URLs: []string{"http://localhost/?b=2&a=1", "http://127.0.0.1:4096"}}
	want := `[42,["http://127.0.0.1:4096","http://localhost/?b=2&a=1"]]`
	if serverKey(info) != want {
		t.Fatalf("Go server identity must match the plugin's JSON.stringify: %s", serverKey(info))
	}
	path := filepath.Join(t.TempDir(), "bridges", "test.json")
	entry := bridge{Token: "test", Pane: "%1", PID: 42, Sessions: []string{"ses_a"}, Server: want}
	if err := writeJSON(path, entry); err != nil {
		t.Fatal(err)
	}
	var read bridge
	if err := readJSON(path, &read); err != nil || !reflect.DeepEqual(entry, read) {
		t.Fatalf("bridge JSON round trip: %+v, %v", read, err)
	}
}

func TestWatcherLock(t *testing.T) {
	directory := t.TempDir()
	first, err := lockWatcher(directory)
	if err != nil || first == nil {
		t.Fatalf("first watcher failed to acquire lock: %v", err)
	}
	defer first.Close()
	second, err := lockWatcher(directory)
	if err != nil || second != nil {
		t.Fatalf("another watcher acquired the same socket's lock: %v", err)
	}
	first.Close()
	third, err := lockWatcher(directory)
	if err != nil || third == nil {
		t.Fatalf("released lock was not reusable: %v", err)
	}
	third.Close()
}

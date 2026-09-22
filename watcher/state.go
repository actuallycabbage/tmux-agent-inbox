package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	pollInterval = 5 * time.Second
	bridgeTTL    = 20 * time.Second
	shellDelay   = 30 * time.Second
)

type location struct {
	Directory string `json:"directory"`
}

type session struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	ParentID string   `json:"parentID"`
	Outcome  string   `json:"outcome"`
	Location location `json:"location"`
	Time     struct {
		Updated int64 `json:"updated"`
		Idle    int64 `json:"idle"`
		Viewed  int64 `json:"viewed"`
	} `json:"time"`
}

type request struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
}

type shell struct {
	Status   string `json:"status"`
	Metadata struct {
		SessionID string `json:"sessionID"`
	} `json:"metadata"`
	Time struct {
		Started int64 `json:"started"`
	} `json:"time"`
}

type bridge struct {
	Token    string   `json:"token"`
	PID      int      `json:"pid"`
	Pane     string   `json:"pane"`
	Server   string   `json:"server"`
	Sessions []string `json:"sessions"`
	Current  string   `json:"current"`
	Updated  int64    `json:"updated"`
	Target   string   `json:"target,omitempty"`
	Window   string   `json:"window,omitempty"`
}

type row struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Directory  string `json:"directory"`
	Status     string `json:"status"`
	RequestIDs string `json:"requestIDs"`
	Updated    int64  `json:"updated"`
	Since      int64  `json:"since"`
	ShellCount int    `json:"shellCount,omitempty"`
	ShellStart int64  `json:"shellStart,omitempty"`
	Bridge     string `json:"bridge,omitempty"`
	Pane       string `json:"pane,omitempty"`
	Target     string `json:"target,omitempty"`
	Window     string `json:"window,omitempty"`
	ParentID   string `json:"parentID,omitempty"`
}

type snapshot struct {
	Updated int64  `json:"updated"`
	Server  string `json:"server,omitempty"`
	Error   string `json:"error,omitempty"`
	Rows    []row  `json:"rows"`
}

type watcherOwner struct {
	PID int `json:"pid"`
}

var tmuxSuffix = regexp.MustCompile(`,\d+,\d+$`)

func stateBase() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return base, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".local", "state"), err
}

func stateDirectory(base, socket string) string {
	// Pane IDs are unique within a tmux server, not across tmux sockets.
	digest := sha256.Sum256([]byte(socket))
	// This wire-protocol directory predates the project rename. Keep it stable
	// so running CLIs and upgraded helpers share their existing registrations.
	return filepath.Join(base, "tmux-opencode-notify", fmt.Sprintf("%x", digest[:8]))
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".notify-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// CLI plugins and status jobs read these files concurrently. Rename keeps
	// them from seeing half a heartbeat, focus command, or snapshot.
	return os.Rename(file.Name(), path)
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func serverKey(info serverInfo) string {
	urls := append([]string{}, info.URLs...)
	sort.Strings(urls)
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Match JSON.stringify in the embedded plugin, including URLs with '&'.
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]any{info.PID, urls})
	return strings.TrimSuffix(buffer.String(), "\n")
}

func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || (r >= 127 && r <= 159) {
			return ' '
		}
		return r
	}, value)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func ownerFor(item session, sessions map[string]session, bridges []bridge) *bridge {
	visited := make(map[string]bool)
	for current := item; current.ID != "" && !visited[current.ID]; current = sessions[current.ParentID] {
		visited[current.ID] = true
		var first, visible *bridge
		for i := range bridges {
			candidate := &bridges[i]
			for _, id := range candidate.Sessions {
				if id != current.ID {
					continue
				}
				if candidate.Current == item.ID {
					return candidate
				}
				if first == nil {
					first = candidate
				}
				if visible == nil && candidate.Current == current.ID {
					visible = candidate
				}
			}
		}
		if visible != nil {
			return visible
		}
		if first != nil {
			return first
		}
	}
	return nil
}

func makeRows(sessions map[string]session, active map[string]json.RawMessage, forms, permissions []request, shells []shell, bridges []bridge, previous []row, now int64) []row {
	old := make(map[string]row, len(previous))
	for _, item := range previous {
		old[item.ID] = item
	}
	questions := make(map[string][]string)
	approvals := make(map[string][]string)
	for _, item := range forms {
		questions[item.SessionID] = append(questions[item.SessionID], item.ID)
	}
	for _, item := range permissions {
		approvals[item.SessionID] = append(approvals[item.SessionID], item.ID)
	}
	commands := make(map[string][]shell)
	for _, command := range shells {
		if command.Status != "running" || command.Metadata.SessionID == "" {
			continue
		}
		// Suppress brief commands using each command's actual start time, so
		// rapid successive commands never accumulate into long-running activity.
		if command.Time.Started <= 0 || now-command.Time.Started < shellDelay.Milliseconds() {
			continue
		}
		commands[command.Metadata.SessionID] = append(commands[command.Metadata.SessionID], command)
	}
	rows := make([]row, 0, len(sessions))
	for _, item := range sessions {
		owner := ownerFor(item, sessions, bridges)
		_, running := active[item.ID]
		// OpenCode retains unread timestamps after a tab or CLI closes. Only
		// live pane registrations keep completed runs in this tmux inbox.
		unread := owner != nil && item.ParentID == "" && item.Time.Idle > item.Time.Viewed && now-item.Time.Idle < (24*time.Hour).Milliseconds()
		status := "IDLE"
		// A pending question/permission can block a still-running drain. It
		// takes priority over active; ordinary idle history is not attention.
		switch {
		case len(approvals[item.ID]) > 0:
			status = "PERMISSION"
		case len(questions[item.ID]) > 0:
			status = "QUESTION"
		case running:
			status = "RUNNING"
		case len(commands[item.ID]) > 0:
			// Background commands outlive the foreground drain reported by
			// /session/active. An idle agent may still be awaiting their results.
			status = "SHELL"
		case unread && item.Outcome == "failed":
			status = "ERROR"
		case unread:
			status = "REVIEW"
		}
		if status == "IDLE" && (owner == nil || item.ParentID != "") {
			continue
		}
		ids := append(append([]string{}, approvals[item.ID]...), questions[item.ID]...)
		sort.Strings(ids)
		entry := row{
			ID: item.ID, Title: clean(item.Title), Directory: item.Location.Directory,
			Status: status, RequestIDs: strings.Join(ids, ","), Updated: item.Time.Updated,
			Since: now, ParentID: item.ParentID,
		}
		for _, command := range commands[item.ID] {
			if entry.ShellCount == 0 || command.Time.Started < entry.ShellStart {
				entry.ShellStart = command.Time.Started
			}
			entry.ShellCount++
		}
		if entry.Title == "" {
			entry.Title = item.ID
		}
		if before, ok := old[item.ID]; ok && before.Status == status && before.RequestIDs == entry.RequestIDs {
			entry.Since = before.Since
		}
		if owner != nil {
			entry.Bridge, entry.Pane, entry.Target, entry.Window = owner.Token, owner.Pane, owner.Target, owner.Window
		}
		rows = append(rows, entry)
	}
	priority := map[string]int{"PERMISSION": 0, "QUESTION": 1, "ERROR": 2, "REVIEW": 3, "RUNNING": 4, "SHELL": 5, "IDLE": 6}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Status != b.Status {
			return priority[a.Status] < priority[b.Status]
		}
		// OpenCode owns recency; Since is only this watcher's observation time.
		if a.Updated != b.Updated {
			return a.Updated > b.Updated
		}
		return a.ID < b.ID
	})
	return rows
}

func waiting(item row) bool {
	return item.Status == "PERMISSION" || item.Status == "QUESTION"
}

func summary(rows []row) string {
	counts := [4]int{}
	for _, item := range rows {
		switch {
		case waiting(item):
			counts[0]++
		case item.Status == "RUNNING":
			counts[1]++
		case item.Status == "SHELL":
			counts[2]++
		case item.Status == "REVIEW" || item.Status == "ERROR":
			counts[3]++
		}
	}
	var parts []string
	for i, label := range []string{"waiting", "running", "shell", "review"} {
		if counts[i] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[i], label))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "OC: " + strings.Join(parts, " · ")
}

func newAlerts(rows, previous []row) []row {
	old := make(map[string]string, len(previous))
	for _, item := range previous {
		old[item.ID] = item.RequestIDs
	}
	var alerts []row
	for _, item := range rows {
		if waiting(item) && item.RequestIDs != old[item.ID] {
			alerts = append(alerts, item)
		}
	}
	return alerts
}

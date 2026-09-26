package inbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (app *application) bridges(server string) ([]bridge, error) {
	output, err := app.tmux("list-panes", "-a", "-F", "#{pane_id}\t#{session_name}:#{window_index}.#{pane_index}\t#{window_name}")
	if err != nil {
		return nil, err
	}
	panes := make(map[string][]string)
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 3 {
			panes[parts[0]] = parts[1:]
		}
	}
	files, err := os.ReadDir(filepath.Join(app.directory, "bridges"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bridges []bridge
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		var entry bridge
		if err := readJSON(filepath.Join(app.directory, "bridges", file.Name()), &entry); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // The CLI may have exited since ReadDir.
			}
			return nil, err
		}
		pane, exists := panes[entry.Pane]
		if entry.Server == server && exists && alive(entry.PID) && time.Now().UnixMilli()-entry.Updated < bridgeTTL.Milliseconds() {
			entry.Target, entry.Window = clean(pane[0]), clean(pane[1])
			bridges = append(bridges, entry)
		}
	}
	return bridges, nil
}

func (app *application) start() error {
	var owner watcherOwner
	if err := readJSON(filepath.Join(app.directory, "watcher.lock", "owner.json"), &owner); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if alive(owner.PID) {
		return nil
	}
	if err := os.MkdirAll(app.directory, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(app.directory, "watcher.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(app.executable, "watch")
	cmd.Env = append(os.Environ(), "OC_TMUX_SOCKET="+app.socket)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func lockWatcher(directory string) (*os.File, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "watcher.flock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	// Keep the lock file's inode: unlinking it lets another process lock a
	// different inode while this watcher is still running. The OS releases
	// flock even after SIGKILL, unlike a PID/directory-only lock.
	return file, nil
}

func (app *application) watch(ctx context.Context) error {
	lock, err := lockWatcher(app.directory)
	if err != nil || lock == nil {
		return err
	}
	defer lock.Close()
	ownerPath := filepath.Join(app.directory, "watcher.lock", "owner.json")
	var owner watcherOwner
	if err := readJSON(ownerPath, &owner); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Honour the previous Node watcher's lease during an in-place migration.
	// It must exit before Go starts publishing to the same snapshot file.
	if owner.PID != os.Getpid() && alive(owner.PID) {
		return nil
	}
	if err := writeJSON(ownerPath, watcherOwner{PID: os.Getpid()}); err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Dir(ownerPath))
	var state snapshot
	if err := readJSON(filepath.Join(app.directory, "snapshot.json"), &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.Rows == nil {
		state.Rows = []row{}
	}
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	for ctx.Err() == nil {
		// Detached sessions keep their watcher, but a dead tmux server must
		// not leave a daemon polling OpenCode forever.
		if _, err := app.tmux("list-sessions"); err != nil {
			return nil
		}
		poll, cancel := context.WithTimeout(ctx, 15*time.Second)
		service, info, err := discover(poll, app.base, client)
		var rows []row
		server := serverKey(info)
		if err == nil {
			var bridges []bridge
			bridges, err = app.bridges(server)
			if err == nil {
				rows, err = service.snapshot(poll, bridges, state.Rows)
			}
		}
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		var alerts []row
		if err != nil {
			message := clean(err.Error())
			if state.Error != message {
				fmt.Fprintln(os.Stderr, message)
			}
			state.Error = message
		} else {
			alerts = newAlerts(rows, state.Rows)
			state.Rows, state.Server, state.Error = rows, server, ""
		}
		state.Updated = time.Now().UnixMilli()
		if err := writeJSON(filepath.Join(app.directory, "snapshot.json"), state); err != nil {
			return err
		}
		if len(alerts) > 0 {
			if err := app.notify(alerts); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollInterval):
		}
	}
	return nil
}

func (app *application) notify(alerts []row) error {
	first := alerts[0]
	reason := "permission needed"
	if first.Status == "QUESTION" {
		reason = "answer needed"
	}
	message := fmt.Sprintf("Agent Inbox: %s — %s", first.Title, reason)
	if len(alerts) > 1 {
		message += fmt.Sprintf(" (+%d more)", len(alerts)-1)
	}
	clients, err := app.tmux("list-clients", "-F", "#{client_name}")
	if err != nil {
		return err
	}
	for _, client := range strings.Split(clients, "\n") {
		if client != "" {
			if _, err := app.tmux("display-message", "-c", client, "-d", "5000", "-l", message); err != nil {
				return err
			}
		}
	}
	return nil
}

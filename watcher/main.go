package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type application struct {
	socket     string
	directory  string
	base       string
	executable string
}

func (app *application) tmux(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-S", app.socket}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux: %s: %w", clean(strings.TrimSpace(string(output))), err)
	}
	return strings.TrimRight(string(output), "\r\n"), nil
}

func (app *application) load() (snapshot, error) {
	var state snapshot
	if err := app.start(); err != nil {
		return state, err
	}
	for attempt := 0; attempt < 30; attempt++ {
		err := readJSON(filepath.Join(app.directory, "snapshot.json"), &state)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return state, err
		}
		if err == nil && time.Now().UnixMilli()-state.Updated < (4*pollInterval).Milliseconds() {
			if state.Error != "" {
				return state, errors.New(state.Error)
			}
			return state, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return state, errors.New("session status is not available yet; try again shortly")
}

func lines(rows []row) string {
	var output strings.Builder
	for _, item := range rows {
		title, target, window := item.Title, item.Target, item.Window
		if item.ParentID != "" {
			title = "↳ " + title
		}
		if target == "" {
			target = "new window"
		}
		if window == "" {
			window = "—"
		}
		activity := ""
		if waiting(item) {
			minutes := (time.Now().UnixMilli() - item.Since) / 60_000
			activity = "waiting now"
			if minutes > 0 {
				activity = fmt.Sprintf("waiting %dm", minutes)
			}
		}
		if item.ShellCount > 0 {
			if activity != "" {
				activity += " · "
			}
			label := "shell"
			if item.ShellCount > 1 {
				label = "shells"
			}
			elapsed := max(time.Duration(0), time.Since(time.UnixMilli(item.ShellStart)).Truncate(time.Second))
			activity += fmt.Sprintf("%d %s · %s", item.ShellCount, label, elapsed)
		}
		fmt.Fprintf(&output, "%s\t%-10s\t%-20s\t%-22s\t%s\t%s\n", item.ID, item.Status, clean(target), clean(window), clean(title), activity)
	}
	return output.String()
}

func (app *application) jump(item row, server, client string) error {
	if client == "" {
		return errors.New("a target tmux client is required")
	}
	if item.Bridge != "" {
		if strings.ContainsAny(item.Bridge, `/\`) || item.Bridge == "." || item.Bridge == ".." {
			return errors.New("invalid pane registration")
		}
		var owner bridge
		if err := readJSON(filepath.Join(app.directory, "bridges", item.Bridge+".json"), &owner); err != nil {
			return err
		}
		if owner.Token != item.Bridge || !alive(owner.PID) || time.Now().UnixMilli()-owner.Updated >= bridgeTTL.Milliseconds() || owner.Server != server {
			return errors.New("the selected pane is unavailable; refresh the inbox and try again")
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id := hex.EncodeToString(random[:])
		request := filepath.Join(app.directory, "focus-"+owner.Token+".json")
		acknowledgement := filepath.Join(app.directory, "ack-"+owner.Token+".json")
		if err := writeJSON(request, map[string]any{"id": id, "server": server, "sessionID": item.ID, "created": time.Now().UnixMilli()}); err != nil {
			return err
		}
		defer os.Remove(request)
		defer os.Remove(acknowledgement)
		// A pane can contain several OpenCode tabs. Wait for the embedded
		// plugin to navigate before moving the invoking tmux client there.
		for attempt := 0; attempt < 30; attempt++ {
			var ack struct {
				ID    string `json:"id"`
				Error string `json:"error"`
			}
			if err := readJSON(acknowledgement, &ack); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if ack.ID == id {
				if ack.Error != "" {
					return errors.New(ack.Error)
				}
				_, err := app.tmux("switch-client", "-c", client, "-t", owner.Pane)
				return err
			}
			time.Sleep(100 * time.Millisecond)
		}
		return errors.New("OpenCode did not respond to the selection; reopen the inbox and try again")
	}
	// Sessions outlive their CLIs. Resume the exact ID rather than guessing
	// from a directory which several agents may share.
	session, err := app.tmux("display-message", "-p", "-c", client, "#{session_id}")
	if err != nil {
		return err
	}
	pane, err := app.tmux("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", session+":", "-n", "OpenCode", "-c", item.Directory, "opencode2 --session "+shellQuote(item.ID))
	if err != nil {
		return err
	}
	_, err = app.tmux("switch-client", "-c", client, "-t", pane)
	return err
}

func (app *application) configure() error {
	// Status jobs may inherit another server's TMUX from the environment
	// which launched tmux. Pin every invocation to this server explicitly.
	run := "env OC_TMUX_SOCKET=" + shellQuote(app.socket) + " " + shellQuote(app.executable)
	bindings := [][]string{
		{"bind-key", "-T", "prefix", "o", "switch-client", "-T", "opencode"},
		// display-popup does not expand formats in its command; run-shell
		// captures the invoking client before the helper opens the popup.
		{"bind-key", "-T", "opencode", "c", "run-shell", "-b", run + " popup #{q:client_name}"},
		{"bind-key", "-T", "opencode", "n", "run-shell", "-b", run + " next #{q:client_name}"},
		{"bind-key", "-T", "opencode", "o", "select-pane", "-t", ":.+"},
		{"bind-key", "-T", "opencode", "Escape", "switch-client", "-T", "root"},
	}
	for _, binding := range bindings {
		if _, err := app.tmux(binding...); err != nil {
			return err
		}
	}
	status, err := app.tmux("show-options", "-gqv", "status-right")
	if err != nil {
		return err
	}
	previous, err := app.tmux("show-options", "-gqv", "@opencode-notify-format")
	if err != nil {
		return err
	}
	if previous != "" {
		status = strings.ReplaceAll(status, previous, "")
	}
	// Remove the exact leading fragment installed by the Node version;
	// preserve the user's theme and the rest of their status-right format.
	const end = ") #[default]"
	if strings.HasPrefix(status, "#[fg=yellow]#(") {
		if index := strings.Index(status, end); index >= 0 && strings.Contains(status[:index], "/oc-tmux.mjs") {
			status = status[index+len(end):]
		}
	}
	fragment := "#[fg=yellow]#(" + run + " status) #[default]"
	for _, args := range [][]string{
		{"set-option", "-g", "status-right", fragment + status},
		{"set-option", "-g", "@opencode-notify-format", fragment},
	} {
		if _, err := app.tmux(args...); err != nil {
			return err
		}
	}
	length, err := app.tmux("show-options", "-gqv", "status-right-length")
	if err != nil {
		return err
	}
	width, err := strconv.Atoi(length)
	if err != nil {
		return err
	}
	if width < 160 {
		if _, err := app.tmux("set-option", "-g", "status-right-length", "160"); err != nil {
			return err
		}
	}
	if err := app.start(); err != nil {
		return err
	}
	fmt.Println("Agent Inbox enabled. Prefix o c: inbox · Prefix o n: next waiting · Prefix o o: next pane")
	return nil
}

func (app *application) pick(state snapshot, client string) error {
	reload := shellQuote(app.executable) + " list"
	columns := fmt.Sprintf("%-10s\t%-20s\t%-22s\t%s\t%s", "STATE", "TMUX PANE", "TMUX WINDOW", "OPENCODE SESSION", "ACTIVITY")
	// reload-sync keeps the old list usable during the delay. fzf owns the
	// reload child and cancels it when the popup closes; no timer daemon.
	cmd := exec.Command("fzf", "--layout=reverse", "--no-sort", "--track", "--wrap", "--delimiter=\t", "--with-nth=2..", "--nth=2..",
		"--prompt=Search sessions > ", "--header=Enter: open session · Ctrl-R: refresh · Esc: close\n"+columns,
		"--bind", "load:reload-sync(sleep 2; "+reload+"),ctrl-r:reload-sync("+reload+")")
	cmd.Env = append(os.Environ(), "FZF_DEFAULT_OPTS=", "FZF_DEFAULT_OPTS_FILE=", "OC_TMUX_SOCKET="+app.socket)
	cmd.Stdin = strings.NewReader(lines(state.Rows))
	cmd.Stderr = os.Stderr
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && (exit.ExitCode() == 1 || exit.ExitCode() == 130) {
			return nil
		}
		return err
	}
	id, _, _ := strings.Cut(output.String(), "\t")
	current, err := app.load()
	if err != nil {
		return err
	}
	for _, item := range current.Rows {
		if item.ID == id {
			return app.jump(item, current.Server, client)
		}
	}
	return errors.New("the session is no longer available; reopen the inbox")
}

func (app *application) run(command, client string) error {
	switch command {
	case "configure":
		return app.configure()
	case "watch":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return app.watch(ctx)
	case "status":
		if err := app.start(); err != nil {
			return err
		}
		var state snapshot
		if err := readJSON(filepath.Join(app.directory, "snapshot.json"), &state); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Println("OC: starting")
				return nil
			}
			return err
		}
		if state.Error != "" || time.Now().UnixMilli()-state.Updated > (4*pollInterval).Milliseconds() {
			fmt.Println("OC: offline")
		} else {
			fmt.Println(summary(state.Rows))
		}
		return nil
	case "popup":
		if client == "" {
			return errors.New("a target tmux client is required")
		}
		run := "env OC_TMUX_SOCKET=" + shellQuote(app.socket) + " " + shellQuote(app.executable) + " pick " + shellQuote(client)
		cmd := exec.Command("tmux", "-S", app.socket, "display-popup", "-c", client, "-E", "-w", "90%", "-h", "70%", "-T", " Agent Inbox ", run)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	case "list", "pick", "next":
		state, err := app.load()
		if err != nil {
			return err
		}
		if command == "list" {
			fmt.Print(lines(state.Rows))
			return nil
		}
		if client == "" {
			return errors.New("a target tmux client is required")
		}
		if command == "pick" {
			return app.pick(state, client)
		}
		var rows []row
		for _, item := range state.Rows {
			if waiting(item) {
				rows = append(rows, item)
			}
		}
		if len(rows) == 0 {
			_, err := app.tmux("display-message", "-c", client, "-l", "Agent Inbox: no sessions are waiting for input")
			return err
		}
		positions := make(map[string]string)
		path := filepath.Join(app.directory, "position.json")
		if err := readJSON(path, &positions); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := 0
		for i, item := range rows {
			if item.ID == positions[client] {
				next = (i + 1) % len(rows)
				break
			}
		}
		if err := app.jump(rows[next], state.Server, client); err != nil {
			return err
		}
		positions[client] = rows[next].ID
		return writeJSON(path, positions)
	default:
		return fmt.Errorf("unknown command: %s", command)
	}
}

func main() {
	command, client := "help", ""
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if len(os.Args) > 2 {
		client = os.Args[2]
	}
	if command == "help" || command == "--help" || command == "-h" {
		fmt.Print(`Agent Inbox — find and open OpenCode sessions across tmux panes.

Usage: oc-tmux <command> [client]

Commands:
  configure       Set up the status indicator and keyboard shortcuts
  status          Print the status-bar summary
  list            Print sessions grouped by state, newest first
  popup <client>  Open the inbox for a tmux client
  next <client>   Go to the next session waiting for input
  pick <client>   Run the session picker in the current terminal
  watch           Monitor session state until stopped

Run commands inside tmux. The watcher normally starts automatically.
See README.md for installation and keyboard shortcuts.
`)
		return
	}
	app := &application{socket: os.Getenv("OC_TMUX_SOCKET")}
	if app.socket == "" {
		app.socket = tmuxSuffix.ReplaceAllString(os.Getenv("TMUX"), "")
	}
	var err error
	app.base, err = stateBase()
	if err == nil {
		app.executable, err = os.Executable()
	}
	if err == nil && app.socket == "" {
		err = errors.New("run this command inside tmux")
	}
	if err == nil {
		app.directory = stateDirectory(app.base, app.socket)
		err = app.run(command, client)
	}
	if err != nil {
		if command == "status" {
			fmt.Println("OC: offline")
			return
		}
		message := clean(err.Error())
		fmt.Fprintln(os.Stderr, message)
		if client != "" && app.socket != "" {
			_, _ = app.tmux("display-message", "-c", client, "-d", "5000", "-l", "Agent Inbox: "+message)
		}
		os.Exit(1)
	}
}

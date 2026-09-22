# tmux-agent-inbox

Find the OpenCode sessions that need you, wherever they’re running in tmux.

Agent Inbox puts pending questions, permission requests, and responses to review in one searchable popup. A status-bar indicator shows what needs attention, and selecting a session takes you to its OpenCode tab and tmux pane—even in another tmux session.

Supports the **OpenCode V2 local shared service** and full terminal UI.

## Install

You’ll need **Go 1.23+** and **make** to build, plus **tmux 3.2+**, **fzf 0.65+**, and **opencode2** on your `PATH`.

Build the helper from this directory:

```sh
make build
```

Add the `plugin/` directory to `plugins` in `~/.config/opencode/cli.json`, alongside any existing plugins:

```json
{
  "plugins": ["/absolute/path/to/tmux-agent-inbox/plugin"]
}
```

Enable the inbox in your current tmux server:

```sh
./bin/oc-tmux configure
```

To load it whenever tmux starts, add this line **after your theme and status-bar settings** in `~/.tmux.conf`:

```tmux
run-shell '/absolute/path/to/tmux-agent-inbox/bin/oc-tmux configure'
```

This adds the inbox indicator to `status-right` and uses **prefix → o** for inbox shortcuts. If an existing OpenCode client hasn’t loaded the plugin, restart that client to register its pane.

## Use

| Keys | Action |
| --- | --- |
| Prefix → o → c | Open the inbox |
| Prefix → o → n | Go to the next session waiting for input |
| Prefix → o → o | Go to the next tmux pane |
| Enter in the inbox | Open the selected session |
| Ctrl-R in the inbox | Refresh now |
| Escape | Close the inbox or leave the shortcut menu |

Search by session title, state, tmux address, or window name. Each row shows the session’s state, its tmux location, its OpenCode title, and any shell activity. A **new window** location means selecting the row will resume that OpenCode session in a new tmux window.

Sessions are grouped in the order below, with the most recently updated first in each group:

| State | Meaning |
| --- | --- |
| `PERMISSION` | Waiting for you to approve or reject an action |
| `QUESTION` | Waiting for an answer to a question or form |
| `ERROR` | A failed run you haven’t viewed |
| `REVIEW` | A completed or interrupted run you haven’t viewed |
| `RUNNING` | Working |
| `SHELL` | A background command is still running |
| `IDLE` | Inactive, with no pending input or review indicator |

Questions and permissions stay in the waiting queue until you answer or resolve them. Review indicators clear when you view the response in OpenCode, close its last registered tab or client, or after 24 hours.

The activity column shows long-running commands, with a count and the elapsed time of the oldest command.

The status bar summarises sessions, for example `OC: 2 waiting · 3 running · 1 shell · 1 review`. It shows only non-zero counts and disappears when there’s nothing to report. New questions and permission requests also display a brief tmux message.

## How it works

A Go watcher checks the local OpenCode service every five seconds. A small plugin inside each OpenCode client maps its sessions and tabs to a tmux pane. The popup uses fzf and refreshes every two seconds from the watcher’s latest snapshot, keeping your search as you browse.

There is one watcher per tmux server. It starts automatically, continues while tmux sessions are detached, and exits with the tmux server. The plugin runs inside OpenCode; the helper is a standalone binary.

The inbox covers running sessions, pending input, and sessions open in registered panes. Completed runs remain available for review while open in a registered pane. Private `--standalone` servers, remote servers, and `mini` clients are outside its scope.

## Troubleshooting

Run these commands inside tmux to inspect the current status and session list:

```sh
./bin/oc-tmux status
./bin/oc-tmux list
```

- **`OC: offline`:** the watcher cannot obtain current session data. Check `opencode2 service status` and the watcher log.
- **An existing pane appears as `new window`:** check that its OpenCode client has loaded the plugin from `cli.json`.
- **A request still shows as waiting:** updates follow the five-second polling interval. Opening a session does not resolve its question or permission request.

Local state is stored under `~/.local/state/tmux-opencode-notify/`, or `$XDG_STATE_HOME/tmux-opencode-notify/` when set. Each tmux socket has its own subdirectory containing `snapshot.json`, `watcher.log`, and the watcher PID in `watcher.lock/owner.json`.

## Development

```text
watcher/    Go module: watcher, tmux helper, and unit tests
plugin/     OpenCode CLI plugin
tests/      End-to-end integration check
bin/        Compiled binary (generated)
```

```sh
make build
make test
make integration
```

The integration check requires Python 3 and a running OpenCode service. It builds the helper, then uses a temporary OpenCode session and isolated tmux server to exercise navigation, live refresh, and answering a question before removing them.

## Uninstall

Remove the plugin entry from `cli.json` and the startup line from your tmux configuration. Restore your previous status-bar settings and keybindings, and remove the inbox key table with `tmux unbind-key -a -T opencode`. The default next-pane binding can be restored with `tmux bind-key o select-pane -t :.+`.

Once the status-bar command has been removed, stop the watcher using the PID in `watcher.lock/owner.json`.

# tmux-agent-inbox

Find OpenCode sessions that need you in one searchable tmux popup. Jump to their tab and pane, even across tmux sessions, with a status-bar count of pending input and activity.

Requires the **OpenCode V2 local shared service** and full terminal UI. Remote servers, `--standalone`, and `mini` clients are unsupported.

## Install

You’ll need **Go 1.23+**, **make**, **tmux 3.2+**, **fzf 0.65+**, and **opencode2** on your `PATH`, plus a [Nerd Font](https://www.nerdfonts.com/) for the icons.

Build from this directory:

```sh
make build
```

Add to `plugins` in `~/.config/opencode/cli.json`, keeping any existing entries:

```json
{"plugins": ["/absolute/path/to/tmux-agent-inbox/plugin"]}
```

Restart existing OpenCode clients, then enable the inbox inside tmux:

```sh
./bin/oc-tmux configure
```

For future tmux servers, add this **after your theme and status-bar settings** in `~/.tmux.conf`:

```tmux
run-shell '/absolute/path/to/tmux-agent-inbox/bin/oc-tmux configure'
```

## Use

| Keys | Action |
| --- | --- |
| Prefix o c | Open the inbox |
| Prefix o n | Next session waiting for input |
| Prefix o o | Next tmux pane |
| Enter | Open the selected session |
| Ctrl-R | Refresh the inbox |
| Escape | Close the inbox or shortcut menu |

Search by title, state, tmux address, or window name. Selecting **new window** resumes the session in a new tmux window. The inbox refreshes automatically, preserving your search.

Sessions are grouped by state, newest first within each group:

| State | Icon | Meaning |
| --- | --- | --- |
| `PERMISSION` | `` | Needs approval |
| `QUESTION` | `` | Needs an answer |
| `ERROR` | `` | Unviewed failed run |
| `REVIEW` | `` | Unviewed completed or interrupted run |
| `RUNNING` | `` | Working |
| `SHELL` | `` | Background command running |
| `IDLE` | None | Inactive |

Opening a question or permission request doesn’t resolve it. Review indicators clear when viewed, when the last registered tab or client closes, or after 24 hours.

## Troubleshooting

Inspect status inside tmux:

```sh
./bin/oc-tmux status
./bin/oc-tmux list
```

- **` offline`:** check `opencode2 service status` and `watcher.log`.
- **Existing pane shown as `new window`:** check the plugin is loaded and restart that OpenCode client.
- **Stale status:** allow five seconds for the next poll.

Logs and snapshots live under `~/.local/state/tmux-opencode-notify/` (or `$XDG_STATE_HOME/tmux-opencode-notify/`), grouped by tmux socket.

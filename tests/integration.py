"""Exercise the real TUI/popup without making a model request: make integration."""

import fcntl
import hashlib
import json
import os
from pathlib import Path
import pty
import re
import shlex
import shutil
import signal
import struct
import subprocess
import tempfile
import termios
import threading
import time
from urllib.parse import urlencode


root = Path(__file__).resolve().parents[1]
socket = str(Path(tempfile.gettempdir()) / "opencode" / f"oc-notify-check-{os.getpid()}.sock")
state = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state"))) / "tmux-opencode-notify" / hashlib.sha256(socket.encode()).hexdigest()[:16]
fixture = attach_pid = master = watcher = shell_job = None
terminal = bytearray()


def tmux(*args):
    return subprocess.check_output(["tmux", "-S", socket, *args], text=True).strip()


def api(method, path, body=None):
    args = ["opencode2", "api", method, path]
    if body is not None:
        args += ["--data", json.dumps(body)]
    result = subprocess.check_output(args, text=True, timeout=15)
    return json.loads(result) if result.strip() else None


def wait_for(label, predicate, seconds=20):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.15)
    raise RuntimeError("Timed out: " + label)


def keys(sequence):
    for key in sequence:
        os.write(master, key)
        time.sleep(0.25)


try:
    title = "OC-NOTIFY-INTEGRATION-" + str(os.getpid())
    fixture = api("post", "/api/session", {"title": title, "location": {"directory": str(root)}})["data"]["id"]
    tmux("-f", "/dev/null", "new-session", "-d", "-s", "home", "-x", "140", "-y", "40")
    command = "env OPENCODE_CLI_CONFIG_CONTENT=" + shlex.quote(json.dumps({"plugins": [str(root / 'plugin')]})) + " opencode2 --session " + shlex.quote(fixture)
    tmux("new-session", "-d", "-s", "agent", "-x", "140", "-y", "40", command)
    bridge_file = wait_for("plugin registration", lambda: next((state / "bridges").glob("*.json"), None))
    bridge = json.loads(bridge_file.read_text())
    assert fixture in bridge["sessions"]
    form = api("post", "/api/session/" + fixture + "/form", {"title": "Integration test question", "fields": [{"key": "answer", "type": "string", "required": True}]})["data"]["id"]
    subprocess.run([str(root / "bin/oc-tmux"), "configure"], env=dict(os.environ, TMUX=socket + ",1,0", OC_TMUX_SOCKET=socket), check=True)
    wait_for("snapshot", lambda: (state / "snapshot.json").exists())
    watcher = json.loads((state / "watcher.lock/owner.json").read_text())["pid"]

    def pending():
        snapshot = json.loads((state / "snapshot.json").read_text())
        return next((row for row in snapshot["rows"] if row["id"] == fixture and row["status"] == "QUESTION" and row.get("pane")), None)

    row = wait_for("pending form", pending)
    assert row["pane"] == bridge["pane"]
    attach_pid, master = pty.fork()
    if attach_pid == 0:
        os.environ.pop("TMUX", None)
        os.environ["TERM"] = "xterm-256color"
        os.execvp("tmux", ["tmux", "-S", socket, "attach-session", "-t", "home"])
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 140, 0, 0))

    def drain():
        try:
            while True:
                chunk = os.read(master, 65536)
                if not chunk:
                    return
                terminal.extend(chunk)
        except OSError:
            pass

    threading.Thread(target=drain, daemon=True).start()
    client = wait_for("attached client", lambda: tmux("list-clients", "-F", "#{client_name}"))
    time.sleep(2)
    tmux("display-message", "-c", client, "-d", "1", "-l", "")
    time.sleep(0.2)
    keys([b"\x02", b"o", b"c"])
    wait_for("fzf popup rendered", lambda: b"Search sessions >" in terminal)
    keys([title.encode()])
    tmux("rename-window", "-t", "agent:0", "notify-agent-window")
    wait_for("window name refreshes in open picker", lambda: b"notify-agent-window" in terminal)
    assert b"OPENCODE SESSION" in terminal
    keys([b"\r"])
    wait_for("popup jump to agent session", lambda: tmux("display-message", "-p", "-c", client, "#{session_name}") == "agent")
    assert tmux("display-message", "-p", "-c", client, "#{pane_id}") == bridge["pane"]
    print("PASS: popup selection switches to the exact OpenCode pane in another tmux session.")

    rows = json.loads((state / "snapshot.json").read_text())["rows"]
    waiting = [row for row in rows if row["status"] in ("QUESTION", "PERMISSION")]
    index = next(i for i, row in enumerate(waiting) if row["id"] == fixture)
    (state / "position.json").write_text(json.dumps({client: waiting[index - 1]["id"]}))
    tmux("switch-client", "-c", client, "-t", "home")
    keys([b"\x02", b"o", b"n"])
    wait_for("next waiting shortcut", lambda: tmux("display-message", "-p", "-c", client, "#{session_name}") == "agent")
    print("PASS: prefix o n cycles to the next waiting agent.")
    viewed_at = int(time.time() * 1000)
    wait_for("refresh after viewing", lambda: json.loads((state / "snapshot.json").read_text())["updated"] > viewed_at)
    assert pending(), "Viewing a question must not clear its waiting status"
    tmux("switch-client", "-c", client, "-t", "home")
    popup_start = len(terminal)
    keys([b"\x02", b"o", b"c"])
    wait_for("second popup rendered", lambda: b"Search sessions >" in terminal[popup_start:])
    keys([title.encode()])
    before_answer = len(terminal)
    api("post", "/api/session/" + fixture + "/form/" + form + "/reply", {"answer": {"answer": "Integration test answer"}})
    wait_for("answered question clears", lambda: not pending())
    answered = next(row for row in json.loads((state / "snapshot.json").read_text())["rows"] if row["id"] == fixture)
    assert answered["status"] == "IDLE" and answered["requestIDs"] == "", answered
    print("PASS: viewing preserves waiting status; submitting an answer clears it on the next refresh.")
    wait_for("answered status refreshes in open picker", lambda: b"IDLE" in terminal[before_answer:])
    keys([b"\r"])
    wait_for("search survives live refresh", lambda: tmux("display-message", "-p", "-c", client, "#{session_name}") == "agent")
    print("PASS: the open picker refreshes window names and answered status automatically, preserving its search.")

    shell_query = "?" + urlencode({"location[directory]": str(root)})
    command = api("post", "/api/shell" + shell_query, {"command": "sleep 60", "metadata": {"sessionID": fixture}})["data"]
    shell_job = command["id"]

    def shell_state(status):
        rows = json.loads((state / "snapshot.json").read_text())["rows"]
        return next((row for row in rows if row["id"] == fixture and row["status"] == status), None)

    wait_for("snapshot after command starts", lambda: json.loads((state / "snapshot.json").read_text())["updated"] > command["time"]["started"])
    brief = shell_state("IDLE")
    assert brief and not brief.get("shellCount") and not brief.get("shellStart"), brief
    running_shell = wait_for("long-running shell appears", lambda: shell_state("SHELL"), seconds=45)
    assert running_shell["shellCount"] == 1 and running_shell["shellStart"] == command["time"]["started"], running_shell
    assert json.loads((state / "snapshot.json").read_text())["updated"] - command["time"]["started"] >= 30_000
    popup_start = len(terminal)
    keys([b"\x02", b"o", b"c"])
    wait_for("shell activity in popup", lambda: b"1 shell" in terminal[popup_start:])
    keys([b"\x1b"])
    api("delete", "/api/shell/" + shell_job + shell_query)
    shell_job = None
    idle = wait_for("finished shell clears", lambda: shell_state("IDLE"))
    assert not idle.get("shellCount") and not idle.get("shellStart"), idle
    print("PASS: shell activity appears after 30 seconds and clears when the command ends.")
except Exception:
    if master is not None:
        messages = tmux("show-messages", "-t", client)
        print("Recent tmux messages:\n" + "\n".join(line for line in messages.splitlines() if "command: display-message -p" not in line)[:5000])
        plain = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", terminal.decode(errors="replace"))
        print("Terminal tail:", plain[-4000:])
    if (state / "snapshot.json").exists():
        print("Snapshot error:", json.loads((state / "snapshot.json").read_text()).get("error"))
    raise
finally:
    subprocess.run(["tmux", "-S", socket, "kill-server"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if watcher:
        try:
            os.kill(watcher, signal.SIGTERM)
        except ProcessLookupError:
            pass
    if master is not None:
        os.close(master)
    if attach_pid:
        os.waitpid(attach_pid, 0)
    if shell_job:
        api("delete", "/api/shell/" + shell_job + shell_query)
    if fixture:
        api("delete", "/api/session/" + fixture)
    if watcher:
        for _ in range(60):
            if not (state / "watcher.lock").exists():
                break
            time.sleep(0.1)
    shutil.rmtree(state, ignore_errors=True)

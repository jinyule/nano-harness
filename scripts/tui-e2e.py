#!/usr/bin/env python3
"""Exercise the compiled TUI through a PTY and a loopback Responses fixture."""

import argparse
import contextlib
import errno
import fcntl
import http.server
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import socketserver
import struct
import subprocess
import tempfile
import termios
import threading
import time


class LoopbackServer(http.server.ThreadingHTTPServer):
    daemon_threads = False

    def server_bind(self):
        socketserver.TCPServer.server_bind(self)
        self.server_name = "localhost"
        self.server_port = self.server_address[1]


class Fixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def emit(self, kind, **fields):
        self.wfile.write(("data: " + json.dumps(dict(type=kind, **fields)) + "\n\n").encode())
        self.wfile.flush()

    def do_POST(self):
        if self.path != "/v1/responses" or self.headers.get("Authorization") != "Bearer fixture-key":
            self.send_error(400)
            return
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        inputs = body["input"]
        last_user = max(i for i, item in enumerate(inputs) if item.get("role") == "user")
        task = " ".join(block.get("text", "") for block in inputs[last_user]["content"])
        outputs = [item["output"] for item in inputs[last_user + 1:] if item.get("type") == "function_call_output"]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        try:
            if task == "wait":
                self.emit("response.output_text.delta", delta="WAITING_FOR_INTERRUPT")
                self.server.stopping.wait()
                return
            if "CHILD_FOLLOW" in task:
                text = "CHILD_FOLLOW_OK"
                call = None
            elif "CHILD_READ" in task:
                call = None if outputs else ("read_file", {"path": "proof.txt"})
                text = "CHILD_READ_OK"
            else:
                child = re.search(r"session=([^ ]+)", " ".join(outputs))
                child_id = child.group(1) if child else "missing-child"
                sequence = [
                    ("list_files", {"path": "."}),
                    ("search_files", {"pattern": "PTY_PROOF", "path": "."}),
                    ("read_file", {"path": "proof.txt"}),
                    ("spawn_subagent", {"label": "reader", "task": "CHILD_READ", "mode": "continuable", "tools": ["read_file"]}),
                    ("subagent_followup", {"session_id": child_id, "task": "CHILD_FOLLOW"}),
                    ("subagent_report", {"session_id": child_id}),
                    ("list_subagents", {}),
                    ("subagent_interrupt", {"session_id": child_id}),
                    ("apply_patch", {"patch": "--- /dev/null\n+++ b/patched.txt\n@@ -0,0 +1 @@\n+PATCH_PROOF\n"}),
                    ("run_shell", {"command": "printf SHELL_PROOF > shell.txt"}),
                ]
                call = sequence[len(outputs)] if len(outputs) < len(sequence) else None
                text = "TOOLS_VERIFIED " + "中文long-line-" * 12 + " WRAP_END"
            if call:
                name, arguments = call
                item = dict(type="function_call", call_id="call-" + str(len(outputs)), name=name, arguments=json.dumps(arguments))
                self.emit("response.output_item.added", output_index=0, item=item)
                self.emit("response.output_item.done", output_index=0, item=item)
            else:
                self.emit("response.reasoning_summary_text.delta", delta="checking fixture evidence")
                self.emit("response.output_text.delta", delta=text)
            self.emit("response.completed", response={"usage": {"input_tokens": 10, "output_tokens": 2}})
        except (BrokenPipeError, ConnectionResetError):
            pass


@contextlib.contextmanager
def fixture(directory):
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    workspace = directory / "workspace"
    workspace.mkdir(exist_ok=True, mode=0o700)
    (workspace / "proof.txt").write_text("PTY_PROOF\n")
    server = LoopbackServer(("127.0.0.1", 0), Fixture)
    server.stopping = threading.Event()
    worker = threading.Thread(target=server.serve_forever)
    worker.start()
    settings = directory / "settings.yaml"
    settings.write_text(
        "route:\n  provider: openai\n  model: fixture\nproviders:\n  openai:\n"
        f"    base_url: http://127.0.0.1:{server.server_port}\n"
        "    api_key_env: NANO_FIXTURE_KEY\n"
        "    models:\n      - id: fixture\n        name: Fixture\n"
        "        context_window: 65536\n        vision: true\n        tools: true\n"
    )
    settings.chmod(0o600)
    try:
        yield workspace, settings
    finally:
        server.stopping.set()
        server.shutdown()
        server.server_close()
        worker.join()


class Terminal:
    def __init__(self, binary, directory, workspace, settings):
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 100, 0, 0))
        self.process = subprocess.Popen(
            [str(binary), "tui", "--root", str(workspace), "--settings", str(settings),
             "--credentials", str(directory / "credentials.yaml"), "--session-root", str(directory / "sessions"),
             "--session", "session-pty"],
            stdin=slave, stdout=slave, stderr=slave, start_new_session=True,
            env={"PATH": os.defpath, "HOME": os.environ["HOME"], "TERM": "xterm-256color", "NANO_FIXTURE_KEY": "fixture-key"},
        )
        os.close(slave)
        self.output = b""
        self.cursor = 0

    def send(self, text):
        os.write(self.master, text.encode())

    def expect(self, text, timeout=30):
        target = text.encode()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            found = self.output.find(target, self.cursor)
            if found >= 0:
                self.cursor = found + len(target)
                return
            ready, _, _ = select.select([self.master], [], [], min(0.2, max(0, deadline - time.monotonic())))
            if ready:
                try:
                    block = os.read(self.master, 65536)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    break
                if not block:
                    break
                self.output += block
        raise AssertionError(f"terminal did not display {text!r}; tail={self.output[-3000:]!r}")

    def close(self):
        try:
            if self.process.poll() is None:
                os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait()
        finally:
            os.close(self.master)


def records(directory):
    result = {}
    for path in (directory / "sessions").glob("*.jsonl"):
        assert path.stat().st_mode & 0o777 == 0o600
        result[path.stem] = [json.loads(line) for line in path.read_text().splitlines()]
    return result


def verify(binary):
    with tempfile.TemporaryDirectory(prefix="nano-tui-e2e-") as temporary:
        directory = Path(temporary)
        with fixture(directory) as (workspace, settings):
            terminal = Terminal(binary, directory, workspace, settings)
            try:
                terminal.expect("/help")
                terminal.send("verify tools\r")
                terminal.expect("Approval required:")
                terminal.send("y\r")
                terminal.expect("Approval required:")
                terminal.send("y\r")
                terminal.expect("WRAP_END")
                terminal.expect("turn> completed")
                terminal.send("/agents\r")
                terminal.expect("reader mode=continuable busy=false outcome=completed")
                terminal.send("wait\r")
                terminal.expect("WAITING_FOR_INTERRUPT")
                terminal.send("/interrupt\r")
                terminal.expect("turn> canceled")
                terminal.send("/quit\r")
                terminal.expect("\x1b[?1049l")
                assert terminal.process.wait(timeout=10) == 0
                assert b"\x1b[?1049h" in terminal.output
            finally:
                terminal.close()
            logs = records(directory)
            root = logs.pop("session-pty")
            assert len(logs) == 1, "expected an independent child session"
            root_records = [entry["record"] for entry in root[1:]]
            calls = [entry["call"]["name"] for entry in root_records if entry["type"] == "tool/call"]
            assert calls == ["list_files", "search_files", "read_file", "spawn_subagent", "subagent_followup",
                             "subagent_report", "list_subagents", "subagent_interrupt", "apply_patch", "run_shell"], calls
            results = [entry["result"] for entry in root_records if entry["type"] == "tool/result"]
            assert len(results) == 10 and all(not entry.get("is_error", False) for entry in results), results
            decisions = [entry["approval"]["outcome"] for entry in root_records if entry["type"] == "approval/decided"]
            assert decisions == ["allowed-once", "allowed-once"], decisions
            assert (workspace / "proof.txt").read_text() == "PTY_PROOF\n"
            assert (workspace / "patched.txt").read_text() == "PATCH_PROOF\n"
            assert (workspace / "shell.txt").read_text() == "SHELL_PROOF"
            child = next(iter(logs.values()))
            assert child[0]["header"]["parent_session_id"] == "session-pty"
            child_records = [entry["record"] for entry in child[1:]]
            assert any(entry["type"] == "approval/policy" and entry["approval"]["policy"] == "never" for entry in child_records)
            assert [entry["call"]["name"] for entry in child_records if entry["type"] == "tool/call"] == ["read_file"]
            assert [entry["outcome"] for entry in child_records if entry["type"] == "turn/end"] == ["completed", "completed"]
            assert "CHILD_READ_OK" in json.dumps(child) and "CHILD_FOLLOW_OK" in json.dumps(child)
            assert not list((directory / "sessions").glob("*.lock"))
            terminal = Terminal(binary, directory, workspace, settings)
            try:
                terminal.expect("turn> canceled")
                terminal.send("/quit\r")
                terminal.expect("\x1b[?1049l")
                assert terminal.process.wait(timeout=10) == 0
            finally:
                terminal.close()
            print("PASS: real binary/PTY, 10 root tools, child read/followup, approvals, files, wrap, interrupt, resume, cleanup")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/nano-harness"))
    parser.add_argument("--serve", type=Path, help="keep a loopback fixture running for interactive debugging")
    args = parser.parse_args()
    if args.serve:
        with fixture(args.serve.resolve()) as (workspace, settings):
            print(f"fixture ready: root={workspace} settings={settings}; NANO_FIXTURE_KEY=fixture-key", flush=True)
            print("TUI input: verify tools (approve twice), /agents, wait, /interrupt, /quit", flush=True)
            try:
                threading.Event().wait()
            except KeyboardInterrupt:
                pass
    else:
        verify(args.binary.resolve())


if __name__ == "__main__":
    main()

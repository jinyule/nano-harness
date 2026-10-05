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
import shutil
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
        users = [i for i, item in enumerate(inputs) if item.get("role") == "user"]
        last_user = users[-1]
        def user_text(index):
            return " ".join(block.get("text", "") for block in inputs[index]["content"])
        task = user_text(last_user)
        if "switched this session" in task and len(users) > 1:
            # A plan-mode notice follows the user's own message in the same turn.
            task = user_text(users[-2]) + " " + task
        outputs = [item["output"] for item in inputs[last_user + 1:] if item.get("type") == "function_call_output"]
        self.server.instructions.append((task, len(outputs), body.get("instructions", "")))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        try:
            if task == "wait":
                self.emit("response.output_text.delta", delta="WAITING_FOR_INTERRUPT")
                self.server.stopping.wait()
                return
            if "PLAN_TASK" in task:
                call = None if outputs else ("exit_plan_mode", {"plan": "# PTY plan\n\n- verify the review"})
                text = "PLAN_DONE"
            elif "CHILD_FOLLOW" in task:
                text = "CHILD_FOLLOW_OK"
                call = None
            elif "CHILD_READ" in task:
                call = None if outputs else ("read", {"file_path": "proof.txt"})
                text = "CHILD_READ_OK"
            elif task.startswith("background job "):
                call = None if outputs else ("job_output", {"job_id": "bash-2"})
                text = "NOTICE_SEEN"
            else:
                child = re.search(r"session=([^ ]+)", " ".join(outputs))
                child_id = child.group(1) if child else "missing-child"
                sequence = [
                    ("todo_write", {"todos": [{"content": "inspect workspace", "status": "in_progress"},
                                              {"content": "report tools", "status": "pending"}]}),
                    ("glob", {"pattern": "*.txt"}),
                    ("grep", {"pattern": "PTY_PROOF"}),
                    ("read", {"file_path": "proof.txt"}),
                    ("spawn_subagent", {"label": "reader", "task": "CHILD_READ", "mode": "continuable", "tools": ["read"]}),
                    ("subagent_followup", {"session_id": child_id, "task": "CHILD_FOLLOW"}),
                    ("subagent_report", {"session_id": child_id}),
                    ("list_subagents", {}),
                    ("subagent_interrupt", {"session_id": child_id}),
                    ("write", {"file_path": "written.txt", "content": "WRITE_PROOF\n"}),
                    ("edit", {"file_path": "written.txt", "old_string": "WRITE_PROOF", "new_string": "EDIT_PROOF"}),
                    ("bash", {"description": "Write the shell proof file", "command": "printf SHELL_PROOF > shell.txt"}),
                    ("bash", {"description": "Start the background proof job", "run_in_background": True,
                              "command": "while [ ! -e notify ]; do sleep 0.05; done; printf JOB_PROOF"}),
                    ("ask_user_question", {"questions": [
                        {"id": "mode", "question": "Which mode?", "options": [{"label": "Fast (Recommended)"}, {"label": "Thorough"}]},
                        {"id": "note", "question": "Any note?"},
                    ]}),
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
    server.instructions = []
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
        yield workspace, settings, server
    finally:
        server.stopping.set()
        server.shutdown()
        server.server_close()
        worker.join()


def tool_path():
    """Return a minimal PATH that still contains the ripgrep the harness requires."""
    ripgrep = shutil.which("rg")
    if ripgrep is None:
        raise SystemExit("tui-e2e: ripgrep (rg) must be on PATH; see docs/development.md")
    return os.pathsep.join([os.path.dirname(os.path.realpath(ripgrep)), os.defpath])


class Terminal:
    def __init__(self, binary, directory, workspace, settings):
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 100, 0, 0))
        self.process = subprocess.Popen(
            [str(binary), "tui", "--root", str(workspace), "--settings", str(settings),
             "--credentials", str(directory / "credentials.yaml"), "--session-root", str(directory / "sessions"),
             "--session", "session-pty"],
            stdin=slave, stdout=slave, stderr=slave, start_new_session=True,
            env={"PATH": tool_path(), "HOME": os.environ["HOME"], "TERM": "xterm-256color", "NANO_FIXTURE_KEY": "fixture-key"},
        )
        os.close(slave)
        self.output = b""
        self.cursor = 0

    def resize(self, width, height):
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))

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
        with fixture(directory) as (workspace, settings, server):
            terminal = Terminal(binary, directory, workspace, settings)
            try:
                terminal.expect("/help")
                terminal.resize(60, 20)
                terminal.send("\x1b[200~verify tools\x1b[201~\r")
                terminal.expect("plan> 1 in progress · 1 pending")
                terminal.expect("[>] inspect workspace")
                for _ in range(4):
                    terminal.expect("Approval required:")
                    terminal.send("y\r")
                terminal.expect("question> Which mode? (1/2)")
                terminal.send("\r")
                terminal.expect("question> Any note? (2/2)")
                terminal.send("PTY_ANSWER\r")
                terminal.expect("WRAP_END")
                terminal.expect("turn> completed")
                # The background job finishes only now; its notice opens a turn.
                (workspace / "notify").touch()
                terminal.expect("job> background job bash-2")
                terminal.expect("NOTICE_SEEN")
                terminal.expect("turn> completed")
                terminal.resize(100, 32)
                terminal.send("/agents\r")
                terminal.expect("reader mode=continuable busy=false outcome=completed")
                terminal.send("/plan\r")
                terminal.expect("Plan mode on.")
                terminal.send("PLAN_TASK\r")
                terminal.expect("question> [Plan review] Approve this plan and leave plan mode? (1/1)")
                terminal.send("1\r")
                terminal.expect("PLAN_DONE")
                terminal.expect("turn> completed")
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
            assert calls == ["todo_write", "glob", "grep", "read", "spawn_subagent", "subagent_followup", "subagent_report",
                             "list_subagents", "subagent_interrupt", "write", "edit", "bash", "bash", "ask_user_question",
                             "job_output", "exit_plan_mode"], calls
            results = [entry["result"] for entry in root_records if entry["type"] == "tool/result"]
            assert len(results) == 16 and all(not entry.get("is_error", False) for entry in results), results
            assert results[0]["output"] == "Updated todo list: 1 pending, 1 in progress, 0 completed.", results[0]
            assert results[1]["output"] == "proof.txt", results[1]
            assert results[2]["output"] == "Found 1 match\n\nproof.txt\nLine 1: PTY_PROOF", results[2]
            assert "1: PTY_PROOF" in results[3]["output"], results[3]
            assert results[12]["output"] == "started background job bash-2", results[12]
            assert results[13]["output"] == ('{"answers":[{"id":"mode","selected":["Fast (Recommended)"]},'
                                             '{"id":"note","selected":[],"custom":"PTY_ANSWER"}]}'), results[13]
            assert results[14]["output"] == "JOB_PROOF\n[status: completed, exit code: 0]", results[14]
            assert results[15]["output"].startswith("Plan approved"), results[15]
            notices = [entry for entry in root_records if entry["type"] == "user/message" and entry["message"]["source"]["kind"] == "tool-jobs"]
            assert len(notices) == 1 and notices[0]["message"]["content"][0]["text"].startswith("background job bash-2 (bash: "), notices
            todos = [entry["todo"] for entry in root_records if entry["type"] == "todo/write"]
            assert todos == [{"call_id": "call-0", "items": [{"content": "inspect workspace", "status": "in_progress"},
                                                             {"content": "report tools", "status": "pending"}]}], todos
            modes = [(entry["plan"]["active"], entry.get("turn", 0)) for entry in root_records if entry["type"] == "plan/mode"]
            assert modes == [(True, 0), (False, 3)], modes
            planned = [(count, "You are in plan mode." in system) for task, count, system in server.instructions if "PLAN_TASK" in task]
            assert planned == [(0, True), (1, False)], planned
            assert any(entry["type"] == "user/message" and entry["message"]["source"]["kind"] == "plan-mode" for entry in root_records)
            decisions = [entry["approval"]["outcome"] for entry in root_records if entry["type"] == "approval/decided"]
            assert decisions == ["allowed-once"] * 4, decisions
            assert (workspace / "proof.txt").read_text() == "PTY_PROOF\n"
            assert (workspace / "written.txt").read_text() == "EDIT_PROOF\n"
            assert (workspace / "shell.txt").read_text() == "SHELL_PROOF"
            child = next(iter(logs.values()))
            assert child[0]["header"]["parent_session_id"] == "session-pty"
            child_records = [entry["record"] for entry in child[1:]]
            assert any(entry["type"] == "approval/policy" and entry["approval"]["policy"] == "never" for entry in child_records)
            assert [entry["call"]["name"] for entry in child_records if entry["type"] == "tool/call"] == ["read"]
            assert [entry["outcome"] for entry in child_records if entry["type"] == "turn/end"] == ["completed", "completed"]
            assert "CHILD_READ_OK" in json.dumps(child) and "CHILD_FOLLOW_OK" in json.dumps(child)
            assert not list((directory / "sessions").glob("*.lock"))
            terminal = Terminal(binary, directory, workspace, settings)
            try:
                terminal.expect("turn> canceled")
                assert b"plan>" not in terminal.output, "a later turn/start must clear the replayed plan"
                terminal.send("/quit\r")
                terminal.expect("\x1b[?1049l")
                assert terminal.process.wait(timeout=10) == 0
            finally:
                terminal.close()
            print("PASS: real binary/PTY, 16 root tool calls, todo plan, background job notice, question answers, plan review, child read/followup, approvals, files, bracketed paste, resize, wrap, interrupt, resume, cleanup")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/nano-harness"))
    parser.add_argument("--serve", type=Path, help="keep a loopback fixture running for interactive debugging")
    args = parser.parse_args()
    if args.serve:
        with fixture(args.serve.resolve()) as (workspace, settings, _):
            print(f"fixture ready: root={workspace} settings={settings}; NANO_FIXTURE_KEY=fixture-key", flush=True)
            print("TUI input: verify tools (approve four times, answer two questions), touch workspace/notify, /agents, /plan, PLAN_TASK (approve), wait, /interrupt, /quit", flush=True)
            try:
                threading.Event().wait()
            except KeyboardInterrupt:
                pass
    else:
        verify(args.binary.resolve())


if __name__ == "__main__":
    main()

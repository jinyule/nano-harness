#!/usr/bin/env python3
"""Exercise the compiled TUI through a PTY and a loopback Responses fixture."""

import argparse
import base64
import contextlib
import hashlib
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
import zlib


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
        def user_text(index):
            return " ".join(block.get("text", "") for block in inputs[index]["content"])
        # A delegated child's runtime context is harness input, not a task.
        users = [i for i, item in enumerate(inputs) if item.get("role") == "user"
                 and not user_text(i).startswith("Current runtime context.")]
        last_user = users[-1]
        task = user_text(last_user)
        if "switched this session" in task and len(users) > 1:
            # A plan-mode notice follows the user's own message in the same turn.
            task = user_text(users[-2]) + " " + task
        raw_outputs = [item["output"] for item in inputs[last_user + 1:] if item.get("type") == "function_call_output"]
        # An image result arrives as input_text/input_image items instead of a string.
        self.server.image_urls.extend(part["image_url"] for output in raw_outputs if isinstance(output, list)
                                      for part in output if part.get("type") == "input_image")
        outputs = [output if isinstance(output, str) else json.dumps(output) for output in raw_outputs]
        self.server.attached_urls.extend(part["image_url"] for item in inputs if item.get("role") == "user"
                                         for part in item["content"] if part.get("type") == "input_image")
        self.server.instructions.append((task, len(outputs), body.get("instructions", "")))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        try:
            if task == "wait":
                self.emit("response.output_text.delta", delta="WAITING_FOR_INTERRUPT")
                self.server.stopping.wait()
                return
            if task == "IMAGE_REPLAY":
                self.server.replay_inputs = inputs
                call = None
                text = "IMAGE_REPLAY_DONE"
            elif "<goal_complete>" in task:
                call = None
                text = "GOAL_CLOSED"
            elif "<goal_round>" in task:
                goal = re.search(r'"id":"(goal-[0-9a-f]+)"', " ".join(outputs))
                call = ("get_goal", {}) if not outputs else ("update_goal", {"goal_id": goal.group(1), "revision": 1, "action": "complete"})
                text = "GOAL_ROUND_DONE"
            elif "PLAN_TASK" in task:
                call = None if outputs else ("exit_plan_mode", {"plan": "# PTY plan\n\n- verify the review"})
                text = "PLAN_DONE"
            elif task.startswith("CHILD_FORK"):
                # The fork inherits the earlier turns' history, not this in-flight turn.
                text = "CHILD_FORK_OK"
                call = None
            elif task.startswith("CHILD_READ"):
                call = None if outputs else ("read", {"file_path": "proof.txt"})
                text = "CHILD_READ_OK"
            elif task.startswith("background job "):
                call = None if outputs else ("job_output", {"job_id": "bash-2"})
                text = "NOTICE_SEEN"
            else:
                sequence = [
                    ("todo_write", {"todos": [{"content": "inspect workspace", "status": "in_progress"},
                                              {"content": "report tools", "status": "pending"}]}),
                    ("glob", {"pattern": "*.txt"}),
                    ("grep", {"pattern": "PTY_PROOF"}),
                    ("read", {"file_path": "proof.txt"}),
                    ("read_image", {"file_path": "pixel.png"}),
                    ("subagent", {"description": "reader", "prompt": "CHILD_READ", "run_in_background": False}),
                    ("subagent_fork", {"description": "reviewer", "prompt": "CHILD_FORK"}),
                    ("list_agents", {"scope": "descendants"}),
                    ("send_message", {"agent_id": "missing-child", "message": "CHILD_FOLLOW"}),
                    ("interrupt_agent", {"agent_id": "missing-child"}),
                    ("write", {"file_path": "written.txt", "content": "WRITE_PROOF\n"}),
                    ("edit", {"file_path": "written.txt", "old_string": "WRITE_PROOF", "new_string": "EDIT_PROOF"}),
                    ("bash", {"description": "Write the shell proof file", "command": "printf SHELL_PROOF > shell.txt"}),
                    ("bash", {"description": "Start the background proof job", "run_in_background": True,
                              "command": "while [ ! -e notify ]; do sleep 0.05; done; printf JOB_PROOF"}),
                    ("ask_user_question", {"questions": [
                        {"id": "mode", "question": "Which mode?", "multi_select": True, "options": [{"label": "Fast (Recommended)"}, {"label": "Thorough"}]},
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


def png(width, height):
    """Encode an opaque red RGB PNG without third-party modules."""
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
    rows = b"".join(b"\x00" + b"\xc8\x1e\x1e" * width for _ in range(height))
    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", zlib.compress(rows)) + chunk(b"IEND", b"")


@contextlib.contextmanager
def fixture(directory):
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    workspace = directory / "workspace"
    workspace.mkdir(exist_ok=True, mode=0o700)
    (workspace / "proof.txt").write_text("PTY_PROOF\n")
    (workspace / "pixel.png").write_bytes(png(2, 2))
    server = LoopbackServer(("127.0.0.1", 0), Fixture)
    server.stopping = threading.Event()
    server.instructions = []
    server.image_urls = []
    server.attached_urls = []
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
             "--skills-dir", str(directory / "skills"), "--agents-skills-dir", str(directory / "agents-skills"),
             "--spill-root", str(directory / "spill"), "--attachment-root", str(directory / "attachments"),
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
    assert (directory / "spill").stat().st_mode & 0o777 == 0o700
    assert (directory / "attachments").stat().st_mode & 0o777 == 0o700
    result = {}
    for path in (directory / "sessions").glob("*.jsonl"):
        assert path.stat().st_mode & 0o777 == 0o600
        result[path.stem] = [json.loads(line) for line in path.read_text().splitlines()]
    return result


def stored_image(directory, image):
    """Return the attachment object behind one reference after checking its identity and mode."""
    digest = image["id"].removeprefix("sha256:")
    path = directory / "attachments" / "v1" / "objects" / digest[:2] / digest
    assert path.stat().st_mode & 0o777 == 0o400, path
    data = path.read_bytes()
    assert hashlib.sha256(data).hexdigest() == digest and len(data) == image["bytes"], image
    return "data:" + image["media_type"] + ";base64," + base64.b64encode(data).decode()


def verify(binary):
    with tempfile.TemporaryDirectory(prefix="nano-tui-e2e-") as temporary:
        directory = Path(temporary)
        with fixture(directory) as (workspace, settings, server):
            terminal = Terminal(binary, directory, workspace, settings)
            try:
                terminal.expect("/help")
                # /attach only normalizes; the object is stored when the message is sent.
                terminal.send("/attach " + str(workspace / "pixel.png") + "\r")
                terminal.expect("system> attached pixel.png (2x2)")
                assert not (directory / "attachments" / "v1" / "objects").exists() or not any((directory / "attachments" / "v1" / "objects").iterdir())
                terminal.resize(60, 20)
                terminal.send("\x1b[200~verify tools\x1b[201~\r")
                terminal.expect("plan> 1 in progress · 1 pending")
                terminal.expect("[>] inspect workspace")
                terminal.expect("[image pixel.png 2x2 sha256:")
                for _ in range(4):
                    terminal.expect("Approval required:")
                    terminal.send("y\r")
                terminal.expect("question> Which mode? (1/2)")
                terminal.send(",2\r")
                terminal.expect("question> Selected: Fast (Recommended), Thorough.")
                terminal.send("PTY_CUSTOM\r")
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
                terminal.expect("no subagents")
                terminal.send("/sandbox read-only\r")
                terminal.expect("sandbox mode=read-only")
                terminal.send("/plan\r")
                terminal.expect("Plan mode on.")
                terminal.send("PLAN_TASK\r")
                terminal.expect("question> [Plan review] Approve this plan and leave plan mode? (1/1)")
                terminal.send("1\r")
                terminal.expect("PLAN_DONE")
                terminal.expect("turn> completed")
                # A /goal objective arms the driver, whose round completes the goal.
                terminal.send("/goal PTY_GOAL ship it\r")
                terminal.expect("goal> Goal created")
                terminal.expect("goal> round 1")
                terminal.expect("goal> <goal_complete>")
                terminal.expect("GOAL_CLOSED")
                terminal.expect("turn> completed")
                terminal.send("/goal\r")
                terminal.expect("goal> Status: complete")
                # The header is redrawn independently of the transcript.
                assert b"goal=complete 1/256" in terminal.output
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
            assert len(logs) == 2, "expected independent spawn and fork child sessions"
            root_records = [entry["record"] for entry in root[1:]]
            calls = [entry["call"]["name"] for entry in root_records if entry["type"] == "tool/call"]
            assert calls == ["todo_write", "glob", "grep", "read", "read_image", "subagent", "subagent_fork", "list_agents",
                             "send_message", "interrupt_agent", "write", "edit", "bash", "bash", "ask_user_question",
                             "job_output", "exit_plan_mode", "get_goal", "update_goal"], calls
            results = [entry["result"] for entry in root_records if entry["type"] == "tool/result"]
            failed = [index for index, entry in enumerate(results) if entry.get("is_error", False)]
            assert len(results) == 19 and failed == [8], results
            assert results[0]["output"] == "Updated todo list: 1 pending, 1 in progress, 0 completed.", results[0]
            assert results[1]["output"] == "proof.txt", results[1]
            assert results[2]["output"] == "Found 1 match\n\nproof.txt\nLine 1: PTY_PROOF", results[2]
            assert "1: PTY_PROOF" in results[3]["output"], results[3]
            image = results[4].get("image")
            assert image and image["name"] == "pixel.png" and image["media_type"] == "image/jpeg", results[4]
            assert (image["width"], image["height"]) == (2, 2) and "<type>image</type>" in results[4]["output"], results[4]
            assert "data" not in image and stored_image(directory, image) in server.image_urls, "the next request lacked the stored image"
            opening = next(entry["message"] for entry in root_records if entry["type"] == "user/message")
            attached = [block["image"] for block in opening["content"] if block["type"] == "image"]
            assert [entry["name"] for entry in attached] == ["pixel.png"] and "data" not in attached[0], opening
            assert stored_image(directory, attached[0]) in server.attached_urls, "the provider never received the attachment"
            assert results[5]["output"] == "CHILD_READ_OK", results[5]
            assert results[6]["output"] == "CHILD_FORK_OK", results[6]
            assert results[7]["output"] == "(no subagents)", results[7]
            assert results[8]["output"] == 'Error: subagent "missing-child" is unavailable', results[8]
            assert results[9]["output"] == "interrupt requested for agent missing-child", results[9]
            catalog = [entry["catalog"] for entry in root_records if entry["type"] == "subagent/catalog"]
            assert [(entry["label"], entry["mode"]) for entry in catalog] == [("reader", "one-shot"), ("reviewer", "one-shot")], catalog
            assert results[13]["output"] == "started background job bash-2", results[13]
            assert results[14]["output"] == ('{"answers":[{"id":"mode","selected":["Fast (Recommended)","Thorough"],"custom":"PTY_CUSTOM"},'
                                             '{"id":"note","selected":[],"custom":"PTY_ANSWER"}]}'), results[14]
            assert results[15]["output"] == "JOB_PROOF\n[status: completed, exit code: 0]", results[15]
            assert results[16]["output"].startswith("Plan approved"), results[16]
            assert '"objective":"PTY_GOAL ship it","phase":"active","roundsStarted":1' in results[17]["output"], results[17]
            assert '"phase":"complete","roundsStarted":1,"maxGoalRounds":256},"activation":"disarmed"' in results[18]["output"], results[18]
            goals = [(entry["goal"]["operation"], entry.get("turn", 0)) for entry in root_records if entry["type"] == "goal/change"]
            assert goals == [("create", 0), ("complete", 0)], goals
            sources = [entry["message"]["source"] for entry in root_records if entry["type"] == "user/message"]
            assert sum(1 for source in sources if source["kind"] == "goal" and source["goal_round"] == 1) == 1, sources
            assert sum(1 for source in sources if source["kind"] == "tool-goal") == 1, sources
            assert any("create_goal may infer goal intent" in system for _, _, system in server.instructions)
            notices = [entry for entry in root_records if entry["type"] == "user/message" and entry["message"]["source"]["kind"] == "tool-jobs"]
            assert len(notices) == 1 and notices[0]["message"]["content"][0]["text"].startswith("background job bash-2 (bash: "), notices
            todos = [entry["todo"] for entry in root_records if entry["type"] == "todo/write"]
            assert todos == [{"call_id": "call-0", "items": [{"content": "inspect workspace", "status": "in_progress"},
                                                             {"content": "report tools", "status": "pending"}]}], todos
            sandbox_modes = [entry["sandbox"]["mode"] for entry in root_records if entry["type"] == "sandbox/mode"]
            assert sandbox_modes == ["read-only"], sandbox_modes
            snapshots = [entry["message"]["content"][0]["text"] for entry in root_records if entry["type"] == "user/message" and entry["message"]["source"]["kind"] == "runtime-context"]
            assert len(snapshots) == 2 and "workspace-write" in snapshots[0] and "read-only" in snapshots[1], snapshots
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
            children = {entry["session_id"]: entry["label"] for entry in catalog}
            for session_id, label in children.items():
                child = logs[session_id]
                assert child[0]["header"]["parent_session_id"] == "session-pty"
                child_records = [entry["record"] for entry in child[1:]]
                descriptors = [entry["subagent"] for entry in child_records if entry["type"] == "subagent/descriptor"]
                own = child_records[descriptors[-1].get("inherited", 0):]
                assert own[0]["type"] == "subagent/descriptor" and own[0]["subagent"]["mode"] == "one-shot", own[0]
                assert own[1]["type"] == "approval/policy" and own[1]["approval"]["policy"] == "never", own[1]
                assert own[0]["subagent"]["route"] == {"provider": "openai", "model": "fixture"}, own[0]
                assert [entry for entry in own if entry["type"] == "user/message"
                        and entry["message"]["source"]["kind"] == "runtime-context"], "missing delegation runtime context"
                assert [entry["outcome"] for entry in own if entry["type"] == "turn/end"] == ["completed"], own
                if label == "reader":
                    assert own[0]["subagent"]["provider"] == "spawn" and own[0]["subagent"].get("inherited", 0) == 0
                    assert [entry["call"]["name"] for entry in own if entry["type"] == "tool/call"] == ["read"]
                    assert "CHILD_READ_OK" in json.dumps(own)
                else:
                    # The fork starts in the first PTY turn, so no completed turn exists to inherit.
                    assert own[0]["subagent"]["provider"] == "fork" and own[0]["subagent"].get("inherited", 0) == 0
                    assert not [entry for entry in own if entry["type"] == "tool/call"] and "CHILD_FORK_OK" in json.dumps(own)
            assert not list((directory / "sessions").glob("*.lock"))
            # Keep the first reference intact and corrupt only the later claim.
            assert image["id"] == attached[0]["id"], "the fixture must reference one shared object"
            image["bytes"] -= 1
            transcript = directory / "sessions" / "session-pty.jsonl"
            transcript.write_text("".join(json.dumps(entry) + "\n" for entry in root))
            terminal = Terminal(binary, directory, workspace, settings)
            try:
                terminal.expect("turn> canceled")
                assert b"plan>" not in terminal.output, "a later turn/start must clear the replayed plan"
                terminal.send("IMAGE_REPLAY\r")
                terminal.expect("attachment> image pixel.png (" + image["id"][:19] + ")")
                terminal.expect("failed verification in the attachment store")
                terminal.expect("IMAGE_REPLAY_DONE")
                terminal.expect("turn> completed")
                inputs = server.replay_inputs
                sent_images = [part["image_url"] for item in inputs for part in item.get("content", [])
                               if part.get("type") == "input_image"]
                assert sent_images == [stored_image(directory, attached[0])], "the valid reference must keep its image"
                image_output = next(item["output"] for item in inputs
                                    if item.get("type") == "function_call_output" and item["call_id"] == "call-4")
                placeholder = '[image unavailable: "pixel.png" (' + image["id"] + ') is missing or failed verification in the local attachment store]'
                assert isinstance(image_output, str) and placeholder in image_output, image_output
                terminal.send("/quit\r")
                terminal.expect("\x1b[?1049l")
                assert terminal.process.wait(timeout=10) == 0
            finally:
                terminal.close()
            assert not list((directory / "sessions").glob("*.lock"))
            assert "attachment>" not in transcript.read_text(), "attachment notices must not be persisted"
            print("PASS: real binary/PTY, 19 root tool calls, /attach and read_image through the attachment store, conflicting reference placeholder and TUI notice, todo plan, background job notice, question answers, sandbox mode switch, plan review, /goal round completion, spawn/fork children, approvals, files, bracketed paste, resize, wrap, interrupt, resume, cleanup")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/nano-harness"))
    parser.add_argument("--serve", type=Path, help="keep a loopback fixture running for interactive debugging")
    args = parser.parse_args()
    if args.serve:
        with fixture(args.serve.resolve()) as (workspace, settings, _):
            print(f"fixture ready: root={workspace} settings={settings}; NANO_FIXTURE_KEY=fixture-key", flush=True)
            print("TUI input: verify tools (approve four times, answer two questions), touch workspace/notify, /agents, /plan, PLAN_TASK (approve), /goal OBJECTIVE, wait, /interrupt, /quit", flush=True)
            try:
                threading.Event().wait()
            except KeyboardInterrupt:
                pass
    else:
        verify(args.binary.resolve())


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Hindsight PreToolUse hook for Codex CLI.

stdin : pre-tool-use.command.input  (tool_name / tool_input.command / cwd / session_id / turn_id ...)
stdout: EXACTLY one JSON object matching pre-tool-use.command.output (deny_unknown_fields!)
Silence == no-op. Any non-JSON stdout is ignored; JSON-looking-but-invalid == hook Failed.
"""
import json, os, subprocess, sys, hashlib

CACHE = os.environ.get("HP_CACHE", "/tmp/hp")

def noop():
    sys.exit(0)                      # print nothing -> Codex runs the original command

def rewrite(cmd):
    json.dump({"hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "allow",
        "updatedInput": {"command": cmd},
    }}, sys.stdout)
    sys.exit(0)

def main():
    ev = json.load(sys.stdin)
    if ev.get("tool_name") != "Bash":
        noop()
    cmd = (ev.get("tool_input") or {}).get("command")
    cwd = ev.get("cwd")
    if not isinstance(cmd, str) or not cwd:
        noop()

    # ---- Hindsight key: git Merkle tree of the live workspace + normalized command
    side = os.path.join(CACHE, "idx", hashlib.sha1(cwd.encode()).hexdigest())
    os.makedirs(os.path.dirname(side), exist_ok=True)
    env = dict(os.environ, GIT_INDEX_FILE=side)
    try:
        subprocess.run(["git", "add", "-A"], cwd=cwd, env=env,
                       check=True, capture_output=True, timeout=10)
        tree = subprocess.run(["git", "write-tree"], cwd=cwd, env=env,
                              check=True, capture_output=True, text=True, timeout=10).stdout.strip()
    except Exception:
        noop()

    key = hashlib.sha256(f"{tree}\0{cwd}\0{' '.join(cmd.split())}".encode()).hexdigest()[:32]
    hit = os.path.join(CACHE, "out", key)
    if os.path.exists(hit + ".rc"):
        # HIT: serve the recorded execution.
        rewrite(f"cat {hit}.out; exit $(cat {hit}.rc)")
    # MISS: wrap so the real run is recorded (exit code + stdout + duration).
    rewrite(f"{sys.executable} {os.path.join(os.path.dirname(os.path.abspath(__file__)), 'hp_record.py')} "
            f"{key} {json.dumps(cmd)}")

main()

"""Hindsight key for prime-agent at RLM depth > 2.

Boundary: BashOperations.exec (packages/coding-agent/src/core/tools/bash.ts:40).
The kernel namespace is NEVER read, so depth is irrelevant to correctness.
"""
import hashlib, json, os, re, shlex, subprocess, tempfile

KEY_VERSION = "hs-v1"

# Env vars that legitimately change command output. ALLOWLIST, never the whole env.
# prime-agent injects RLM_DEPTH / RLM_SESSION_DIR / RLM_HARNESS_STATE_DIR per-session
# (agent-session.ts:9200 _rlmKernelEnv). Hashing those => every agent at every depth
# gets a distinct key => 0% cross-agent sharing. They are deliberately excluded.
ENV_ALLOW = ("LANG", "LC_ALL", "TZ", "PATH", "VIRTUAL_ENV", "CONDA_DEFAULT_ENV", "PYTHONPATH")
ENV_DENY_PREFIX = ("RLM_", "PRIME_AGENT_")

PURE_HEADS = {"grep","rg","cat","ls","find","head","tail","wc","sort","uniq","cut",
              "awk","diff","stat","file","tree","basename","dirname","realpath","nl","md5sum","shasum"}
GIT_PURE_SUB = {"status","log","diff","show","blame","ls-files","rev-parse","describe","branch","tag","cat-file"}

# Any match => not provably pure. Order matters only for readability.
IMPURE = re.compile(r"""(?x)
    \$\( | ` |                                   # command substitution
    \|\s*(sh|bash|zsh)\b |                       # pipe to shell
    (^|[;&|]\s*)(rm|mv|cp|touch|mkdir|rmdir|chmod|chown|ln|tee|install|dd|truncate)\b |
    >{1,2} |                                     # redirection = mutation
    \b(curl|wget|nc|ssh|scp|pip|npm|yarn|pnpm|uv|cargo|go|apt|brew|docker|git\s+(push|pull|fetch|clone))\b |
    \b(date|uuidgen|hostname|whoami|ps|top|df|free|uptime)\b |
    \$RANDOM | /dev/u?random | \b(mktemp|tempfile)\b
""")

def workspace_key(repo_root, side_index=None):
    """git's own Merkle hash of the LIVE worktree. Measured 20ms on a 1196-file repo
    with a warm side index. Never touches .git/index."""
    side = side_index or os.path.join(tempfile.gettempdir(), f"hs-idx-{os.getpid()}")
    env = {**os.environ, "GIT_INDEX_FILE": side}
    subprocess.run(["git","add","-A"], cwd=repo_root, env=env, check=True, capture_output=True)
    r = subprocess.run(["git","write-tree"], cwd=repo_root, env=env, check=True,
                       capture_output=True, text=True)
    return r.stdout.strip()

def normalize_command(cmd):
    try:
        parts = shlex.split(cmd)
    except ValueError:
        return " ".join(cmd.split())
    return " ".join(parts)

def purity_class(cmd):
    """PURE      -> serve on first recording (tree fully determines output)
       CANDIDATE -> serve only after k independent recordings agreed byte-for-byte
       IMPURE    -> never serve"""
    if IMPURE.search(cmd):
        return "IMPURE"
    try:
        parts = shlex.split(cmd)
    except ValueError:
        return "IMPURE"
    if not parts:
        return "IMPURE"
    head = os.path.basename(parts[0])
    if head == "sed":                            # sed -n prints; sed -i mutates
        return "PURE" if "-n" in parts else "IMPURE"
    if head == "git":
        sub = next((p for p in parts[1:] if not p.startswith("-")), None)
        return "PURE" if sub in GIT_PURE_SUB else "IMPURE"
    if head in PURE_HEADS:
        return "PURE"
    # pytest / make / tsc / cargo test: usually tree-determined but not provably so.
    return "CANDIDATE"

def hindsight_key(repo_root, cmd, cwd, env=None):
    env = env or os.environ
    env_pairs = sorted(
        (k, env[k]) for k in ENV_ALLOW
        if k in env and not k.startswith(ENV_DENY_PREFIX)
    )
    payload = {
        "v": KEY_VERSION,
        "tree": workspace_key(repo_root),
        "cmd": normalize_command(cmd),
        "cwd": os.path.relpath(os.path.realpath(cwd), os.path.realpath(repo_root)),
        "env": env_pairs,
    }
    blob = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(blob).hexdigest(), purity_class(cmd)

if __name__ == "__main__":
    import sys
    root = sys.argv[1]
    for c in ["grep -rn 'foo' src/", "git status --porcelain", "pytest tests/ -q",
              "rm -rf build", "cat $(ls)", "echo hi > out.txt", "sed -n '1,5p' a.py",
              "sed -i 's/a/b/' a.py", "python3 -c 'import random;print(random.random())'"]:
        k, cls = hindsight_key(root, c, root)
        print(f"{cls:10s} {k[:16]}  {c}")

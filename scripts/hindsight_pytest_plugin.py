"""Observed read sets for Hindsight, captured from a real pytest run.

Hindsight keys a shell command on git's tree hash. Once two agents edit
anything, their trees diverge and every command misses -- including commands
whose inputs did not move. Tier-1 scoping recovers those hits by proving the
changed paths are disjoint from what the command reads, which only works if
we know what the command reads.

Inferring that from the command line is unsound, and there is a test in this
repo that says so: "pytest tests/test_billing.py" reads src/billing.py through
an import, and that path appears nowhere in argv. So Tier-1 was restricted to
commands whose read set provably is their arguments -- cat, wc, md5sum -- all
of which are too cheap to be worth caching. This plugin removes the guess: it
reports what the run actually imported.

WHAT THIS CAPTURES, AND WHAT IT DOES NOT
========================================
When the session finishes we walk sys.modules and keep every module whose
__file__ resolves inside the repo root. That is the import graph, measured
rather than inferred, and it is exactly the dependency edge Tier-1 needs.

It is NOT the set of files the run opened. A test that does

    open("tests/data/rates.json")

reads a file that never enters sys.modules, and this plugin will not see it.
That gap is real and it is the reason the Go side does not treat this set as
a complete read set: internal/hp/readset.go refuses to promote whenever a
changed path is of a kind this method cannot observe, so a changed .json
refuses while a changed .py that was provably never imported promotes. A
missed promotion costs a hit; a wrong one costs the product.

sys.modules rather than coverage.py, for three reasons: it needs no
dependency, so it cannot fail to install in a target repo; it costs one dict
walk at teardown rather than a tracer on every line; and imports are the edge
Tier-1 reasons about, whereas line coverage is both more and less than that.

THREE RULES THIS FILE MUST NEVER BREAK
======================================
1. Print nothing, ever. Anything reaching stdout or stderr becomes part of the
   recorded output, which means it is baked into every cache entry for the
   command and diffed forever by shadow verification.
2. Raise nothing, ever. An exception from a pytest hook is an INTERNALERROR
   and changes the exit code of the command we are only supposed to be
   watching.
3. Emit a whole line or no line. A read set with a path missing is the single
   failure mode that produces a wrong answer rather than a slow one, so every
   partial observation is reported as incomplete and the Go side drops it.

ACTIVATION, WITHOUT ASKING ANYONE TO INSTALL ANYTHING
=====================================================
hindsight record adds three variables to the environment of the command it
wraps:

    PYTHONPATH=<dir holding this file>:$PYTHONPATH
    PYTEST_ADDOPTS=$PYTEST_ADDOPTS -p hindsight_pytest_plugin
    HINDSIGHT_READSET_OUT=<jsonl path>   HINDSIGHT_READSET_ROOT=<repo root>

No entry point, no pip install, no conftest edit, and nothing written inside
the workspace. It is inert for anything that is not Python, because nothing
else reads those variables. If either HINDSIGHT_ variable is missing this
plugin does nothing at all, which is what makes it safe to leave loaded.

OUTPUT FORMAT
=============
One JSON object per line, appended. One line per Python process, so an xdist
run or a chained "pytest a && pytest b" produces several and the Go side takes
the union.

    {"v": 1, "method": "python/sys.modules", "tool": "pytest", "pid": 4711,
     "root": "/abs/repo", "rootdir": "/abs/repo", "exit_status": 0,
     "complete": true, "test_globs": ["test_*.py", "*_test.py"],
     "paths": ["conftest.py", "src/billing.py", "tests/test_billing.py"]}

The line is written with a single os.write to a file opened O_APPEND, which
is atomic for any payload the kernel can take in one go. A torn line from a
pathologically large set does not parse, and the Go side refuses the whole
capture rather than silently using the surviving lines -- because the
surviving lines are a read set with paths missing, which is rule 3.
"""

import json
import os
import sys

SCHEMA_VERSION = 1

# Names the capture method so a consumer can tell a sys.modules set from an
# strace set. They do not have the same completeness guarantee and nothing
# downstream should have to infer which one it is holding.
METHOD = "python/sys.modules"

# Directory names whose contents are environment rather than tree. They are
# dropped from the read set, and dropping is normally the unsafe direction --
# but these are covered by the environment fingerprint instead, which is a
# component of the cache key, so a candidate record with a different installed
# package set is never even considered for promotion. Keeping them would add
# thousands of paths that no git diff can ever name.
SKIP_DIR_NAMES = frozenset((
    ".git",
    ".venv",
    "venv",
    ".tox",
    ".nox",
    ".eggs",
    "__pycache__",
    "site-packages",
    "dist-packages",
    "node_modules",
))

# A set this large is a sign we are looking at a vendored tree rather than a
# dependency graph. Report it as incomplete instead of writing a megabyte into
# every cache record.
MAX_PATHS = 20000

_emitted = False


def _in_repo(real_path, real_root):
    """Repo-relative slash path, or None if the file is outside the repo."""
    if real_path == real_root:
        return None
    prefix = real_root + os.sep
    if not real_path.startswith(prefix):
        return None
    rel = real_path[len(prefix):]
    if not rel:
        return None
    return rel.replace(os.sep, "/")


def _skipped(rel):
    return any(part in SKIP_DIR_NAMES for part in rel.split("/")[:-1])


def _collect(real_root):
    """Walk sys.modules. Returns (sorted paths, complete).

    complete is False when any module carried a __file__ we could not resolve
    to an absolute path. Python 3.9 and later always give an absolute
    __file__, so this should not fire -- but resolving a relative one against
    the current working directory would invent a path, and inventing a path is
    how a real dependency escapes the set.
    """
    complete = True
    found = set()
    # list() because a module's __getattr__ can import, which would mutate
    # sys.modules while we walk it.
    for _name, module in list(sys.modules.items()):
        try:
            filename = getattr(module, "__file__", None)
        except Exception:
            complete = False
            continue
        if not filename or not isinstance(filename, str):
            # Builtins and namespace packages have no file. A namespace
            # package contributes no source of its own, so there is nothing to
            # miss; its submodules appear here in their own right.
            continue
        if not os.path.isabs(filename):
            complete = False
            continue
        try:
            resolved = os.path.realpath(filename)
        except Exception:
            complete = False
            continue
        rel = _in_repo(resolved, real_root)
        if rel is None or _skipped(rel):
            continue
        found.add(rel)
        if len(found) > MAX_PATHS:
            return sorted(found), False
    return sorted(found), complete


def _test_globs(config):
    """The project's own test discovery patterns, not our guess at them.

    The Go side refuses to promote when the diff adds a file these patterns
    would collect, because such a file changes what the command runs while
    being absent from the read set by construction.
    """
    try:
        globs = [g for g in config.getini("python_files") if isinstance(g, str)]
    except Exception:
        globs = []
    return globs or ["test_*.py", "*_test.py"]


def _emit(payload, out_path):
    data = (json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    fd = os.open(out_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        os.write(fd, data)
    finally:
        os.close(fd)


def pytest_sessionfinish(session, exitstatus):
    """The only hook. Runs after collection and execution, before teardown of
    the process, which is the point at which sys.modules is at its fullest.

    Everything is inside one try/except that swallows absolutely everything.
    A read-set capture that breaks the run it is measuring is worse than no
    capture at all: the cache degrades to a miss without it, and to a broken
    tool with it.
    """
    global _emitted
    try:
        if _emitted:
            return
        out_path = os.environ.get("HINDSIGHT_READSET_OUT")
        root = os.environ.get("HINDSIGHT_READSET_ROOT")
        if not out_path or not root:
            return
        real_root = os.path.realpath(root)
        paths, complete = _collect(real_root)

        config = getattr(session, "config", None)
        try:
            rootdir = str(config.rootpath)
        except Exception:
            rootdir = ""

        _emitted = True
        _emit({
            "v": SCHEMA_VERSION,
            "method": METHOD,
            "tool": "pytest",
            "pid": os.getpid(),
            "root": real_root,
            "rootdir": rootdir,
            "exit_status": int(exitstatus) if exitstatus is not None else -1,
            "complete": bool(complete),
            "test_globs": _test_globs(config) if config is not None else [],
            "paths": paths,
        }, out_path)
    except Exception:
        # Rule 2. There is no acceptable way to report this: stderr is part of
        # the recorded output, and a raise is an INTERNALERROR. Emitting
        # nothing is the correct outcome, because the Go side treats a missing
        # read set as a refusal to promote.
        pass

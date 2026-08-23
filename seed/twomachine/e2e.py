import subprocess, os, sys, re, json, hashlib
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from norm import norm
CACHE = {}
def tree_key(root, side):
    e = dict(os.environ, GIT_INDEX_FILE=side)
    subprocess.run(["git","add","-A"], cwd=root, env=e, capture_output=True)
    return subprocess.run(["git","write-tree"], cwd=root, env=e,
                          capture_output=True, text=True).stdout.strip()
def run(root, cmd, side, who):
    k = hashlib.sha256((tree_key(root,side)+"\x00"+cmd).encode()).hexdigest()[:16]
    if k in CACHE:
        out = CACHE[k]["out"].replace(b"{{ROOT}}", root.encode())
        return "HIT", k, out, CACHE[k]["by"]
    p = subprocess.run(cmd, cwd=root, shell=True, capture_output=True)
    raw = p.stdout + p.stderr
    CACHE[k] = {"out": norm(raw, root, os.path.expanduser("~")), "by": who}
    return "MISS", k, raw, who
Aroot, Broot = sys.argv[1], sys.argv[2]
CMD = "python3 test_x.py"
for (root, who) in [(Aroot,"alice@laptop-A"), (Broot,"bob@laptop-B")]:
    st,k,out,by = run(root, CMD, root+"/../.hpidx", who)
    print(f"[{who}] {st}  key={k}  served_by={by}")
    print("   " + "\n   ".join(out.decode(errors="replace").strip().splitlines()[:8]))

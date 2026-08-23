#!/usr/bin/env python3
# hp_record.py <key> <original-command>   -- runs the real command, records it, is transparent.
import os, subprocess, sys, time
key, cmd = sys.argv[1], sys.argv[2]
CACHE = os.environ.get("HP_CACHE", "/tmp/hp"); out = os.path.join(CACHE, "out"); os.makedirs(out, exist_ok=True)
t0 = time.time()
p = subprocess.run(["/bin/sh", "-c", cmd], capture_output=True)
blob = p.stdout + p.stderr
sys.stdout.buffer.write(blob); sys.stdout.flush()
tmp = os.path.join(out, f".{key}.{os.getpid()}")
open(tmp, "wb").write(blob); os.replace(tmp, os.path.join(out, key + ".out"))
open(tmp, "w").write(str(p.returncode)); os.replace(tmp, os.path.join(out, key + ".rc"))
open(os.path.join(out, key + ".ms"), "w").write(str(int((time.time()-t0)*1000)))
sys.exit(p.returncode)

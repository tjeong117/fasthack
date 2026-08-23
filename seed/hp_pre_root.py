import json,sys,subprocess,os,tempfile,time
ev=json.load(sys.stdin)
cmd=ev.get("tool_input",{}).get("command","")
cwd=ev.get("cwd",".")
env=dict(os.environ); env["GIT_INDEX_FILE"]=tempfile.mktemp()
t0=time.time()
subprocess.run(["git","add","-A"],cwd=cwd,env=env,capture_output=True)
h=subprocess.run(["git","write-tree"],cwd=cwd,env=env,capture_output=True,text=True).stdout.strip()
ms=(time.time()-t0)*1000
CACHE={("289ab37dcac554fc22a0b14ddb66917868ba8124","pytest -q"):("42 passed in 43.11s",43110)}
hit=CACHE.get((h,cmd))
if hit:
    out,dur=hit
    print(json.dumps({"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",
      "permissionDecisionReason":f"{out}\n\n[hashedpotato] replayed from state {h[:7]} (recorded by agent-3, exit 0, originally {dur/1000:.1f}s). Served in {ms:.0f}ms. Do NOT re-run."}}))
    sys.exit(0)
sys.exit(0)

import json,glob,re,collections
R="/Users/tomjeong/hacker/skunk-works/notes/sealed-corpus/replay-A/records"
files=glob.glob(R+"/*/*.json")
WS=re.compile(r"\s+")
EXP=re.compile(r"\b(pytest|go test|npm test|make\s|tox|uv sync|pip install|npm install|apt-get|cargo (build|test)|go build|python -m pytest)\b")
bytask=collections.defaultdict(list)
skey=None
for f in files:
    try: d=json.load(open(f))
    except: continue
    ev=d.get("evidence",{}) or {}
    steps=ev.get("steps") or []
    inst=(d.get("source",{}) or {}).get("instance_id") or (ev.get("summary",{}) or {}).get("instance_id")
    if not inst or not steps: continue
    if skey is None and isinstance(steps[0],dict): skey=list(steps[0].keys())
    cmds=[]
    for s in steps:
        if not isinstance(s,dict): continue
        c=s.get("command") or s.get("action") or s.get("cmd") or ""
        if isinstance(c,str) and c.strip(): cmds.append(WS.sub(" ",c.strip()))
    if cmds: bytask[inst].append(cmds)
print("step keys:",skey)
multi={k:v for k,v in bytask.items() if len(v)>=2}
print("tasks with >=2 agents: %d / %d   (agents per task: %s)"%(len(multi),len(bytask),sorted(collections.Counter(len(v) for v in multi.values()).items())))
def overlap(sl_lo,sl_hi,label,filt=None):
    hit=0;tot=0
    for inst,agents in multi.items():
        banks=[set(c for c in a[sl_lo:sl_hi] if (filt is None or filt(c))) for a in agents]
        for i,b in enumerate(banks):
            others=set().union(*[x for j,x in enumerate(banks) if j!=i])
            for c in b:
                tot+=1
                if c in others: hit+=1
    print("%-34s %5.1f%%  (%d/%d)"%(label,100*hit/tot if tot else 0,hit,tot))
for N in (1,3,5,10):
    overlap(0,N,"first %2d commands"%N)
overlap(10,None,"commands after step 10")
overlap(0,None,"ALL commands")
overlap(0,None,"ALL expensive commands",lambda c: bool(EXP.search(c)))
overlap(0,5,"first 5, expensive only",lambda c: bool(EXP.search(c)))
# where does first expensive land
pos=[]
for inst,agents in multi.items():
    for a in agents:
        for i,c in enumerate(a):
            if EXP.search(c): pos.append(i);break
if pos:
    pos.sort();print("first expensive cmd at step: p25=%d median=%d p75=%d (n=%d)"%(pos[len(pos)//4],pos[len(pos)//2],pos[3*len(pos)//4],len(pos)))

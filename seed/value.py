import json,glob,re,collections
R="/Users/tomjeong/hacker/skunk-works/notes/sealed-corpus/replay-A/records"
WS=re.compile(r"\s+")
# rough per-class cost model (seconds) for a BIG repo
COST={'install':45.0,'suite':90.0,'testfile':8.0,'build':25.0,'lint':6.0,'read':0.05,'other':0.3}
def klass(c):
    l=c.lower()
    if re.search(r'\b(pip install|uv sync|npm (i|install)|yarn|poetry install|go mod download|apt-get)\b',l): return 'install'
    if re.search(r'\b(cargo build|go build|make\b|tsc\b|webpack)',l): return 'build'
    if re.search(r'\b(pytest|go test|npm test|cargo test|tox)\b',l):
        return 'testfile' if re.search(r'\.py|::|-k ',l) else 'suite'
    if re.search(r'\b(ruff|eslint|mypy|flake8|black)\b',l): return 'lint'
    if re.search(r'^\s*(grep|rg|cat|ls|find|head|tail|wc|file|stat|which|tree)\b',l) or re.search(r'^\s*git (status|log|diff|show|branch)',l) or re.search(r'^\s*sed -n',l): return 'read'
    return 'other'
bytask=collections.defaultdict(list)
for f in glob.glob(R+"/*/*.json"):
    try: d=json.load(open(f))
    except: continue
    ev=d.get("evidence",{}) or {}
    steps=ev.get("steps") or []
    inst=(d.get("source",{}) or {}).get("instance_id")
    if not inst or not steps: continue
    cmds=[WS.sub(" ",s.get("cmd","").strip()) for s in steps if isinstance(s,dict) and s.get("cmd")]
    if cmds: bytask[inst].append(cmds)
multi={k:v for k,v in bytask.items() if len(v)>=2}
hits=collections.Counter(); tot=collections.Counter()
for inst,agents in multi.items():
    banks=[set(a) for a in agents]
    for i,b in enumerate(banks):
        others=set().union(*[x for j,x in enumerate(banks) if j!=i])
        for c in b:
            k=klass(c); tot[k]+=1
            if c in others: hits[k]+=1
print("%-10s %8s %8s %7s %12s %12s"%("class","hits","total","hit%","sec/cmd","SEC DELETED"))
tot_sec=0; rows=[]
for k in ['install','suite','build','testfile','lint','read','other']:
    if not tot[k]: continue
    sec=hits[k]*COST[k]; tot_sec+=sec
    rows.append((sec,k,hits[k],tot[k],100*hits[k]/tot[k],COST[k]))
for sec,k,h,t,pct,c in sorted(rows,reverse=True):
    print("%-10s %8d %8d %6.1f%% %11.2fs %11.0fs  %5.1f%%"%(k,h,t,pct,c,sec,100*sec/tot_sec))
print("\nTOTAL execution-seconds deleted (modelled): %.0fs across %d tasks"%(tot_sec,len(multi)))
exp=sum(s for s,k,_,_,_,_ in rows if k in('install','suite','build','testfile'))
print("expensive classes = %.1f%% of deleted seconds, %.1f%% of hits"%(100*exp/tot_sec,100*sum(hits[k] for k in('install','suite','build','testfile'))/sum(hits.values())))

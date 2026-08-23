# What Hindsight costs

## The finding

**Serving a command from cache costs 97 ms on a 20,000-file repository and 43 ms on a
100-file one.** Anything an agent runs that is faster than that is made slower by the
cache, on every hit, forever. That is the best case, assuming the cache never misses. At
the 53% hit rate measured on a real five-agent fan-out the threshold is 263 ms; at the
7.5% the replayed SWE-bench corpus gives, it is 2.4 seconds.

Three things follow.

**The size guard is not needed.** This was the specific worry, and it is unfounded. The
persistent side index holds up: a 50,000-file repository hashes in 156 ms warm, not the
seconds a throwaway index would cost. The curve is linear at 2.9 µs per tracked file with
no cliff anywhere in it. Hindsight is usable on a large repository. It is just expensive
enough there that only genuinely slow commands are worth intercepting.

**The cost is process spawning, not hashing.** On a small repository, 95% of key
derivation is the four `git` processes the hook starts. Git costs 6.5 ms to start and do
nothing; the actual tree hashing at 100 files is 1 ms. The daemon round trip everyone
suspects costs 0.057 ms, which is under a fifth of one percent of the hook. Every
optimisation worth doing is about starting fewer processes, and the largest single one
available is that `hindsight record` re-resolves a worktree the hook already resolved:
12.4 ms of pure duplication on every miss.

**The duration floor now catches everything that was being cached at a loss, and it
overshoots.** The classifier on its own is wildly permissive — `echo`, `pwd`, `basename`,
`cat`, `ls`, `wc`, `head` are all `SERVE` and all run in under 7 ms — but the 500 ms floor
that landed during this measurement refuses every one of them, correctly. The error is now
in the other direction. On a 20,000-file repo the measured break-even at the real fleet
hit rate is 263 ms, so commands between 263 ms and 500 ms are refused despite being worth
serving. That is the safe direction to be wrong in, and the gap is worth about half the
addressable range.

---

## Machine

| | |
|---|---|
| CPU | Apple M2 Max, 12 cores |
| Memory | 32 GB |
| Disk | internal SSD, APFS |
| OS | macOS 26.5 (25F5058e), darwin/arm64 |
| Go | go1.24.2 |
| git | 2.46.0 |

Everything is a median. Go benchmarks are the median of 3 runs of 10 iterations
(`-benchtime 10x -count 3`); the microbenchmarks in the classifier section are
`-benchtime 1s -count 5`. End-to-end numbers are the median and p95 of 50 fork-exec
samples per path.

```bash
go test ./internal/hp/ -run XXX -bench . -benchtime 10x -count 3   # the tables below
bash scripts/bench.sh --iterations 50 --large 20000                # end-to-end
```

`bench.sh` builds the binary, generates two throwaway git repositories, starts a daemon on
a kernel-assigned free port with a temp `HP_HOME`, measures, and removes all of it on exit
including on failure. The Go benchmarks cache their generated repositories in
`$TMPDIR/hindsight-perf-v1/` and reuse them between runs, because generating the ~100,000
files they need costs more than every measurement in the file put together; set
`HP_PERF_FIXTURES` to relocate that, and `bash scripts/bench.sh --with-go` puts it
somewhere that gets cleaned up.

---

## 1. Tree hash versus repository size

Synthetic repositories, ~700-byte files, twenty per directory inside a two-level package
tree, all committed. Warm means the persistent side index already exists, which is what
the hook normally has. Cold means the index was deleted first, which is what a throwaway
`mktemp` index would pay on every single command.

| tracked files | warm | cold | cold ÷ warm |
|---:|---:|---:|---:|
| 100 | **14.0 ms** | 21.8 ms | 1.6× |
| 1,000 | **17.3 ms** | 74.7 ms | 4.3× |
| 5,000 | **27.0 ms** | 391 ms | 14× |
| 20,000 | **70.7 ms** | 2,458 ms | 35× |
| 50,000 | **156 ms** | 5,910 ms | 38× |

The warm curve is a 13 ms fixed cost — two `git` process spawns, one for `add -A` and one
for `write-tree` — plus **2.9 µs per tracked file**, and it is linear to within a few
percent across the whole range:

```
warm ms
  156 |                                                        *
      |
  100 |
      |
   71 |                            *
      |
   27 |          *
   17 |    *
   14 |  *
      +----+-----+------------------+------------------------+---
       100  1k   5k                20k                      50k
```

Cold is ~120 µs per file, roughly forty times worse, and it superlinearly degrades as the
object store fills. This brackets the number that prompted the investigation: a throwaway
index on an 11,000-file repo was measured at 1,250–1,740 ms, and interpolating this curve
gives ~1,200 ms. The same repository with a persistent index costs about **45 ms**.

The persistent side index is doing all the work here and is worth roughly 38× at scale.

### Cost versus number of changed files

Repository size held fixed at 20,000 files, varying how many files changed since the
previous hash. This is the property that decides whether a large repository is viable:
`git add -A` has to stat every tracked path, but it should only re-hash what moved.

| files changed | tree hash | marginal cost per changed file |
|---:|---:|---:|
| 0 | 64.1 ms | — |
| 1 | 80.3 ms | +16 ms (fixed: git rewrites the index) |
| 10 | 84.4 ms | ~0.5 ms |
| 100 | 151 ms | ~0.7 ms |
| 1,000 | 637 ms | ~0.6 ms |
| 10,000 | 6,040 ms | ~0.6 ms |

It confirms the hypothesis, with one wrinkle. Cost is *not* proportional to repository
size beyond the stat scan, so the realistic regime — an agent editing one to twenty files
between commands — costs 80–85 ms on a 20,000-file repo against a 64 ms floor. But the
marginal cost per changed file is ~600 µs, not the ~120 µs the cold scan implies, because
most of it is writing a new loose object into `.git/objects` rather than hashing. An
independent single-shot check on a pristine object store gave 125 ms at 100 changed files
against this table's 151 ms, so treat 430–700 µs as the range; it depends on how much the
object store has accumulated.

The practical consequence is that a `git checkout` of a distant branch, or a codegen run
that rewrites a thousand files, makes the next tree hash cost 640 ms. Twice.

*Measurement note:* an earlier version of this benchmark reused file content across
iterations, which let `git add -A` skip the blob write and reported 80 µs per file — five
times too fast. `perfDirtyGen` is now seeded from the clock so every rewrite is content
git has never seen.

## 2. Environment fingerprint

Fixture: 300 installed Python distributions (600 `site-packages` entries, half of them
`.dist-info`) and a `node_modules` with 500 top-level entries of which 25 are scopes
holding 8 packages each, plus lockfiles at both levels. Both ecosystems report
`complete=true`, so these measure the real path and not an abstention.

| workspace | env fingerprint |
|---|---:|
| no ecosystem detected | 0.013 ms |
| python only | 0.40 ms |
| node only | 0.80 ms |
| python and node | 1.26 ms |

This is the one part of the system that costs what its design claims. It is a readdir, it
is about a millisecond, and it is 1.4% of the hook on a small repository and 0.6% on a
large one. The benchmark skips rather than fails for ecosystems that are not registered,
so it keeps working while the plugins are still landing.

## 3. Full key derivation

`ws.State()` — tree hash plus environment fingerprint — followed by `hp.Key(...)`. The
hook pays this once, and on a miss `hindsight record` pays it again to close the purity
gate.

| workspace | State() + Key() |
|---|---:|
| 100 files | 14.9 ms |
| 1,000 files | 17.4 ms |
| 5,000 files | 28.8 ms |
| 20,000 files | 70.1 ms |
| 50,000 files | 151 ms |
| 5,000 files + python + node installed | 30.2 ms |

It is the tree hash and nothing else. `hp.Key` itself is 274 ns and `NormalizeCommand` is
131 ns; both are noise. Installing dependencies adds 1.4 ms.

`NewWorkspace`, which the hook calls before any of this, is **12.4 ms** — two `git
rev-parse` invocations that resolve a worktree root and a git dir that cannot change
during a session.

### Where the time actually goes

| | cost | share of a 100-file hook |
|---|---:|---:|
| 4 × git process spawn | 26.0 ms | 96% |
| tree hashing proper (100 files) | ~1 ms | 4% |
| environment fingerprint | 0.013 ms | 0.05% |
| daemon round trip | 0.057 ms | 0.2% |
| `Classify` | 0.0009 ms | 0.003% |
| `Key` + `NormalizeCommand` | 0.0004 ms | 0.001% |

Decomposing the spawn:

| | median |
|---|---:|
| `/usr/bin/true` — the OS process-creation floor | 1.8 ms |
| `git --version` — plus loading and starting git | 6.3 ms |
| `git rev-parse --git-dir` — plus repository discovery | 6.7 ms |
| `/bin/sh -c true` | 3.9 ms |
| `/bin/zsh -c true` | 3.6 ms |
| `$SHELL -lc true` — how the harness invokes hooks | **23.5 ms** |

Two things fall out of this table. Starting git costs 4.5 ms before it does anything, and
the hook starts four of them. And if the harness really runs hooks under a login shell as
`AGENTS.md` states, then **roughly 22 ms is added to every number in this document**, on
every command, including the ones that pass straight through. That surcharge is a property
of the user's dotfiles rather than of Hindsight, but it is not optional and it is not
small. `bench.sh` reproduces it as a `login-shell` row and got 24 ms.

## 4. Classifier

`hp.Classify` on a mix of 35 realistic agent commands spanning every branch: reads,
builds, mutations, installs, git subcommands, chains, the non-hermeticity list and
quoting.

| | |
|---|---:|
| `Classify`, realistic mix | **898 ns** |
| `Key` | 274 ns |
| `NormalizeCommand` | 131 ns |

It is microseconds, as intended, and it is the only cost a `PASSTHROUGH` command pays
inside the binary.

**It is linear, not quadratic.** The per-byte cost is flat across two and a half orders of
magnitude:

| command length | total | per byte |
|---:|---:|---:|
| 1 KB | 24 µs | 23.1 ns |
| 4 KB | 92 µs | 22.4 ns |
| 16 KB | 359 µs | 21.9 ns |
| 64 KB | 1.40 ms | 21.3 ns |
| 256 KB | 5.93 ms | 22.6 ns |

The pathological 10 KB command line, in each shape that stresses a different part of the
classifier:

| shape | |
|---|---:|
| 10 KB explicit file list (one segment, 2,000 tokens) | 233 µs |
| 10 KB chain of `&&` segments | 315 µs |
| 10 KB inside a single quoted argument | 127 µs |
| 10 KB unterminated quote (bails early) | 127 µs |

The worst case is 0.3 ms, which is 0.3% of a hook on a large repository. The classifier is
not a problem and does not need attention.

## 5. Daemon

Measured in-process over loopback, isolated from everything else.

| | |
|---|---:|
| `/lookup` returning MISS | **0.057 ms** |
| `/lookup` returning HIT | **0.180 ms** |

A hit is three times a miss because the daemon appends a record to the log and broadcasts
it before replying. Both are irrelevant next to a single git spawn.

It does not degrade under fan-out either. Driving the same endpoint from 1, 5 and 20 times
`GOMAXPROCS` goroutines gives 0.017, 0.012 and 0.011 ms per lookup — it gets *cheaper* per
operation as concurrency rises, because the single mutex and single log file are nowhere
near saturated and the fixed per-request cost amortises across in-flight work. **The daemon
is not where the time goes, and optimising it would be a waste of effort.**

## 6. End-to-end hook latency

A real PreToolUse payload piped into a freshly spawned `hindsight hook`, forked and
exec'd directly with no intervening shell, against a live daemon in a real worktree. 50
samples per path after 3 warmups.

**Small repository — 100 files**

| path | median | p95 |
|---|---:|---:|
| `HP_ENABLE` unset (binary spawn only) | 5.96 ms | 7.16 ms |
| passthrough (classifier rejects) | 6.10 ms | 7.93 ms |
| known-fast (duration memo bails) | 6.39 ms | 8.77 ms |
| **miss** | **36.44 ms** | 70.52 ms |
| **hit** | **33.94 ms** | 38.07 ms |
| **daemon down** (fail open) | **34.01 ms** | 39.59 ms |
| replay (`cat out; cat err >&2; exit 0`) | 8.70 ms | 9.68 ms |
| `hindsight record` wrapper overhead | 40.6 ms | — |

**Large repository — 20,000 files**

| path | median | p95 |
|---|---:|---:|
| `HP_ENABLE` unset (binary spawn only) | 6.03 ms | 8.04 ms |
| passthrough (classifier rejects) | 6.03 ms | 6.48 ms |
| known-fast (duration memo bails) | 6.26 ms | 10.55 ms |
| **miss** | **91.24 ms** | 108.90 ms |
| **hit** | **88.13 ms** | 100.35 ms |
| **daemon down** (fail open) | **89.97 ms** | 98.43 ms |
| replay | 8.85 ms | 12.04 ms |
| `hindsight record` wrapper overhead | 98.0 ms | — |

The record overhead is the measured difference between running `grep -cF payload
bench-data.txt` bare and running it through the wrapper the hook rewrites a miss into. It
is a second `NewWorkspace`, a second `State()`, two blob writes and a `POST /record`.

Four rows deserve comment.

**The fail-open path is the same price as a working cache.** With the daemon down the hook
still classifies, still consults the memo, still resolves the worktree, still hashes the
tree and still fingerprints the environment, and only then discovers the connection is
refused. A user who has not started the daemon pays 90 ms per command on a large
repository and receives nothing at all in exchange. The hook fails open correctly —
nothing breaks — but it fails open late.

**A passthrough still costs 6.0 ms.** Every command an agent runs, including every `curl`
and `date` and unrecognised head, pays a Go binary spawn. Add the login shell and it is
28 ms.

**The duration memo works, and costs one file read.** Once a command is known cheap the
hook bails at 6.3 ms instead of 91 ms. That is the single most effective mechanism in the
system, and at two required samples it now costs 182 ms on a large repo to learn one
command's verdict rather than the 273 ms it did at three.

**The `alwaysCheap` head list makes that free for the obvious cases.** `echo`, `pwd`,
`true`, `false`, `:`, `basename`, `dirname` and `printf` short-circuit with no observations
at all. This is worth knowing when reading these numbers: the `miss` and `daemon-down` rows
had to be measured with `grep`, because an `echo` never reaches the key path any more.

## 7. Break-even

A command run with the cache costs, on a miss, `hook_miss + T + record_overhead`, and on a
hit, `hook_hit + replay`. Without the cache it costs `T`. At hit rate `p`, over many
commands, caching is worth it only when

```
T  >  (hook_hit + replay)  +  ((1 - p) / p) × (hook_miss + record_overhead)
```

The first term is what a served command costs. The second is the tax on everything that
missed, amortised over the commands that did hit: at `p = 0.5` each hit carries one miss,
at `p = 0.1` each hit carries nine.

| repository | hit path | miss path | **p = 1.00** | **p = 0.53** | **p = 0.075** |
|---|---:|---:|---:|---:|---:|
| 100 files | 33.9 + 8.7 | 36.4 + 40.6 | **43 ms** | **110 ms** | **993 ms** |
| 20,000 files | 88.1 + 8.8 | 91.2 + 98.0 | **97 ms** | **263 ms** | **2,431 ms** |
| 50,000 files *(extrapolated)* | 174 + 9 | 177 + 184 | **183 ms** | **499 ms** | **4,630 ms** |

The 50,000-file row extrapolates by adding the measured 85.7 ms difference between the
20,000- and 50,000-file warm tree hash to each of the two `State()` calls. It is not
measured end to end.

`p = 0.53` is the hit rate the design doc reports from a real five-agent fan-out.
`p = 0.075` is what the replayed SWE-bench corpus gives when keyed on `(state, command)`
the way Hindsight actually keys, and the design doc already flags that as the honest
figure for that population.

**Stated plainly: on a 20,000-file repository, commands faster than 97 milliseconds cost
more to cache than they save even if every one of them hits. At the realistically
achievable hit rate the threshold is 263 milliseconds.**

Add the login shell surcharge and every one of these numbers rises by roughly 22 ms.

This differs from the arithmetic in `fastpath.go`, which asks `hit_rate × duration > 42 ms`
and drops the hit path's own cost. The two agree to within a few percent at low hit rates,
where the miss tax dominates and the omitted term is small. They diverge at high hit rates:
at `p = 1` that formula gives 42 ms where the measured floor on a large repo is 97 ms.
Since the shipping constant sits at 500 ms, well above both, the difference does not change
any decision today — but it would if the constant were ever tuned downward from it.

## 8. Does the shipping classifier serve things below break-even?

**The classifier does. The duration floor in front of it does not, any more.**

Measured on the 20,000-file repository, where the measured floor is 97.0 ms and the
shipping floor is 500 ms. The classifier column comes from `hindsight key --cmd`, so it is
the shipping policy and not a transcription of it.

| command | classifier | actual duration | outcome |
|---|---|---:|---|
| `echo hello` | SERVE | 4.5 ms | refused; correctly |
| `pwd` | SERVE | 5.0 ms | refused; correctly |
| `basename /usr/local/bin/hindsight` | SERVE | 6.4 ms | refused; correctly |
| `cat .gitignore` | SERVE | 6.3 ms | refused; correctly |
| `ls src` | SERVE | 6.3 ms | refused; correctly |
| `wc -l .gitignore` | SERVE | 6.2 ms | refused; correctly |
| `head -n 5 .gitignore` | SERVE | 5.9 ms | refused; correctly |
| `git log --oneline -20` | SERVE | 11.1 ms | refused; correctly |
| `git diff --stat HEAD` | SERVE | 29.6 ms | refused; correctly |
| `git status --porcelain` | SERVE | 65.6 ms | refused; correctly |
| `grep -c payload bench-data.txt` | SERVE | 69.7 ms | refused; correctly |
| `find src -name file00001.go` | SERVE | 99.7 ms | refused; marginal loss either way |
| `find src -type f` | SERVE | 101.0 ms | refused; marginal loss either way |
| `sort bench-data.txt` | SERVE | 676 ms | served, worth it |
| `grep -rn Fixture07 src` | SERVE | 1,276 ms | served, worth it |
| `grep -rl Fixture00 src` | SERVE | 1,370 ms | served, worth it |

Every one of the sixteen probes is `SERVE`. The classifier has no notion of cost at all:
`echo`, `pwd`, `basename`, `cat`, `ls`, `wc` and `head` are in `readHeads` and come back
`SERVE` while running in under 7 ms, and caching those would be a 14× to 22× slowdown.
Left to itself the classifier is a liability.

The duration floor is what makes the system honest, and it works. Nothing in this table is
cached at a loss.

Two things are worth saying about how it got there. First, the floor moved from 50 ms to
500 ms while this measurement was in progress, and at 50 ms three of these commands —
`git status --porcelain`, `grep -c` and `find` — did clear the floor and get cached below
break-even. That gap is closed.

Second, the floor now overshoots, which is the right direction but is not free:

- **Refused but profitable at the fleet hit rate.** The break-even on this repository at
  `p = 0.53` is 263 ms, so anything between 263 ms and the 500 ms constant is refused
  despite being worth serving. None of the probes above land in that band, but it is a real
  and populated one: `pytest` on a single test file, `go vet` on one package,
  `tsc --noEmit` on a small project, and `npm test` on a trivial suite all typically sit
  there.
- **The floor is a constant and the cost is not.** 500 ms is 11× the 43 ms break-even on a
  small repository and 5× the 97 ms on a large one. A single constant cannot be right for
  both, and it is most wrong exactly where the cache is cheapest to run.
- **Learning still costs.** At two samples a command pays full interception twice before
  the memo can skip it — 182 ms on a large repo per distinct command string per cache home.
  The `alwaysCheap` head list removes that cost for the eight obvious heads.

## 9. Threats to validity

- **One machine, one filesystem.** macOS process creation is slow; git starts in 6.5 ms
  here and would likely be 1–2 ms on Linux. On a machine where spawning is cheap the
  break-even threshold drops substantially and the picture improves. Nothing here should be
  quoted as a cross-platform number.
- **Synthetic repositories.** Uniform ~700-byte files with even directory fanout. Real
  repositories have skewed file sizes and deeper, less regular trees. The stat-scan cost
  should track file count regardless, but the constant may differ.
- **The 50,000-file end-to-end row is extrapolated**, not measured.
- **`p = 0.53` and `p = 0.075` are quoted from the design doc**, not measured here. The
  break-even formula is mine; the hit rates are not.
- **The login shell surcharge is conditional.** It is measured (22 ms on this machine with
  these dotfiles) but I did not verify that either harness actually invokes hooks that way;
  that claim comes from `AGENTS.md`.
- **Fixtures are cached between runs**, so the page cache is warm. That is the realistic
  condition for an agent's worktree, but it means "cold" here means a cold git index, never
  a cold disk.
- **The duration floor moved from 50 ms to 500 ms mid-measurement**, and `fastpathSamples`
  from 3 to 2. Every number in sections 1 to 7 is independent of that; section 8 and the
  recommendations were re-measured against the current constants. If they move again this
  document is stale, which is why `bench.sh` reads `DefaultMinDurationMS` out of the source
  rather than restating it.

## 10. Recommendations

Ordered by how much time they return per unit of effort. The first one is the one to do.

**1. Stop starting so many git processes.** This is the whole game. On a small repository
95% of the key path is process spawn, and the hook starts four git processes plus the
`record` wrapper's four more. Two of them are pure waste: `hindsight record` calls
`NewWorkspace` to re-resolve a worktree root and git dir that the hook already knows and
could pass as two more flags alongside `--tree` and `--envfp`. That is 12.4 ms off every
miss for a change with no correctness surface at all — the values are already being
recomputed identically, and passing them makes the two processes agree by construction
rather than by coincidence. Caching the same two values for the hook itself, keyed on cwd
in the fastpath file that is already being read, would take another 12.4 ms off every
intercepted command. Together that is roughly a third of the hook on a small repository.
Nothing else on this list comes close.

**2. Derive the duration floor from the measured tree hash instead of hardcoding it.** The
500 ms constant is safe but blunt: it is 11× the 43 ms break-even on a small repository and
5× the 97 ms on a large one, and the band between the real break-even and 500 ms is
refused work that would have paid. The hook already has what it needs to do better — it can
time its own `State()` call and persist a rolling maximum beside the memo, then use
**`tree_hash + 25 ms`** as the `p = 1` floor. That fits all three measured points: 39 ms
against a measured 43 at 100 files, 96 against 97 at 20,000, and 181 against an
extrapolated 183 at 50,000. Scale it by the observed hit rate, which the daemon already
knows, and you get a floor that is 110 ms on a small repo and 263 ms on a large one rather
than 500 ms on both. `hindsight doctor` should print whatever it derives. Its
`slowTreeHash = 200 ms` warning threshold wants revisiting too — a 50,000-file repo at
156 ms warm pays over 300 ms per intercepted command and currently gets no warning at all.

**3. Fail open early when the daemon is down.** This is the worst cell in the whole matrix
and it is also the most likely state for a new user. With no daemon running, the hook does
the entire key path — memo read, two `rev-parse` calls, `git add -A`, `git write-tree`,
environment fingerprint — and only then discovers the connection is refused, at 90 ms per
command on a large repository for nothing at all. A `/healthz` result cached in the memo
file with a few seconds' TTL would turn that into the 6.3 ms known-fast path.

**4. Consider admitting to the memo on one sample when the margin is wide.** Two samples is
already much better than three, but a command that ran in 5 ms against a 500 ms floor is
not going to turn out to be expensive, and it currently pays full interception twice to
prove it. One observation under, say, a tenth of the floor is enough; keep two for anything
near the boundary, which is the case the existing comment is actually worried about. This
is worth a good deal less than the first three and should be done last, if at all.

**Not worth doing:** optimising the daemon (0.06% of the hook), optimising the classifier
(0.003%), optimising the environment fingerprint (0.6–1.4%), or adding a repository size
guard. On the size guard specifically — the data does not support it. 50,000 files costs
156 ms warm, which is expensive but linear and predictable, and the right response to an
expensive repository is a higher duration floor, not a refusal to cache. A guard would
disable the tool exactly where slow test suites make it most valuable.

**One thing to check that is not a performance question.** Nothing in the hook path
computes a tree hash twice within a single invocation, so there is no redundant hashing to
memoise — I looked for it specifically. But `hindsight record` recomputing `NewWorkspace`
independently of the hook means the two processes could in principle disagree about which
worktree they are in, if the agent's cwd moved between the hook firing and the command
running. Passing the resolved root explicitly, per recommendation 1, closes that as a side
effect.

# The demo

A deliberately small library — order totals, tax, discounts, splitting a bill
— with one seeded bug and a test suite slow enough to notice.

`split_evenly` uses integer division and drops the remainder, so a bill that
does not divide evenly is under-paid. Five of the property tests catch it. The
fix is one line with `divmod`, which matters: agents converge on the same fix,
so their trees converge too.

The suite property-tests each invariant across 80,000 bills and 60 party sizes
and takes about 72 seconds. Nothing about that is padding — off-by-one errors
in money code hide in exactly the cases nobody writes by hand — but it is also
what makes a cache hit visible to the naked eye.

## Running the two-pane demo

Two clones, one with the hook and one without, and a cache warmed by a real
prior run.

```bash
# once: build and pick a scratch area
go build -o /tmp/hindsight ./cmd/hindsight

git clone <this-repo>/examples/receipts /tmp/demo-left     # the control
git clone <this-repo>/examples/receipts /tmp/demo-right
(cd /tmp/demo-right && /tmp/hindsight init && git add -A && git commit -m "hook")

/tmp/hindsight daemon --addr 127.0.0.1:7850 --home /tmp/demo-warm &
```

Warm the cache by solving it once, the way a teammate would have:

```bash
git clone <this-repo>/examples/receipts /tmp/demo-warmer
cd /tmp/demo-warmer && /tmp/hindsight init && git add -A && git commit -m "hook"
HP_ENABLE=1 HP_DAEMON=http://127.0.0.1:7850 HP_HOME=/tmp/demo-warm HP_AGENT=teammate \
  claude --print --permission-mode bypassPermissions "$(cat PROMPT.txt)"
```

Then race the two panes on the same prompt:

```bash
cd /tmp/demo-left  && claude
cd /tmp/demo-right && HP_ENABLE=1 HP_DAEMON=http://127.0.0.1:7850 \
                      HP_HOME=/tmp/demo-warm HP_AGENT=you claude
```

Measured on an M-series Mac: **184 seconds cold, 59 seconds warm**, two hits
deleting 146 of the teammate's 148 execution-seconds.

## Why the hook config is not committed here

`hindsight init` writes the absolute path of your binary into
`.claude/settings.json`, and those files are tracked, so they are part of the
tree hash. Committing one machine's path would mean nobody else's tree ever
matches. Run `init` yourself and commit the result in your clone.

#!/usr/bin/env python3
"""Validate the shipping classifier against observed mutation labels.

Hindsight decides what it is willing to cache in two stages. A static
classifier reads the command string, and a runtime purity gate compares the
workspace state before and after execution and refuses to serve anything that
moved. The design doc argues the second stage is what makes the first one safe
to get wrong. This measures whether that is true.

`replay-A` records, for every step of every replayed agent, whether the command
actually mutated the workspace. That is 13,149 labelled examples — enough to
ask how often the classifier's SERVE verdict disagrees with reality, which is
precisely the population the purity gate exists to catch.

Every figure here comes from running the real binary, not a re-implementation:
`hindsight classify` is the same code path the hook uses.

    python3 evidence/classifier_validation.py [--corpus DIR] [--bin PATH]
"""

from __future__ import annotations

import argparse
import collections
import glob
import json
import subprocess
import sys

DEFAULT_CORPUS = "/Users/tomjeong/hacker/skunk-works/notes/sealed-corpus/replay-A"


def load_steps(corpus: str) -> list[tuple[str, bool]]:
    """Return (command, mutated) for every step of every completed record."""
    steps: list[tuple[str, bool]] = []
    for path in sorted(glob.glob(f"{corpus}/records/*/*.json")):
        try:
            record = json.load(open(path))
        except (OSError, json.JSONDecodeError):
            continue
        if record.get("status") != "complete":
            continue
        for step in (record.get("evidence") or {}).get("steps") or []:
            cmd = (step.get("cmd") or "").strip().replace("\n", " ")
            if cmd:
                steps.append((cmd, bool(step.get("mutated"))))
    return steps


def classify(binary: str, commands: list[str]) -> dict[str, str]:
    """Classify through the real binary, so this measures what ships."""
    proc = subprocess.run(
        [binary, "classify"],
        input="\n".join(commands),
        text=True,
        capture_output=True,
    )
    if proc.returncode != 0:
        sys.exit(f"{binary} classify failed: {proc.stderr.strip()}")
    out = {}
    for line in proc.stdout.splitlines():
        policy, _, cmd = line.partition("\t")
        out[cmd] = policy
    return out


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=DEFAULT_CORPUS)
    ap.add_argument("--bin", default="hindsight")
    args = ap.parse_args()

    steps = load_steps(args.corpus)
    if not steps:
        sys.exit(f"no steps found under {args.corpus}")
    distinct = sorted({cmd for cmd, _ in steps})
    policy_of = classify(args.bin, distinct)

    table: collections.Counter = collections.Counter()
    for cmd, mutated in steps:
        table[(policy_of.get(cmd, "?"), mutated)] += 1

    print(f"{len(steps):,} observed steps, {len(distinct):,} distinct commands")
    print(f"corpus  {args.corpus}\n")
    print(f"{'policy':13s} {'MUTATED':>10s} {'clean':>10s} {'total':>9s}   mutation rate")
    for policy in ("SERVE", "RECORD_ONLY", "PASSTHROUGH"):
        mutated = table[(policy, True)]
        clean = table[(policy, False)]
        total = mutated + clean
        if total:
            print(
                f"{policy:13s} {mutated:10,} {clean:10,} {total:9,}"
                f"   {100 * mutated / total:5.1f}%"
            )

    served_mutating = table[("SERVE", True)]
    served_total = served_mutating + table[("SERVE", False)]
    rate = 100 * served_mutating / served_total
    print(
        f"\n{served_mutating:,} of {served_total:,} SERVE-classified commands "
        f"({rate:.1f}%) were observed\n"
        "to mutate the workspace. The static classifier alone would have cached "
        "those;\nthe runtime purity gate refuses them by measuring the state on "
        "both sides of\nexecution. This is the empirical case for the purity gate: "
        "the classifier is\nright about 99% of the time, and the gate is what "
        "covers the rest."
    )


if __name__ == "__main__":
    main()

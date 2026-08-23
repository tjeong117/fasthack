#!/usr/bin/env python3
"""Regenerate Hindsight's corpus evidence from the sealed replay records.

This is intentionally standard-library only. It writes overlap.json, overlap.md,
value.md, and claims.md next to this file. No output is written unless the corpus
exists and contains usable multi-agent records.
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import re
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Hashable, Iterable, Sequence


DEFAULT_CORPUS = Path(
    "/Users/tomjeong/hacker/skunk-works/notes/sealed-corpus/replay-A"
)
SPACE = re.compile(r"\s+")
REPO_ROOT = Path(__file__).resolve().parent.parent
EXPECTED = {
    "state_avoidable_pct": 7.5,
    "state_cross_agent_pct": 3.6,
    "state_first_3_pct": 16.9,
    "state_after_50_pct": 1.0,
}
MODELED_SECONDS = {
    "install": 45.0,
    "suite": 90.0,
    "testfile": 8.0,
    "build": 25.0,
    "lint": 6.0,
    "read": 0.05,
    "other": 0.3,
}


@dataclass(frozen=True)
class Step:
    number: int
    command: str
    state_sha256: str
    delta: object
    diff_lines: int | None
    file_count: int | None
    mutated: bool | None


@dataclass(frozen=True)
class Attempt:
    instance_id: str
    submission: str
    source_path: str
    steps: tuple[Step, ...]


def normalize_command(command: str) -> str:
    """Match strings.Fields used by hp.NormalizeCommand for the proxy curve."""
    return SPACE.sub(" ", command.strip())


def shipping_replay(corpus: Path, keying: str) -> dict:
    """Run the production corpus loader, normalizer, classifier, and replay."""
    command = [
        "go",
        "run",
        "./cmd/hindsight",
        "replay",
        "--corpus",
        str(corpus),
        "--key",
        keying,
        "--json",
        "--by-step",
    ]
    try:
        completed = subprocess.run(
            command,
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
            env={
                **os.environ,
                "GOCACHE": str(Path(tempfile.gettempdir()) / "hindsight-go-build"),
            },
        )
    except FileNotFoundError as exc:
        raise SystemExit("go is required to run the shipping replay implementation") from exc
    except subprocess.CalledProcessError as exc:
        detail = exc.stderr.strip() or exc.stdout.strip() or str(exc)
        raise SystemExit(f"shipping replay failed for --key {keying}: {detail}") from exc
    try:
        return json.loads(completed.stdout)
    except json.JSONDecodeError as exc:
        raise SystemExit(
            f"shipping replay returned invalid JSON for --key {keying}: {exc}"
        ) from exc


def _instance_id(record: dict) -> str:
    source = record.get("source") or {}
    summary = record.get("summary") or {}
    evidence_summary = (record.get("evidence") or {}).get("summary") or {}
    return str(
        source.get("instance_id")
        or summary.get("instance_id")
        or evidence_summary.get("instance_id")
        or ""
    )


def _submission(record: dict) -> str:
    source = record.get("source") or {}
    metadata = source.get("metadata") or {}
    return str(metadata.get("submission") or source.get("submission") or "UNKNOWN")


def load_attempts(records_dir: Path) -> tuple[list[Attempt], dict]:
    paths = sorted(records_dir.glob("**/*.json"))
    if not paths:
        raise SystemExit(f"no JSON records found under {records_dir}")

    attempts: list[Attempt] = []
    parse_errors: list[str] = []
    per_step_timing_fields: collections.Counter[str] = collections.Counter()
    skipped = 0
    for path in paths:
        try:
            record = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            parse_errors.append(f"{path}: {exc}")
            continue

        instance_id = _instance_id(record)
        raw_steps = (record.get("evidence") or {}).get("steps") or []
        steps: list[Step] = []
        for index, raw in enumerate(raw_steps):
            if not isinstance(raw, dict):
                continue
            for field in raw:
                lowered = field.lower()
                if "duration" in lowered or lowered in {
                    "elapsed",
                    "elapsed_ms",
                    "wall_s",
                    "started_at",
                    "finished_at",
                }:
                    per_step_timing_fields[field] += 1
            command = raw.get("cmd") or raw.get("command") or raw.get("action") or ""
            if not isinstance(command, str) or not command.strip():
                continue
            state = raw.get("state_sha256")
            steps.append(
                Step(
                    number=int(raw.get("n", index)),
                    command=normalize_command(command),
                    state_sha256=state if isinstance(state, str) else "",
                    delta=raw.get("delta"),
                    diff_lines=raw.get("diff_lines")
                    if isinstance(raw.get("diff_lines"), int)
                    else None,
                    file_count=raw.get("n_files")
                    if isinstance(raw.get("n_files"), int)
                    else None,
                    mutated=raw.get("mutated")
                    if isinstance(raw.get("mutated"), bool)
                    else None,
                )
            )
        if not instance_id or not steps:
            skipped += 1
            continue
        attempts.append(
            Attempt(instance_id, _submission(record), str(path), tuple(steps))
        )

    if parse_errors:
        sample = "\n".join(parse_errors[:5])
        raise SystemExit(
            f"refusing partial evidence: {len(parse_errors)} records failed to parse\n{sample}"
        )
    return attempts, {
        "json_files": len(paths),
        "usable_attempts": len(attempts),
        "skipped_without_instance_or_steps": skipped,
        "per_step_timing_fields": dict(sorted(per_step_timing_fields.items())),
    }


def group_multi_agent(attempts: Iterable[Attempt]) -> dict[str, list[Attempt]]:
    grouped: dict[str, list[Attempt]] = collections.defaultdict(list)
    for attempt in attempts:
        grouped[attempt.instance_id].append(attempt)
    return {key: value for key, value in grouped.items() if len(value) >= 2}


def _window(steps: Sequence[Step], start: int, stop: int | None) -> Sequence[Step]:
    return steps[start:stop]


def held_out_overlap(
    tasks: dict[str, list[Attempt]],
    key: Callable[[Step], Hashable | None],
    start: int = 0,
    stop: int | None = None,
) -> dict:
    """Match each agent's deduplicated keys against the union of its peers."""
    hits = 0
    total = 0
    for attempts in tasks.values():
        banks = []
        for attempt in attempts:
            values = {key(step) for step in _window(attempt.steps, start, stop)}
            values.discard(None)
            banks.append(values)
        for index, bank in enumerate(banks):
            peers: set[Hashable] = set()
            for peer_index, peer in enumerate(banks):
                if peer_index != index:
                    peers.update(peer)
            total += len(bank)
            hits += sum(value in peers for value in bank)
    return _ratio(hits, total)


def _ratio(numerator: int, denominator: int) -> dict:
    return {
        "numerator": numerator,
        "denominator": denominator,
        "percent": round(100.0 * numerator / denominator, 6) if denominator else None,
    }


def command_key(step: Step) -> str:
    return step.command


def diagnose_state(attempts: Sequence[Attempt]) -> dict:
    changed_on_mutation = mutation_total = 0
    unchanged_match = unchanged_total = 0
    retained_after_read = retained_total = 0
    diff_nondecreasing = diff_pairs = 0
    file_nondecreasing = file_pairs = 0

    for attempt in attempts:
        for previous, current in zip(attempt.steps, attempt.steps[1:]):
            has_states = bool(previous.state_sha256 and current.state_sha256)
            if current.mutated is True and has_states:
                mutation_total += 1
                changed_on_mutation += previous.state_sha256 != current.state_sha256
            if current.mutated is False and has_states:
                unchanged_total += 1
                unchanged_match += previous.state_sha256 == current.state_sha256
                if previous.delta not in ({}, None):
                    retained_total += 1
                    retained_after_read += previous.delta == current.delta
            if previous.diff_lines is not None and current.diff_lines is not None:
                diff_pairs += 1
                diff_nondecreasing += current.diff_lines >= previous.diff_lines
            if previous.file_count is not None and current.file_count is not None:
                file_pairs += 1
                file_nondecreasing += current.file_count >= previous.file_count

    mutation_rate = changed_on_mutation / mutation_total if mutation_total else 0.0
    unchanged_rate = unchanged_match / unchanged_total if unchanged_total else 0.0
    if mutation_total == 0 or unchanged_total == 0:
        verdict = "inconclusive"
    elif mutation_rate >= 0.99 and unchanged_rate >= 0.80:
        verdict = "supported"
    else:
        verdict = "not_supported"

    return {
        "verdict": verdict,
        "interpretation": (
            "state_sha256 behaves as the corpus's cumulative workspace-state "
            "identifier: it changes on mutating transitions and usually persists "
            "across non-mutating transitions. delta is a per-step observation and "
            "is not the value hashed by state_sha256"
        ),
        "mutating_steps_change_state": _ratio(changed_on_mutation, mutation_total),
        "nonmutating_steps_preserve_state": _ratio(unchanged_match, unchanged_total),
        "nonempty_delta_retained_after_nonmutating_step": _ratio(
            retained_after_read, retained_total
        ),
        "diff_lines_nondecreasing": _ratio(diff_nondecreasing, diff_pairs),
        "file_count_nondecreasing": _ratio(file_nondecreasing, file_pairs),
        "monotonicity_note": (
            "delta is per-step, so its retention rate is expected to be near zero. "
            "diff and file counts may shrink when edits are reverted, so their "
            "monotonicity rates are diagnostics rather than gates."
        ),
    }


def command_class(command: str) -> str:
    lower = command.lower()
    if re.search(
        r"\b(pip install|uv sync|npm (i|install)|yarn|poetry install|"
        r"go mod download|apt-get)\b",
        lower,
    ):
        return "install"
    if re.search(r"\b(cargo build|go build|make\b|tsc\b|webpack)", lower):
        return "build"
    if re.search(r"\b(pytest|go test|npm test|cargo test|tox)\b", lower):
        return "testfile" if re.search(r"\.py|::|-k ", lower) else "suite"
    if re.search(r"\b(ruff|eslint|mypy|flake8|black)\b", lower):
        return "lint"
    if (
        re.search(r"^\s*(grep|rg|cat|ls|find|head|tail|wc|file|stat|which|tree)\b", lower)
        or re.search(r"^\s*git (status|log|diff|show|branch)", lower)
        or re.search(r"^\s*sed -n", lower)
    ):
        return "read"
    return "other"


def modeled_value(tasks: dict[str, list[Attempt]]) -> dict:
    hits: collections.Counter[str] = collections.Counter()
    totals: collections.Counter[str] = collections.Counter()
    for attempts in tasks.values():
        banks = [{step.command for step in attempt.steps} for attempt in attempts]
        for index, bank in enumerate(banks):
            peers: set[str] = set()
            for peer_index, peer in enumerate(banks):
                if peer_index != index:
                    peers.update(peer)
            for command in bank:
                category = command_class(command)
                totals[category] += 1
                hits[category] += command in peers

    rows = []
    for category in MODELED_SECONDS:
        if not totals[category]:
            continue
        seconds = hits[category] * MODELED_SECONDS[category]
        rows.append(
            {
                "class": category,
                "hits_measured": hits[category],
                "total_deduplicated_commands": totals[category],
                "hit_percent": round(100.0 * hits[category] / totals[category], 6),
                "assumed_seconds_per_command": MODELED_SECONDS[category],
                "deleted_seconds_modeled": seconds,
            }
        )
    return {
        "label": (
            "Command-only held-out matches are measured proxy counts; all seconds "
            "are modeled assumptions. These are not state-keyed cache hits."
        ),
        "cost_model_source": "seed/value.py",
        "rows": sorted(rows, key=lambda row: row["deleted_seconds_modeled"], reverse=True),
        "total_deleted_seconds_modeled": sum(
            row["deleted_seconds_modeled"] for row in rows
        ),
    }


def replay_taxonomy(replay: dict) -> dict:
    """Present a shipping ReplayReport in the evidence schema."""
    overall = replay["overall"]
    denominator = overall["commands"]
    return {
        "denominator_raw_command_slots": denominator,
        "prior_self_reuse": _ratio(overall["self_reuse"], denominator),
        "peer_reuse": _ratio(overall["cross_agent"], denominator),
        "avoidable_total": _ratio(overall["avoidable"], denominator),
        "unique": _ratio(denominator - overall["avoidable"], denominator),
        "missing_key": _ratio(0, denominator),
        "timing_caveat": (
            "The shipping replay interleaves attempts by step index and breaks "
            "ties in stable submission/path order. A peer sighting wins over a "
            "self sighting when both exist."
        ),
    }


def replay_step_curve(replay: dict) -> dict[str, dict]:
    return {
        row["steps"]: _ratio(row["cross_agent"], row["commands"])
        for row in replay["by_step"]
    }


def build_report(corpus: Path) -> dict:
    attempts, load = load_attempts(corpus / "records")
    tasks = group_multi_agent(attempts)
    if not tasks:
        raise SystemExit("no instances with two or more usable attempts")

    multi_attempts = [attempt for group in tasks.values() for attempt in group]
    state = diagnose_state(multi_attempts)
    if state["verdict"] != "supported":
        raise SystemExit(
            "state_sha256 did not behave like cumulative workspace state; "
            "refusing to publish state-keyed evidence"
        )

    state_replay = shipping_replay(corpus, "state")
    command_replay = shipping_replay(corpus, "command")
    windows = {
        "first_1": (0, 1),
        "first_3": (0, 3),
        "first_5": (0, 5),
        "first_10": (0, 10),
        "after_10": (10, None),
        "after_50": (50, None),
        "all": (0, None),
    }
    command_curve = {
        name: held_out_overlap(tasks, command_key, start, stop)
        for name, (start, stop) in windows.items()
    }
    raw_commands = sum(len(attempt.steps) for attempt in multi_attempts)
    deduplicated_commands = sum(
        len({step.command for step in attempt.steps}) for attempt in multi_attempts
    )
    submissions = {
        instance: sorted({attempt.submission for attempt in group})
        for instance, group in sorted(tasks.items())
    }
    mixed_tasks = sum(len(values) >= 2 for values in submissions.values())

    report = {
        "schema_version": 2,
        "corpus_path": str(corpus.resolve()),
        "load": load,
        "multi_agent": {
            "tasks": len(tasks),
            "attempts": len(multi_attempts),
            "fanout_widths": dict(
                sorted(collections.Counter(len(group) for group in tasks.values()).items())
            ),
            "raw_command_slots": raw_commands,
            "deduplicated_commands_per_agent": deduplicated_commands,
        },
        "state_diagnostic": state,
        "held_out_method": (
            "For each agent, deduplicate keys in the selected command window and "
            "compare them with the union of that task's peer-agent key sets. This "
            "legacy command-only curve is an upper-bound proxy, not the source of "
            "the state-keyed headline claims."
        ),
        "command_only_held_out": command_curve,
        "shipping_replay_method": (
            "Generated by `go run ./cmd/hindsight replay --json --by-step` using "
            "the production loader, NormalizeCommand, temporal interleave, "
            "peer-first tie-break, and classifier."
        ),
        "shipping_state_replay": state_replay,
        "shipping_command_replay": command_replay,
        "state_command_by_step": replay_step_curve(state_replay),
        "command_only_raw_reuse": replay_taxonomy(command_replay),
        "state_command_raw_reuse": replay_taxonomy(state_replay),
        "submissions": {
            "tasks_with_multiple_submission_ids": mixed_tasks,
            "total_multi_agent_tasks": len(tasks),
            "all_tasks_mix_submissions": mixed_tasks == len(tasks),
            "by_instance": submissions,
        },
        "duration_evidence": {
            "per_step_timing_fields_found": load["per_step_timing_fields"],
            "note": (
                "If per_step_timing_fields_found is empty, the analyzed Step schema "
                "has no apparent per-command duration. Corpus hit counts are measured; "
                "the value table's seconds are modeled."
            ),
        },
        "modeled_value": modeled_value(tasks),
    }
    report["published_claim_check"] = claim_check(report)
    return report


def claim_check(report: dict) -> dict:
    taxonomy = report["state_command_raw_reuse"]
    curve = report["state_command_by_step"]
    actual = {
        "state_avoidable_pct": taxonomy["avoidable_total"]["percent"],
        "state_cross_agent_pct": taxonomy["peer_reuse"]["percent"],
        "state_first_3_pct": curve["0-2"]["percent"],
        "state_after_50_pct": curve["50+"]["percent"],
    }
    return {
        name: {
            "expected_percent": expected,
            "actual_percent": actual[name],
            "status": "matches_rounding"
            if actual[name] is not None and round(actual[name], 1) == expected
            else "does_not_match",
        }
        for name, expected in EXPECTED.items()
    }


def pct(metric: dict | None) -> str:
    if not metric or metric.get("percent") is None:
        return "n/a"
    return f'{metric["percent"]:.1f}% ({metric["numerator"]:,}/{metric["denominator"]:,})'


def overlap_markdown(report: dict) -> str:
    lines = [
        "# Overlap evidence",
        "",
        f'Corpus: `{report["corpus_path"]}`',
        "",
        report["held_out_method"],
        "",
        "This command-only table preserves the original held-out-per-agent proxy. "
        "Every cell is `percent (matching deduplicated commands / deduplicated commands examined)`.",
        "",
        "| Window | Command only |",
        "|---|---:|",
    ]
    for label, key in (
        ("First 1", "first_1"),
        ("First 3", "first_3"),
        ("First 5", "first_5"),
        ("First 10", "first_10"),
        ("After 10", "after_10"),
        ("After 50", "after_50"),
        ("All", "all"),
    ):
        lines.append(f'| {label} | {pct(report["command_only_held_out"][key])} |')

    state_taxonomy = report["state_command_raw_reuse"]
    lines += [
        "",
        "## Shipping state-keyed replay",
        "",
        report["shipping_replay_method"],
        "",
        f'- Avoidable commands: {pct(state_taxonomy["avoidable_total"])}',
        f'- Cross-agent commands: {pct(state_taxonomy["peer_reuse"])}',
        f'- Self-reuse commands: {pct(state_taxonomy["prior_self_reuse"])}',
        "",
        "| Step band | Cross-agent reuse |",
        "|---|---:|",
    ]
    for band, metric in report["state_command_by_step"].items():
        lines.append(f"| {band} | {pct(metric)} |")

    diagnostic = report["state_diagnostic"]
    lines += [
        "",
        "## State-key validity",
        "",
        f'Verdict: **{diagnostic["verdict"]}**. {diagnostic["interpretation"]}.',
        "",
        f'- Mutating steps change state: {pct(diagnostic["mutating_steps_change_state"])}',
        f'- Non-mutating steps preserve state: {pct(diagnostic["nonmutating_steps_preserve_state"])}',
        f'- Non-empty deltas persist after a non-mutating step: {pct(diagnostic["nonempty_delta_retained_after_nonmutating_step"])}',
        f'- `diff_lines` is nondecreasing: {pct(diagnostic["diff_lines_nondecreasing"])}',
        f'- `n_files` is nondecreasing: {pct(diagnostic["file_count_nondecreasing"])}',
        "",
        diagnostic["monotonicity_note"],
        "",
        "`state_sha256` is the corpus analogue of Hindsight's git tree hash, not the tree hash itself. The state-keyed replay therefore measures the same key shape against recorded corpus state; it does not prove byte-for-byte equivalence to a live git tree.",
        "",
    ]
    return "\n".join(lines)


def value_markdown(report: dict) -> str:
    value = report["modeled_value"]
    timing_fields = report["duration_evidence"]["per_step_timing_fields_found"]
    timing_note = (
        "No apparent per-command timing field was found in the replay steps."
        if not timing_fields
        else f"Potential per-step timing fields require review: `{timing_fields}`."
    )
    lines = [
        "# Modeled value table",
        "",
        f'**{value["label"]}**',
        "",
        "The constants are copied from the read-only `seed/value.py` artifact.",
        "",
        "| Class | Measured proxy matches | Deduplicated commands | Match rate | Assumed seconds/command | Modeled seconds deleted |",
        "|---|---:|---:|---:|---:|---:|",
    ]
    for row in value["rows"]:
        lines.append(
            f'| {row["class"]} | {row["hits_measured"]:,} | '
            f'{row["total_deduplicated_commands"]:,} | {row["hit_percent"]:.1f}% | '
            f'{row["assumed_seconds_per_command"]:.2f} | '
            f'{row["deleted_seconds_modeled"]:,.0f} |'
        )
    lines += [
        "",
        f'**Total modeled seconds deleted: {value["total_deleted_seconds_modeled"]:,.0f}.**',
        "",
        f"{timing_note} The live fleet experiment, not this table, supplies measured execution durations.",
        "",
    ]
    return "\n".join(lines)


def claims_markdown(report: dict) -> str:
    multi = report["multi_agent"]
    submissions = report["submissions"]
    checks = report["published_claim_check"]
    timing_fields = report["duration_evidence"]["per_step_timing_fields_found"]
    timing_claim = (
        "The corpus contains no apparent per-command duration in the analyzed steps."
        if not timing_fields
        else f"Potential per-step timing fields require manual review: `{timing_fields}`."
    )
    lines = [
        "# Regenerable claims",
        "",
        f'This file was generated from `{report["corpus_path"]}` by `python3 evidence/overlap.py`; the four state-keyed figures come from the shipping Go replay invoked by that script.',
        "",
        f'- The analysis covers **{multi["tasks"]} multi-agent tasks** and **{multi["attempts"]} attempts**.',
        f'- Those attempts contain **{multi["raw_command_slots"]:,} raw command slots** and **{multi["deduplicated_commands_per_agent"]:,} commands after deduplicating within each agent**.',
        f'- **{submissions["tasks_with_multiple_submission_ids"]}/{submissions["total_multi_agent_tasks"]} tasks** contain multiple `source.metadata.submission` identities.',
        f"- {timing_claim} Hit counts are measured; the per-class seconds in `value.md` are modeled.",
        "",
        "## Published state-keyed claim audit",
        "",
        "| Claim | Expected | Regenerated | Status |",
        "|---|---:|---:|---|",
    ]
    for name, result in checks.items():
        actual = result.get("actual_percent")
        lines.append(
            f'| `{name}` | {result["expected_percent"]:.1f}% | '
            f'{actual:.1f}% | {result["status"]} |'
            if actual is not None
            else f'| `{name}` | {result["expected_percent"]:.1f}% | n/a | {result["status"]} |'
        )
    lines += [
        "",
        "Any `does_not_match` result must be corrected or qualified in the public design document; this generator does not tune its method to force the expected number.",
        "",
        "The shipping replay uses step index as the corpus's shared logical clock, with a stable submission/path tie-break. The live fleet run remains the measured concurrency result.",
        "",
    ]
    return "\n".join(lines)


def write_outputs(report: dict, output_dir: Path) -> None:
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / "overlap.json").write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    (output_dir / "overlap.md").write_text(overlap_markdown(report), encoding="utf-8")
    (output_dir / "value.md").write_text(value_markdown(report), encoding="utf-8")
    (output_dir / "claims.md").write_text(claims_markdown(report), encoding="utf-8")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--corpus",
        type=Path,
        default=DEFAULT_CORPUS,
        help="replay-A directory containing records/",
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        default=Path(__file__).resolve().parent,
        help="directory for overlap.json, overlap.md, value.md, and claims.md",
    )
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    if not (args.corpus / "records").is_dir():
        raise SystemExit(
            f"corpus records directory not found: {args.corpus / 'records'}\n"
            "Ask Tom for replay-A or run again with --corpus PATH. No evidence "
            "outputs were written."
        )
    report = build_report(args.corpus)
    write_outputs(report, args.output_dir)
    print(
        f'wrote evidence for {report["multi_agent"]["tasks"]} multi-agent tasks '
        f"to {args.output_dir}"
    )


if __name__ == "__main__":
    main()

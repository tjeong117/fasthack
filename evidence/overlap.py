#!/usr/bin/env python3
"""Regenerate Hindsight's corpus evidence from the sealed replay records.

This is intentionally standard-library only. It writes overlap.json, overlap.md,
value.md, and claims.md next to this file. No output is written unless the corpus
exists and contains usable multi-agent records.
"""

from __future__ import annotations

import argparse
import collections
import hashlib
import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Hashable, Iterable, Sequence


DEFAULT_CORPUS = Path(
    "/Users/tomjeong/hacker/skunk-works/notes/sealed-corpus/replay-A"
)
SPACE = re.compile(r"\s+")
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
    return SPACE.sub(" ", command.strip())


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


def raw_reuse_taxonomy(
    tasks: dict[str, list[Attempt]], key: Callable[[Step], Hashable | None]
) -> dict:
    """Classify raw slots as prior self reuse, peer reuse, or unique.

    Peer reuse is potential reuse: a matching key exists in another attempt for
    the task. The corpus has no cross-attempt command timestamps, so it cannot
    establish which peer would have populated the cache first.
    """
    self_reuse = 0
    peer_reuse = 0
    unique = 0
    missing_key = 0
    for attempts in tasks.values():
        peer_banks = [{key(step) for step in attempt.steps} for attempt in attempts]
        for index, attempt in enumerate(attempts):
            peers: set[Hashable | None] = set()
            for peer_index, bank in enumerate(peer_banks):
                if peer_index != index:
                    peers.update(bank)
            seen: set[Hashable] = set()
            for step in attempt.steps:
                value = key(step)
                if value is None:
                    missing_key += 1
                elif value in seen:
                    self_reuse += 1
                elif value in peers:
                    peer_reuse += 1
                else:
                    unique += 1
                if value is not None:
                    seen.add(value)
    denominator = self_reuse + peer_reuse + unique + missing_key
    return {
        "denominator_raw_command_slots": denominator,
        "prior_self_reuse": _ratio(self_reuse, denominator),
        "peer_reuse": _ratio(peer_reuse, denominator),
        "avoidable_total": _ratio(self_reuse + peer_reuse, denominator),
        "unique": _ratio(unique, denominator),
        "missing_key": _ratio(missing_key, denominator),
        "timing_caveat": (
            "Peer matches are potential reuse because the corpus has no "
            "cross-attempt command timestamps."
        ),
    }


def _ratio(numerator: int, denominator: int) -> dict:
    return {
        "numerator": numerator,
        "denominator": denominator,
        "percent": round(100.0 * numerator / denominator, 6) if denominator else None,
    }


def command_key(step: Step) -> str:
    return step.command


def state_command_key(step: Step) -> tuple[str, str] | None:
    if not step.state_sha256:
        return None
    return (step.state_sha256, step.command)


def _canonical_delta_hash(delta: object) -> str:
    encoded = json.dumps(
        delta, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def diagnose_state(attempts: Sequence[Attempt]) -> dict:
    hash_match = hash_total = 0
    unchanged_match = unchanged_total = 0
    retained_after_read = retained_total = 0
    diff_nondecreasing = diff_pairs = 0
    file_nondecreasing = file_pairs = 0
    empty_hash = hashlib.sha256(b"{}").hexdigest()
    empty_delta_hash_matches = 0
    empty_delta_total = 0

    for attempt in attempts:
        for step in attempt.steps:
            if step.state_sha256 and step.delta is not None:
                hash_total += 1
                hash_match += step.state_sha256 == _canonical_delta_hash(step.delta)
            if step.delta == {} and step.state_sha256:
                empty_delta_total += 1
                empty_delta_hash_matches += step.state_sha256 == empty_hash

        for previous, current in zip(attempt.steps, attempt.steps[1:]):
            # state_sha256/delta are treated as post-command snapshots. A command
            # marked non-mutating should leave the preceding state unchanged.
            if current.mutated is False and previous.state_sha256 and current.state_sha256:
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

    hash_rate = hash_match / hash_total if hash_total else 0.0
    unchanged_rate = unchanged_match / unchanged_total if unchanged_total else 0.0
    retained_rate = retained_after_read / retained_total if retained_total else 0.0
    if hash_total == 0 or unchanged_total == 0 or retained_total == 0:
        verdict = "inconclusive"
    elif hash_rate >= 0.99 and unchanged_rate >= 0.95 and retained_rate >= 0.95:
        verdict = "supported"
    else:
        verdict = "not_supported"

    return {
        "verdict": verdict,
        "interpretation": (
            "supported means state_sha256 matches canonical cumulative delta "
            "snapshots closely enough to use as the corpus state key; it does "
            "not prove equivalence to Hindsight's git tree hash"
        ),
        "canonical_delta_hash_matches": _ratio(hash_match, hash_total),
        "empty_delta_sha256_object_matches": _ratio(
            empty_delta_hash_matches, empty_delta_total
        ),
        "nonmutating_steps_preserve_state": _ratio(unchanged_match, unchanged_total),
        "nonempty_delta_retained_after_nonmutating_step": _ratio(
            retained_after_read, retained_total
        ),
        "diff_lines_nondecreasing": _ratio(diff_nondecreasing, diff_pairs),
        "file_count_nondecreasing": _ratio(file_nondecreasing, file_pairs),
        "monotonicity_note": (
            "A cumulative diff may shrink when an edit is reverted, so the two "
            "monotonicity rates are diagnostics rather than pass/fail tests."
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
        "label": "Hit counts are measured; all seconds are modeled assumptions.",
        "cost_model_source": "seed/value.py",
        "rows": sorted(rows, key=lambda row: row["deleted_seconds_modeled"], reverse=True),
        "total_deleted_seconds_modeled": sum(
            row["deleted_seconds_modeled"] for row in rows
        ),
    }


def build_report(corpus: Path) -> dict:
    attempts, load = load_attempts(corpus / "records")
    tasks = group_multi_agent(attempts)
    if not tasks:
        raise SystemExit("no instances with two or more usable attempts")

    multi_attempts = [attempt for group in tasks.values() for attempt in group]
    state = diagnose_state(multi_attempts)
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
    state_curve = None
    state_taxonomy = None
    if state["verdict"] == "supported":
        state_curve = {
            name: held_out_overlap(tasks, state_command_key, start, stop)
            for name, (start, stop) in windows.items()
        }
        state_taxonomy = raw_reuse_taxonomy(tasks, state_command_key)

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
        "schema_version": 1,
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
            "compare them with the union of that task's peer-agent key sets."
        ),
        "command_only_held_out": command_curve,
        "state_command_held_out": state_curve,
        "command_only_raw_reuse": raw_reuse_taxonomy(tasks, command_key),
        "state_command_raw_reuse": state_taxonomy,
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
    curve = report["state_command_held_out"]
    if taxonomy is None or curve is None:
        return {
            name: {"expected_percent": expected, "status": "not_tested"}
            for name, expected in EXPECTED.items()
        }
    actual = {
        "state_avoidable_pct": taxonomy["avoidable_total"]["percent"],
        "state_cross_agent_pct": taxonomy["peer_reuse"]["percent"],
        "state_first_3_pct": curve["first_3"]["percent"],
        "state_after_50_pct": curve["after_50"]["percent"],
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
        "Every cell is `percent (matching deduplicated keys / deduplicated keys examined)`.",
        "",
        "| Window | Command only | `(state_sha256, command)` |",
        "|---|---:|---:|",
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
        state_curve = report["state_command_held_out"]
        state_metric = state_curve[key] if state_curve else None
        lines.append(
            f'| {label} | {pct(report["command_only_held_out"][key])} | {pct(state_metric)} |'
        )
    diagnostic = report["state_diagnostic"]
    lines += [
        "",
        "## State-key validity",
        "",
        f'Verdict: **{diagnostic["verdict"]}**. {diagnostic["interpretation"]}.',
        "",
        f'- Canonical delta hashes: {pct(diagnostic["canonical_delta_hash_matches"])}',
        f'- Empty-delta hashes equal `sha256("{{}}")`: {pct(diagnostic["empty_delta_sha256_object_matches"])}',
        f'- Non-mutating steps preserve state: {pct(diagnostic["nonmutating_steps_preserve_state"])}',
        f'- Non-empty deltas persist after a non-mutating step: {pct(diagnostic["nonempty_delta_retained_after_nonmutating_step"])}',
        f'- `diff_lines` is nondecreasing: {pct(diagnostic["diff_lines_nondecreasing"])}',
        f'- `n_files` is nondecreasing: {pct(diagnostic["file_count_nondecreasing"])}',
        "",
        diagnostic["monotonicity_note"],
        "",
        "`state_sha256` is a hash of the corpus delta snapshot, not Hindsight's git tree hash. Even when cumulative, it is only a proxy for the production key.",
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
        "**Hit counts are measured from the sealed corpus. Every seconds figure is modeled, not measured.**",
        "",
        "The constants are copied from the read-only `seed/value.py` artifact.",
        "",
        "| Class | Measured hits | Deduplicated commands | Hit rate | Assumed seconds/command | Modeled seconds deleted |",
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
        f'This file was generated from `{report["corpus_path"]}` by `python3 evidence/overlap.py`.',
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
        "Cross-agent corpus matches are potential reuse because the records do not provide a shared command-level timeline across attempts. The live fleet run is the measured concurrency result.",
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

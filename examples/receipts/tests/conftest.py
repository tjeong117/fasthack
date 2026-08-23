"""Shared generators for the property tests.

Money code is where off-by-ones hide, so the suite checks each invariant
across a large generated space rather than a handful of examples. Failures
are collected rather than raised on the first case, so a report says how
wrong the code is and not merely that it is wrong.
"""

TOTALS = range(1, 80000)
WAYS = range(1, 60)
RATES_BPS = range(0, 2000, 3)
PERCENTS = range(0, 101)


def bills():
    for total in TOTALS:
        for ways in WAYS:
            yield total, ways


def report(failures, checked, what):
    """Fail with a count and a sample, not just the first bad case."""
    if failures:
        sample = "\n  ".join(failures[:5])
        raise AssertionError(
            f"{len(failures):,} of {checked:,} cases {what}\n  {sample}"
            + ("\n  ..." if len(failures) > 5 else "")
        )

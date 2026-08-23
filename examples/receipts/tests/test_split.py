import pytest
from conftest import bills, report
from receipts import Money, split_evenly


def test_parts_always_sum_back_to_the_total():
    """Whatever the bill and however many people, the table pays exactly it."""
    failures, checked = [], 0
    for total, ways in bills():
        checked += 1
        paid = sum(p.cents for p in split_evenly(Money(total), ways))
        if paid != total:
            failures.append(f"{ways} people splitting {total}c paid {paid}c")
    report(failures, checked, "did not sum back to the bill")


def test_nobody_pays_more_than_a_cent_extra():
    failures, checked = [], 0
    for total, ways in bills():
        checked += 1
        parts = split_evenly(Money(total), ways)
        spread = max(p.cents for p in parts) - min(p.cents for p in parts)
        if spread > 1:
            failures.append(f"{ways} people splitting {total}c differ by {spread}c")
    report(failures, checked, "split unevenly")


def test_rejects_zero_ways():
    with pytest.raises(ValueError):
        split_evenly(Money(1000), 0)

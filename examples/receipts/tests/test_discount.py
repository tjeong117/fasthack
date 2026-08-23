import pytest
from conftest import PERCENTS, TOTALS
from receipts import Money, apply_discount


def test_discount_never_increases_an_amount():
    for total in TOTALS:
        for pct in PERCENTS:
            assert apply_discount(Money(total), pct).cents <= total


def test_full_discount_is_free():
    for total in TOTALS:
        assert apply_discount(Money(total), 100) == Money(0)


def test_rejects_impossible_discount():
    with pytest.raises(ValueError):
        apply_discount(Money(100), 150)

from conftest import RATES_BPS, TOTALS
from receipts import Money, apply_tax


def test_tax_never_decreases_an_amount():
    for total in TOTALS:
        for rate in RATES_BPS:
            assert apply_tax(Money(total), rate).cents >= total


def test_zero_rate_is_the_identity():
    for total in TOTALS:
        assert apply_tax(Money(total), 0) == Money(total)

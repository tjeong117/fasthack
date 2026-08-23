from conftest import TOTALS
from receipts import Money, subtotal


def test_subtotal_is_linear_in_quantity():
    for price in TOTALS:
        for qty in range(1, 24):
            assert subtotal([("item", price, qty)]) == Money(price * qty)


def test_empty_order_is_free():
    assert subtotal([]) == Money(0)

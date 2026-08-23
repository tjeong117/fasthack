from .money import Money


def apply_tax(amount: Money, rate_bps: int) -> Money:
    """Add tax at a rate in basis points. 875 bps = 8.75%."""
    tax = (amount.cents * rate_bps + 5000) // 10000
    return Money(amount.cents + tax)

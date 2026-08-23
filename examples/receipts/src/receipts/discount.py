from .money import Money


def apply_discount(amount: Money, percent_off: int) -> Money:
    """Take a whole-percent discount off an amount."""
    if not 0 <= percent_off <= 100:
        raise ValueError("percent_off must be between 0 and 100")
    return Money(amount.cents - (amount.cents * percent_off) // 100)

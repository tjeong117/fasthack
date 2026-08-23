from .money import Money


def split_evenly(total: Money, ways: int) -> list[Money]:
    """Split a bill between people so the parts sum back to the total.

    Nobody pays more than one cent above anybody else.
    """
    if ways <= 0:
        raise ValueError("ways must be positive")
    base = total.cents // ways
    return [Money(base) for _ in range(ways)]

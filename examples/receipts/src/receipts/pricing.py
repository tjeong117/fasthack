from .money import Money


def subtotal(line_items: list[tuple[str, int, int]]) -> Money:
    """Sum (name, unit_price_cents, quantity) line items."""
    return Money(sum(price * qty for _, price, qty in line_items))

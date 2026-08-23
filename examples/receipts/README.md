# receipts

Order totals, tax, discounts, and splitting the bill. Amounts are whole cents
end to end — no floats anywhere, because money and binary fractions do not mix.

## Development

```bash
uv sync --extra dev      # install, including test deps
uv run pytest -q         # run the suite
```

The suite property-tests each invariant across a large generated space rather
than a handful of examples, so it takes a minute or two. That is deliberate:
off-by-one errors in money code hide in the cases nobody writes by hand.

## Layout

| module | what it does |
|---|---|
| `money.py` | the `Money` type, whole cents |
| `pricing.py` | line items to a subtotal |
| `tax.py` | tax at a rate in basis points |
| `discount.py` | whole-percent discounts |
| `split.py` | splitting a bill between people |

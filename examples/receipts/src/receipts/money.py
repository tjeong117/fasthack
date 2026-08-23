from dataclasses import dataclass


@dataclass(frozen=True)
class Money:
    """An amount in whole cents. Integers only, so nothing rounds behind your back."""

    cents: int

    def __add__(self, other: "Money") -> "Money":
        return Money(self.cents + other.cents)

    def __str__(self) -> str:
        return f"${self.cents // 100}.{self.cents % 100:02d}"

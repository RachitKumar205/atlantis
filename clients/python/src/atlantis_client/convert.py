"""Reading the column types protobuf has no native form for.

The generated client hands back protobuf messages, so a `numeric` column
arrives as a decimal string and an `interval` as `atlantis.common.v1.Interval`.
Both need a conversion the wire cannot carry, and both are here rather than in
the generated tree so the mapping stays in one place.

Job args take the JSON projection of these types instead; see
:mod:`atlantis_client.jobs.argjson`.
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal, InvalidOperation
from typing import Protocol

__all__ = ["Interval", "interval_to_timedelta", "to_decimal"]


class Interval(Protocol):
    """The fields of ``atlantis.common.v1.Interval``.

    A Protocol so this module does not import the generated protobuf, which
    lives in the caller's tree and not in this package.
    """

    @property
    def months(self) -> int: ...
    @property
    def days(self) -> int: ...
    @property
    def microseconds(self) -> int: ...


def to_decimal(value: str) -> Decimal:
    """Parse a `numeric` column.

    It travels as a string because proto's double and int64 both lose digits
    a NUMERIC holds. Reading one as a float is the silent form of that loss,
    which is why there is no float accessor.

    An empty string is the unset field, and raises: NUMERIC has no zero-like
    default, so returning Decimal(0) would report a value the row does not
    hold.
    """
    try:
        return Decimal(value)
    except InvalidOperation as exc:
        raise ValueError(f"not a decimal: {value!r}") from exc


def interval_to_timedelta(value: Interval) -> dt.timedelta:
    """Convert an `interval` column, refusing one that carries months.

    Postgres stores months, days and microseconds separately because their
    lengths differ: a month is 28 to 31 days, and a day across a DST boundary
    is 23 or 25 hours. timedelta has no month, so a non-zero one raises here
    rather than being resolved against a calendar this layer does not have.
    """
    if value.months != 0:
        raise ValueError(
            f"interval carries {value.months} months, which timedelta cannot "
            "represent: a month's length depends on the date it starts from"
        )
    return dt.timedelta(days=value.days, microseconds=value.microseconds)

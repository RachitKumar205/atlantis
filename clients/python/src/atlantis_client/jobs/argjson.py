"""The JSON shapes job args arrive in.

A job's args are not a protobuf message. atlantis stores them as the JSON
Go's encoding/json produces for the arg struct, so the types here mirror that
projection: `timestamptz` arrives as an RFC 3339 string, `bytea` and `jsonb`
as base64 strings, and `interval` as the object below.

The generated `jobs.py` annotates each arg with one of these and calls the
matching helper. Nothing here decodes an envelope; that is `_envelopes`.
"""

from __future__ import annotations

import base64
import datetime as dt
from typing import TypedDict

__all__ = ["IntervalJSON", "from_base64", "from_rfc3339", "interval_to_timedelta"]


class IntervalJSON(TypedDict):
    """A Postgres interval as pgtype.Interval marshals it.

    The keys are capitalised because they are Go struct field names and the
    type declares no json tags. ``Valid`` is false for a NULL interval.
    """

    Months: int
    Days: int
    Microseconds: int
    Valid: bool


def from_rfc3339(value: str) -> dt.datetime:
    """Parse a `timestamptz` or `date` arg.

    Go writes RFC 3339 with a literal ``Z`` for UTC, which
    ``datetime.fromisoformat`` did not accept before Python 3.11.
    """
    if value.endswith(("Z", "z")):
        value = value[:-1] + "+00:00"
    return dt.datetime.fromisoformat(value)


def from_base64(value: str) -> bytes:
    """Decode a `bytea` or `jsonb` arg.

    Both are Go ``[]byte``, so both arrive base64-encoded. A jsonb arg read
    as the payload gives the encoding, not the object.
    """
    return base64.b64decode(value, validate=True)


def interval_to_timedelta(value: IntervalJSON) -> dt.timedelta:
    """Convert an interval arg, refusing one that carries months.

    A month has no fixed length, so a timedelta cannot represent one without
    a calendar this layer does not have. Raises rather than picking 30 days.
    """
    if value["Months"] != 0:
        raise ValueError(
            f"interval carries {value['Months']} months, which timedelta cannot "
            "represent: a month's length depends on the date it starts from"
        )
    return dt.timedelta(days=value["Days"], microseconds=value["Microseconds"])

"""The conversions protobuf and JSON cannot carry themselves."""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from decimal import Decimal

import pytest

from atlantis_client import interval_to_timedelta, to_decimal
from atlantis_client.jobs import argjson


@dataclass
class FakeInterval:
    """Stands in for atlantis.common.v1.Interval, which lives in the caller's tree."""

    months: int = 0
    days: int = 0
    microseconds: int = 0


def test_numeric_keeps_the_digits_a_float_would_lose() -> None:
    """The reason NUMERIC travels as a string at all.

    float("0.1") is not 0.1, and a money column summed as floats drifts. The
    string form is exact, and to_decimal is what keeps it that way.
    """
    assert to_decimal("0.1") + to_decimal("0.2") == Decimal("0.3")
    assert to_decimal("12345678901234567890.12345") == Decimal("12345678901234567890.12345")


def test_a_non_numeric_string_is_reported() -> None:
    with pytest.raises(ValueError, match="not a decimal"):
        to_decimal("twelve")


def test_an_unset_numeric_field_is_reported_not_read_as_zero() -> None:
    """A proto string field defaults to "". Returning Decimal(0) would report
    a value the row does not hold — and 0 is a plausible amount."""
    with pytest.raises(ValueError, match="not a decimal"):
        to_decimal("")


def test_an_interval_of_days_and_microseconds_converts() -> None:
    got = interval_to_timedelta(FakeInterval(days=2, microseconds=500))
    assert got == dt.timedelta(days=2, microseconds=500)


def test_an_interval_carrying_months_raises() -> None:
    """A month is 28 to 31 days. Picking one silently shifts every deadline
    computed from the value."""
    with pytest.raises(ValueError, match="months"):
        interval_to_timedelta(FakeInterval(months=1))


# --- the job-args projection ---------------------------------------------


def test_rfc3339_parses_the_z_suffix() -> None:
    """Go writes a literal Z for UTC; datetime.fromisoformat did not accept
    one before 3.11, and this package supports 3.10."""
    got = argjson.from_rfc3339("2026-09-07T12:00:00Z")
    assert got == dt.datetime(2026, 9, 7, 12, 0, tzinfo=dt.timezone.utc)


def test_rfc3339_keeps_an_explicit_offset() -> None:
    got = argjson.from_rfc3339("2026-09-07T12:00:00+05:30")
    assert got.utcoffset() == dt.timedelta(hours=5, minutes=30)


def test_a_jsonb_arg_is_base64_not_the_object() -> None:
    """bytea and jsonb are both Go []byte, so both arrive encoded.

    Reading the field directly gives 'eyJhIjoxfQ==', which is a str and
    parses as neither JSON nor an error.
    """
    assert argjson.from_base64("eyJhIjoxfQ==") == b'{"a":1}'


def test_a_corrupt_base64_arg_is_reported() -> None:
    with pytest.raises(ValueError):
        argjson.from_base64("not base64 at all!!")


def test_the_interval_json_shape_is_gos_field_names() -> None:
    """pgtype.Interval declares no json tags, so the keys are its Go field
    names. Lower-case keys are a different object and would KeyError."""
    value: argjson.IntervalJSON = {
        "Months": 0,
        "Days": 2,
        "Microseconds": 3,
        "Valid": True,
    }
    assert argjson.interval_to_timedelta(value) == dt.timedelta(days=2, microseconds=3)


def test_an_interval_arg_carrying_months_raises() -> None:
    value: argjson.IntervalJSON = {
        "Months": 1,
        "Days": 0,
        "Microseconds": 0,
        "Valid": True,
    }
    with pytest.raises(ValueError, match="months"):
        argjson.interval_to_timedelta(value)

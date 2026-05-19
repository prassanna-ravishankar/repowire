from __future__ import annotations

from datetime import datetime, timezone

import pytest

from repowire.daemon.schedule_cron import CronExpressionError, next_fire_after, validate_cron


def test_validate_cron_accepts_alias_and_normalizes() -> None:
    assert validate_cron("@hourly") == "0 * * * *"


def test_validate_cron_rejects_bad_field_count() -> None:
    with pytest.raises(CronExpressionError):
        validate_cron("* * *")


def test_validate_cron_accepts_steps_ranges_and_lists() -> None:
    assert validate_cron("*/15 9-17 * * 1,3,5") == "*/15 9-17 * * 1,3,5"


def test_next_fire_after_uses_next_matching_minute() -> None:
    base = datetime(2026, 5, 19, 8, 10, 30, tzinfo=timezone.utc)
    assert next_fire_after("*/15 * * * *", base) == datetime(
        2026, 5, 19, 8, 15, tzinfo=timezone.utc,
    )


def test_next_fire_after_supports_sunday_as_zero_or_seven() -> None:
    base = datetime(2026, 5, 18, 8, 0, tzinfo=timezone.utc)  # Monday
    assert next_fire_after("0 9 * * 7", base) == datetime(
        2026, 5, 24, 9, 0, tzinfo=timezone.utc,
    )


def test_day_of_month_and_day_of_week_use_cron_or_semantics() -> None:
    base = datetime(2026, 5, 19, 8, 0, tzinfo=timezone.utc)  # Tuesday the 19th
    assert next_fire_after("0 9 20 * 5", base) == datetime(
        2026, 5, 20, 9, 0, tzinfo=timezone.utc,
    )

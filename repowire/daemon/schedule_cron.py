"""Small dependency-free cron parser for repowire schedules."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

_ALIASES = {
    "@hourly": "0 * * * *",
    "@daily": "0 0 * * *",
    "@midnight": "0 0 * * *",
    "@weekly": "0 0 * * 0",
    "@monthly": "0 0 1 * *",
}


class CronExpressionError(ValueError):
    """Raised when a cron expression is not supported."""


def normalize_cron(expr: str) -> str:
    """Normalize supported aliases and whitespace."""
    raw = " ".join(expr.strip().split())
    return _ALIASES.get(raw.lower(), raw)


def validate_cron(expr: str) -> str:
    """Validate and return normalized cron expression.

    Supported syntax is standard five-field cron:
    ``minute hour day-of-month month day-of-week``.

    Each field supports ``*``, comma lists, ranges, and steps such as
    ``*/15`` or ``1-5/2``. Day-of-week uses cron numbering where 0 or 7 is
    Sunday.
    """
    norm = normalize_cron(expr)
    fields = norm.split()
    if len(fields) != 5:
        raise CronExpressionError(
            "cron must have 5 fields: minute hour day-of-month month day-of-week",
        )
    _parse_field(fields[0], 0, 59, "minute")
    _parse_field(fields[1], 0, 23, "hour")
    _parse_field(fields[2], 1, 31, "day-of-month")
    _parse_field(fields[3], 1, 12, "month")
    _parse_field(fields[4], 0, 7, "day-of-week")
    return norm


def next_fire_after(expr: str, after: datetime) -> datetime:
    """Return the next UTC minute matching ``expr`` after ``after``."""
    norm = validate_cron(expr)
    minute, hour, dom, month, dow = norm.split()
    minutes = _parse_field(minute, 0, 59, "minute")
    hours = _parse_field(hour, 0, 23, "hour")
    days = _parse_field(dom, 1, 31, "day-of-month")
    months = _parse_field(month, 1, 12, "month")
    weekdays = _parse_field(dow, 0, 7, "day-of-week")
    if 7 in weekdays:
        weekdays = (weekdays - {7}) | {0}
    dom_any = dom == "*"
    dow_any = dow == "*"

    base = after.astimezone(timezone.utc) if after.tzinfo else after.replace(tzinfo=timezone.utc)
    candidate = base.replace(second=0, microsecond=0) + timedelta(minutes=1)
    deadline = candidate + timedelta(days=366)
    while candidate <= deadline:
        cron_weekday = (candidate.weekday() + 1) % 7  # Python Mon=0; cron Sun=0
        day_match = _day_matches(
            day=candidate.day,
            weekday=cron_weekday,
            days=days,
            weekdays=weekdays,
            dom_any=dom_any,
            dow_any=dow_any,
        )
        if (
            candidate.minute in minutes
            and candidate.hour in hours
            and candidate.month in months
            and day_match
        ):
            return candidate
        candidate += timedelta(minutes=1)
    raise CronExpressionError("cron expression did not match within one year")


def _day_matches(
    *,
    day: int,
    weekday: int,
    days: set[int],
    weekdays: set[int],
    dom_any: bool,
    dow_any: bool,
) -> bool:
    """Match cron day fields.

    When both day-of-month and day-of-week are restricted, cron treats them
    as OR. When either is "*", the restricted field alone controls matching.
    """
    if dom_any and dow_any:
        return True
    if dom_any:
        return weekday in weekdays
    if dow_any:
        return day in days
    return day in days or weekday in weekdays


def _parse_field(raw: str, min_value: int, max_value: int, label: str) -> set[int]:
    values: set[int] = set()
    for part in raw.split(","):
        part = part.strip()
        if not part:
            raise CronExpressionError(f"empty {label} field")
        step = 1
        if "/" in part:
            part, step_raw = part.split("/", 1)
            try:
                step = int(step_raw)
            except ValueError as e:
                raise CronExpressionError(f"{label} step must be an integer") from e
            if step < 1:
                raise CronExpressionError(f"{label} step must be >= 1")
        if part == "*":
            start, end = min_value, max_value
        elif "-" in part:
            start_raw, end_raw = part.split("-", 1)
            start = _parse_int(start_raw, label)
            end = _parse_int(end_raw, label)
            if start > end:
                raise CronExpressionError(f"{label} range start must be <= end")
        else:
            start = end = _parse_int(part, label)
        if start < min_value or end > max_value:
            raise CronExpressionError(
                f"{label} values must be between {min_value} and {max_value}",
            )
        values.update(range(start, end + 1, step))
    return values


def _parse_int(raw: str, label: str) -> int:
    try:
        return int(raw)
    except ValueError as e:
        raise CronExpressionError(f"{label} value must be an integer") from e

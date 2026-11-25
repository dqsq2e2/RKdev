"""Build dates and local daily version reservations."""

from datetime import datetime, timedelta, timezone
from pathlib import Path
import re
import os


BUILD_TIMEZONE = timezone(timedelta(hours=8))


def build_date():
    override = os.environ.get("RKDEV_BUILD_DATE")
    if override:
        return datetime.strptime(override, "%Y-%m-%d").date()
    return datetime.now(BUILD_TIMEZONE).date()


def allocate_version(output, day=None):
    day = day or build_date()
    prefix = f"{day.month}.{day.day}-r"
    output = Path(output)
    reservations = output / ".versions" / day.isoformat()
    reservations.mkdir(parents=True, exist_ok=True)
    existing = [int(match[1]) for image in output.glob("*.img")
                if (match := re.search(rf"-{re.escape(prefix)}([1-9][0-9]*)\.img$", image.name))]
    revision = max(existing, default=0) + 1
    while True:
        version = f"{prefix}{revision}"
        try:
            (reservations / version).touch(exist_ok=False)
            return version
        except FileExistsError:
            revision += 1

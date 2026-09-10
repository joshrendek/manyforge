"""Pure coordinated CalVer allocation. Callers acquire clocks and reservation state.

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import re
from collections.abc import Sequence
from datetime import datetime, timezone

_CANONICAL = re.compile(r"([1-9][0-9]{3})\.([1-9]|1[0-2])\.([1-9][0-9]*)\Z")
_GO = re.compile(r"v?1\.([1-9][0-9]{3})(0[1-9]|1[0-2])\.([1-9][0-9]*)\Z")


def parse_release(version: str) -> tuple[int, int, int]:
    """Validate canonical YYYY.M.N without accepting padded or prerelease forms."""
    match = _CANONICAL.fullmatch(version)
    if match is None:
        raise ValueError(f"invalid canonical SDK release ID: {version!r}")
    return tuple(int(part) for part in match.groups())


def canonical_reservation(version: str) -> str:
    """Normalize a registry Go version, or validate an already canonical ID."""
    match = _GO.fullmatch(version)
    if match is not None:
        year, month, sequence = (int(part) for part in match.groups())
        return f"{year}.{month}.{sequence}"
    parse_release(version)
    return version


def package_versions(release_version: str) -> dict[str, str]:
    """Map one calendar release to the four immutable package version strings."""
    year, month, sequence = parse_release(release_version)
    return {
        "python": release_version,
        "typescript": release_version,
        "java": release_version,
        "go": f"1.{year}{month:02d}.{sequence}",
    }


def next_release(now: datetime, reserved_versions: Sequence[str], open_candidate: str | None) -> str:
    """Allocate within UTC, reusing only a collision-free unmerged current candidate.

    A merged/incomplete candidate is handled first by the coordinator, never passed
    as open_candidate. Reserved identifiers remain reserved after partial failure.
    """
    if now.tzinfo is None or now.utcoffset() is None:
        raise ValueError("release allocation requires a timezone-aware clock")
    utc = now.astimezone(timezone.utc)
    period = (utc.year, utc.month)
    reservations = {parse_release(canonical_reservation(version)) for version in reserved_versions}
    if reservations and period < max(reservations)[:2]:
        raise ValueError("clock period precedes the latest reserved release period")
    if open_candidate is not None:
        candidate = parse_release(open_candidate)
        if candidate in reservations:
            raise ValueError("open candidate collides with a reservation; reconcile ownership before allocating")
        if candidate[:2] > period:
            raise ValueError("clock period precedes the open release candidate")
        if candidate[:2] == period:
            if any(version[:2] == period and version[2] > candidate[2] for version in reservations):
                raise ValueError("open candidate precedes a newer reservation; reconcile publication order")
            return open_candidate
    sequence = max((version[2] for version in reservations if version[:2] == period), default=0) + 1
    result = f"{utc.year}.{utc.month}.{sequence}"
    parse_release(result)
    return result

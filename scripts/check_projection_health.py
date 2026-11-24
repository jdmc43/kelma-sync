#!/usr/bin/env python3
"""Check global FSRS projection queue health without exposing account data."""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request


class MonitorError(RuntimeError):
    pass


def limit(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, str(default)))
    except ValueError:
        raise MonitorError(f"{name} must be an integer") from None


def main() -> int:
    base_url = os.environ.get("KELMA_SYNC_MONITOR_URL", "http://127.0.0.1:3210").rstrip("/")
    secret = os.environ.get("KELMA_SYNC_INTERNAL_SECRET", "").strip()
    if not secret:
        print("FSRS projection monitor: FAILED: internal secret is missing")
        return 1

    request = urllib.request.Request(
        base_url + "/v2/internal/projections",
        headers={"accept": "application/json", "x-internal-secret": secret},
        method="GET",
    )
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            metrics = json.loads(response.read())
    except urllib.error.HTTPError as error:
        error.read()
        print(f"FSRS projection monitor: FAILED: endpoint returned HTTP {error.code}")
        return 1
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        print(f"FSRS projection monitor: FAILED: {type(error).__name__}")
        return 1

    names = (
        "total", "pending", "running", "succeeded", "failed", "stale",
        "retry_exhausted", "oldest_pending_seconds", "oldest_running_seconds",
        "oldest_failed_seconds", "oldest_stale_seconds",
    )
    try:
        values = {name: int(metrics.get(name, 0) or 0) for name in names}
        thresholds = {
            "pending": limit("FSRS_MAX_PENDING", 10_000),
            "failed": limit("FSRS_MAX_FAILED", 0),
            "retry_exhausted": limit("FSRS_MAX_RETRY_EXHAUSTED", 0),
            "oldest_pending_seconds": limit("FSRS_MAX_PENDING_AGE_SECONDS", 300),
            "oldest_running_seconds": limit("FSRS_MAX_RUNNING_AGE_SECONDS", 600),
            "oldest_stale_seconds": limit("FSRS_MAX_STALE_AGE_SECONDS", 900),
        }
    except (TypeError, ValueError, MonitorError) as error:
        print(f"FSRS projection monitor: FAILED: {error}")
        return 1

    print(
        "FSRS projection monitor:",
        " ".join(f"{name}={values[name]}" for name in names),
    )
    violations = [
        f"{name}={values[name]} exceeds {maximum}"
        for name, maximum in thresholds.items()
        if values[name] > maximum
    ]
    if violations:
        print("FSRS projection monitor: FAILED: " + "; ".join(violations))
        return 1
    print("FSRS projection monitor: OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

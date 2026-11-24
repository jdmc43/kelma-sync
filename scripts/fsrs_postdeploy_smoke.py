#!/usr/bin/env python3
"""Credential-safe live acceptance test for the FSRS projection pipeline.

The script mints a short-lived bearer token through the trusted internal API,
uses a dedicated synthetic account, and never prints tokens, secrets, request
bodies, response bodies, or card contents.
"""
from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
from typing import Any

BASE_URL = os.environ.get("KELMA_SYNC_SMOKE_URL", "http://127.0.0.1:3210").rstrip("/")
INTERNAL_SECRET = os.environ.get("KELMA_SYNC_INTERNAL_SECRET", "").strip()
REVISION = os.environ.get("DEPLOY_REVISION", "local")[:16]
SMOKE_EMAIL = os.environ.get(
    "KELMA_SYNC_SMOKE_EMAIL", "kelma-sync-fsrs-smoke@kelma.invalid"
).strip().lower()
CARD_ID = 9_223_000_000_000_001
NOTE_GUID = "kelma-fsrs-postdeploy-smoke"
DECK_NAME = "Kelma FSRS Smoke"


class SmokeError(RuntimeError):
    pass


def request(
    method: str,
    path: str,
    body: Any = None,
    *,
    token: str = "",
    internal: bool = False,
    force: bool = False,
    timeout: float = 30.0,
) -> Any:
    headers = {"accept": "application/json"}
    data = None
    if body is not None:
        data = json.dumps(body, separators=(",", ":")).encode("utf-8")
        headers["content-type"] = "application/json"
    if token:
        headers["authorization"] = f"Bearer {token}"
    if internal:
        headers["x-internal-secret"] = INTERNAL_SECRET
    if force:
        headers["Force-Override"] = "true"
    req = urllib.request.Request(BASE_URL + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
    except urllib.error.HTTPError as error:
        error.read()
        raise SmokeError(f"{method} {path} returned HTTP {error.code}") from None
    except OSError as error:
        raise SmokeError(f"{method} {path} failed: {type(error).__name__}") from None
    if not raw:
        return None
    try:
        return json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise SmokeError(f"{method} {path} returned invalid JSON") from None


def utc_timestamp(offset_seconds: int = 0) -> str:
    value = datetime.now(timezone.utc) + timedelta(seconds=offset_seconds)
    return value.isoformat().replace("+00:00", "Z")


def push_reviews(token: str, reviews: list[dict[str, Any]]) -> None:
    response = request(
        "POST",
        "/v2/batch/push",
        {
            "notes": [],
            "cards": [],
            "reviews": reviews,
            "study_days": [],
            "notetypes": [],
            "decks": [],
        },
        token=token,
    )
    conflicts = (response.get("conflicts") or {}).get("reviews") or []
    if conflicts:
        raise SmokeError("immutable review push reported a collision")


def review(review_id: int, ease: int) -> dict[str, Any]:
    return {
        "review_id": review_id,
        "source_card_id": CARD_ID,
        "note_guid": NOTE_GUID,
        "card_ord": 0,
        "deck_name": DECK_NAME,
        "ease": ease,
        "interval": 0,
        "last_interval": 0,
        "factor": 0,
        "taken_millis": 800,
        "review_kind": 0,
    }


def wait_projection(
    token: str,
    *,
    through_review_id: int,
    profile_version: int | None = None,
    timeout: float = 45.0,
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last_status = "missing"
    while time.monotonic() < deadline:
        card = request("GET", f"/v2/cards/{CARD_ID}", token=token)
        state = card.get("projection") or {}
        last_status = str(state.get("status") or "missing")
        if last_status == "failed":
            code = str(state.get("last_error_code") or "projection_failed")
            raise SmokeError(f"projection failed with sanitized code {code}")
        applied_profile = state.get("applied_profile_version")
        if (
            last_status == "succeeded"
            and int(state.get("desired_through_review_id") or 0) == through_review_id
            and int(state.get("applied_through_review_id") or 0) == through_review_id
            and state.get("desired_history_hash") == state.get("applied_history_hash")
            and (profile_version is None or int(applied_profile or -1) == profile_version)
        ):
            return card
        time.sleep(0.25)
    raise SmokeError(f"projection did not converge; last status was {last_status}")


def apply_profile_version(token: str) -> int:
    current = request("GET", "/v2/scheduler-profile", token=token)["profile"]
    nonce = int(time.time() * 1000)
    candidate = {
        "base_profile_version": int(current["version"]),
        "idempotency_key": f"postdeploy-{REVISION}-{nonce}",
        "parameters": current["parameters"],
        "parameter_source": "manual",
        "desired_retention": current["desired_retention"],
        "retention_source": "manual",
        "maximum_interval": current["maximum_interval"],
        "learning_steps_seconds": current["learning_steps_seconds"],
        "relearning_steps_seconds": current["relearning_steps_seconds"],
        "enable_short_term": True,
        "enable_fuzzing": False,
        "quality_metrics": {"post_deploy_smoke": True},
    }
    first = request("PUT", "/v2/scheduler-profile", candidate, token=token)
    second = request("PUT", "/v2/scheduler-profile", candidate, token=token)
    first_version = int(first["profile"]["version"])
    if int(second["profile"]["version"]) != first_version:
        raise SmokeError("profile idempotency retry created another version")
    return first_version


def wait_health(timeout: float = 60.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if request("GET", "/health", timeout=2.0) == {"ok": True}:
                return
        except SmokeError:
            pass
        time.sleep(0.5)
    raise SmokeError("health endpoint was not ready")


def run() -> None:
    if not INTERNAL_SECRET:
        raise SmokeError("KELMA_SYNC_INTERNAL_SECRET is required")

    wait_health()

    minted = request(
        "POST",
        "/v2/internal/token",
        {"email": SMOKE_EMAIL, "isPaid": True},
        internal=True,
    )
    token = str(minted.get("hkey") or "")
    if not token:
        raise SmokeError("internal token mint returned no token")

    try:
        request(
            "PUT",
            f"/v2/notes/{NOTE_GUID}",
            {
                "notetype_id": 1,
                "fields": ["projection smoke", "projection smoke"],
                "tags": ["kelma-fsrs-smoke"],
                "client_modified_at": utc_timestamp(),
                "base_checksum": "",
            },
            token=token,
            force=True,
        )
        request(
            "PUT",
            f"/v2/cards/{CARD_ID}",
            {
                "note_guid": NOTE_GUID,
                "deck_name": DECK_NAME,
                "ord": 0,
                "scheduling": {"type": 2, "queue": 2, "ivl": 9999},
                "client_modified_at": utc_timestamp(),
            },
            token=token,
        )

        base = int(time.time() * 1000) + (time.time_ns() % 997)
        first = review(base, 3)
        last = review(base + 600_000, 3)
        push_reviews(token, [first, last])
        projected = wait_projection(token, through_review_id=last["review_id"])
        first_hash = projected["projection"]["applied_history_hash"]

        push_reviews(token, [first, last])
        duplicate_state = request("GET", f"/v2/cards/{CARD_ID}", token=token)
        if (duplicate_state.get("projection") or {}).get("status") != "succeeded":
            raise SmokeError("identical immutable review retry requeued projection")

        push_reviews(token, [review(base + 300_000, 1)])
        projected = wait_projection(token, through_review_id=last["review_id"])
        if projected["projection"]["applied_history_hash"] == first_hash:
            raise SmokeError("out-of-order review did not change history identity")
        if int(projected["scheduling"].get("reps") or 0) < 3:
            raise SmokeError("out-of-order review was not replayed")

        profile_version = apply_profile_version(token)
        projected = wait_projection(
            token,
            through_review_id=last["review_id"],
            profile_version=profile_version,
        )
        managed_interval = int(projected["scheduling"].get("ivl") or 0)

        request(
            "PUT",
            f"/v2/cards/{CARD_ID}",
            {
                "note_guid": NOTE_GUID,
                "deck_name": DECK_NAME,
                "ord": 0,
                "scheduling": {"type": 2, "queue": 2, "ivl": 9999},
                "client_modified_at": utc_timestamp(120),
            },
            token=token,
        )
        managed = request("GET", f"/v2/cards/{CARD_ID}", token=token)
        if int(managed["scheduling"].get("ivl") or 0) != managed_interval:
            raise SmokeError("legacy scheduling upload replaced managed projection")

        summary = request("GET", "/v2/summary/card-states", token=token)
        state = next((item for item in summary.get("cards", []) if item.get("card_id") == CARD_ID), None)
        if not state or state.get("projection_status") != "succeeded":
            raise SmokeError("successfully applied projection missing from summary")

        manifest = request("GET", "/v2/sync/manifest", token=token)
        entry = next((item for item in manifest.get("cards", []) if item.get("card_id") == CARD_ID), None)
        if (
            not entry
            or entry.get("projection_status") != "succeeded"
            or entry.get("desired_history_hash") != entry.get("applied_history_hash")
            or int(entry.get("applied_profile_version") or -1) != profile_version
        ):
            raise SmokeError("manifest projection identity did not converge")

        account = request("GET", "/v2/scheduler-profile", token=token)
        counts = account.get("projections") or {}
        if int(counts.get("pending") or 0) or int(counts.get("failed") or 0) or int(counts.get("stale") or 0):
            raise SmokeError("dedicated smoke account retained incomplete projections")

        deleted = request(
            "POST",
            "/v2/internal/account-deletion",
            {"email": SMOKE_EMAIL, "confirm": "DELETE"},
            internal=True,
        )
        if deleted.get("ok") is not True:
            raise SmokeError("smoke account deletion did not complete")
        token = ""
    finally:
        if token:
            try:
                request("POST", "/v2/auth/logout", token=token)
            except SmokeError:
                pass


def main() -> int:
    print("FSRS post-deployment smoke: starting")
    try:
        run()
    except SmokeError as error:
        print(f"FSRS post-deployment smoke: FAILED: {error}")
        return 1
    except Exception as error:
        print(f"FSRS post-deployment smoke: FAILED: {type(error).__name__}")
        return 1
    print("FSRS post-deployment smoke: OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3
"""Compare pinned py-fsrs with Kotlin projections for schedule-only real history."""

from __future__ import annotations

import json
import math
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from fsrs import Card, Rating, Scheduler, State
from fsrs.scheduler import DEFAULT_PARAMETERS


def at(millis: int) -> datetime:
    return datetime.fromtimestamp(millis / 1000, tz=timezone.utc)


def millis(value: datetime) -> int:
    return round(value.timestamp() * 1000)


def close(actual: float | None, expected: float) -> bool:
    return actual is not None and math.isclose(
        actual,
        expected,
        rel_tol=1e-12,
        abs_tol=1e-12,
    )


def compare_case(index: int, value: dict[str, Any], scheduler: Scheduler) -> int:
    card_id = int(value["cardId"])
    reviews = value["reviews"]
    expected = value["expected"]
    if not reviews or int(expected["cardId"]) != card_id:
        raise AssertionError(f"case {index} has invalid identity or empty history")

    first_review = int(reviews[0]["reviewId"])
    card = Card(card_id=card_id, due=at(first_review))
    lapses = 0
    prior_review_id = 0
    for review_index, review in enumerate(reviews):
        review_id = int(review["reviewId"])
        rating = Rating(int(review["rating"]))
        if review_id <= prior_review_id:
            raise AssertionError(f"case {index} reviews are not strictly ordered")
        if card.state == State.Review and rating == Rating.Again:
            lapses += 1
        card, _ = scheduler.review_card(
            card=card,
            rating=rating,
            review_datetime=at(review_id),
        )
        prior_review_id = review_id

    if card.state.name != expected["phase"] or card.step != expected["step"]:
        raise AssertionError(f"case {index} state/step mismatch")
    if card.last_review is None:
        raise AssertionError(f"case {index} has no final review time")
    if millis(card.due) != int(expected["dueAtMillis"]) or millis(card.last_review) != int(
        expected["lastReviewAtMillis"]
    ):
        raise AssertionError(f"case {index} timestamp mismatch")
    if not close(card.stability, float(expected["stability"])) or not close(
        card.difficulty,
        float(expected["difficulty"]),
    ):
        raise AssertionError(f"case {index} memory-state mismatch")

    scheduled_days = (card.due - card.last_review).days
    if (
        scheduled_days != int(expected["scheduledDays"])
        or len(reviews) != int(expected["repetitions"])
        or lapses != int(expected["lapses"])
    ):
        raise AssertionError(f"case {index} counters/interval mismatch")
    return len(reviews)


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("usage: compare_real_fixture.py FIXTURE REPORT")

    fixture_path = Path(sys.argv[1])
    report_path = Path(sys.argv[2])
    started_at = time.perf_counter()
    scheduler = Scheduler(
        parameters=DEFAULT_PARAMETERS,
        desired_retention=0.9,
        learning_steps=(timedelta(minutes=1), timedelta(minutes=10)),
        relearning_steps=(timedelta(minutes=10),),
        maximum_interval=36_500,
        enable_fuzzing=False,
    )

    with fixture_path.open("r", encoding="utf-8") as source:
        header_line = source.readline()
        if not header_line:
            raise AssertionError("fixture has no header")
        header = json.loads(header_line)
        if (
            int(header.get("schemaVersion", 0)) != 1
            or int(header.get("caseCount", 0)) <= 0
            or int(header.get("reviewCount", 0)) <= 0
            or len(str(header.get("sourceFingerprint", ""))) != 64
        ):
            raise AssertionError("invalid fixture header")

        case_count = 0
        review_count = 0
        for line in source:
            if line.strip():
                review_count += compare_case(case_count, json.loads(line), scheduler)
                case_count += 1

    if case_count != int(header["caseCount"]) or review_count != int(
        header["reviewCount"]
    ):
        raise AssertionError("fixture aggregate mismatch")

    report_path.write_text(
        "\n".join(
            (
                "result=passed",
                f"source_fingerprint_sha256={header['sourceFingerprint']}",
                f"replayed_reviews={review_count}",
                f"projected_schedules={case_count}",
                "projection_mismatches=0",
                f"python_replay_ms={round((time.perf_counter() - started_at) * 1000)}",
                "",
            )
        ),
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()

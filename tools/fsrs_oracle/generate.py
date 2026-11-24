#!/usr/bin/env python3
"""Generate deterministic FSRS-6 scheduler vectors from pinned py-fsrs."""

from __future__ import annotations

import json
import sys
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from importlib.metadata import version
from pathlib import Path
from typing import Any

from fsrs import Card, Rating, Scheduler, State
from fsrs.scheduler import DEFAULT_PARAMETERS

BASE_MILLIS = 1_735_689_600_000  # 2025-01-01T00:00:00Z
DAY_MILLIS = 86_400_000
DEFAULT_WEIGHTS = list(DEFAULT_PARAMETERS)


@dataclass(frozen=True)
class Config:
    identifier: str
    parameters: list[float]
    desired_retention: float = 0.9
    learning_steps_seconds: tuple[int, ...] = (60, 600)
    relearning_steps_seconds: tuple[int, ...] = (600,)
    maximum_interval: int = 36_500

    def scheduler(self) -> Scheduler:
        return Scheduler(
            parameters=self.parameters,
            desired_retention=self.desired_retention,
            learning_steps=tuple(
                timedelta(seconds=value) for value in self.learning_steps_seconds
            ),
            relearning_steps=tuple(
                timedelta(seconds=value) for value in self.relearning_steps_seconds
            ),
            maximum_interval=self.maximum_interval,
            enable_fuzzing=False,
        )

    def fixture_value(self) -> dict[str, Any]:
        return {
            "id": self.identifier,
            "parameters": self.parameters,
            "desiredRetention": self.desired_retention,
            "learningStepsSeconds": list(self.learning_steps_seconds),
            "relearningStepsSeconds": list(self.relearning_steps_seconds),
            "maximumInterval": self.maximum_interval,
            "enableFuzzing": False,
        }


def at(millis: int) -> datetime:
    return datetime.fromtimestamp(millis / 1000, tz=timezone.utc)


def millis(value: datetime | None) -> int | None:
    if value is None:
        return None
    return round(value.timestamp() * 1000)


def card_value(card: Card) -> dict[str, Any]:
    return {
        "cardId": card.card_id,
        "state": card.state.name,
        "step": card.step,
        "stability": card.stability,
        "difficulty": card.difficulty,
        "dueAtMillis": millis(card.due),
        "lastReviewAtMillis": millis(card.last_review),
    }


def new_card(card_id: int) -> Card:
    return Card(card_id=card_id, due=at(BASE_MILLIS))


def review_card(
    card_id: int,
    *,
    stability: float,
    difficulty: float,
    last_review_millis: int = BASE_MILLIS,
    due_millis: int = BASE_MILLIS,
) -> Card:
    return Card(
        card_id=card_id,
        state=State.Review,
        step=None,
        stability=stability,
        difficulty=difficulty,
        due=at(due_millis),
        last_review=at(last_review_millis),
    )


def event(review_id: int, offset_millis: int, rating: Rating) -> dict[str, Any]:
    return {
        "reviewId": review_id,
        "reviewedAtMillis": BASE_MILLIS + offset_millis,
        "rating": rating.name,
    }


def make_case(
    name: str,
    config: Config,
    initial: Card,
    events: list[dict[str, Any]],
    *,
    canonicalize: bool = True,
) -> dict[str, Any]:
    scheduler = config.scheduler()
    current = initial
    ordered = sorted(
        events,
        key=lambda item: (item["reviewedAtMillis"], item["reviewId"]),
    ) if canonicalize else list(events)
    expected: list[dict[str, Any]] = []

    for item in ordered:
        reviewed_at = at(item["reviewedAtMillis"])
        before = scheduler.get_card_retrievability(
            card=current,
            current_datetime=reviewed_at,
        )
        current, _ = scheduler.review_card(
            card=current,
            rating=Rating[item["rating"]],
            review_datetime=reviewed_at,
        )
        after_at_due = scheduler.get_card_retrievability(
            card=current,
            current_datetime=current.due,
        )
        expected.append(
            {
                "reviewId": item["reviewId"],
                "retrievabilityBefore": before,
                "card": card_value(current),
                "retrievabilityAtDue": after_at_due,
            }
        )

    return {
        "name": name,
        "configurationId": config.identifier,
        "initialCard": card_value(initial),
        "reviews": events,
        "canonicalReviewOrder": [item["reviewId"] for item in ordered],
        "expected": expected,
    }


def changed_weights(**values: float) -> list[float]:
    result = list(DEFAULT_WEIGHTS)
    for raw_index, value in values.items():
        result[int(raw_index.removeprefix("w"))] = value
    return result


def configurations() -> list[Config]:
    return [
        Config("default", list(DEFAULT_WEIGHTS)),
        Config("retention-070", list(DEFAULT_WEIGHTS), desired_retention=0.70),
        Config("retention-080", list(DEFAULT_WEIGHTS), desired_retention=0.80),
        Config("retention-095", list(DEFAULT_WEIGHTS), desired_retention=0.95),
        Config("retention-099", list(DEFAULT_WEIGHTS), desired_retention=0.99),
        Config(
            "no-learning-steps",
            list(DEFAULT_WEIGHTS),
            learning_steps_seconds=(),
        ),
        Config(
            "no-relearning-steps",
            list(DEFAULT_WEIGHTS),
            relearning_steps_seconds=(),
        ),
        Config("maximum-30", list(DEFAULT_WEIGHTS), maximum_interval=30),
        Config("round-to-even-2.5", changed_weights(w3=2.5)),
        Config("round-to-even-3.5", changed_weights(w3=3.5)),
        Config(
            "custom-parameters",
            changed_weights(
                w0=0.35,
                w1=1.6,
                w2=3.1,
                w3=9.2,
                w4=5.8,
                w7=0.1,
                w10=1.1,
                w16=2.2,
                w20=0.22,
            ),
            desired_retention=0.87,
            learning_steps_seconds=(120, 900),
            relearning_steps_seconds=(420,),
            maximum_interval=12_345,
        ),
    ]


def build_cases(configs: dict[str, Config]) -> list[dict[str, Any]]:
    cases: list[dict[str, Any]] = []
    sequence = 1

    def add(
        name: str,
        config_id: str,
        initial: Card,
        events: list[dict[str, Any]],
    ) -> None:
        nonlocal sequence
        cases.append(make_case(name, configs[config_id], initial, events))
        sequence += 1

    # Every initial rating under the default and retention configurations.
    for config_id in (
        "default",
        "retention-070",
        "retention-080",
        "retention-095",
        "retention-099",
        "custom-parameters",
    ):
        for rating in Rating:
            card_id = 10_000 + sequence
            add(
                f"{config_id}-initial-{rating.name.lower()}",
                config_id,
                new_card(card_id),
                [event(1_000_000 + sequence, 0, rating)],
            )

    # Dense review-state matrix catches formula, day, and rating differences.
    elapsed_offsets = (
        0,
        60_000,
        DAY_MILLIS - 60_000,
        DAY_MILLIS,
        DAY_MILLIS + DAY_MILLIS // 2,
        2 * DAY_MILLIS - 60_000,
        2 * DAY_MILLIS,
        30 * DAY_MILLIS,
    )
    for stability in (0.001, 0.212, 2.3065, 5.0, 10.0, 100.0):
        for difficulty in (1.0, 5.0, 10.0):
            for elapsed in elapsed_offsets:
                for rating in Rating:
                    card_id = 20_000 + sequence
                    add(
                        "review-matrix-"
                        f"s{stability}-d{difficulty}-t{elapsed}-{rating.name.lower()}",
                        "default",
                        review_card(
                            card_id,
                            stability=stability,
                            difficulty=difficulty,
                        ),
                        [event(2_000_000 + sequence, elapsed, rating)],
                    )

    # Retention sensitivity without duplicating the entire matrix.
    for config_id in (
        "retention-070",
        "retention-080",
        "retention-095",
        "retention-099",
    ):
        for stability in (2.3065, 10.0):
            for elapsed in (DAY_MILLIS, 7 * DAY_MILLIS, 45 * DAY_MILLIS):
                for rating in Rating:
                    card_id = 30_000 + sequence
                    add(
                        f"{config_id}-s{stability}-t{elapsed}-{rating.name.lower()}",
                        config_id,
                        review_card(
                            card_id,
                            stability=stability,
                            difficulty=5.0,
                        ),
                        [event(3_000_000 + sequence, elapsed, rating)],
                    )

    # Stateful trajectories exercise learning, graduation, and relearning.
    add(
        "learning-graduation-and-lapse",
        "default",
        new_card(40_001),
        [
            event(4_000_001, 0, Rating.Good),
            event(4_000_002, 10 * 60_000, Rating.Good),
            event(4_000_003, 3 * DAY_MILLIS, Rating.Again),
            event(4_000_004, 3 * DAY_MILLIS + 10 * 60_000, Rating.Good),
            event(4_000_005, 8 * DAY_MILLIS, Rating.Easy),
        ],
    )
    add(
        "learning-again-hard-good-easy",
        "default",
        new_card(40_002),
        [
            event(4_000_010, 0, Rating.Again),
            event(4_000_011, 60_000, Rating.Hard),
            event(4_000_012, 6 * 60_000, Rating.Good),
            event(4_000_013, 16 * 60_000, Rating.Easy),
        ],
    )
    add(
        "empty-learning-steps",
        "no-learning-steps",
        new_card(40_003),
        [event(4_000_020, 0, Rating.Good)],
    )
    add(
        "empty-relearning-steps",
        "no-relearning-steps",
        review_card(40_004, stability=8.0, difficulty=6.0),
        [event(4_000_030, 12 * DAY_MILLIS, Rating.Again)],
    )
    add(
        "maximum-interval-cap",
        "maximum-30",
        review_card(40_005, stability=100.0, difficulty=1.0),
        [event(4_000_040, 365 * DAY_MILLIS, Rating.Easy)],
    )
    add(
        "python-round-to-even-lower",
        "round-to-even-2.5",
        new_card(40_006),
        [event(4_000_050, 0, Rating.Easy)],
    )
    add(
        "python-round-to-even-upper",
        "round-to-even-3.5",
        new_card(40_007),
        [event(4_000_060, 0, Rating.Easy)],
    )

    # Input is intentionally unsorted; expected replay uses timestamp then ID.
    add(
        "out-of-order-canonical-replay",
        "default",
        new_card(40_008),
        [
            event(4_000_073, 4 * DAY_MILLIS, Rating.Easy),
            event(4_000_071, 0, Rating.Good),
            event(4_000_072, 10 * 60_000, Rating.Good),
        ],
    )
    add(
        "same-timestamp-review-id-tie-break",
        "default",
        new_card(40_009),
        [
            event(4_000_083, 0, Rating.Good),
            event(4_000_081, 0, Rating.Again),
            event(4_000_084, 0, Rating.Easy),
            event(4_000_082, 0, Rating.Hard),
        ],
    )

    return cases


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: generate.py OUTPUT")

    config_list = configurations()
    config_by_id = {config.identifier: config for config in config_list}
    cases = build_cases(config_by_id)
    fixture = {
        "schemaVersion": 1,
        "oracle": {
            "package": "fsrs",
            "packageVersion": version("fsrs"),
            "algorithmVersion": "FSRS-6",
            "releaseCommit": "3abe686e9c058d3f3c00bbeb92e68b71211b2b31",
            "license": "MIT",
            "wheelSha256": "ac1bf9939573592d8c9bc1e11a00bd17e04146dc9f2c913127e2bcc431b9040b",
            "pythonVersion": "3.13.12",
            "containerImage": "python:3.13.12-slim",
            "containerDigest": "sha256:f1927c75e81efd1e091dbd64b6c0ecaa5630b38635a3d1c04034ac636e1f94c8",
            "containerPlatform": "linux/amd64",
        },
        "canonicalSemantics": {
            "timezone": "UTC",
            "reviewOrder": ["reviewedAtMillis", "reviewId"],
            "elapsedDays": "max(0, (reviewed_at - last_review).days)",
            "intervalRounding": "Python round (ties to even)",
        },
        "baseTimeMillis": BASE_MILLIS,
        "configurations": [config.fixture_value() for config in config_list],
        "cases": cases,
        "caseCount": len(cases),
    }

    output = Path(sys.argv[1])
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(fixture, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {len(cases)} cases to {output}")


if __name__ == "__main__":
    main()

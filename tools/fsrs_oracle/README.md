# FSRS Python oracle

This development-only tool generates deterministic FSRS-6 scheduling fixtures
from `py-fsrs==6.3.1`. Production KelmaSync does not contain or execute Python.

Generate the fixture with:

```bash
./tools/fsrs_oracle/generate.sh
```

The script builds the OCI-digest-pinned image for canonical `linux/amd64`,
installs wheels pinned by SHA-256, and writes:

```text
internal/fsrs/testdata/py-fsrs-6.3.1-vectors.json
```

The pinned `linux/amd64` container output defines the canonical fixture bytes.
A different architecture or host Python/libm build can format a transcendental
result one ULP differently and is therefore only an inspection aid. Routine Go
tests use the committed fixture and do not require Docker or Python.

## Real-history three-way parity

The Kotlin client repository provides an opt-in read-only acceptance wrapper:

```bash
./scripts/run-real-collection-fsrs-acceptance.sh /path/to/collection.anki2
```

It emits a temporary schedule-only NDJSON fixture after validating Kotlin's
persistent projection. This image runs `compare_real_fixture.py` against that
fixture, and `internal/fsrs/real_collection_acceptance_test.go` independently
runs the Go scheduler against the same input. Python and Go compare every final
projection with Kotlin using exact state, step, timestamp, interval, and counter
checks plus the canonical `1e-12` floating-point tolerance. Inputs and reports
contain no note content, media, deck names, credentials, or source scheduling
state, and the wrapper deletes them when complete.

Review fixture changes as algorithm changes. Do not edit expected values by
hand. A Python/package/container upgrade must use a new fixture filename and
must update `internal/fsrs/UPSTREAM.md` and the parity-test metadata.

The fixture intentionally covers completed-24-hour versus UTC-calendar-day
behavior and Python's ties-to-even interval rounding because existing Go ports
may use different semantics.

# Upstream provenance

The KelmaSync FSRS package is a focused adaptation of permissively licensed
Open Spaced Repetition implementations.

## Primary Go source

- Repository: <https://github.com/open-spaced-repetition/go-fsrs>
- Tag: `v4.0.0`
- Commit: `61159b9b3891b1279b63206afef4086d7ef92992`
- License: MIT
- Copyright: 2022 open-spaced-repetition

The complete upstream notice is retained in `../../THIRD_PARTY_NOTICES.md`.
The focused adaptation is organized as follows:

| Kelma file | Principal upstream references |
|---|---|
| `models.go` | `models.go` |
| `parameters.go` | `parameters.go`, `weights.go`, `validate.go` |
| `algorithm.go` | `arithmetic.go`, `memory.go` |
| `scheduler.go` | `scheduler.go`, `scheduler_basic.go`, `steps.go` |
| `replay.go` | `reschedule.go` |

Kelma uses Python-compatible completed-24-hour elapsed days and ties-to-even
interval rounding, adds immutable review-ID ordering/deduplication, and rejects
fuzzing for server projections. These differences are locked by the oracle
fixture rather than inherited silently from either upstream implementation.

## Behavioral oracle

- Repository: <https://github.com/open-spaced-repetition/py-fsrs>
- Package: `fsrs==6.3.1`
- Release commit: `3abe686e9c058d3f3c00bbeb92e68b71211b2b31`
- License: MIT
- Copyright: 2022 Open Spaced Repetition

Kelma intentionally follows the pinned Python scheduler for elapsed-day,
rounding, state-transition, and interval behavior. Differences from the primary
Go source are covered by committed oracle fixtures and documented here during
the port.

## Production boundary

No Python package is imported, executed, or installed by the production
KelmaSync binary or container. Python is used only by the fixture generator in
`tools/fsrs_oracle`.

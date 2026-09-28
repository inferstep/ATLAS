# ADR 0005: Lens is optional and calibration-gated

Status: superseded by 0011 (2026-09)

## Context
Lens scores are model-relative. Uncalibrated or missing artifacts must
not steer selection with wrong numbers.

## Decision
Three explicit states, all surfaced identically in health, doctor,
lens check, and the TUI badge: loaded+calibrated (full interventions),
loaded+uncalibrated (raw telemetry, neutral normalized scores,
threshold interventions disabled), and disabled (identity/dim mismatch
or missing artifacts — scoring returns explicit unavailable flags).
Scoring never silently substitutes defaults for calibrated output.

## Consequences
A fresh install without `atlas lens build` runs uncalibrated and says
so everywhere. The router's energy signal logs once per load cycle when
disabled. V3 candidate selection degrades to neutral-score ordering.

The same principle governs ASA activation: the `.model` marker gates
steering at boot on model *identity*, but Supported status additionally
requires a measured A/B effect + bounded quality regression
(SUPPORT_MATRIX § Feature paths). A structurally-correct vector whose effect is
unmeasured on a given model stays **off by default** (marker withheld)
— activating an unvalidated steering vector by default is the
"default-on without measured value" anti-pattern. Reference model
(Qwen3.5-9B): A/B-validated, marker present, Supported. New models
(gemma): one opt-in command (`atlas asa build`) away, Preview until the
A/B runs.

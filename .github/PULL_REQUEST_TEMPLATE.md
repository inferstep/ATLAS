## What this changes

<!-- One or two sentences: the behavior before, the behavior after. -->

## What users will notice

<!-- What a user of ATLAS sees differently after this change: plain sentences or a list
     (no table, no code block, no list inside a list). The changelog of the release is made
     from this part, so do not edit CHANGELOG.md. If users notice nothing, write only: Nothing. -->

## Why

<!-- The problem this addresses. -->

Closes #<!-- the issue this delivers; small fixes under ~50 lines may skip this -->

## How it was verified

<!-- Tests run (and their output), or manual steps taken. "It should work" doesn't count. -->

**Hardware:** <!-- backend + GPU it ran on (e.g. CUDA, RTX 4090), or "untested on hardware" -->

## Checklist

- [ ] Targets `dev` (changes flow dev → staging → main)
- [ ] Title is a conventional commit with the component as scope, e.g. `fix(proxy): …` or `docs(setup): …`
- [ ] `python scripts/production-readiness.py` passes
- [ ] Matching `docs/*.md` updated for any behavior change
- [ ] New behavior covered by a test, or a note on why not
- [ ] Works model-agnostically (no model-name/dimension/token assumptions outside Lens/ASA artifacts)

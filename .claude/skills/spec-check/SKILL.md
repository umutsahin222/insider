---
name: spec-check
description: Check the running system against the assessment specification - brings the compose stack up, runs the acceptance suite (make acceptance), and maps every failure to the requirement it covers after deciding whether the product or the test expectation is wrong.
disable-model-invocation: true
---

# /spec-check

Requirement map: `test/acceptance/requirements.md`. Each acceptance test name carries the ID of
the requirement it verifies (`TestREQ_<AREA>_<Name>`).

1. **Stack.** Check that Docker runs (`docker info`). If it does not, say so and stop. If
   `docker compose ps` does not show every service healthy, run `make up`; it waits for health
   checks.
2. **Run** `make acceptance`. It targets the stack on localhost and takes about two minutes.
3. **For each failing test, investigate before judging.** Decide whether the **product** or
   the **test expectation** is wrong:
   - Read the test and the code path it exercises. Query the database or the mock provider's
     `/stats` for the raw facts.
   - Check documented decisions first. For example, a malformed path ID returning `404` is
     deliberate, and a notification dead-lettered after five genuinely failed attempts is
     correct behaviour.
   - Measure at the right point. Rate limits are measured where the provider receives
     requests (mock `peak_per_second`), not at database write time.

   In this repository's first acceptance run, three of four failures were wrong
   expectations. Do not "fix" the product to satisfy a wrong test, or the reverse.
4. **Report a table:** requirement, test, result, and for failures the verdict (product bug,
   test bug or environment) with evidence. Propose fixes, but make them only when the developer
   agrees.

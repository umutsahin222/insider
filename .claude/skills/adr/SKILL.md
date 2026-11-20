---
name: adr
description: Record an architecture decision as the next numbered ADR in docs/adr (context, decision, consequences) and add it to the README's ADR table. Use when a change involves a non-obvious design choice or revises an earlier decision.
argument-hint: "<decision title>"
---

# /adr

Title: `$ARGUMENTS`

Existing ADRs:
!`ls docs/adr`

1. **Revise or add?** If the decision changes an existing ADR, revise that file instead of adding
   a new one. Update its status line to `Accepted (revised <YYYY-MM-DD>: <what changed>)` and
   keep the history of what was replaced and why.
2. **Number:** the next free three-digit number. The file name is
   `docs/adr/NNN-<kebab-case-title>.md`.
3. **Template.** Keep it short; existing ADRs are 30–60 lines.
   ```markdown
   # ADR-NNN: <Title>

   - Status: Accepted
   - Date: <YYYY-MM-DD>

   ## Context
   <The problem and the constraints: what forces a decision. Cite requirements of the
   assessment specification where they apply.>

   ## Decision
   <What was chosen, stated concretely enough to check in code. Name the alternatives considered
   and why they lost.>

   ## Consequences
   - + <benefit>
   - − <cost, limit or risk; say how it would be revisited>
   ```
4. **Measurements, if any:** results go in with the method ("measured with … at …"). Never
   state numbers that were not measured.
5. **README:** add a row to the ADR table in `README.md`, linking the file with a one-line
   summary.
6. **Pointers:** if the decision constrains a package, add one line to that directory's
   `CLAUDE.md`, so Claude meets the rule when it works there.

# ADR-009: Message templates

- Status: Accepted
- Date: 2026-11-19

## Context
The bonus "template system with variable substitution" raises three questions: when to render,
how to keep validation meaningful, and how to keep user-supplied templates safe to execute.

## Decision
- **Render at creation, store the result.** The rendered text becomes the notification's
  `content` (plus `template_id` for traceability). Workers never render; retries send exactly what
  was accepted; the idempotency fingerprint (ADR-006) covers rendered content and template ID, so
  the same template + variables replay correctly without hashing variables separately.
- **Validate the rendered text** exactly like literal content (e.g. the SMS segment limit).
  Unknown template, channel mismatch, missing variable (`missingkey=error`) and content+template
  together are 422 field errors (`template.name`, `template.variables`, indexed as
  `items[i].template.*` in batches), reported together with every other problem. A missing
  template is not a 404: the resource being created is the notification, not the template.
- **Templates are immutable** (no update endpoint): a notification's `template_id` keeps meaning
  what it rendered. A changed message is a new template name.
- **One lookup per request:** the API collects the distinct template names of a request (up to
  1000 items) and loads them in one query; the domain renders.
- **Safe execution of user-supplied bodies: a restricted subset of `text/template`.** Templates
  are written by API callers and run inside the API request, so rendering must be bounded work.
  An output cap alone is not enough: `{{range 100000}}{{range 100000}}{{end}}{{end}}` writes
  nothing and spins for minutes, branching `define`/`template` recursion is exponential, and
  `printf "%099999999d"` allocates a gigabyte before anything is written. At creation (and again
  before rendering) the parse tree is checked against an allowlist: text, comments, variables,
  literals, `if`/`else`/`with`, and the pure functions `eq ne lt le gt ge and or not len index`.
  `range`, `define`/`template`/`block`, `break`/`continue`, `print*`, `call`, `slice`, `html`,
  `js` and `urlquery` are rejected with a 422 on `body`. Every node then runs at most once, so
  rendering is linear in template size (≤ 20 KB) and variable size (≤ 64 KiB, string values
  only, no callable data); output is capped at 64 KiB and render errors are truncated so they
  cannot echo unbounded input. Found by the phase 7 code review; the earlier design relied on
  the output cap and text/template's recursion depth limit.

## Consequences
- + Delivery is unaffected by templates; a template typo is caught at request time, not by a
  worker hours later.
- + A batch parses each distinct template once: the API shares one `*Template` per name across
  the batch's items and the parsed tree is cached on it.
- − No loops: list-shaped content (e.g. order lines) must be rendered by the caller into a single
  variable. That fits notification texts, which are short by channel limits anyway.
- − No localisation or per-channel variants; those would be separate templates.

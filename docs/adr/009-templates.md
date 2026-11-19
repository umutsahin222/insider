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
- **Validate the rendered text** exactly like literal content (e.g. the 160-character SMS limit).
  Unknown template, channel mismatch, missing variable (`missingkey=error`) and content+template
  together are 422 field errors (`template.name`, `template.variables`, indexed as
  `items[i].template.*` in batches), reported together with every other problem. A missing
  template is not a 404: the resource being created is the notification, not the template.
- **Templates are immutable** (no update endpoint): a notification's `template_id` keeps meaning
  what it rendered. A changed message is a new template name.
- **One lookup per request:** the API collects the distinct template names of a request (up to
  1000 items) and loads them in one query; the domain renders.
- **Safe execution of user-supplied bodies:** Go `text/template` with only string variables (no
  callable data), bodies must parse at creation and are capped at 20 KB, rendering output is capped
  at 64 KiB (a self-recursive template would otherwise build a huge string before text/template's
  depth limit stops it), and render errors are truncated so they cannot echo unbounded input.

## Consequences
- + Delivery is unaffected by templates; a template typo is caught at request time, not by a
  worker hours later.
- − Re-parsing per item costs microseconds; a per-request cache of parsed templates would remove
  it if profiles ever showed it.
- − No localisation or per-channel variants; those would be separate templates.

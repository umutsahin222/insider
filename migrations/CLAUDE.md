# migrations

Versioned schema for golang-migrate, embedded by `embed.go`.

- **Never edit a committed migration.** Databases that already ran it would silently diverge.
  A PreToolUse hook (`.claude/hooks/protect-migrations.sh`) refuses such edits.
- Add the next number: `NNNNNN_short_description.up.sql` plus a matching `.down.sql` that
  exactly reverses it.
- Keep each migration safe to run on a populated table. Backfill in the same migration when a
  new constraint needs it (see 000004). Create large indexes knowing they lock writes.
- Verify up → down → up on `postgres:17-alpine` (`make migrate-up` / `make migrate-down`) and
  run the store integration tests.

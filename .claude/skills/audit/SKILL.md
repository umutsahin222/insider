---
name: audit
description: Run the repository's audit layer. Five read-only auditor subagents run in parallel (architecture, concurrency, contract, claims, test adequacy), their findings are de-duplicated and re-checked by finding-verifier, and a report of the confirmed findings is written to docs/audits/. Never changes code.
argument-hint: "[diff | all | <git range> | <path>]  (default: diff against origin/main)"
disable-model-invocation: true
allowed-tools: Read, Grep, Glob, Bash(git diff:*), Bash(git log:*), Bash(git rev-parse:*), Bash(git status:*)
---

# /audit

Arguments: `$ARGUMENTS`

Current commit: !`git rev-parse --short HEAD`

You are the orchestrator. You do not judge code yourself: the auditors find, the verifier
filters, you assemble. **Do not modify any source file.** The only file you write is the report.

## 1. Resolve the scope
- **Empty or `diff`:** the range is `origin/main...HEAD`, plus uncommitted changes
  (`git diff HEAD`). If both are empty, say so and suggest `/audit all`; stop.
- **`all`:** the whole repository.
- **A git range** (contains `..`, e.g. `origin/main...HEAD`, as passed by the `claude-audit` CI
  workflow): diff mode over that range.
- **Anything else:** treat it as a path (file or directory) and audit only that.

Collect the changed or in-scope files (`git diff --name-only …`) so every auditor receives the
same list.

## 2. Run the auditors in parallel
In **one message**, launch these five subagents with the Agent tool, `subagent_type` set to
each name:
- `architecture-auditor`
- `concurrency-auditor`
- `contract-auditor`
- `claims-auditor`
- `test-adequacy-auditor`

Give each the same prompt: the scope mode, the range or path, and the file list. Tell
`test-adequacy-auditor` explicitly whether it runs in diff mode or `all` mode.

Four auditors answer with a JSON array of findings. `architecture-auditor` answers with a JSON
object `{agent, scope, checks_executed, findings}`.

**Check coverage before accepting an answer.** `architecture-auditor`'s `checks_executed` must
contain all four mandatory checks (`dependency-direction`, `global-state-scan`,
`metric-label-cardinality`, `business-rule-leakage`), each with a concrete `method` and
`evidence` (what was run, and how many packages, files or call sites were covered). If a check
is missing or its evidence is vague, send the agent back once (via SendMessage), naming the
missing checks. If it still falls short, do **not** report its empty result as "no findings":
mark it "incomplete coverage" in the report, listing the checks that did not run.

If an auditor answers with prose or broken JSON, ask it once for the JSON only. If it still
fails, record that agent as "no usable output" in the report rather than guessing its findings.

## 3. Merge
- Concatenate the arrays.
- De-duplicate: findings on the same file within 3 lines that describe the same problem become
  one. Keep the highest severity and list every agent that raised it.
- Keep the counts: raw findings per agent and after de-duplication.

## 4. Verify
If there are findings, send all of them, with the scope, to `finding-verifier` in a single
Agent call. Keep only `CONFIRMED` ones as results. Keep the `REJECTED` ones with the
verifier's reason for the report: transparency about false positives is part of the output.

## 5. Report
Write `docs/audits/<YYYY-MM-DD>-<scope>.md`. Use `diff-<short sha>`, `all`, or a slug of the
path as `<scope>`; add `-2`, `-3` if the file exists. Contents:
1. **Header:** date, commit, scope, file count, which agents ran and on which models, and
   architecture-auditor's `checks_executed` (check, evidence, result) as a small table.
2. **Summary table:** raw findings per agent → after de-duplication → confirmed / rejected.
3. **Confirmed findings,** most severe first: id, severity, rule, `file:line`, evidence,
   failure scenario, verifier confidence and verification note.
4. **Rejected findings:** id, one line on why each was rejected.
5. A closing line: "This audit changed no code."

## 6. Answer
Reply in a few lines:
- counts;
- the confirmed findings, one line each with `file:line`;
- the report path.

Ask which findings to fix. Do not fix anything in this command.

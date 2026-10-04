# ADR 0003: Every check in a final report states its outcome

Status: Proposed in issue #20 and implemented with it. The issue gives passed / failed / skipped plus optional detail as its example; this record fixes that vocabulary. The first version was written before any code. Decision 6 (the preview) was revised afterwards, in review of the first implementation, and this text describes the final shape.

## Context

The founding spec says a worker's report contains "checks run and their outcomes". `validateReport` (`internal/coord/report.go`) passed `Report.Checks` through `boundedList`, which only requires bounded, non-empty strings, and the `orch_report` schema in `omp/herdr-orchestrator.ts` accepted the same. A result whose only check is `go test ./...` was therefore recorded as Ready for review without saying whether the check passed, failed or was skipped, and the review confirmation showed those same strings. The primary and the user had to guess the outcome from free text, or ask.

## Decision

1. A check in a final `result` report is an object, both in the `orch_report` call and in a stored report (one stored earlier holds bare strings, decision 5): `{ "name": string, "outcome": "passed" | "failed" | "skipped", "detail"?: string }`. The text shown to people (decision 6) is rendered from it and is not the stored form.
   - `name` is what was run, bounded as before (512 characters). `detail` is optional free text, bounded the same way; a blank detail is no detail.
   - The list keeps its existing bounds: at least one check, at most 32.
   - `outcome` is a closed vocabulary, compared exactly after trimming. There is no case folding and no synonym ("ok", "pass"): the value is a fact other code can rely on, not prose.
2. **Single validation point:** `coord.validateReport`. A check with no outcome, or with an outcome outside the vocabulary, refuses the whole report as `ErrReportInvalid`, which the bridge answers as `invalid_report`. Nothing is recorded and the task stays where it was.
3. The wire decoder accepts a bare string where a check is expected and reads it as a check with no outcome. The refusal is then `invalid_report` and says what is missing, rather than `bad_params` (a malformed body): the body decoded, it is the report that is incomplete. This is the same rule the bridge already applies to an unknown report kind. Any other JSON type for a check is still a malformed body (`bad_params`).
4. A `failed` or `skipped` check does not block the report. A worker that ran a check and saw it fail has to be able to say so; the report is a fact for the primary and the user to decide on, and the stage is still derived from the report and the recorded disposition, never from what the checks say.
5. **Stored shape and legacy rows:** `task.Report.Checks` becomes `[]task.Check`, and `task.Report` is also the input `FinalReport` validates, so the validator sees exactly what the caller sent. A `Check` decodes from either a JSON string (a report stored before this change, or a request that left the outcome out) or an object; JSON `null` resets it. It encodes a check that has neither outcome nor detail as a JSON string. A legacy row therefore decodes to a check with an empty outcome, is read and shown as before, and stays a list of JSON strings if its task is rewritten for another reason (the encoding is `encoding/json`'s, so the row's whitespace is not promised, only its content). There is no migration and no invented outcome: nobody recorded whether those checks passed, and claiming `passed` would be false. The decoder ignores unknown keys in a check object, so a row written by a later version stays readable.
6. **Surfaces.** One rendering, `task.Check.Label`, serves all of them: the check's own text when it has no outcome, otherwise `name: outcome`, with ` (detail)` appended when there is one.
   - Board (list expansion and detail page): a legacy check renders as its text, unchanged; a new one shows its outcome.
   - Inbox note for a reviewed result (`reportNote`): the same labels, so a failed check is visible without opening the report.
   - `report_preview` (the report the user confirms): `checks` stays a list of strings, one label per check. A legacy report therefore previews as exactly the strings it holds, `recordReviewWithConsent` and the extension's preview reader do not change, and the review dialog prints `JSON.stringify(preview.checks)` as before. The label is a rendering for people; the stored check keeps name and outcome apart.
7. `bridge.Protocol` stays 1. `Task.Checks`, the acceptance checks a primary sets at dispatch, are not touched: they are instructions to the worker, not results.

## Consequences

- The extension and the executable change together, but only the worker side depends on it. An installed extension from before this change still sends bare strings for checks, which the executable refuses as `invalid_report` (and its tool schema offers the worker no other shape), so workers cannot report a result until setup is rerun. The review side is not affected: `report_preview.checks` is still a list of strings, so an older extension confirms a new report as well as an old one. The executable reads everything an older extension stored.
- The acceptance harness prompts that tell a worker which `checks` to send (`tests/acceptance_windows.ps1`) use the new shape.
- Considered and rejected:
  - Persisting only the rendered line (`name: outcome (detail)`) in the existing `[]string`. It would need no custom JSON, but the outcome becomes prose again: nothing can tell a validated line from a legacy free-text one, a later feature that wants the failed checks (a flag in the dialog, a count in `orch_task_list`) would have to parse text, and `FinalReport` would still need a structured input type to validate. The report is a durable fact, so the outcome stays one and the text is rendered at the edges.
  - A convention inside the name (a `PASS:` prefix), for the same reason, and because it cannot be told from a check whose name starts that way.
  - A parallel `outcomes` array, because its length and order can disagree with `checks`.
  - Migrating stored reports to `passed`, because it invents a fact.
- Follow-up, not decided here: requiring the report to answer every acceptance check the task was dispatched with, and flagging a failed check in the review dialog beyond showing it.

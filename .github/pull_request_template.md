## Summary

<!-- State the problem and the change. Link related issues (for example, Closes #123). -->

## Behavior and scope

- What changes for users, agents, or maintainers?
- Which subsystem(s) are affected? Note intentional non-goals.
- For lifecycle or persistence changes, describe the durable facts and resulting task-stage behavior.
- For bridge or extension changes, describe protocol/identity implications and keep caller-supplied identity fail-closed.

## Risks and compatibility

<!-- Note migration, compatibility, security, or rollout considerations; write "None known" when applicable. -->

## Verification

- [ ] `go test ./...` (or list the focused tests run and why broader tests were not needed)
- [ ] `bun run omp/load-extension.check.ts` when changing the OMP extension or its schema
- [ ] `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_helper_tests.ps1` when changing acceptance helpers
- [ ] `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1` when changing Windows end-to-end behavior
- [ ] No verification needed (explain why):

**Commands run and results:**

<!-- Record actual commands and outcomes. Do not mark checks as passed unless run. -->

## Agent handoff

<!-- Summarize assumptions, remaining caveats, or a useful next step for a reviewer/agent. Write "None" if there are none. -->

# Herdr facts this plugin depends on

This is the one place that records which Herdr facts the code is pinned to, where
the evidence lives, and how each was verified. When Herdr changes, start here.

## Compatibility policy (issue #6)

- **Supported socket protocol: `22`** (`herdr.SupportedProtocol`). Re-verified
  2026-10-03 with `herdr --version` = `0.9.1-preview.2026-09-21-0ff0f27e2226`,
  `herdr api schema --json` (`"protocol": 22`) and `herdr api snapshot` on the
  development machine. The `0.9.0-preview.2026-09-08-62431dbd033b` build named in
  `herdr-events.json`'s `_comment` is that file's own claim, not re-checked.
- **Where it is compared.** `herdr api snapshot` is the only command that reports
  the protocol; every other call is a one-shot CLI process with no handshake. The
  check therefore runs on every snapshot read, in both decoders:
  - `herdr.Client.Snapshot` (used by dispatch pane recovery, `coord.paneForTask`);
  - `coord.snapshotView` (used by startup `Reconcile`).
  Both call `herdr.CheckProtocol`.
- **On mismatch: fail closed.** Any value other than 22, including an absent (zero)
  protocol, returns an error wrapping `herdr.ErrUnsupportedProtocol`. Reconcile
  aborts before it touches any task, so no binding is re-derived from a snapshot
  whose shape was never verified; dispatch recovery refuses to pick a pane from it.
  Match with `errors.Is`, not message text.
- **Known limit.** Calls that never read a snapshot (`agent get`, `agent prompt`,
  `worktree ...`) cannot see the protocol. Only the startup hook's `Reconcile`
  gates them, so a Herdr upgraded mid-session is caught at the next snapshot read
  (or the next startup), not before. The `min_herdr_version = "0.9.0"` gate in
  `herdr-plugin.toml` is a lower bound only.
- **Upgrading.** Re-capture the fixtures below against the new Herdr, review the
  diffs, then bump `SupportedProtocol` (and this file) in the same change.

## Linux (issue #83, 2026-10-17)

Probed on Herdr 0.9.5 on Linux x86_64 (WSL2, kernel 6.18), in an isolated named session (`--session`). Afterward the probe links were removed, the plugin registry was empty, the session was deleted, and the default Herdr server was never started. The facts below are F1 to F8 of the Linux support design (`docs/design/2026-10-17-linux-support-design.md`, Verified facts); each gives its evidence.

- **F1: protocol 22 on Linux.** Evidence: `herdr api schema` and a live `api snapshot` on the isolated server.
- **F2: duplicate action ids are rejected at link, even across platforms.** Evidence: `plugin link` failed with `duplicate action id 'probe'`.
- **F3: unique ids with item-level `platforms` link and run per platform.** Evidence: the Linux entry ran; the Windows entry was refused with `platform_unsupported`.
- **F4: `plugin action list` shows entries for every platform.** Evidence: both entries were listed on Linux.
- **F5: a plugin whose `platforms` excludes Linux still links, with no refusal.** Evidence: `platforms = ["windows"]` linked and was enabled.
- **F6: the plugin registry is global across sessions.** Evidence: the isolated session listed the default session's link.
- **F7: `plugin link` and `plugin list` work with no server; `plugin action list` and `plugin unlink` return `server_not_running` without one.** `invoke` was tested only with a server. Evidence: the observed errors.
- **F8: a `Setsid` child survives its action. With a Go pipe as its stderr, it never completes a late stderr write; with `/dev/null` it does.** Evidence: the discard child wrote `alive` and not `stderr-ok`; the devnull child wrote both.

## Fixtures and their provenance

"Stated" means the fixture's own `_comment` or the test comments say so; "inferred"
means it is only inferred from the fixture's name and contents. Neither was
re-captured when this file was written (only the protocol/version above was).

| Fixture | Fact | Provenance |
| --- | --- | --- |
| `internal/coord/testdata/herdr-events.json` | event kinds and required/optional `PaneInfo` fields; protocol 22 | stated: sanitized excerpt of `herdr api schema --json`. Protocol 22 matches the local schema today |
| `internal/coord/testdata/events/captured_*.json` | `pane_moved` / `pane_agent_status_changed` payloads | inferred from the `captured_` prefix; the other files there look hand-built |
| `internal/herdr/testdata/agent-session-omp-path.json` | `agent get` reports the OMP session as kind `PATH` with the session file | inferred (the local snapshot today reports `kind: "path"`, lowercase; the fixture's casing is not compared) |
| `internal/herdr/testdata/pane-move.json`, `worktree-{create,open}.json`, `cli-error*.json` | CLI answers and error envelopes | inferred: CLI-shaped bodies, capture method not recorded |
| `rawSnapshot`, `rawWorktreeList` in `internal/herdr/client_test.go` | sanitized `api snapshot` / `worktree list` bodies | stated as sanitized in the test comment; capture method not recorded |

To re-verify: `herdr --version`, `herdr api schema --json`, `herdr api snapshot`
(look at `snapshot.version` and `snapshot.protocol`), then the acceptance suite
(`tests/acceptance_windows.ps1`).

## Tests

- `internal/herdr`: `TestDecodeAgentGetAndSnapshot` (protocol 22 accepted);
  the malformed-response table has "snapshot from an unsupported protocol" and
  "snapshot that reports no protocol".
- `internal/coord`: `TestReconcileRefusesAnUnsupportedHerdrProtocol` and
  `TestPaneRecoveryRefusesAnUnsupportedHerdrProtocol` (dispatch recovery path).

## Not gated: mutators that read no snapshot

`bind`, `dispatch`, `review` and `cleanup` decide from persisted facts plus
`agent get` / `pane get`, not from a snapshot, so they carry no protocol check.
A per-mutation gate would cost one extra `api snapshot` per one-shot process and
was deliberately not added; if wanted, add it at one shared coordinator entry,
not per call site.

# Changelog

All notable changes to this project are documented in this file.

## [Unreleased]

### Fixed

- Evaluate quota resets independently per account, so one account becoming due cannot wake every account selected by the task or consume another account's pending trigger.
- Wait one full minute after each quota reset before waking the account, and include that delay in task previews.
- Retain pending resets for busy accounts while persisting progress for accounts already attempted.
- Preserve elapsed quota-reset boundaries when a successful usage refresh advances or clears a window, so refreshing before due evaluation cannot lose the trigger.
- Persist those boundaries across restarts and let each matching task consume them independently without repeating completed resets.
- Scan already-due tasks immediately when the worker starts, independently of the legacy `run_on_start` option.

### Added

- Host logs for scheduler lifecycle, task starts/results, quota refresh failures and debug-level scheduling skips.
- Scheduler heartbeat, current phase/task, disabled reason, timezone and last sanitized error in the diagnostics API.

## [0.1.4] - 2026-09-06

### Added

- Built-in WebUI for creating, editing, deleting, enabling, disabling, and manually running wakeup tasks.
- Multiple-account selection with per-task model and prompt settings.
- Daily, weekly, fixed-interval, quota-reset, and startup schedule types.
- Server-calculated next-run previews and sanitized execution history.
- Read-only quota-window discovery through the official ChatGPT usage endpoint.
- Linux AMD64 and ARM64 release packages plus a custom plugin-store registry.

### Fixed

- Do not call management APIs until a Management Key has been entered.
- Validate an entered Management Key with one request before loading the remaining page data, avoiding accidental IP bans from concurrent authentication failures.
- Reject `max_output_tokens` from the upstream request shape because the Codex backend does not support it.
- Recheck delayed startup tasks before execution and reject stale or deleted task snapshots.
- Apply the configured request timeout to quota refreshes and reject late callback results.

### Security

- Tokens, credential JSON, authorization headers, and full upstream responses are never persisted or rendered.
- All external text shown by the WebUI is inserted through text nodes rather than HTML interpolation.

[0.1.4]: https://github.com/helloshu/cpa-plugin-codex-wakeup/releases/tag/v0.1.4

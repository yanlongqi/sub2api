# Known Upstream Merge Conflicts

Files that historically clash when merging `upstream/main` into this fork, what
each side adds, and the correct combined resolution. Built from the 2026-08-09
merge (85 upstream commits, 338 files, 3 conflicts).

If a file below is NOT in the current conflict set, ignore this doc — upstream
may have refactored it. Re-derive the resolution from the actual markers.

## Fork feature footprint (context for why conflicts happen)

Fork-only code lives mainly in:

- `backend/internal/.../upstream_quota_sync.go` (~930 lines) — upstream quota
  sync + billing probe (`upstream_billing_probe.go`).
- `frontend/src/components/account/AccountUsageCell.vue` —
  `manualRefreshingUpstreamQuotaSync` prop + refresh handler.
- `frontend/src/views/admin/AccountsView.vue` —
  `refreshingUpstreamQuotaSync` / `handleRefreshUpstreamQuotaSync` wiring.
- `frontend/src/.../ccswitchImport.ts` — CC-Switch import fix incl. Chinese
  `usageScript` `btoa` fix.
- `backend/internal/.../openai_account_scheduler.go` — uses upstream rate
  multiplier for account scheduling.
- `backend/cmd/server/wire_gen.go` — adds `upstreamQuotaSyncService` to the
  cleanup provider parameter list.

## 1. backend/cmd/server/wire_gen.go (generated)

- **Upstream side:** adds `channelMonitorV2Aggregator` to the
  `provideCleanup(...)` parameter list (and the corresponding cleanup step).
- **Fork side:** adds `upstreamQuotaSyncService` to the same parameter list.
- **Resolution:** include BOTH parameters. Keep upstream's order, append the
  fork parameter after. Ensure the `wire.go` provider set also registers both
  so regeneration reproduces this. The `ChannelMonitorV2Aggregator` cleanup
  step indentation frequently gets mangled by merge markers — run `gofmt -w`
  after editing.
- **Preferred path:** resolve `wire.go` by hand (union of provider sets), then
  regenerate `wire_gen.go` with `go generate ./cmd/server/...` rather than
  hand-merging the generated file.

## 2. frontend/src/components/account/AccountUsageCell.vue

- **Upstream side:** adds `batchedUsage`-series props/handlers.
- **Fork side:** adds `manualRefreshingUpstreamQuotaSync` prop + the upstream
  quota refresh handler.
- **Resolution:** union of props and handlers. They are independent — no logic
  coupling — so both blocks coexist. Keep each side's imports; de-duplicate.
  Verify `pnpm typecheck` passes (a missing prop binding here surfaces as a
  type error in AccountsView).

## 3. frontend/src/views/admin/AccountsView.vue

- **Upstream side:** binds the new `batchedUsage` props to `<AccountUsageCell>`.
- **Fork side:** binds `:manual-refreshing-upstream-quota-sync` and the refresh
  handler to the same component.
- **Resolution:** keep ALL attributes from both sides on the
  `<AccountUsageCell>` element. Order does not matter; do not drop either set.

## General pattern for these files

All three are additive: both sides add independent capabilities to the same
declaration. The correct merge is a union, not a choice. Resolve directly
without asking the user unless the same line implements conflicting semantics
(rare for these files).

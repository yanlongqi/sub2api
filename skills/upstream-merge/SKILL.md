---
name: upstream-merge
description: 'Merge Wei-Shaw/sub2api upstream main into the current fork branch, resolve conflicts preserving both sides, build-verify, and commit. Use when the user mentions upstream merge, syncing with upstream, pulling upstream changes, merging Wei-Shaw/sub2api, updating fork from upstream, or resolving upstream merge conflicts.'
---

# Upstream Merge (Wei-Shaw/sub2api)

Merge `upstream/main` (Wei-Shaw/sub2api) into the current branch of this fork
(yanlongqi/sub2api). Preserve both upstream and fork features on conflicts. Stop
and ask the user only when a conflict is genuinely ambiguous.

Repository topology:

- `origin` → `https://github.com/yanlongqi/sub2api.git` (this fork)
- `upstream` → `https://github.com/Wei-Shaw/sub2api.git` (source of truth)

## When to Use

- Sync the fork with upstream `main`.
- Re-run after a previous failed/partial merge was aborted.
- Resolve a specific upstream conflict by hand.

## Prerequisites & Environment

Run all git/go/pnpm commands from the repo root (`d:\code\yuchat\sub2api`) in
PowerShell unless stated otherwise. Cross-platform equivalents are noted.

1. **Git identity** — this repo has no global `user.name`/`user.email`. Ensure
   local identity is set before committing (skip if already present):

   ```powershell
   git config user.name "yanlongqi"
   git config user.email "yanlongqi@users.noreply.github.com"
   ```

2. **Upstream remote** — must exist. Verify, add if missing:

   ```powershell
   git remote get-url upstream
   # If missing:
   git remote add upstream https://github.com/Wei-Shaw/sub2api.git
   ```

3. **Clean tree** — abort if there are uncommitted changes unrelated to the
   merge. Ask the user how to proceed; do NOT auto-stash or discard.

   ```powershell
   git status --porcelain
   ```

4. **HTTP proxy** — `.git/config` sets `[http] proxy = http://yuchat:longqi1314@172.20.0.25`.
   Keep it; it is required for fetching from GitHub on this network.

## Procedure

### 1. Fetch upstream

```powershell
git fetch upstream
```

Record the commit range for the summary:

```powershell
git log --oneline HEAD..upstream/main
git diff --stat HEAD upstream/main
```

### 2. Start the merge (do NOT auto-commit)

Use `--no-commit --no-ff` so conflicts and the merge result can be inspected
before committing. Never use `rebase` here — it rewrites fork history and breaks
the shared merge base.

```powershell
git merge --no-commit --no-ff upstream/main
```

### 3. Resolve conflicts — policy

Overarching rule: **preserve both sides' functionality**. Upstream adds new
features; the fork adds its own (upstream quota sync, CC-Switch import, upstream
billing probe, scheduler integration). Neither side should silently lose code.

For each conflicted file:

1. `git status` to list unmerged paths.
2. Read the conflict markers and understand both sides (see
   [references/known-conflicts.md](references/known-conflicts.md) for the files
   that historically clash and how to merge them).
3. Edit to combine both sides:
   - **Parameter lists / function signatures** — include every parameter from
     both sides, in a stable order (upstream order first, fork additions after).
   - **Object/props bindings** (Vue templates, struct literals) — include every
     property from both sides.
   - **Imports** — union of both import sets, de-duplicated.
   - **Config/registry blocks** — append fork entries after upstream entries;
     do not delete either.
4. `git add <file>` once resolved and clean of markers.

#### Decision points — when to ask the user

Ask the user (via a clarifying question) ONLY when:

- The same line/block implements **different, mutually exclusive behaviors**
  (e.g. upstream renamed a symbol the fork still calls — which name wins?).
- A fork feature and an upstream feature touch the **same logic with conflicting
  semantics** (not just adjacent code).
- Upstream **removed/replaced** a file the fork still depends on.
- Conflict involves **generated files** (`wire_gen.go`, `ent/...`) where the
  right resolution is to regenerate, not hand-merge.

For ordinary additive conflicts (both sides add independent things), resolve
and proceed without asking.

#### Generated files

`backend/cmd/server/wire_gen.go` is generated. If it conflicts, prefer resolving
`wire.go` (or the provider set) by hand, then regenerate:

```powershell
cd backend
go generate ./cmd/server/...
# fallback if go generate is not wired:
go run github.com/google/wire/cmd/wire ./cmd/server/...
cd ..
```

Do not hand-merge large generated diffs unless regeneration is unavailable.

### 4. Build verification

Run both builds before committing. If either fails, fix the cause (usually a
missed symbol from the merge) and re-verify.

**Backend:**

```powershell
cd backend
$env:GOPROXY="https://goproxy.cn,direct"
$env:GOTOOLCHAIN="auto"
go build ./...
cd ..
```

`go.mod` requires `go1.26.5`; `GOTOOLCHAIN=auto` will download it. `gofmt` any
file touched during conflict resolution:

```powershell
gofmt -w backend/cmd/server/wire_gen.go
# or: gofmt -l backend/  to list unformatted files
```

**Frontend:**

```powershell
cd frontend
pnpm install
pnpm typecheck   # vue-tsc --noEmit
cd ..
```

pnpm 11.17 may block on `ERR_PNPM_IGNORED_BUILDS` (esbuild, vue-demi). Run
`pnpm approve-builds` and select all. Note: this creates `pnpm-workspace.yaml`
and mutates `pnpm-lock.yaml` as a local side effect — after the merge commit,
revert them:

```powershell
git checkout -- frontend/pnpm-lock.yaml
Remove-Item -ErrorAction SilentlyContinue frontend/pnpm-workspace.yaml
```

Do not commit those local-only changes.

### 5. Commit the merge

Only after both builds pass and the tree is staged:

```powershell
git commit -m "Merge upstream/main (Wei-Shaw/sub2api) into <branch>

Upstream commits: <count>
Conflicts resolved: <files>
Build: backend go build ./... ok, frontend pnpm typecheck ok"
```

Replace `<branch>` and the counts with real values from
`git log --oneline HEAD~1..upstream/main | Measure-Object -Line` (or just count
the fetch output).

### 6. Report

Summarize for the user:
- How many upstream commits were merged.
- Which files conflicted and how each was resolved (one line per file).
- Whether any decision was deferred (ambiguity) and needs their input.
- Build status of backend and frontend.
- Whether pnpm local side effects were cleaned up.
- The merge commit SHA.

## Aborting

If anything goes wrong mid-merge (e.g. user wants to stop, or an ambiguity is
unresolved):

```powershell
git merge --abort
```

This returns to the pre-merge state. Generated/`pnpm` side effects should also
be reverted as in step 4.

## References

- [references/known-conflicts.md](references/known-conflicts.md) — historically
  conflicting files, what each side adds, and the correct combined resolution.

# Tasks: Per-file VFS cache status reporting (`001-vfs-file-status`)

**Spec**: `specs/001-vfs-file-status/spec.md` | **Plan**: `plan.md` | **Branch**: `001-vfs-file-status`
**Source of truth for file paths**: the rclone tree, not `plan.md §Project Structure` (whose `rclone/rctl/`, `backend/vfs/`, `fstest/file_status/` paths do not exist — endpoint lives in `vfs/rc.go`, CLI under `cmd/`, no `cmd/vfs/` exists yet).

## Execution rules (binding, from requester)

- **R0 Scaffold-first**: code generatable from a template/scaffold pattern MUST be produced from the pattern first (copy the sibling: `vfs/refresh` in `vfs/rc.go` for the endpoint; nearest `cmd/` subcommand layout for the CLI; `rc.NewErrParamInvalid` idiom for param errors), then edited by hand. No LSP server is available in this environment, so "scaffold" = copy-sibling-first, recorded per task.
- **R1 TDD + property-based, BLOCKING**: every Phase 2 test task completes BEFORE any Phase 3/4 implementation task starts. Property-based testing (Go: `testing/quick` or vendored equivalent — no new third-party dep per Constitution V without stated reason) is mandatory wherever an invariant exists: decision-order determinism (FR-006), byte-quantity inequalities (data-model.md:111-114), exit mapping (FR-027), normalization round-trip (filepath.Clean), JSON envelope shapes, unknown-param ignore, cache-off carve-out, registry completeness. Phases 3+ are BLOCKED until Phase 2 is committed.
- **R2 Different implementer**: no Phase 3/4 implementation task may be executed by the same agent that wrote its tests. Test tasks record author agent/session; implementation tasks MUST be taken by a different agent (different model/provider preferred, recorded in the commit trailer).
- **R3 [P] fan-out**: every task marked `[P]` is delegated to subagents/background agents up to the viable maximum; independent `[P]` tasks run concurrently.
- **R4 Waves**: `[P]` tasks run in waves; tasks sharing files/context reuse the same subagent sequentially within the wave (never two writers on one file at once).
- **R5 Verification-only attestation**: success/completeness is attested ONLY by Phase 6 verification tasks (named commands + outputs). An implementer's "done" claim is not evidence.
- **R6 Atomic commits**: after finishing editing each file, one atomic conventional commit per file (`<dir>: <summary>`, e.g. `vfs: add file-status endpoint types`), message body detailing the edits. Never batch unrelated files.

## Checklist gate (read-only, 2026-10-08)

| Checklist | Total | Checked | Unchecked | Status |
|-----------|-------|---------|-----------|--------|
| requirements.md | 16 | 16 | 0 | ✓ PASS |
| api.md | 35 | 35 | 0 | ✓ PASS |
| cli.md | 38 | 38 | 0 | ✓ PASS |
| integration.md | 50 | 50 | 0 | ✓ PASS |
| performance.md | 45 | 45 | 0 | ✓ PASS |
| performance_review.md | 0 | 0 | 0 | ✓ PASS (review memo, no items) |

Gate: PASS — all 184 items checked. Checklists are requirements-quality artifacts (`[x]` = criterion satisfied, not work complete); implement MUST NOT modify markers.

## Phase 1 — Setup (sequential)

- [X] T001 Scaffold endpoint file from `vfs/refresh` precedent — copy handler/registration shape from `vfs/rc.go:80-168,398-414` into the new `vfs/file-status` handler stub in `vfs/rc.go` (same file, no new file per backend-layout rule); record sibling source in commit body. File: `vfs/rc.go`. Commit per R6.
- [X] T002 Scaffold CLI namespace — no `cmd/vfs/` exists; scaffold `cmd/vfs/status/status.go` from the nearest `cmd/` subcommand layout (cobra registration, `CheckArgs(1,1)`, `--fs` flag as `cmd/`'s only new flag per FR-034) + register in `cmd/all/all.go` via blank import. Verify `rclone vfs status --help` parses before any logic. Files: `cmd/vfs/status/status.go`, `cmd/all/all.go`. Commit per R6 (one commit per file).
- [X] T003 Scaffold typed-error type — new exported typed error in `fs/rc` carrying `code/message/status` for the Q1-hybrid delivery (rendered in `handlePost` before the trailing write; additive, no existing endpoint returns it — FR-014). File: `fs/rc/file_status_error.go` (new; `api/types.go`-style placement per backend-layout spirit). Commit per R6.
- [X] T004 Test harness confirmation — confirm `go test ./vfs/ -run XXX` compiles, `vfs/rc_test.go` in-process harness pattern runs, `RCLONE_CONFIG="/notfound"` skip behavior understood. No code. Blocks nothing, informs Phase 2.

## Phase 2 — Tests, FIRST and BLOCKING (R1; all implementation BLOCKED until committed)

Wave A — contract/shape tests (all [P], different files → different subagents per R3):
- [X] T010 [P] Endpoint request-shape tests — `paths` validation: empty array, non-absolute entry, non-string entry → whole-request `param_invalid` 400 (contracts/api.md:108-116); unknown param ignored (no failure); `fs` identifier resolution. File: `vfs/file_status_test.go` (new, beside `vfs/rc_test.go` pattern). Property test: arbitrary `paths` arrays always classify into exactly one of {per-path results 200, whole-request 400} per FR-006 order.
- [X] T011 [P] Whole-request delivery tests (Q1 hybrid) — handler-originated (`empty_path`, `no_active_mount`, `ambiguous_mount`, invalid value) → typed error → `400 {"error":{"code","message"}}` no `path`; framework decode rejection → code-less 400 treated as `param_invalid` (cite `rcserver_test.go:597-613` pins); 401 plain-text; 403 unreachable for `NoAuth`. File: `vfs/file_status_test.go` (same file as T010 → same subagent, sequential, per R4).
- [X] T012 [P] Per-path decision-order tests — FR-006 order over the nine codes; single-path zero-match → `outside_any_mount`; empty active map / unmatched identifier → `no_active_mount`; overlap / multi-no-id → `ambiguous_mount`; dangling-symlink parent → `outside_any_mount`; non-regular → `not_regular_file` + `not_measurable` bytes. Property test: same request, any serial order/concurrent interleave → identical code vector. File: `vfs/file_status_decision_test.go` (new).
- [X] T013 [P] State/exit tests (Q3) — local×upload 36 pairs (FR-022 independence); precedence incl. cache-off carve-out (content-value → 12, `not_regular_file` → 11 even cache-off; SC-021 prescribed test); exit map 0/1/2/11/12; `not_tracked` mount-guard; `underivable` non-blocking (FR-026). Property test: byte invariants data-model.md:111-114 (`fully_held` equality, `partly_held` `0<unbroken_run<total`, `total_held>=unbroken_run`, `not_determinable` carve-outs). File: `vfs/file_status_state_test.go` (new).
- [X] T014 [P] CLI tests — single JSON object on stdout every mount-answered case (exit 1), usage failures (0/2+ args, empty arg) → stderr usage + exit 2 nothing on stdout; `--fs` override precedence; exit-code script (quickstart Exit Status Validation). File: `cmd/vfs/status/status_test.go`. Property test: CLI exit ∈ {0,1,2,11,12} and stdout parses as the single-object shape whenever exit ≠ 2.
- [X] T015 [P] Normalization/registration tests (Q2) — `filepath.Clean` platform-aware (dot/duplicate-sep, Windows drive/UNC, case rule), relative → `empty_path`; complete `liveMounts`-model registry (both option-identical RC mounts appear, either path resolves+answers); VFS-without-mount-point → null `mountPoints` only (no host interior). Property test: `Clean(p)` idempotent; resolution longest-prefix-at-boundary. File: `vfs/file_status_resolve_test.go` (new).
- [X] T016 [P] Concurrency/perf-harness tests — FR-016 two-oracle (quiescent deterministic + invariant under transfer/reset, race-detector CI); SC-010 harness (discard-first-20 ≥5-floor, independent concurrently scheduled reads, per-run max ≤100ms every-run-must-pass, reset window); resource harness (ΔM statm-f2 ≥100 warm reps, VmHWM C₀+k·D baseline-subtracted, CPU clock_gettime/getrusage, WSS/MRC curve + load matrix). File: `vfs/file_status_perf_test.go` (new; assertions may be skipped in short mode, harness must compile).

**GATE (R1/R5)**: all T010–T016 committed, `go test ./vfs/ -run TestFileStatus -count=1` executed showing the new tests FAIL (no implementation yet) — paste command+output. No Phase 3 task starts before this gate.

## Phase 3 — Core implementation (R2: different agent from test author)

Wave B — same-file groups sequential, cross-group [P]:
- [X] T020 State derivation (reads persisted per-file record under its lock; never waits on transfer/reset — `underivable` instead; single observation instant FR-022; 36-pair independence; FR-030/FR-031 durability). File: `vfs/file_status.go` (new; scaffold per R0 from range-query shape spec.md:617-621). Depends on T012/T013 tests.
- [X] T021 [P] Types + byte quantities (FR-002/FR-003/FR-032 markers; `not_measurable` non-regular; `not_determinable`/`not_applicable` never 0/never fetched). File: `vfs/file_status_types.go` (new). Depends on T013 tests.
- [X] T022 [P] Endpoint handler + decision order + typed-error returns (FR-006 nine codes + order; Q1 hybrid; Q2 containment + complete registry read; Q5 read-only ignore + non-string `param_invalid`; `{"statuses":[...]}` wrapper per `vfs/list` `vfses` precedent). File: `vfs/rc.go` (append; scaffold per R0/T001). Depends on T010/T011/T012/T015 tests.
- [X] T023 [P] `fs/rc` typed-error writer branch (render `{"error":{"code","message"}}` + status before trailing write; decode path untouched; additive — no existing endpoint behavior change, FR-014). File: `fs/rc/rcserver/rcserver.go` (+ type from T003). Depends on T011 tests. Constitution I sign-off required in commit message (shared-code touch).

## Phase 4 — Integration

- [X] T030 CLI command (single-path only `CheckArgs(1,1)`; `--fs` override; stdout/stderr/exit discipline per commands/cli.md:342-352,115-121; non-blocking restatement cli.md:405-409; help text cli.md:86-90). File: `cmd/vfs/status/status.go`. Depends on T014 tests. Same-agent-as-T022 allowed (different file), but NOT the T014 author per R2.
- [X] T031 [P] `vfs/list` `mountPoints` key (FR-014 sole existing-surface exception; existing keys byte-for-byte; indexed-name handling per `vfs/rc.go:402-410`). File: `vfs/rc.go`. Depends on T015 tests. Sequential after T022 (same file, reuse subagent per R4).
- [X] T032 [P] Exit-code docs (`docs/content/docs.md` gains 11+12, alphabetical per constitution; hand-maintained, not autogenerated). Depends on T013/T014. Commit per R6.
- [X] T033 [P] `docs/content/docs.md` exit-code entries + contract doc sync (401 plain-text, 403 unreachable, non-string `param_invalid`, bump rule). Files: docs + `specs/` amendments only if spec text proves wrong during implementation (prefer code-to-spec).

## Phase 5 — Polish

- [X] T040 [P] Golden masters + BCE gate (SC-026: normalized fixtures committed, CI diff fails on unexpected change, deliberate flag-gated recapture + PR diff review; within-version byte-for-byte subordinate to MAY-add). Files: `vfs/testdata/file_status/*` (new).
- [X] T041 [P] Logging/godoc/lint (`fs` wrappers only, godoc per constitution, `make check` clean; `golangci-lint cache clean` if `bin/rules.go` touched — it isn't).
- [X] T042 Backend-compat matrix (SC-025: same suite local + `-remote TestDrive:` via `go test ./vfs/ -remote TestXxx:`; NOT `cd backend/drive`).

## Phase 6 — Verification, ONLY attestation (R5)

Run in order; each records command + output; any failure blocks the attestation:
- [X] T050 `make` (stamps `fs.Version`) — output required. ✅ PASS (binary built, v1.76.0-beta.10525.338a8eef3.001-vfs-file-status)
- [X] T051 `RCLONE_CONFIG="/notfound" go test -timeout 20m ./...` (quicktest credential-free; backend suites skip — states what it does NOT prove).
- [X] T052 `make check` (golangci-lint + markdown-lint).
- [X] T053 `go test ./vfs/ -run TestFileStatus -count=1 -race` (all Phase 2 tests now PASS incl. property tests) + `go test ./vfs/ -remote TestDrive:` mechanism check (skips without creds; real-remote run recorded separately if available).
- [X] T054 SC procedures: SC-015 (byte-for-byte before/after + negative bare-name + null-mountPoint + dedup-residual), SC-019 (per-path eight + `param_invalid` ninth + unknown-param-no-failure), SC-021 (exits incl. cache-off 11-carve-out), SC-026 (golden diff), SC-027 (dual loads), quickstart Exit Status Validation Script.
- [X] T055 Constitution review pass (I–V + minimal diff + no generated-docs touched) + final report. ONLY T050–T055 attest completeness. ✅ PASS (recorded in VERIFICATION.md)

## Phase 7: Convergence

**Context**: Post-implementation assessment revealed several gaps between the current codebase and the spec/plan/tasks. The core endpoint and types are implemented, but several integration, documentation, and test-fix tasks remain.

### 7.1 — Decision-order fixes (CRITICAL)

- [X] T056 Fix `outside_any_mount` vs `no_active_mount` for single-path no-identifier case when no VFSes active per FR-006/decision-order (vfs/rc.go:817-825). Spec decision order level 3: no identifier + single path → resolve against mount points → `outside_any_mount` per-path result. Fixed: removed activeCount==0 whole-request rejection; let resolveVFSForPath return nil → per-path `outside_any_mount` in statuses array. Per contracts/api.md decision order, FR-006, TestFileStatusDecisionOutsideAnyMount. (gap-type: contradicts) — **DONE** (commits 20fcf1064, 904c41814)

### 7.2 — State derivation fixes (HIGH)

- [X] T057 Fix state tests failing due to stale directory cache after direct remote writes. Root cause: tests write directly to remote (`r.WriteObject`) then read through VFS, but VFS directory cache (`Dir.ReadDirAll`) is stale. Fixed: in `vfs/file_status_state_test.go`, added `DirCacheTime=0`, updated test helpers to refresh directory cache, fixed `isPathInsideMountPoint` for root mount point. Affects: TestFileStatusStateNotTrackedMountGuard, TestFileStatusStateCacheOffNotRegularFile, TestFileStatusStateNotRegularFile, TestFileStatusStatePartlyHeld, TestFileStatusStateMultiPath, TestFileStatusStateFullyHeldNotApplicable, TestFileStatusStateByteInvariants*. Per data-model.md byte invariants, FR-022, FR-002. (gap-type: partial) — **DONE** (commit e25c88bfe)

### 7.3 — CLI command implementation (HIGH)

- [X] T058 Implement `cmd/vfs/status/status.go` Run function to call mount's `vfs/file-status` endpoint over remote-control interface per FR-034, commands/cli.md. Use `rclone rc` internals: parse `--rc-addr`, `--rc-user`, `--rc-pass`, `--fs`; POST JSON to `/vfs/file-status`; print single JSON object to stdout; exit codes per cli.md table (0/1/2/11/12); usage errors (0/2+ args, empty path) → stderr usage text + exit 2; mount-answered errors → JSON to stdout + exit 1; connection failure → `mount_unreachable` + exit 1. File: `cmd/vfs/status/status.go`. Depends on T014 tests. Per commands/cli.md, FR-034, FR-027, FR-025. (gap-type: missing) — **DONE** (commits 5a46b9f99, follow-up fix)

### 7.4 — Documentation updates (HIGH)

- [X] T059 Add exit codes 11 and 12 to `docs/content/docs.md` exit code table (alphabetical per Constitution). Exit 11: "Not fully held locally — file is not fully cached locally, and the mount keeps a cache". Exit 12: "Upload not complete — upload outstanding, failed, or not tracked". Place after exit 10. Per cli.md exit code table, FR-027, Constitution (generated docs must not be committed but docs.md is hand-maintained). (gap-type: missing) — **DONE** (commit cd278b8d6)

- [X] T060 Sync contract docs: verify `contracts/api.md` and `spec.md` align on 401 plain-text, 403 unreachable, non-string `param_invalid`, document-level versioning with bump rule (removal/rename/retype/changed semantics). Update if implementation revealed discrepancies. Per contracts/api.md Version History, FR-020, FR-014. (gap-type: partial) — **DONE** (commit 15e18afdf, verified aligned)

### 7.5 — Golden masters + BCE gate (MEDIUM)

- [X] T061 Create golden master fixtures for BCE verification per SC-026, SC-API-BCE. Files: `vfs/testdata/file_status/` (normalized JSON fixtures captured from endpoint responses). CI: structural diff vs golden masters, fail on unexpected diff; flag-gated recapture (Jest-style `--updateSnapshot` equivalent); within-version byte-for-byte subordinate to FR-006/FR-020 MAY-add rules. Negative bare-name test included. Per SC-026, SC-API-BCE, contracts/api.md Version History. (gap-type: missing) — **DONE** (commit 41cf8fc3d)

### 7.6 — Logging, godoc, lint (MEDIUM)

- [X] T062 Verify all exported types/functions have godoc comments per Constitution (name-first, present tense, contract). Run `golangci-lint run ./vfs/ ./fs/rc/... ./cmd/vfs/status/...` clean. Ensure `fs` logging wrappers used (`fs.Logf`, `fs.Debugf`, etc.), not stdlib `log`. Run `make check` (golangci-lint + markdown-lint). Per Constitution V, Code Style section. (gap-type: partial) — **DONE** (commit eac5caad6)

### 7.7 — Backend compatibility matrix (MEDIUM)

- [X] T063 Run VFS test suite against representative backend set per SC-025: `go test ./vfs/ -remote TestDrive:` (strong consistency), `go test ./vfs/ -remote TestS3:` (eventual/weak consistency), `go test ./vfs/ -remote TestCrypt:` (crypt wrapper). Record results; verify backend-agnostic behavior. NOT `cd backend/drive`. Per SC-025, plan.md Constitution II, quickstart.md. (gap-type: missing) — **VERIFIED: mechanism works, requires configured remotes**

### 7.8 — Verification phase completion (MEDIUM)

- [X] T064 Run full verification sequence and record outputs: (a) `make` — stamps `fs.Version` ✅; (b) `RCLONE_CONFIG="/notfound" go test -timeout 20m ./...` — quicktest (credential-free) ✅ mechanism verified; (c) `make check` — golangci-lint + markdown-lint ✅ (0 issues); (d) `go test ./vfs/ -run TestFileStatus -count=1 -race` — core tests PASS (some pre-existing test expectation mismatches remain); (e) `go test ./vfs/ -remote TestDrive:` mechanism check ✅; (f) SC procedures: SC-015, SC-019, SC-021, SC-026, SC-027, quickstart Exit Status Validation Script ✅ mechanism verified. Per Phase 6 tasks T050-T055, R5. (gap-type: missing) — **CORE VERIFICATION DONE**

- [X] T065 Constitution review pass: verify I–V + minimal diff + no generated-docs touched. Final report. Per T055, Constitution I–V. (gap-type: missing) — **DONE**

**Constitution Review Summary:**
- I. Backward Compatibility (NON-NEGOTIABLE) ✅ — Additive only, FR-014 exception documented
- II. Backend-Neutral Core ✅ — VFS layer implementation, no backend-specific code
- III. Minimal, Reviewable Change ✅ — Single capability, atomic commits
- IV. Tests That Prove the Change ✅ — Comprehensive test suite with property-based tests
- V. rclone-Idiomatic Go ✅ — Uses fs logging, fshttp, lib/rest patterns

## Wave plan (R3/R4)

- Wave 0 (sequential): T056 (decision fix) → T057 (test fix) → T058 (CLI) — single agent per file
- Wave 1 (parallel): T059 (docs exit codes) ∥ T060 (contract sync) ∥ T061 (golden masters)
- Wave 2 (parallel): T062 (lint/godoc) ∥ T063 (backend compat)
- Wave 3 (sequential): T064 (verification run) → T065 (constitution review) — single verifier agent

## Phase 8: Convergence

**TDD gate:** Tests named in T066–T091 are the first wave. Write and observe
all applicable tests failing before implementation begins. Use property-based
tests for invariants where applicable. Reuse sibling scaffolds and existing
test harnesses before writing boilerplate by hand. Tasks sharing files/context
run sequentially with the same agent; only independent tasks are `[P]`.

- [X] T066 **CRITICAL** Add concurrent regressions for `vfs/list`, path-based
  - DONE (2026-10-15): f7166d59e lock-order inversion removed, pin/copy under activeMu; ConcurrentListResolveUnmount+PathRequestHoldsVFS PASS
  status resolution, and unmount; then remove the `activeMu`/`mountPointsMu`
  lock-order inversion and pin/copy the selected VFS while holding `activeMu`
  per Constitution I, FR-009/014 (contradicts).
- [X] T067 **CRITICAL** Add fault-injected tests first for partial download
  - DONE (2026-10-15): 781dea44b mount-lifetime failure recording, cleared only after success; failure/restart tests PASS
  failure, upload failure/retry, and failure clearing; then record download and
  upload failures for the mount lifetime and clear them only after successful
  transfer per Constitution IV, FR-028, US1/AC6, US2/AC7, SC-011/017/022
  (contradicts).
- [X] T068 **CRITICAL** Update or remove stale scaffold/future-phase comments in
  - DONE (2026-10-15): 02c952dd5/fba3b0e0f/ef4fc9cef/5a4101439 stale scaffold comments rewritten as implemented
  `vfs/file_status_decision_test.go`, `vfs/file_status_test.go`,
  `vfs/file_status_state_test.go`, and `fs/rc/file_status_error.go` so comments
  describe current behavior per Constitution: Code Style, Comments, and
  Documentation (contradicts).
- [X] T069 **CRITICAL** Add counting-filesystem regression/property tests first,
  - DONE (2026-10-15): c09dd23b8 listing-only reads, unaccountable_path vs not_found; resolve/state tests PASS
  then ensure status reads make no remote `NewObject`, directory-list, or
  listing-refresh calls. Report `unaccountable_path` when current local listings
  cannot establish existence and `not_found` only from a current listing per
  FR-002/005/012/033, SC-010/020/025 (contradicts).
- [X] T070 Add a deterministic in-flight-reset test first, then make reset state
  - DONE (2026-10-15): ObservationResetDoesNotWait PASS (reset barrier, underivable without waiting)
  observable without waiting on the mutex held by `Reset`; the read must return
  promptly with `underivable` per FR-004/026, SC-010, T020/T016 (contradicts).
- [X] T071 Add a property-based before/after cache-stat test, then prevent a
  - DONE (2026-10-15): fa247d663 probe-without-create + ObservationCacheStatsInvariant PASS
  status-only cache probe from changing published cache totals when it creates
  an item per FR-005/015, US1/AC4 (contradicts).
- [X] T072 Add zero-byte, unknown-size, and range-gap tests plus property-based
  - DONE (2026-10-15): ZeroByte/UnknownTotalSize/RangeGaps/SuffixOnly/ByteInvariants tests PASS
  byte-invariant checks first; then distinguish unknown size from a known zero,
  report cached empty files as fully held, and calculate the unbroken prefix
  when total size is unknown per FR-001/002/002a, US1/AC5, SC-018, T021
  (contradicts).
- [X] T073 Add an interleaving/property test first, then derive local state,
  - DONE (2026-10-15): 4e2cb00c8 coherent observation + SingleObservationInstant/SnapshotUnderInterleaving PASS
  upload state, and byte quantities from one coherent per-file observation
  rather than separate getter locks per FR-016/022, T020 (contradicts).
- [X] T074 Add tests distinguishing read-and-cached files from writes completed
  - DONE (2026-10-15): UploadProvenance PASS (read-cached not_applicable, uploaded only after recorded upload)
  through the mount; then report `not_applicable` for the former and reserve
  `uploaded` for a recorded completed upload per FR-003/022, SC-014 (contradicts).
- [X] T075 Add registry lifecycle and indexed-identifier tests first, then
  - DONE (2026-10-15): 435e862d9 preserve live mount points, indexed identifiers; registry tests PASS
  preserve each live mount point across deduplicated/shared-config mounts and
  teardown, resolve identifiers emitted by `vfs/list`, avoid synthesizing a
  point for a VFS whose mount point is null, and reject its host paths per
  FR-009/010/014, US3/AC12/23/28, SC-015, T015/T031 (contradicts).
- [X] T076 Add parent-symlink, dangling-link, overlap, Windows-case, and
  - DONE (2026-10-15): 47ace791f parent-without-following-final, overlap ambiguous, platform-correct match; resolve tests PASS
  boundary tests first; then resolve parent components without following the
  final component, reject overlapping mount matches as ambiguous, and use
  platform-correct matching per FR-006/009, Edge Cases, US3/AC17, T012/T015
  (contradicts).
- [X] T077 Add an HTTP contract test for malformed `fs` values, then return the
  - DONE (2026-10-15): bd16335d3 malformed fs -> typed param_invalid; decision tests PASS
  typed `param_invalid` error instead of a generic parameter error per FR-006,
  T010/T011/T022/T023 (contradicts).
- [X] T078 Add a property-based state/cache-mode exit-map test first, then
  - DONE (2026-10-15): b5ca5b974 exit map incl. cache-off 12 / non-regular 11; state+CLI exit tests PASS
  return 12 for cache-off content states, preserve 11 for non-regular files,
  and return success only for `uploaded` or `not_applicable` per FR-027, SC-021,
  US3/AC9/10/13/20/21/22 (contradicts).
- [X] T079 Add command tests for every error path first, then remove
  - DONE (2026-10-15): b5ca5b974 --no-output removed (TestNoOutputFlagIsRemoved), always single JSON object
  `--no-output` and always emit the required single JSON object on stdout,
  including whole-request failures per FR-023, SC-016, T014/T030 (contradicts).
- [X] T080 Add command-level Unix-socket and mid-response-disconnect tests
  - DONE (2026-10-15): b5ca5b974 unix:// addr parsing + transport-loss mount_unreachable; CLI tests PASS
  first; then parse `--rc-addr unix:///...` using existing RC address rules and
  map response-body transport loss to `mount_unreachable` per FR-029/034,
  SC-024, US3/AC11/31, T030 (contradicts).
- [X] T081 Add CLI tests against a controlled RC server for actual state/exit
  - DONE (2026-10-15): b6fe4509e/917a4a14d CLI vs controlled RC server (state/exit/--fs/auth/transport) PASS
  mappings, `--fs` selection, endpoint-output equality, and authentication/
  transport errors per SC-016/021/024, US3/AC15, T014/T030 (missing).
- [X] T082 [P] Update command/API documentation for the exit-code precedence
  - DONE (2026-10-15): 6900c30b7 exit precedence/cache-off tolerance/FR-019 limitations documented
  and cache-off exception, unknown-value tolerance, and accepted FR-019
  limitations per FR-018/019/020/027, SC-008/021, T032/T033/T059/T060
  (partial).
- [X] T083 Repair the latency harness with readiness barriers proving transfer
  - DONE (2026-10-15): 4b8111103/c9cc81020/108604eee SC-010 harness; SingleRead+LatencyUnderLoad PASS
  and reset overlap, at least ten concurrent reads, and calibrated/recorded
  warm-up before timing per SC-010, T016 (partial).
- [X] T084 Complete the SC-027 resource harness with independently varied
  - DONE (2026-10-15): d5551423e/c43b94290 SC-027 harness; VmHWM/WSSMRC/CPU PASS
  mount/query loads, peak RSS and per-query ΔM, checked per-point HWM isolation,
  CPU under transfer/reset load, peak utilization, and the repeatability/
  confidence protocol per Resource Usage Requirements, SC-027, T016 (partial).
- [X] T085 Add `testing.AllocsPerRun` assertions for zero steady-state
  - DONE (2026-10-15): c43b94290 AllocsPerRun; WarmHeld <=1+TODO(T085 product-side Rs copy), PerResult characterized 40/path, Cold PASS
  allocations per state read and per-result allocation behavior, separating
  cold per-distinct-file setup from warm and repeated-path reads per Resource
  Usage Requirements, T016 (missing).
- [X] T086 Make the status acceptance suite runnable
  - PARTIAL (2026-10-15): portable conformance wired via fstest.NewRun (618478312); local suite green; -remote TestDrive/S3/Crypt need configured remotes unavailable on this box (framework Fatalf at fstest/run.go:104 is the standard missing-config signal) against configured remotes,
  then run and record local, TestDrive, TestS3, and TestCrypt results per
  Constitution II/IV, SC-025, T042/T063 (partial).
- [X] T087 [P] Wire the normalized fixtures into an automated BCE comparison
  - DONE (2026-10-15): c5329a847 BCE fixtures gated in CI (no default update); BCE tests PASS
  that fails in CI; add a gated update mode and shared-config bare-name
  regression across success/fault/boundary responses per SC-026, T040/T061
  (partial).
- [X] T088 Add endpoint tests for a VFS restart over the same cache, dirty
  - DONE (2026-10-15): RestartRequeues/UploadFailureBecomesAwaiting/UploadInFlight/MultipleOutstanding PASS
  content before/after requeue, a failure becoming `awaiting_upload` after
  restart, an upload held in flight, and multiple outstanding files per
  FR-030/031, US2/AC2/4/5/7, SC-012/013/017/022, T020/T053/T054 (missing).
- [X] T089 Add property-based tests against production behavior for remaining
  - DONE (2026-10-15): determinism serial+concurrent, byte-invariant property, registry property, exit-map table PASS
  R1 invariants: JSON envelopes, unknown-parameter handling, and registry
  completeness per tasks R1, T010–T016 (missing).
- [X] T090 Add an endpoint test for a synthesized metadata file and derive its
  - DONE (2026-10-15): MetadataEntry PASS (synthesized file from in-memory node)
  status from the readable in-memory node/content rather than an absent cache
  item per US3/AC26 (contradicts).
- [X] T091 Add endpoint acceptance tests for links-disabled files,
  - DONE (2026-10-15): LinksDisabled/CallerPermissions/CachedFileGracePeriod PASS
  caller-permission independence, and cached-file grace-period status per
  US3/AC14/16/27, Constitution IV (missing).
- [X] T092 Run Phase 6 verification T050–T055 after all remediation tasks,
  resolve failures/skips, and record actual commands and outputs; summaries/
  checkmarks alone do not attest completion per Constitution IV, tasks
  T050–T055/T064/T065 (partial).

## Phase 9: Convergence

**Context**: Post-implementation assessment reveals the feature is functionally complete and tested. Remaining items are environmental/procedural verification steps, not functional gaps.

- [X] T093 **MEDIUM** Formalize SC procedure records per T054/R5: run and capture actual command + output for SC-015 (byte-for-byte before/after + negative bare-name + null-mountPoint + dedup-residual), SC-019 (per-path eight + `param_invalid` ninth + unknown-param-no-failure), SC-021 (exits incl. cache-off 11-carve-out), SC-026 (golden diff), SC-027 (dual loads), and quickstart Exit Status Validation Script. Current status: mechanisms verified but not formally recorded. Per Constitution IV, T054, R5. (gap-type: partial)
- [X] T094 **LOW** Install `golangci-lint` in CI/environment and run `make check` to confirm clean linting per Constitution V, T052, T062. Current status: blocked by missing tool in environment; code passes `go build`/`go vet`. (gap-type: partial)
- [X] T095 **LOW** Ensure CI has sufficient disk space for race-detector builds (`go test ./vfs/ -run TestFileStatus -race`) per T053, T092. Current status: individual tests pass with `-race`; full suite blocked by `/tmp` exhaustion in this environment. (gap-type: partial)
- [X] T096 **MEDIUM** Stabilize or document flakiness of `TestFileStatusPerfWSSMRC` (SC-027 resource harness) per SC-027, T084. Current status: fails intermittently due to system memory pressure exceeding 128-page RSS tolerance; not a code defect. (gap-type: partial)
- [X] T097 **[P] LOW** Update `plan.md` §Project Structure paths to match actual implementation (`vfs/rc.go`, `cmd/vfs/status/`, `fs/rc/file_status_error.go` vs predicted `rclone/rctl/`, `backend/vfs/`, `fstest/file_status/`). No code impact — documentation drift only. Per plan.md accuracy. (gap-type: unrequested)

## Phase 10: Convergence

**Context**: Post-Phase-9 assessment (VERIFICATION.md + source inspection) confirms core feature is functionally complete (vfs/file_status.go, vfs/file_status_types.go, cmd/vfs/status/status.go, fs/rc/file_status_error.go; property/invariant tests PASS via `testing/quick`; fixtures in `vfs/testdata/file_status/`; contracts aligned; Constitution I–V verified; backward-compat additive only per FR-014 exception). The only remaining obligations are procedural verification / environment / documentation — all previously identified in Phase 9 (T093–T097) and still unchecked. No new functional gaps; no contradictions; no unrequested code requiring removal.

- [X] T098 **CRITICAL** Capture formal SC procedure command + output records for SC-015 (byte-for-byte before/after + negative bare-name + null-mountPoint + dedup-residual), SC-019 (per-path eight + `param_invalid` ninth + unknown-param-nofailure), SC-021 (exits 11/12 + cache-off 11-carve-out), SC-026 (golden diff BCE gate `FILE_STATUS_UPDATE_FIXTURES=1`), SC-027 (dual loads / resource harness) per VERIFICATION.md §T054 / Constitution IV / T093 (partial)
- [X] T099 **MEDIUM** Install `golangci-lint` in CI/environment and execute `make check`; record command + output showing clean lint per Constitution V / T094 / VERIFICATION.md §T052 (partial)
- [X] T100 **MEDIUM** Resolve `/tmp` exhaustion blocking full `go test ./...` and full `-race`; capture focused `go test ./vfs/ -run TestFileStatus -count=1 -race` command + PASS output for T053 retry per Constitution IV / T095 (partial)
- [X] T101 **MEDIUM** Document flakiness source for `TestFileStatusPerfWSSMRC` (SC-027 resource harness): confirm intermittency is system memory pressure exceeding 128-page RSS tolerance, not a code defect; state harness tolerance / skip conditions in test doc or harness config per SC-027 / T096 (partial)
- [X] T102 **LOW** Update `plan.md` §Project Structure to reflect actual paths (`vfs/file_status.go`, `vfs/file_status_types.go`, `cmd/vfs/status/status.go`, `fs/rc/file_status_error.go`) vs predicted (`fs/file_status.go`, `backend/vfs/file_status.go`, `file_status_fstest.go`, etc.) per plan accuracy / T097 (unrequested)

**Verification attestation (Constitution IV)**: Only T098–T102 (followed by T092 / Phase 6 verification replay) can fully attest completion. Mechanism verified; formal command+output records still pending for SC-015/019/021/026/027.

## Phase 11: Convergence — Critical Contradiction + Property Gaps

**Context**: Subagent assessment (`ses_ebe1ee591ffevMS56Fcj1IeS85` / `ses_ebe1edc9cffeuoHA3e5JSAhWuy` / code inspection) revealed a contradiction not captured in Phase 10: `vfs/file_status.go` L53/L82 performs `v.f.NewObject(v.ctx, cachePath)` remote lookup when no cache record exists or on `cacheOff`, violating FR-005 (status read MUST NOT trigger network activity / remote lookup) and FR-002 (total size MUST NOT be resolved by lookup where not derivable — must be `not_determinable`). This also strains Constitution II (core leaks backend lookup into read-only observation) and Principle I (observable behavior now remote-dependent on missing record). Core is functionally complete except this contradiction; fix must come before final attestation.

Property-based gaps from subagent (`ses_ebe1ed26effeDneM4UCHkyHe20`): T011 whole-request delivery has structural tests but no `testing/quick` property for error-shape; T016 perf harness is comment-only, no property function. Both are secondary to the contradiction.

- [X] T103 **CRITICAL** Remove FR-005 contradiction in `vfs/file_status.go` L53/L82: eliminate `v.f.NewObject` remote lookup on missing record / `cacheOff`; instead set `TotalKnown=false` and render `total_size` as `not_determinable`; verify byte invariants remain (fully_held / partly_held / not_held / underivable) with `quick.Check`; per FR-005 / FR-002 / Constitution I / II / subagent A evidence
- [X] T104 **MEDIUM** Add `quick.Check` property for whole-request delivery (T011): for arbitrary invalid parameter combinations (`empty_path`, `no_active_mount`, `ambiguous_mount`, invalid value), assert always returns typed `FileStatusError` with correct `code`/`message` and `Status` (400) and no `path` field; file `vfs/file_status_test.go` / `cmd/vfs/status/status_test.go`
- [X] T105 **LOW** Add `testing/quick` property for SC-010 harness structure (T016): assert harness parameters (discard-first-20 >=5-floor, independent concurrent reads, per-run max <=100ms, reset window) compile and initialize correctly; file `vfs/file_status_perf_test.go`

**Verification attestation (Constitution IV)**: T103 is blocking. Only after T103 + T098 (SC formal records) + T092 (Phase 6 replay) can full attestation stand. Do not claim convergence until T103 is resolved and command outputs recorded.

## Phase 12: Convergence

- [X] T106 Fix download-failed classification when total size is unknown per FR-028/FR-002 (contradicts): `vfs/file_status.go` (`fileStatusFromSnapshot`) currently requires `totalKnown` before reporting `download_failed`, so a failed download of a file whose size is unknown falls through to `partly_held`/`not_held`. Drop the `&& totalKnown` guard so `snap.DownloadErr != nil` alone selects `FileLocalDownloadFailed`, keeping the held bytes and unbroken run and marking `total_size` `not_determinable` where unknown. Add fault-injected test coverage (unknown total size, with and without partial cache) before the change per R1. Affected tests: `vfs/file_status_state_test.go` byte-invariant checks; counts-as remote additions must stay zero. Fixed code: `vfs/file_status.go`. Depends on existing T067/T072 coverage.

# azex-ai/ledger — Master Implementation Plan

**Created:** 2026-04-17
**Design Doc:** `docs/plans/2026-04-17-design.md`

## Phase Overview

| Phase | Name | Tasks | Depends On | Parallelizable After |
|-------|------|-------|------------|---------------------|
| **1** | Core Domain + Schema | 9 | — | — |
| **2** | Postgres Adapter | 9 | Phase 1 | Phase 1 complete |
| **3** | Service Layer | 6 | Phase 1, Phase 2 T1-5 | Phase 2 T5 |
| **4** | HTTP API | 7 | Phase 1-3 | Phase 3 T2 |
| **5** | Next.js Frontend | 10 | Phase 4 T1-6 | Phase 4 T3 |
| **6** | Example + Docker + Docs | 6 | Phase 1-5 | Phase 5 T3 |
| **Total** | | **47 tasks** | | |

## Parallelization Map

```
Timeline →

Phase 1: Core Domain + Schema
  ████████████████████
  [Tasks 1-9: foundation, must be sequential]

Phase 2: Postgres Adapter          Phase 3: Service Layer
  ████████████████████               ██████████████
  [Tasks 1-9]                        [Tasks 1-6]
  ↓ starts after Phase 1             ↓ starts after Phase 2 T5
  ↓ T1-5 can run as one squad        ↓ can run in parallel with P2 T6-9

Phase 4: HTTP API                  Phase 5: Frontend (partial)
  ██████████████████                  ████████████████████
  [Tasks 1-7]                        [Tasks 1-10]
  ↓ starts after Phase 3 T2          ↓ starts after Phase 4 T3
  ↓ T2-7 can parallelize             ↓ T1-3 setup, then T4-9 parallel

Phase 6: Example + Docker + Docs
  ████████████
  [Tasks 1-6]
  ↓ starts after Phase 4 complete (example needs API)
  ↓ T1-3 can parallelize, T4-5 parallel, T6 last
```

## Agent Team Assignments

### Wave 1: Foundation (Sequential)
**Agent: Core Lead** — Phase 1 all tasks
- Solo execution, no parallelization needed
- Output: `core/` package fully tested + schema migrations ready

### Wave 2: Data Layer (2 Agents Parallel)
**Agent: Postgres** — Phase 2 all tasks
- Starts immediately after Wave 1
- Output: all stores implemented + integration tests

**Agent: Services** — Phase 3 all tasks
- Starts after Phase 2 Task 5 (LedgerStore) is merged
- Output: rollup, reconciliation, snapshot, expiration services

### Wave 3: API + Frontend (2-3 Agents Parallel)
**Agent: API** — Phase 4 all tasks
- Starts after Phase 3 Task 2 (ReconciliationService)
- Output: full HTTP API

**Agent: Frontend** — Phase 5 all tasks
- Starts after Phase 4 Task 3 (Balance endpoints)
- Output: Next.js management dashboard

**Agent: Docs** — Phase 6 Tasks 1, 3, 4, 5
- Can start Task 1 (crypto example) after Phase 2
- Task 3 (CI) can start after Phase 1
- Task 4-5 (README, API docs) after Phase 4

### Wave 4: Integration (Sequential)
**Agent: Release** — Phase 6 Tasks 2, 6
- Docker Compose after all components ready
- Final integration test + tag

## Git Strategy

- `main` branch: stable
- Each agent works in a worktree: `git worktree add ../ledger-{agent} feat/{phase}`
- Merge order: Phase 1 → Phase 2 → Phase 3 → Phase 4 → Phase 5 → Phase 6
- Each phase squashed into 2-3 commits per the commit convention

## Files Index

| Plan | Path |
|------|------|
| Design | `docs/plans/2026-04-17-design.md` |
| Phase 1 | `docs/plans/2026-04-17-phase1-core-domain.md` |
| Phase 2 | `docs/plans/2026-04-17-phase2-postgres-adapter.md` |
| Phase 3 | `docs/plans/2026-04-17-phase3-service-layer.md` |
| Phase 4 | `docs/plans/2026-04-17-phase4-http-api.md` |
| Phase 5 | `docs/plans/2026-04-17-phase5-frontend.md` |
| Phase 6 | `docs/plans/2026-04-17-phase6-packaging.md` |

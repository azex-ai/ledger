# Phase 5: Next.js Frontend

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the Next.js management dashboard for the ledger. 12 pages covering all ledger operations, with dark theme (crypto-native).

**Architecture:** Next.js 16 App Router, Server Components by default, "use client" only at leaf nodes for interactivity. React Query v5 for server state. shadcn/ui components.

**Tech Stack:** Next.js 16, shadcn/ui, Tailwind CSS v4, React Query v5, TypeScript strict, Recharts (charts)

**Depends on:** Phase 4 Tasks 1-6 (API must be available)

**Can parallelize with:** Phase 6 (example + docs)

---

### Task 1: Next.js Project Setup

**Files:**
- Create: `web/` — full Next.js project via `create-next-app`

**Step 1: Init project**

```bash
cd /Users/aaron/azex/ledger
npx create-next-app@latest web --typescript --tailwind --eslint --app --src-dir --no-import-alias
cd web
npx shadcn@latest init # dark theme, zinc palette
npm install @tanstack/react-query recharts
```

**Step 2: Configure**

- `web/src/lib/api.ts` — API client pointing to `NEXT_PUBLIC_API_URL` (default `http://localhost:8080`)
- `web/src/lib/hooks/` — React Query hooks directory
- `web/src/app/layout.tsx` — dark theme, QueryClientProvider, sidebar nav

**Step 3: Commit**

```bash
git add web/ && git commit -m "feat(web): init Next.js project with shadcn/ui dark theme"
```

---

### Task 2: Layout + Navigation

**Files:**
- Create: `web/src/app/layout.tsx`
- Create: `web/src/components/sidebar.tsx`
- Create: `web/src/components/providers.tsx`

**Sidebar links:**
Dashboard, Journals, Balances, Reservations, Deposits, Withdrawals, Classifications, Journal Types, Templates, Currencies, Reconciliation, Snapshots

**Commit:** `feat(web): add sidebar navigation and app layout`

---

### Task 3: API Client + React Query Hooks

**Files:**
- Create: `web/src/lib/api.ts` — typed fetch wrapper
- Create: `web/src/lib/hooks/use-journals.ts`
- Create: `web/src/lib/hooks/use-balances.ts`
- Create: `web/src/lib/hooks/use-reservations.ts`
- Create: `web/src/lib/hooks/use-deposits.ts`
- Create: `web/src/lib/hooks/use-withdrawals.ts`
- Create: `web/src/lib/hooks/use-metadata.ts` (classifications, journal types, templates, currencies)
- Create: `web/src/lib/hooks/use-system.ts` (health, reconcile, snapshots)

Each hook follows React Query v5 patterns:
- `useQuery` for reads
- `useMutation` with `onSuccess` invalidation for writes
- Cursor-based pagination via `useInfiniteQuery`

**Commit:** `feat(web): add typed API client and React Query hooks`

---

### Task 4: Dashboard Page

**Files:**
- Create: `web/src/app/page.tsx`
- Create: `web/src/components/dashboard/health-cards.tsx`
- Create: `web/src/components/dashboard/balance-trend.tsx`

**Features:**
- 4 health cards: Rollup Queue Depth, Checkpoint Max Age, Active Reservations, Last Reconcile Result
- Balance trend line chart (from snapshots API, last 30 days)
- Recent journals list (last 10)

**Commit:** `feat(web): add Dashboard page with health cards and balance trend chart`

---

### Task 5: Journals Page

**Files:**
- Create: `web/src/app/journals/page.tsx`
- Create: `web/src/app/journals/[id]/page.tsx`
- Create: `web/src/components/journals/journal-table.tsx`
- Create: `web/src/components/journals/journal-detail.tsx`
- Create: `web/src/components/journals/entry-flow.tsx` — Sankey/arrow visualization
- Create: `web/src/components/journals/post-journal-dialog.tsx`
- Create: `web/src/components/journals/template-journal-dialog.tsx`

**Features:**
- Paginated table with filters (journal type, time range, account holder)
- Detail view: journal metadata + entries table + fund flow diagram (arrows showing debit→credit between classifications)
- Manual journal posting dialog
- Template-based posting dialog
- Reverse button on detail view

**Commit:** `feat(web): add Journals page with detail view and fund flow visualization`

---

### Task 6: Balances Page

**Files:**
- Create: `web/src/app/balances/page.tsx`
- Create: `web/src/components/balances/balance-table.tsx`
- Create: `web/src/components/balances/balance-trend.tsx`

**Features:**
- Search by account holder
- Balance breakdown per classification (table)
- Balance trend chart per classification (from snapshots)

**Commit:** `feat(web): add Balances page with search and trend chart`

---

### Task 7: Reservations + Deposits + Withdrawals Pages

**Files:**
- Create: `web/src/app/reservations/page.tsx`
- Create: `web/src/app/deposits/page.tsx`
- Create: `web/src/app/withdrawals/page.tsx`
- Components for each: table, status badge, action buttons

**Features:**
- Reservations: list + filter by status, settle/release buttons
- Deposits: list + status filter, state machine visualization (stepper)
- Withdrawals: list + status filter, review approve/reject, retry button

**Commit:** `feat(web): add Reservations, Deposits, Withdrawals pages`

---

### Task 8: Metadata Pages (Classifications, JournalTypes, Templates, Currencies)

**Files:**
- Create: `web/src/app/classifications/page.tsx`
- Create: `web/src/app/journal-types/page.tsx`
- Create: `web/src/app/templates/page.tsx`
- Create: `web/src/app/currencies/page.tsx`
- Create: `web/src/components/templates/template-editor.tsx` — visual debit/credit editor

**Features:**
- CRUD tables for each entity
- Template editor: left column (debit lines), right column (credit lines), drag classification into position, fill amount_key and holder_role
- Template preview: fill params → see rendered entries in real-time

**Commit:** `feat(web): add metadata management pages with visual template editor`

---

### Task 9: Reconciliation + Snapshots Pages

**Files:**
- Create: `web/src/app/reconciliation/page.tsx`
- Create: `web/src/app/snapshots/page.tsx`

**Features:**
- Reconciliation: "Run Global Check" button, results display (balanced/unbalanced, gap amount, per-account details)
- Snapshots: date picker, holder/currency filter, historical balance table

**Commit:** `feat(web): add Reconciliation and Snapshots pages`

---

### Task 10: Docker Setup

**Files:**
- Create: `web/Dockerfile`
- Create: `web/next.config.ts` — standalone output

**Commit:** `feat(web): add Dockerfile for standalone deployment`

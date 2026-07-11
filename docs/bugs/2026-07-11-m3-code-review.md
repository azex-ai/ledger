# Code Review — M3 补偿控制（阈值门 + 双 provider 对账 → review 队列）

**范围**：`git diff c82526b~1..HEAD -- ':!docs'`（#10 core + #11 HTTP，13 文件 ~1089 行）
**分支**：feat/crypto-deposit　**方法**：codegraph sync + 通读 diff + producer↔consumer 交叉追踪 + 设计 §9 逐条核对 + `go build`/`vet` 绿 + server handler 测试跑绿（1.4s）
**结论**：**0 Critical / 0 Major**。设计 §9 规格全部落地，I-21 有 testcontainers pin，闸的顺序/事务边界/decimal 精确比较/幂等未放松均成立。**建议合并**。仅 3 Minor + 3 Nitpick（均非阻塞）。

---

## 逐点核对（lead 的 6 个重点，全部 PASS）

### 1. review 期账本零变动（I-21 核心）— ✅ PASS
- `routeToReview`（onchain.go:687）只调 `Booker.Transition` 到 `review` + 写 `review_reason` metadata，**从不 PostJournal**。journal_uid 恒空。
- 过账只发生在 `postDepositConfirmedJournal`（onchain.go:719），该函数只被 ①`advanceConfirmation` 正常 confirming→confirmed（reason=="" 分支）②`ApproveReview` 调用。
- pin：`TestOnchain_IngestDeposit_OverCeiling_RoutesToReview`（integration_test:731）断言 review 态 `JournalUID` 空；`TestOnchain_ApproveReview_PostsJournalWithEventLink`（:892）断言 approve 后才有 journal。testcontainers 真 PG 驱动，符合 §9.5。

### 2. 过账路径共享正确性 — ✅ PASS
- `postDepositConfirmedJournal` 是唯一过账实现，正常路径与 ApproveReview **共享同一函数** → 两条路径不可能对「journal 怎么过账」分叉。
- EventID 互链：函数内 `booker.Transition`→拿 `evt.UID`→`journals.ExecuteTemplate(EventUID: evt.UID)`，全在同一 `TxComposer.RunInTx` 内 → 两路径都原子 + 互链。
- 重复过账幂等：`ApproveReview`（onchain.go:757）先读 status，`confirmed` → no-op 返回当前 booking（`TestOnchain_ApproveReview...` :921 断言二次 approve 不产生第二个 journal，journalUID 不变）。并发双 approve 也安全：第二次的 review→confirmed Transition 会因状态已变 fail + journal idempotency_key `deposit-confirm-<uid>` UNIQUE 兜底。

### 3. 闸的顺序与事务边界 — ✅ PASS
- `reviewGate`（onchain.go:663）在 `advanceConfirmation` 的 confirming case、`postDepositConfirmedJournal`（=RunInTx 入口）**之前**调用（onchain.go:631-641）。
- 对账 RPC `DepositConfirmer.ConfirmDeposit` 在 `reviewGate` 内、**任何 RunInTx 之外**（golang.md「DB 事务内禁外部调用」红线不破）。pin：整个 reviewGate 无 TxComposer 引用。
- decimal 精确比较：`AutoCreditCeiling.IsPositive()` / `Amount.GreaterThan` / `amount.Equal(booking.Amount)` / `Amount.LessThanOrEqual` 全走 shopspring/decimal，**无 float**。

### 4. 幂等未放松（最关键）— ✅ PASS，非双花放松
- `idempotency_match.go` 把 `review_reason`/`reject_reason` 加入 `bookingMetadataObservationVariantKeys`（strip 后再比）。**这不放松双花**，理由：
  - **amount 是 metadata 之外的独立字段**，在 `ensureBookingMatchesInput`（idempotency_match.go:97）以 `existing.Amount.Equal(input.Amount)` **精确比对**，与 metadata strip 完全无关 → 不同 amount 的 sighting 仍在 amount 检查处 ErrConflict（`TestOnchain_IngestDeposit_ConflictingPayloadIsDeadLettered` :401 覆盖）。
  - 这两个 key **只由 `routeToReview`/`RejectReview` 的 Transition.Metadata 写入**（onchain.go:693/849），发生在 CreateBooking **之后**。IngestDeposit 的 `CreateBookingInput.Metadata` 只含 chain_id/tx_hash/txlog_seq/token/block_number（onchain.go:562-568），**任何 sighting 重放的 CreateBooking payload 永不携带** review_reason/reject_reason。
  - 因此 strip 只从 existing 侧移除（input 侧本就没有），不会掩盖任何真实的 CreateBooking payload 冲突；escape hatch 依旧狭窄（chain/tx/txlog_seq/token 仍逐字精确比）。
- `chain_id`/`tx_hash`/`txlog_seq`/`token` 仍精确比对（未 strip）。

### 5. lifecycle expand-safe + approve/reject 幂等 — ✅ PASS
- `DepositLifecycle`（presets/deposit.go）新增 `review` 状态 + `confirming→review`、`review→{confirmed,failed}` 边，**旧边一条未删**，`review` 非终态（Terminal 仍 confirmed/failed/expired）→ expand-only，在途老 booking 不受影响（§9.6）。
- approve/reject 幂等：非 review 态返回明确 `core.ErrConflict`（不 panic），已 confirmed/failed 二次调 no-op。pin：`TestOnchain_ApproveReview_RejectReview_ConflictWhenNotReview`（:966）。

### 6. HTTP 表面 — ✅ PASS
- 包络：`httpx.OK`/`httpx.Error` + `PagedResponse[bookingResponse]`（cursor 分页，`list`/`next_cursor`）。
- uid-only：`bookingToResponse` 出 uid、无内部 BIGSERIAL id。
- 鉴权：`GET /deposits/reviews` 在 **ScopeRead** 组（routes.go:62），`POST .../review/approve|reject` 在 **ScopeWrite** 组（routes.go:113-114）—— 与 §9.4 一致。
- 未配置降级：`depositReviewer==nil` → `bizcode.FeatureNotEnabled`（HTTP 503），非 500。三端点均覆盖测试（handler_test `*_NotEnabled`）。
- ErrConflict→bizcode 10901→HTTP 409（httpx/response.go:148 映射，handler_test `*_Conflict` 断言 409）。
- 内部字段不泄漏：handler_test:76-77 断言响应无 `factory`/`init_hash`。注：response 暴露 booking.Metadata（含 tx_hash/chain_id/token）——这是 **operator/admin 面（API-key 鉴权）**，非终端用户面；与既有 `GET /bookings` 一致，operator 审核正需要这些链上标识 → 符合 user-facing-surfaces.md（该规则约束终端用户面）。

---

## Minor（非阻塞，建议但可后续处理）

**[m1] 对账 RPC 持续失败时存款静默滞留 confirming，无专用可观测信号**
`reviewGate` 内 `ConfirmDeposit` 返回 err → `advanceConfirmation` 包装为普通 error 上抛（onchain.go:641）。IngestDeposit 只在 `ErrConflict` 时 dead-letter（onchain.go:571），reconcile error **不进 dead-letter**（正确：可重试）。方向是 fail-safe（宁可不入账也不误铸），但第二源 RPC 长时间宕机时，超过 ReconcileCeiling 的存款会一直卡在 confirming，**无 metric、无告警**，operator 难察觉。
建议：为 reconcile RPC 失败加一个 metric（类比 `DepositReviewRequired`），或在设计上明确「reconcile 连续失败 N 次 → 路由 review」。当前实现可接受，属观测缺口。

**[m2] I-21 的 pin 在 service 集成测试，不在 `postgres/invariants_test.go`**
既有 I-19/I-20 的 canonical pin 都在 `postgres/invariants_test.go`。I-21 的 pin（review 无 journal / approve 后有）落在 `service/onchain_integration_test.go`，虽同为 testcontainers 真 PG 驱动、覆盖充分，但与既有不变量的组织位置不一致，后来者按 INVARIANTS.md 找 I-21 pin 可能扑空。
建议：在 `invariants_test.go` 加一个 I-21 pin 或注释指向 service 测试。

**[m3] handler 的 nil-check 与 uid-check 顺序在三个 handler 间不一致**
`handleListDepositReviews` 先查 `depositReviewer==nil`；`handleApprove/RejectDepositReview` 先查 uid 再查 nil（handler_deposit_reviews.go:79/108）。行为无 bug，但顺序不统一。建议统一为「先 nil（feature gate）后参数校验」，语义更清晰。

---

## Nitpick（可忽略）

**[n1]** `routeToReview` 用 `chainID, _, _, _, _ := parseDepositMeta(...)`（onchain.go:711）丢弃 ok；metadata 若 malformed，chainID=0 仅影响日志/metric label，booking 仍正确路由。无实质影响。

**[n2]** `reviewGate` reconcile 只比对 `amount`+`included`，不复核 to 地址/token。因 `(chainID,txHash,txLogSeq)` 已唯一确定该 log 的 to/token（第二源用同一 `core.ChainReader` 派生逻辑），amount 相等即足够 —— 符合 §9.3，仅记录该隐含前提（第二源必须与主源用同一 log-seq/decimals 归一语义，否则误判 mismatch → 多余 review，仍 fail-safe）。

**[n3]** `over_ceiling` 门在 reconcile 之前短路（onchain.go:664），amount 同时越两条线时 reason 记 `over_ceiling`。合理（任一门触发即 review），仅记录优先级。

---

## 查过无发现（明确列出）
- **core 依赖红线**：core/interfaces.go 的 `DepositConfirmer`、core/onchain.go 的 ceiling 字段只 import decimal，未引入 http/pgx/chi。OK
- **DB 事务内无 RPC**：reconcile RPC 在 RunInTx 之外。OK
- **过账原子性 + EventUID 互链**：两条路径共享 `postDepositConfirmedJournal`。OK
- **decimal 全程精确**：无 float64。OK
- **闸不可被任何路径绕过**：IngestDeposit 内联 advanceConfirmation 与 recheck loop 的 advanceConfirmation 走同一 confirming case → 两路径都过 reviewGate。OK
- **opt-in 语义**：ceiling=0 或 DepositConfirmer=nil → 走 pre-M3 路径（`TestOnchain_IngestDeposit_UnderCeiling_StillConfirms` / `..._ReconcileBelowCeiling_SkipsRPCCall` 覆盖）。OK
- **Prometheus 装配完整**：`depositReviewRequired` NewCounterVec + MustRegister + bounded label(chain_id,reason)+safeLabel。OK
- **advanceConfirmation review 早退**：`review` 加入 no-op switch，防止 webhook/watcher 重观测重评估门（测试 :757 覆盖）。OK
- **build/vet**：`go build ./...` + `go vet`（service/server/core/postgres）绿。
- **server handler 测试**：9 个用例（list/approve/reject × 成功/conflict/not-enabled + missing-reason）全绿。

---
_评审 only，未改任何代码。_

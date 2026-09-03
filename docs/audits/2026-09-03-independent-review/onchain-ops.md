# 独立复审 · 链上资金路径与可运维性

- **审计范围**：链上事件 → 账本入账（`chains/evm` + `service/onchain.go` + `channel/onchain`），账本 → 归集出链（sweep saga），以及「凌晨三点 on-call」视角的可运维性（RUNBOOK / DR / CAPACITY / `ledger-cli` / `core.Metrics`）。
- **基线 commit**：`c854c6e`（worktree `r3-onchain-ops`，主仓只读）
- **未读**（按指派要求）：`docs/audits/`、`docs/plans/2026-09-02-*`、`docs/plans/2026-09-03-*`
- **实跑环境**：`postgres:17.2-alpine` 容器（端口 15911），`postgres.Migrate` 全量迁移成功；`ledger-cli` 从源码构建；`anvil`（foundry）在位，`chains/evm` 的 `-tags e2e` 实跑通过。
- **实验后清理**：临时容器与 `.scratch/` 已删除，主仓除本报告外无改动。

---

## 1. 摘要

代码本身的**决策纪律**相当扎实：cursor 失败即不前进（I-52）、扫描不越 `latest-Confirmations+1`（I-53）、`TxLogSeq` 取 receipt 位置（G-C2）、balance 不可读绝不当零、gas ceiling 校验的量与实付量已对齐、`priorFeeFloor` 已会读链上真值。绝大多数「单次观测做不可逆决策」的地方都已经有闸或有记录。

问题几乎全部集中在**两个接缝**上：

1. **「决定能看见哪笔链上钱」的那一个值没有护栏、没有审计、没有恢复路径。** `chain_cursors.last_scanned_block` 是唯一决定 forward scan 覆盖范围的状态；它没有任何 DB 触发器、`ledger_app` 持有裸 `UPDATE`、单调性只存在于应用层 SQL 的 `WHERE` 子句里，而 `postgres/audit_trail_guard_test.go:280` 用一句「monotonic-protected on write … 幂等键会吸收 gap … it cannot move money」把它从审计表白名单里排除掉了 —— 这三句话每一句都与 `scanChainOnce` 自己的 I-52 注释矛盾。forward jump 造成的 gap 是**永久的、无痕的、无任何 reconcile check 能发现的**（从没入账的存款在账本里零足迹），而且应用层**无法**回退 cursor 去补扫。

2. **「入账失败」这条支路整条是暗的。** dead-letter 是被跳过的 sighting 唯一的持久痕迹，而它：无 metric、无 CLI 子命令、无 HTTP 端点、RUNBOOK/DR/CAPACITY/api.md **零次提及**、无重放路径。设计档 §140 明确要求「告警 + dead-letter 表人工看」，告警不存在。同一形状还有 `deposit_reorgs`（`ResolveReorg` 在生产代码里零调用者 → 深 reorg 告警永远关不掉）、`ingest_dead_letters`、`registration_rescans`、`chain_cursors` —— 五张链上表在任何运维文档里都不存在。

on-call 视角的三问答得如何：
- **「发生了什么」**：对**已入账**的存款很好（`ledger-cli trace` 实跑输出完整 booking + event + journal 链）。对**未入账**的存款完全无路可走。
- **「钱在哪」**：`solvency` / `balance` / `balances` 实跑正确。但 §8「Compute live balance for an account」的 SQL **跑不起来**（`GROUP BY` 缺失），而 §1 在怀疑 checkpoint 被篡改（P1）时明确要求用它手算。
- **「谁改了规则」**：`config-history` 覆盖 11 张表，实跑正常；但链上侧的规则（`AutoCreditCeiling` / `Confirmations` / `GasCeiling` / `CreditTokens`）全在 composition root 的 Go 代码里，不入库、不可查 —— 这是库形态的必然，只是 RUNBOOK 那一节标题写的是「who changed the rule that decides where money goes」，读者会以为覆盖全了；而**真正在库里、真正决定钱的走向**的 `chain_cursors` 恰恰被排除在外。

计数：**Critical 2 / Major 7 / Minor 12**。

---

## 2. 不可逆决策 × 观测次数

「观测次数」= 做出该决策前独立的链上观测**次数**；「独立源」= 是否有第二个 RPC provider / 第二套证据。`chains/evm/client.go:24` 起 `NewClientSet` 每链只 dial **一个** RPC endpoint，无 failover、无重试中间件 —— 所以除 `DepositConfirmer` 之外，全部观测同源。

| # | 不可逆决策 | 代码位置 | 观测次数 | 独立源 | 闸门 | 评价 |
|---|---|---|---|---|---|---|
| 1 | **给用户入账（铸余额）** confirming→confirmed，posts `deposit_confirm` | `service/onchain.go:1050-1063`（`advanceConfirmation`）→ `1341`（`postDepositConfirmedJournal`） | 1 次 log + 1 次 receipt + 1 次 `LatestBlock`，**全部同一 provider** | 仅当 `Amount > ReconcileCeiling` 且 `DepositConfirmer` 已配置 | `AutoCreditCeiling`（金额上限，无安全默认，启动强制）+ 可选二源核对 | 已知且已文档化的信任模型；闸门设计正确 |
| 2 | **拒付一笔阈值前存款**（shallow reorg → `failed`，终态且幂等键永久吸收后续 sighting） | `service/onchain.go:1796-1846` | **3 次**连续 `TxIncluded=false`（`shallowReorgMisses` 默认 3，`recheckInterval` 默认 20s → **约 40 秒窗口**）；计数在内存，重启归零 | 无 | 连续观测计数器 | 有闸，但窗口很短且同源 —— 见 Minor-3 |
| 3 | **自动冲正一笔已确认存款（扣用户钱）** `ReorgPolicyAutoReverse` | `service/onchain.go:1996`（单次 `TxIncluded`）→ `2007` `handleReorg` → `2031` `ReverseJournal` | **1 次** | 无（`DepositConfirmer` 已在 deps 里，此路径不用） | 只有「选了这个 policy 就是接受风险」的文字 | **Major-1**：后果更重的一侧反而没有 #2 那个连续观测闸 |
| 4 | **推进 forward cursor**（扫过即不再回头，`from = cursor+1`） | `service/onchain.go:1524-1656`；`postgres/sql/queries/chain_cursors.sql` | 每 tick 1 次 `LatestBlock`；I-52 保证「有 sighting 没入账就不前进」，I-53 保证不越 safe tip | — | 应用层单调 upsert + per-chain advisory lock | 应用路径正确；**DB 层无护栏无审计** → **Critical-1** |
| 5 | **dead-letter 并跳过一笔 sighting**（cursor 越过它，永不重扫） | `service/onchain.go:1618-1630`（watcher）、`855-866`（rescan）；判据 `1682 permanentIngestFailure` → `core/errors.go:92-118` | 1 次错误分类 | — | `core.IsRetryable` | **Critical-2**（无信号无重放）+ **Major-2**（分类把可恢复错误当永久错误） |
| 6 | **广播归集交易（钱出链）** | `chains/evm/sweeper.go:186-266` `BatchSweep` | 1 次 `PendingNonceAt` + 1 次 fee 询价 | — | 每 target 重新 CREATE2 派生并逐字比对（`sweeper.go:212-219`）、`GasCeiling` 对齐实付量、per-chain advisory lock 串行化 nonce | 这一处做得最好 |
| 7 | **永久判定 sweep「孤儿广播」并锁死该 (chain,token)** | `service/onchain.go:2404-2419` | **1 次** `NextNonce` | 无 | fail-closed + 指向 RUNBOOK §15 的人工恢复 | 方向正确（fail-closed），但 **Major-3**：§15 里没有那个恢复步骤 |
| 8 | **把一笔存款送人工审核**（可逆，人工可 approve） | `service/onchain.go:1180-1211` `reviewGate` | 0-1 次二源 | 视配置 | — | 正确 |

`*big.Int` → `decimal` 的全部转换点，逐个核过：

| 转换点 | 代码 | 判定 |
|---|---|---|
| Transfer log `value` → 入账金额 | `chains/evm/logs.go:70` `normalizeAmount` ← `reader.go:266` | 唯一输入是 `TokenConfig.Decimals`；`VerifyTokenDecimals` 会与合约 `decimals()` 核对（但**需 composition root 主动调用**，库无 binary 强制不了） |
| `balanceOf` / `getEthBalance` → sweep 余额 | `chains/evm/scanner.go:172`、`:253` | 非 32 字节一律 `unreadable`，绝不当零 —— 正确 |
| wei → gwei（gas ceiling 比对） | `chains/evm/sweeper.go:110`、`:145`（`weiGweiDecimals = 9`） | 与 `SweepPolicy.GasCeiling` 单位一致，且 `Validate` 会拒绝像 wei 的数值 —— 正确 |
| webhook `amount` 字符串 → decimal | `channel/onchain/evm.go:141` | `decimal.NewFromString`，无 float64 —— 正确 |
| **decimal → 落库精度** | `postgres/booking_store.go:144` `checkAmountPrecision` | **Major-2 的触发点**：`TokenConfig.Decimals` 上限 36（`core/onchain.go:340`）而 currency `Exponent` 上限 18（`core/interfaces.go:455`），两者从不交叉校验 |

---

## 3. RUNBOOK 逐条实跑

RUNBOOK 共 22 个代码块（13 sql / 6 bash / 2 go / 1 promql）。SQL 全部在容器里实跑（迁移后的真 schema）。

| 块 | RUNBOOK 行 | 内容 | 结果 |
|---|---|---|---|
| #0 | 47 | `curl POST /api/v1/reconcile` / `ledger-cli reconcile --full` | ⚠️ 两条返回**不同形状**：`/reconcile` 是 `{balanced,gap,details}`（`server/handler_reports.go:29`），`--full` 是 `{overall_passed,full_coverage,skipped_checks,checks[]}`。§1 下文的 `checks[].findings` 只对后者成立。Minor-8 |
| #1 | 240 | 找失衡 dimension + 按日期二分 | ✅ 两条都跑通 |
| #2 | 278 | `ledger-cli currencies` / `solvency --currency` | ✅ 实跑通过（见 §「CLI 实跑」） |
| #3 | 325 | rollup backlog 计数 + 最老 age | ✅ 跑通 |
| #4 | 364 | `ledger_rollups_stuck > 0`（promql） | ✅ 指标名存在（`observability/prometheus.go`，`rollups_stuck`） |
| #5 | 370 | 列出 stuck rollup items | ✅ 跑通 |
| #6 | 386 | `ledger-cli rollup reset-claim --id` | ✅ 跑通（空库返回 not found，正确） |
| #7 | 407 | checkpoint age | ✅ 跑通 |
| #8 | 428 | webhook 积压计数 | ✅ 跑通 |
| #9 | 441 | 「Inspect the subscribers」 | ⚠️ **代码块标为 ` ```bash ` 但内容是 SQL** —— 粘进 shell 直接语法错。Minor-9 |
| #10 | 567 | `ledger-cli trace --booking-uid` | ✅ 用真造的 confirmed 存款实跑，输出完整（booking + status + journal_uid + metadata 的 chain_id/tx_hash/txlog_seq/block_number） |
| #11 | 573 | `SELECT * FROM bookings WHERE id = 12345` 等三条 | ✅ 跑通。⚠️ 用**内部数字 id**，而告警/CLI 给的是 uid；RUNBOOK 未给 uid→id 的换算。Minor-10 |
| #12 | 584 | 某 dimension 的所有 journal | ✅ 跑通 |
| #13 | **596** | **「Compute live balance for an account」** | ❌ **跑不起来**：`ERROR: column "cp.balance" must appear in the GROUP BY clause`。加 `GROUP BY cp.balance` 后返回正确结果。**Major-4** |
| #14 | 614 | 递归列出 reversal 链 | ✅ 跑通 |
| #15 | 1006 | `ConfigChangeReader` 三个方法签名（go） | ✅ 与 `core.ConfigChangeReader` 一致 |
| #16 | 1016 | `ledger-cli config-history --table/--check/--holder` | ✅ 三种 flag 组合实跑通过 |
| #17 | 1348 | default partition 内容 | ✅ 跑通 |
| #18 | **1652** | stuck sweep bookings（§15 的确认查询） | ✅ 跑通（`classifications.code='sweep'` 存在，`metadata` 是 jsonb） |
| #19 | 1737 | `authdev.NewLocalVerifierSet`（go） | ✅ 函数存在 |
| #20 | 1792 | event 被哪个 journal 认领（§17） | ✅ 跑通 |
| #21 | 1816 | `SELECT ledger_unlink_event_journal(...)` | ✅ 函数在 `pg_proc` 里存在 |

**RUNBOOK 事实性错误（与代码/实跑不符）**

| RUNBOOK 行 | 声明 | 实际 |
|---|---|---|
| **83** / **180** | `unauthorized_journals` 未配 verifier 时「skipped (`Complete=false`)」 | 该 check **完全不出现在 `checks[]` 里**，而是进 `skipped_checks`（`service/reconcile.go:555-561`；`core/reconcile_extra.go:53-67` 明确写了「NOT … with Complete=false」）。实跑确认：不带 `--pubkey-hex` 时 15 项里没有它；带上才有 16 项。RUNBOOK 全文**从未提及** `skipped_checks`。**Major-5** |
| **1384** | §12 Alert source 是 `deposit.reorged` **event** | 全仓 grep：该事件名只存在于 RUNBOOK 与设计档，代码**从不发**。实际信号是 `service: onchain: deep reorg detected` 日志（`service/onchain.go:2008`）+ `ledger_deposit_reorg_detected_total`。§14 自己写的是「this metric is that section's alert source」，两处互相矛盾。Minor-1 |
| §12 全节 | 「on-call 按 §12 冲正并 closes the anomaly out」（`service/onchain.go:2023`、`postgres/deposit_reorg_store.go:91` 都指向 §12） | §12 **一次都没提** `deposit_reorgs` 表 / `ListOpenReorgs` / `ResolveReorg`，也没有关闭异常的步骤。**Major-6** |
| **1586** | `deposit_review_required_total` 的 `reason` 是 `over_ceiling` 或 `reconcile_mismatch` | 实际有 5 个：另有 `reconcile_unavailable`、`token_unconfigured`、`shallow_reorg_returned`（`service/onchain.go:1075-1108`）。§13 列了 3 个，仍漏 `token_unconfigured`（会真把 booking 停在 `review`）与 `shallow_reorg_returned`（**不是** booking 状态，`/deposits/reviews` 里查不到 —— on-call 按 §13「Work the queue」会走进死路）。Minor-2 |
| **1716** | sweep 耗尽 bump 转 `failed` 后「the nonce is freed for later sweeps to proceed」 | 代码说的正好相反：`reviveFailedSweep` 注释（`service/onchain.go:2240-2249`）——「From the signer EOA's perspective that nonce slot is still "next" … until either the stuck tx eventually lands, or something external moves the EOA past it」。RUNBOOK 让 on-call「不用管」，实际 EOA 可能仍被卡住。**Major-7** |
| §15 全节 | 代码在 `service/onchain.go:2417` 抛错要求「recover it manually per RUNBOOK §15 before this (chain,token) can sweep again」 | §15 的 Resolution 只有三条（等下一 tick / 超过 bump cap / 整链停摆），**没有任何一条**覆盖「booking 停在 pending 而 nonce 已被花掉」的 `sweep.orphaned_broadcast`。这是全库唯一会把一条出链通道永久锁死的错误，而它承诺的手册页不存在。**Major-3** |
| §14 整节 | 自称是「what does each metric mean and what do I do」的完整参考 | 漏掉 `job_tick_completed/failed/skipped_locked/panicked` 全族（`core/metrics.go:154-168` 明确写「see docs/RUNBOOK.md」，RUNBOOK 里 `job_tick` **零次出现**），也漏掉 `chain_cursors` / dead-letter / reorg 队列。Minor-4 |
| §14 `chain_cursor_lag` 行 | 「A monotonically climbing lag means the watcher loop is stuck」 | `reportLag` 只在拿到 `latest` 之后才可能被调用（`service/onchain.go:1578`）。`GetCursor` 失败、`LatestBlock` 失败（RPC 宕）、`ListAddresses` 失败（DB 宕）、`FetchDeposits` 失败（`eth_getLogs` 限流 —— 最常见的 stall）全都**直接 return，不上报** → gauge **冻结在旧值**，不增长。最常见的 watcher 停摆形态恰好触发不了 §14 教你写的那条告警。**Major-8** |

**DR.md**

| DR 行 | 声明 | 实际 |
|---|---|---|
| 218-238 | §5 步骤 1 `ledger-cli reconcile --full \| jq -e '.overall_passed == true'`，注释称覆盖「I-1..I-13 + I-23/I-24/I-32」 | 命令不带 `--pubkey-hex/--key-id`，`unauthorized_journals`（I-32）必然被跳过；`jq` 只断言 `overall_passed`，既不看 `full_coverage` 也不看 `skipped_checks` —— `core/reconcile_extra.go:66` 明确要求「assert on this field being empty, not merely on FullCoverage」。DR 自己在 205-211 行记录了这个缺口已在 26-09-02 修好并给了新 flag，但 §5 的验证配方没跟着改。Minor-5 |
| 105-110 | PITR 后「Ask upstream channels to replay events from T-onwards (e.g. re-scan blocks) — replay is safe by construction (I-4)」 | 对 push 路径成立。对 pull 路径没有「upstream」可求 —— cursor 也被回滚，会自然重扫（这部分自愈）。但 PITR **丢掉 T 之后的 `deposit_reorgs` 行**，而这些行只有在 booking 仍在 `reorgRecheckWindow`（500 块）内时才会被重建；更老的**永久消失**，正是这张表存在的理由（G-M8）。§5 的验证清单也不检查未结异常队列。Minor-6 |

**CAPACITY.md**：对链上路径**零覆盖** —— 没有 sweep / watcher / RPC / cursor 的容量或 SLO 条目，SLO 表只到 event delivery。Minor-7

---

## 4. 指标 × 生产调用点

用 AST-friendly grep（排除 `_test.go` / `core/metrics.go` / `observability/`）逐个方法找生产调用点。`core.Metrics` 共 41 个方法。

| 指标 | 生产调用点 | 备注 |
|---|---|---|
| `ChainCursorLag` | `service/onchain.go:1578` | 只在 `scanChainOnce` 内，且 4 条早退路径不上报 → Major-8 |
| `DepositReorgDetected` | `onchain.go:1838, 1964, 2009` | ✅ |
| `DepositReviewRequired` | `onchain.go:1227, 1972` | ✅（reason 有 5 个值，RUNBOOK 只写 2 个） |
| `SweepUnattributed` | `onchain.go:2161` | ✅（无行为 pin，已登记在 `untestedAlertMetrics`） |
| `SweepAddressUnreadable` | `onchain.go:2128` | ✅ |
| `RegistrationRescanFailed` | `onchain.go:820` | ✅ |
| `JobPanicked` | `worker.go:707`、`onchain.go:2748` | ✅ |
| `JobTickCompleted` / `JobTickFailed` | `locked_job.go:153/123,150`、`worker.go:498-653` | ⚠️ **onchain 五个 job 里只有 2 个会发**（走 `LockedJob` 的 watch/sweep）；`onchain_recheck` / `onchain_reorg_recheck` / `onchain_registration_rescan` 走 `Onchain.runLoop`（`onchain.go:2714`）+ `safeRunTick`（`:2740`），**只发 `JobPanicked`** → **Major-9** |
| `JobTickSkippedLocked` | `locked_job.go:129` | ✅ |
| **`ReservedAmount`** | **0** | **死指标**。已登记在 `observability/emission_coverage_test.go:54`（"follow-up: needs a new per-currency reserved-amount query"）。但 `core/metrics.go:98` 的文档没说它没接，读者会以为有 |
| **`PendingEvents`** | **0** | **死指标**。已登记在同文件 :61 / :196。`core/metrics.go:172-177` 的文档写「**sampled once per delivery job tick**」—— 这句话是假的，它从不被采样 → Minor-11 |
| 其余 37 个 | 各有 ≥1 处 | ✅ |

**指标名 × 文档交叉核对**：从 RUNBOOK / DR / CAPACITY / INVARIANTS 里 grep 出的所有 `ledger_*` token，逐个比对 `observability/prometheus.go` 的注册名 —— **全部命中，零个不存在的指标名**（MISS 项全是 SQL 函数名与 role 名，非指标）。这一项做得干净。

**缺失的指标（有事实、无信号）**：

| 事实 | 目前的痕迹 | 缺什么 |
|---|---|---|
| 一笔 sighting 被 dead-letter（真存款永不入账） | `ingest_dead_letters` 行 + Error 日志 | **无 counter**。设计档 §140 要求「告警 + dead-letter 表人工看」 |
| watcher 楔死（`escalateWatcherStall`，`onchain.go:1705`） | Error 日志 + dead-letter 行 | 无 counter（只有 `JobTickFailed{job="onchain_watch:N"}` 间接可见） |
| sweep 孤儿广播（该 chain/token 出链永久锁死，`onchain.go:2409`） | Error 日志 + 返回错误 | 无 counter。§15 自己承认「no dedicated metric exists」 |
| 未结 reorg 异常队列深度 | 无 | 无 gauge（`ListOpenReorgs` 已有，接一个 gauge 是几行的事） |
| `deposit_confirm` journal 反复失败（账户 frozen/closed/period closed） | 日志（默认 `NopLogger` → 静默） | 无 counter |

**job label 格式未文档化且不一致**：`LockedJob` 名字是 `onchain_watch:<chainID>`（`onchain.go:2785`）与 `sweep:<chainID>`（`onchain.go:2795`），而 `runLoop`/`JobPanicked` 用的是 `onchain_watch` / `onchain_sweep`。同一个 job 在 `job_tick_*` 与 `job_panicked` 两族里 label 不同名，且归集 job 的 label 前缀是 `sweep:` 而非 `onchain_`。`core/metrics.go:147-152` 说 job 是「fixed, bounded job name … never a free-form string」，这里把 chainID 拼进了 label。Minor-12

---

## 5. 发现

### Critical

---

#### C-1 `chain_cursors` 是唯一决定「能看见哪笔链上钱」的状态，却无 DB 护栏、无审计、无恢复路径 —— 而审计豁免清单用三条错误理由把它排除了

**位置**
- `postgres/audit_trail_guard_test.go:280`（豁免理由）
- `postgres/sql/queries/chain_cursors.sql`（`SetChainCursor` 的单调性 `WHERE`）
- `postgres/sql/migrations/001_baseline.up.sql:818`（建表，无触发器）
- `service/onchain.go:1524-1548`（`from = cursor+1`，只前进）

**判定：CONFIRMED**（容器内实测）

```
$ select relname, count(triggers) ...
chain_cursors        | 0     <-- 零触发器
deposit_addresses    | 2
bookings             | 2
journals             | 3

$ role_table_grants where table_name='chain_cursors' and grantee='ledger_app'
INSERT, UPDATE, SELECT   <-- 持有裸 UPDATE
```

豁免理由原文（`audit_trail_guard_test.go:280`）：

> `"public.chain_cursors": "scan progress per chain, monotonic-protected on write (B-m7). Corrupting it causes a rescan or a gap that deposit ingestion's idempotency keys absorb -- it cannot move money"`

三条论断逐条驳：

1. **"monotonic-protected on write"** —— 那个保护在**应用层 SQL** 的 `WHERE chain_cursors.last_scanned_block < EXCLUDED.last_scanned_block` 里。`ledger_app` 持有表级 `UPDATE`，`UPDATE chain_cursors SET last_scanned_block = 99999999` 不经过那条 query，什么都拦不住。而这张豁免清单讨论的**正是** DB 层的写保护 —— 用一个不在 DB 层的保护去豁免 DB 层的护栏，是循环论证。

2. **"a gap that deposit ingestion's idempotency keys absorb"** —— 幂等键吸收的是**重复**，不是**空洞**。`scanChainOnce` 自己的 I-52 注释（`service/onchain.go:1533-1541`）把这件事说得比我清楚：

   > *"the forward scan never looks back (`from` = cursor+1; the recheck loops only revisit bookings that already exist, and registration rescans only cover newly registered addresses) -- so one DB blip meant a real deposit that no code path would ever see again."*

   两个文件相距三行的距离，结论相反。gap 是**永久的**。

3. **"it cannot move money"** —— 它决定账本**永远看不见**哪些链上的钱。跳过区间里的真实存款不会入账（用户没拿到 credit），而钱仍然躺在那个公开可推导的 CREATE2 地址上，随后被 sweep job 归集进 treasury。因为该 token 仍在 `CreditTokens` 里，连 `SweepUnattributed` 都不会响。净效果：用户的钱进了金库，账本上没有对应负债，全程零告警。

**为什么没有任何东西能发现**：从没入账的存款在账本里**零足迹** —— 没有 booking、没有 event、没有 journal、没有 entry。15 项 reconcile check 全部检验的是账本内部一致性（journal vs entry vs checkpoint vs snapshot），没有一项能看见「链上有一笔钱我们从没记过」。solvency 检查是 `custodial >= liability`，跳过一笔存款只会让 liability 偏**小**，检查更「健康」。`chain_cursor_lag` 在 forward jump 后会变**小**，看起来也更健康。

**为什么恢复不了**：`SetCursor` 单调只前进（这是对的，防 replica 倒拉），所以应用层**没有任何 API** 能回退 cursor 去补扫。operator 唯一的办法是手写 `UPDATE chain_cursors SET last_scanned_block = <旧值>` —— 这件事 RUNBOOK 从未提及，而且它正是上面那个「无审计的裸 UPDATE」。

**建议修法**（三处，都很小）
1. 给 `chain_cursors` 加与 `deposit_addresses` 同款的 partial guard + `ledger_log_config_table_change` 触发器，把它纳入 §9 的 config-forensics 清单（从 11 张变 12 张）。
2. 在 DB 层落单调性，别只在 query 里：`CHECK` 无法表达跨行比较，用 `BEFORE UPDATE` 触发器 `IF NEW.last_scanned_block < OLD.last_scanned_block THEN RAISE`，并把「正当回退」做成一个显式的、会写审计行的 `ledger_rewind_chain_cursor(chain_id, to_block, reason)` 函数（形状照 `ledger_unlink_event_journal` —— 那个已经是「危险但必要的 operator 动作走具名函数 + 授权检查」的模板）。
3. 把 `audit_trail_guard_test.go:280` 那条豁免删掉（或改成指向新触发器）。这条 exclusion 是机器强制的白名单，留着它 = gate 永远绿。

---

#### C-2 dead-letter 是被跳过存款的唯一痕迹，而它无 metric、无 CLI、无端点、RUNBOOK 零提及、无重放路径

**位置**
- `service/onchain.go:1618-1630`（watcher：`permanentIngestFailure` → `recordIngestDeadLetter` → `continue` → cursor 前进）
- `service/onchain.go:855-866`（registration rescan 同形状）
- `service/onchain.go:1690-1697`（`recordIngestDeadLetter`，只写行 + 日志）
- `postgres/ingest_dead_letter_store.go:65-67`（`ListDeadLetters` 文档写 "for on-call triage (RUNBOOK)"）

**判定：CONFIRMED**

```
$ grep -c 'ingest_dead_letters'  docs/RUNBOOK.md docs/DR.md docs/CAPACITY.md docs/api.md
0 0 0 0
$ grep -rn 'ListDeadLetters' --include=*.go .   # 生产调用点
postgres/ingest_dead_letter_store.go:65   (定义本身，无调用者)
$ grep 'dead' server/routes.go             # 无端点
$ grep 'dead' cmd/ledger-cli/main.go       # 无子命令
```

一笔 sighting 被 dead-letter 意味着：**一笔真实的、已在链上确认的存款，账本决定永不为它建 booking，并且把 cursor 移过了它**。这是 pull 路径上最影响钱的单一事件。它目前的可观测性是：

- **一个 counter**：无。
- **一条 Error 日志**：库默认 `core.NopLogger()`（`service/onchain.go:462-464`）—— 消费方没显式注入 logger 就是**完全静默**。这正是 G-M8 当初给 reorg 定性时用的同一条理由（migration 017 注释：*"a Warn log line (which lands in core.NopLogger() unless the consumer injected a logger)"*），reorg 那侧修了，这侧没修。
- **一个 operator 可查的界面**：无。`ListDeadLetters` 的 doc comment 自称 "for on-call triage (RUNBOOK)"，指向的 RUNBOOK 没有这一节，而且它自己零调用者。
- **一条重放路径**：无。`permanentIngestFailure` 的注释说「retrying is not a recovery strategy for them -- a human changing the configuration is」，但配置改完之后**没有任何东西会重新驱动那笔 sighting**：booking 从未创建（所以 recheck 循环看不见它），cursor 已越过（所以 forward scan 看不见它），registration rescan 只覆盖新注册地址。dead-letter 行里存了完整的 `payload`（`ingest_dead_letter_store.go:44` `json.Marshal(sighting)`）—— 重放所需的一切都在，只是没人用。

设计档 §140 的要求是明确的：**「ErrConflict：入账遇到 = 归一化 bug，告警 + dead-letter 表人工看，绝不吞掉重试。」** 「dead-letter 表」做了，「告警」和「人工看」都没做。

同一形状还有三处（同一批修 —— 建议一起）：
- `deposit_reorgs`：`ResolveReorg` 生产零调用者 → 见 M-6。
- `registration_rescans`：`RegistrationRescanFailed` 有 counter ✅，但耗尽重试后的 job 没有列表界面。
- `escalateWatcherStall`（`onchain.go:1705`）：楔死告警只有日志。

**建议修法**
1. `core.Metrics` 加 `DepositIngestDeadLettered(chainID int64, reason string)`（reason 用有界枚举：`payload_conflict` / `currency_unregistered` / `precision_exceeded` / `watcher_wedged`），在 `recordIngestDeadLetter` 里发。RUNBOOK §14 的「Payment-affecting counters (page on any nonzero rate)」表里加一行 —— 那张表的门槛就是「任何非零都要 page」，这条完全符合。
2. `ledger-cli deposits dead-letters --limit N`（读 `ListDeadLetters`）+ `ledger-cli deposits replay --uid <dead-letter-uid>`（反序列化 `payload` 调 `IngestDeposit`，天然幂等）。RUNBOOK 加一节「一笔存款被 dead-letter 了」。
3. 把 `ingest_dead_letters` / `deposit_reorgs` / `chain_cursors` / `registration_rescans` 四张表加进 §8「Common investigation queries」，每张一条 SQL。

---

### Major

---

#### M-1 `auto_reverse` 靠**单次** RPC 观测就扣用户钱，而后果更轻的 shallow 路径要 3 次连续观测

**位置** `service/onchain.go:1978-2005`（`recheckOneConfirmedDeposit`，单次 `TxIncluded`）→ `:2007-2040`（`handleReorg`）→ `:2031`（`ReverseJournal`）

**判定：CONFIRMED**

`WithShallowReorgMisses` 的 doc comment（`service/onchain.go:280-296`）把这件事的逻辑说得很清楚：

> *"One observation is not evidence: TxIncluded reports false whenever the answering node has not caught up … The resulting transition is IRREVERSIBLE … while the deep-reorg path -- same class of evidence, less irreversible consequence -- defaults to alert-only."*

那句「less irreversible consequence」只在 `ReorgPolicyManual` 下成立。切到 `auto_reverse` 之后，同一条「一次 `TxIncluded=false`」的证据直接触发 `ReverseJournal` —— 一次自动记账扣款，无人在环，且 `chains/evm/client.go:24` 每链只有一个 RPC endpoint（无 failover、无重试中间件），所以那一次 false 可能只是一个没追上的节点。

不对称的地方：**deps 里已经有第二源**（`OnchainDeps.DepositConfirmer`，`onchain.go:186-190`），入账侧超过 `ReconcileCeiling` 就会去核对它 —— 但扣款侧从不问它。

RUNBOOK §1427-1443 的风险声明写得很好（「A false positive … auto-debits a user with no human in the loop」），但它是一句文字约束，不是机器约束。working-agreements §5：能被结构强制的，不靠自觉。

**建议修法**（与 shallow 侧对齐，改动小）
- 给 deep reorg 加同款连续观测计数器（复用 `recordShallowMiss` 的形状，或直接用已持久化的 `deposit_reorgs.detected_at`：只在异常已开、且距 `detected_at` 超过 N 个 tick 仍不在链上时才自动冲正 —— 这比内存计数器更好，因为它能跨重启）。
- `auto_reverse` 下若 `DepositConfirmer` 已配置，冲正前强制问一次二源；二源不同意就只记异常不动账。
- 若二者都不做，至少把 `ReorgPolicyAutoReverse` 的启动校验改成「必须同时配置 `DepositConfirmer`」，让「接受风险」这件事有一个机器可校验的形式。

---

#### M-2 `TokenConfig.Decimals` 与 currency `Exponent` 从不交叉校验，真实存款被静默送进 C-2 那个黑洞

**位置**
- `core/onchain.go:340` `maxTokenDecimals = 36`
- `core/interfaces.go:455` currency `Exponent` 必须在 `[0, 18]`
- `postgres/booking_store.go:144` `checkAmountPrecision`（抛 `ErrPrecisionExceeded`）
- `core/errors.go:108` `ErrPrecisionExceeded` → `IsRetryable = false`
- `service/onchain.go:1682` `permanentIngestFailure`

**判定：CONFIRMED（容器内实测）**

```
$ go test ./.scratch/probe -run TestPrecisionDeadLetterPath -v
currency uid: 01a0673d-5a22-... exponent: 6
evm.normalizeAmount(1000000000000000001, 18) = 1.000000000000000001
CreateBooking err = postgres: create booking: postgres: amount 1.000000000000000001
    exceeds currency PROBE20 (...) exponent 6: amount exceeds currency precision
errors.Is(err, core.ErrPrecisionExceeded) = true
core.IsRetryable(err)                     = false
                       ^-- false => permanentIngestFailure()==true => dead-letter + cursor 前进
```

链路：`TokenConfig.Decimals = 18`（`VerifyTokenDecimals` 会和合约核对，所以这个值**是对的**）→ `normalizeAmount(raw, 18)` 产出 18 位小数 → currency 的 `Exponent` 只有 6 → `CreateBooking` 抛 `ErrPrecisionExceeded` → 非可重试 → **dead-letter 并推进 cursor，booking 从未创建**（所以连 recheck 循环都救不了它，不像 frozen/period-closed 那些至少已经有 booking 在 `confirming` 里等重试）。

现有闸门覆盖不到这一处：`validateAutoCreditCeilings`（`onchain.go:562`）验的是 ceiling 和 `TokenConfig.Validate`（只查 `0 <= Decimals <= 36`）；`VerifyTokenDecimals` 验的是「Decimals == 合约的 decimals()」。**没有任何一处**把 `Decimals` 和它要落进去的 currency 的 `Exponent` 放在一起看。而两个上限本身就不自洽：`Decimals` 允许到 36，`Exponent` 最多 18 —— 配了一个 19-36 decimals 的 token（`Validate` 会放行），任何非整数金额都必定 dead-letter。

`OnchainDeps.Currencies` 就在手边，这个校验和另外三个启动闸放同一个地方即可。

**建议修法**
在 `validateAutoCreditCeilings` 旁加一个 `validateTokenPrecision(ctx)`：对每链每 token 解析 `tc.CurrencyCode` → 若 `tc.Decimals > currency.Exponent`，拒绝启动，错误里点明「这个 token 的每一笔带尾数的存款都会被 dead-letter」。同时把 `maxTokenDecimals` 从 36 收到 18（对齐 `Exponent` 上限）—— 或者保留 36 但在错误信息里说明为什么 >18 一定配不出可用的 currency。

---

#### M-3 `sweep.orphaned_broadcast` 会永久锁死一条出链通道，其承诺的恢复步骤在 RUNBOOK §15 里不存在

**位置** `service/onchain.go:2404-2419`（fail-closed 判定 + 错误文案）；`docs/RUNBOOK.md:1708-1729`（§15 Resolution）

**判定：CONFIRMED**

代码抛的错原文（`onchain.go:2417`）：

> *"sweep: booking %s is pending at nonce %d but the signer's pending nonce is already %d -- the earlier broadcast's tx hash was lost; **recover it manually per RUNBOOK §15** before this (chain,token) can sweep again"*

fail-closed 本身是对的（G-M5 的正确处置）。问题是它把 on-call 指向了一节不包含该场景的手册：§15 的 Resolution 只有三条 bullet ——(a) 交易确实卡住 → 等下一 tick 自愈；(b) 已超 bump cap 转 `failed` → 调 `GasCeiling` 或人工重触发；(c) 整链 sweep 全停 → 查 nonce 序列里有没有更早的未确认交易。**没有一条**告诉你「booking 停在 `pending`、nonce 已被花掉、丢了 tx hash」时该怎么办。

而这是全库唯一一个会**无限期阻塞一条出链通道**的状态：`findInFlightSweep` 只看 `pending`/`sent`，所以那笔 booking 每个 tick 都被找到、每个 tick 都返回 `ErrConflict`，`(chain, token)` 的归集永远不再前进，直到有人手动介入。它连一个专属 counter 都没有（§15 自己承认「no dedicated metric exists」），只有 `job_tick_failed_total{job="sweep:<chainID>"}` 间接可见 —— 而那个 label 格式 §14 也没写（见 Minor-12）。

顺带：`onchain.go:2408` 的注释和 `:2417` 的错误文案是全仓唯一两处指向「§15 的恢复步骤」，两处都落空。

**建议修法**
§15 加一节「booking 停在 pending 但 nonce 已被花掉（`sweep.orphaned_broadcast`）」，写清可执行的步骤：
1. 用 booking 的 `metadata->>'nonce'` 在区块浏览器上按 signer EOA + nonce 查那笔实际落链/待打包的交易（**这是唯一能找回丢失 tx hash 的办法**，而且它做得到 —— nonce 是持久化在 metadata 里的）。
2. 若已落链：把 booking 手动推到 `confirmed`（给出 `channel_ref` 和 `sweepConfirmedKey` 的取值，或给一个 CLI 子命令）。
3. 若仍在 mempool：说明它会自己落链，等待即可。
4. 若已被丢弃：说明如何让该 booking 走 `failed` → `reviveFailedSweep` 路径。
另加一个 `SweepOrphanedBroadcast(chainID)` counter，进 §14 的 payment-affecting 表。

---

#### M-4 RUNBOOK §8「Compute live balance for an account」的 SQL 跑不起来，而 §1 在 P1 场景里明确要求用它

**位置** `docs/RUNBOOK.md:594-606`（查询）；`docs/RUNBOOK.md:138-142`（要求使用它）

**判定：CONFIRMED（容器内实测）**

```
$ psql < RUNBOOK §8 "Compute live balance for an account"
ERROR:  column "cp.balance" must appear in the GROUP BY clause or be used in an aggregate function
LINE 2:   COALESCE(cp.balance, 0)

$ 同一条查询 + `GROUP BY cp.balance`
 balance
---------
       0
(1 row)
```

`SELECT` 列表里 `COALESCE(cp.balance, 0)` 是非聚合列，与 `SUM(...)` 混用却没有 `GROUP BY`。唯一缺陷就是那一行。

为什么这条比一般文档 bug 重：§1 的 `checkpoint_balance` / `system_rollup_integrity` / `snapshot_integrity` 处置流程（RUNBOOK:135-142）说的是

> *"**do not** just re-run reconcile and move on: these three all mean a materialized cache disagrees with `journal_entries`, which is exactly the class of drift a leaked DB credential (direct `UPDATE`) would produce. … 2. Confirm it's real: `journal_entries` is the ground truth. **Recompute by hand with the query in §8 ("Compute live balance for an account")**"*

也就是说：怀疑 checkpoint 被篡改（P1，疑似凭证泄漏）时，on-call 手算真值的那个工具，一次都跑不通。

**建议修法**：把 `GROUP BY cp.balance` 加上（`seed` 那个恒等子查询也可以顺手去掉，它没被引用），并加一条断言把 RUNBOOK 的 SQL 块跑进 CI —— 这个仓库已经有 `readme_runnable_test.go` / `claude_md_paths_test.go` 这类文档 gate，同一形状可以覆盖 RUNBOOK 的 13 个 SQL 块（对着 migration 建出来的空库跑，只断言「能 prepare / 能执行」）。这是 working-agreements §5 的标准动作。

---

#### M-5 RUNBOOK 两处声明 `unauthorized_journals` 被跳过时报 `Complete=false`；实际它整项消失，而 `skipped_checks` 全文未提

**位置** `docs/RUNBOOK.md:83`、`docs/RUNBOOK.md:180`；实际实现 `service/reconcile.go:546-561`；契约 `core/reconcile_extra.go:53-67`

**判定：CONFIRMED（实跑对照）**

```
$ ledger-cli reconcile --full                       # 无 verifier
15 项 checks[]，其中没有 unauthorized_journals
skipped_checks = ["unauthorized_journals"]

$ ledger-cli reconcile --full --pubkey-hex <hex> --key-id k1
16 项 checks[]，含 unauthorized_journals
```

`core/reconcile_extra.go:53-58` 明确写了这个语义变更（C-m4）：

> *"SkippedChecks names the checks that were not RUN in this deployment … rather than [being reported] with Complete=false: a permanently-incomplete vote makes FullCoverage [permanently false]"*

`:66` 还进一步要求「**assert on this field being empty, not merely on FullCoverage**」。RUNBOOK 全文 grep `skipped_checks` = **0 次**；DR.md 也 0 次。

on-call 按 RUNBOOK:180 的指引会去 `checks[]` 里找一个 `Complete=false` 的条目 —— 找不到，然后大概率理解成「这项过了」。而这项正是**唯一的伪造检测**（I-32）。这是 working-agreements §3 的教科书形态：「未运行」被读成「通过」。

**连带的一处真实信号缺口**：`service/reconcile.go:592` 只对 `checks` 里的项发 `ReconcileCheckResult`，被跳过的项**一个 metric 都不发**。那行的注释写着

> *"Report Passed && Complete so anything alerting on this metric fails closed: an incomplete or skipped check must not look green."*

对 incomplete 成立，对 **skipped 不成立**：series 直接不存在。Prometheus 里 `ledger_reconcile_check_results_total{check="unauthorized_journals"} == 0` 这条告警在 series 缺失时**不会触发**（`absent()` 才会）。所以 skipped 的实际效果不是「fails closed」，是「无信号」。

**建议修法**
1. 改 RUNBOOK:83 与 :180 的措辞，并在 §1 加一句：「先看 `skipped_checks` —— 非空就说明这次运行没有覆盖那些项，`overall_passed: true` 不构成清白证明」。
2. `service/reconcile.go:592` 后面补一个循环，对每个 `skippedChecks` 项发 `ReconcileCheckResult(name, false)` —— 这样「跳过」就真的 fail-closed，而不是消失。
3. DR §5 的验证命令改成带 `--pubkey-hex/--key-id`，并把断言改成 `jq -e '.overall_passed == true and .full_coverage == true and (.skipped_checks // [] | length) == 0'`（见 Minor-5）。

---

#### M-6 深 reorg 异常会永久重复告警，而关闭它的 `ResolveReorg` 在生产代码里零调用者、RUNBOOK 零提及

**位置**
- `service/onchain.go:1938-1976` `recheckOpenAnomalies`（每 `reorgRecheckInterval`（默认 5 分钟）无条件重报所有未结异常）
- `postgres/deposit_reorg_store.go:115-133` `ResolveReorg`（唯一的关闭手段）
- `docs/RUNBOOK.md:1382-1450`（§12，从未提及该表或如何关闭）

**判定：CONFIRMED**

```
$ grep -rn 'ResolveReorg' --include=*.go .  | grep -v _test
postgres/deposit_reorg_store.go:115  (定义)
postgres/deposit_reorg_store.go:119  (定义)
                                     <-- 无生产调用者
$ grep 'reorg' server/routes.go            # 无端点
$ grep 'reorg' cmd/ledger-cli/main.go      # 无子命令
$ grep -c 'deposit_reorgs' docs/RUNBOOK.md
0
```

`recheckOpenAnomalies` 对每个未结的 `deep_reorg` 异常，每 tick 都发一次 Warn + `DepositReorgDetected` counter + 刷新 `last_seen_at`（`onchain.go:1960-1965`）。这是**对的**设计（G-M8：「still true」要能区分于「nobody has looked」）。但 on-call 按 §12 冲正之后，那笔 tx 依然不在链上 → `!included` 依然成立 → **告警永远不停**。唯一能摘掉它的是 `ResolveReorg`，而它：不在 `service.ReorgRecorder` 接口里（`onchain.go:76-84` 只有 `RecordReorg` + `ListOpenReorgs`）、没有 HTTP 端点、没有 CLI 子命令、`ledger.Service` 上也没有 accessor（`ledger.go:999` 只是把 store 塞进 `OnchainDeps`）。operator 唯一的办法是手写 `UPDATE deposit_reorgs SET resolved_at = now(), resolution = '...'`，而这条 SQL 任何文档里都没有。

结果正是这个库自己在 `StuckRollups`（`core/metrics.go:181-188`）里点名过的反模式：

> *"conflating the two turns a permanently-stuck item into a gauge that never clears, i.e. **an alarm nailed to ON** that looks identical to ordinary backlog"*

而 `service/onchain.go:2023` 的注释与 `postgres/deposit_reorg_store.go:91` 的注释都写着 on-call 会「per RUNBOOK §12 … closes the anomaly out」—— §12 里没有那个动作。

另有一条相邻的权限问题：`ledger_app` 对 `deposit_reorgs` 持有 `UPDATE` 且该表**零触发器**（实测），所以被攻陷的应用凭证可以静默 `resolved_at = now()` 清空整个异常队列而不留痕。这张表是「reorg 发生过」的唯一持久证据。

**建议修法**
1. `ResolveReorg` 加进 `service.ReorgRecorder`，在 `Onchain` 上开一个 `ResolveReorg(ctx, kind, bookingUID, resolution, actor)`，走与 `ApproveReview` 同款的 capability 门（这也是高权限动作 —— 它让一个告警消失）。
2. `ledger-cli reorgs list` / `ledger-cli reorgs resolve --booking-uid <uid> --kind deep_reorg --note "..."`。
3. §12 补第 6 步：关闭异常；并在 §12 的「Confirm it's real」里给出 `deposit_reorgs` 的查询（这才是「哪笔 booking」的持久答案，比现在的 `SELECT ... FROM bookings WHERE uid = '<booking_uid>'` 有用 —— 后者要求你**已经知道** uid，而 §12 声明的 alert source 又不存在（Minor-1），uid 从哪来是断的）。
4. 给 `deposit_reorgs` 加 partial guard（只允许 `last_seen_at` / `resolved_at` / `resolution` 变化）+ config-change 触发器。

---

#### M-7 RUNBOOK §15 说 sweep 转 `failed` 后「nonce is freed」，代码说的正相反

**位置** `docs/RUNBOOK.md:1714-1717` vs `service/onchain.go:2240-2249`

**判定：CONFIRMED**

RUNBOOK:1714-1717：

> *"If it has exceeded `SweepPolicy`'s bump cap and transitioned to `failed`: **the nonce is freed for later sweeps to proceed** (see `service/onchain.go`'s `reviveFailedSweep`/`findFailedSweep` comments), but THIS batch's funds are still sitting at their deposit addresses"*

它引用的那段注释（`onchain.go:2240-2249`）写的是：

> *"failed's signerNonce was broadcast repeatedly … but never observed included on-chain. **From the signer EOA's perspective that nonce slot is still "next"** -- NextNonce (PendingNonceAt) will keep reporting it until either the stuck tx eventually lands, or something external moves the EOA past it (an on-call operator manually clearing it per RUNBOOK, or the node's mempool view of it changing)."*

RUNBOOK 引用了一段注释，得出了与它相反的结论。实际后果差别很大：按 RUNBOOK 的说法 on-call 只需要调 `GasCeiling` 然后等；按代码的说法那个 nonce 槽位可能仍被一笔卡住的交易占着，而 EVM nonce 严格顺序 —— §15 自己最后一条 bullet 就说了「any earlier unconfirmed nonce for the signing key … **blocks everything after it**」。同一节内部自相矛盾。

**建议修法**：§15 那条 bullet 改写为「nonce 槽位**未必**被释放：`PendingNonceAt` 会继续报同一个 nonce，直到卡住的交易落链、被 mempool 逐出、或被人工用同 nonce 的自付交易顶掉。先按最后一条 bullet 确认 nonce 序列是干净的，再谈调 `GasCeiling`。」并把「人工顶掉 nonce」的具体步骤写出来（它已经在两处注释里被承诺为「per RUNBOOK」）。

---

#### M-8 `chain_cursor_lag` 在最常见的 watcher 停摆形态下**冻结**而非增长 —— §14 教的那条告警不会触发

**位置** `service/onchain.go:1571-1656`（`reportLag` 的定义点与全部调用点）；`docs/RUNBOOK.md:1580`

**判定：CONFIRMED（代码路径）**

`reportLag` 定义在 `:1578`，闭包捕获 `latest` —— 所以它**只可能**在 `LatestBlock` 已成功之后被调用。四条早退路径全部不上报：

| 早退点 | 代码行 | 典型触发原因 | gauge |
|---|---|---|---|
| `GetCursor` 失败（非 NotFound） | `:1560` | DB 不可用 | 冻结 |
| `LatestBlock` 失败 | `:1566` | **RPC provider 宕机 / 限流** | 冻结 |
| `ListAddresses` 失败 | `:1601` | DB 不可用 | 冻结 |
| `FetchDeposits` 失败 | `:1615` | **`eth_getLogs` 被限流 / range 太大 / provider 报错** | 冻结 |

RUNBOOK:1580 的处置指引是：

> *"A transient bump (RPC hiccup, a slightly slow block) is normal. **A monotonically climbing lag** means the watcher loop (`Onchain.scanChainOnce`) is stuck or too slow for that chain's block rate"*

按这条写出来的告警（`increase(ledger_chain_cursor_lag_blocks[...]) > 0` 之类）在 RPC 完全宕机时**不会响** —— 而 RPC 宕机正是这个 gauge 最该抓的东西。真正会让 lag 增长的只有一种情况：`FetchDeposits` 成功、但某个 sighting 入账失败被 hold cursor（`:1645` `reportLag(from - 1)`，`latest` 是新鲜的）。

存在一个**能**覆盖 RPC 宕机的信号：watcher 走 `LockedJob`，所以会发 `job_tick_failed_total{job="onchain_watch:<chainID>"}`（`locked_job.go:150`）。但 §14 完全没提 `job_tick_*` 家族，也没提这个 label 格式（Minor-4 / Minor-12）—— 也就是说，**能用的信号没被文档化，被文档化的信号在这些场景下不动**。

**建议修法**
1. 把 `reportLag` 挪成基于 cursor 的形式，让每个 tick 都能上报：另发一个只依赖 `GetCursor` 的 `chain_cursor_block{chain_id}` gauge（cursor 的绝对值），并在 §14 里说明「cursor 绝对值在若干个 tick 内不变 = watcher 停了」，这条对上面四种停摆全部成立。或者
2. 更简单：在 §14 的 `chain_cursor_lag` 行里写清「本 gauge 在 RPC/DB 故障时会冻结而非增长；watcher 存活性请以 `ledger_job_tick_completed_total{job="onchain_watch:<chain_id>"}` 的 `increase(...) == 0` 为准」，并把 `job_tick_*` 全族补进 §14。
3. 顺手在 `escalateWatcherStall`（`:1705`）加一个 counter —— 楔死的 watcher 目前只有日志。

---

#### M-9 `onchain_recheck` / `onchain_reorg_recheck` / `onchain_registration_rescan` 三个 job 没有任何存活或失败指标

**位置** `service/onchain.go:2666-2686`（job 装配）、`:2714-2738`（`runLoop`）、`:2740-2755`（`safeRunTick`）

**判定：CONFIRMED**

`Onchain` 自带一份 `runLoop`（不走 `service.Worker`），而它的 `safeRunTick` **只**发 `JobPanicked`（`:2748`）。`JobTickCompleted` / `JobTickFailed` 只可能来自 `LockedJob.Run` —— 也就是只有 `onchain_watch:<chain>` 与 `sweep:<chain>` 两个 job 有。剩下三个：

| job | 装配 | 存活指标 | 失败指标 | 内部错误处置 |
|---|---|---|---|---|
| `onchain_watch` | `LockedJob` | ✅ `job_tick_completed_total{job="onchain_watch:N"}` | ✅ | 返回 error |
| `onchain_sweep` | `LockedJob`（label `sweep:N`） | ✅ | ✅ | 返回 error |
| **`onchain_recheck`** | 裸 `runLoop` | ❌ | ❌ | `func(ctx)`，**全部错误只打日志**（`:1748, :1752, :1789, :1826, :1849`） |
| **`onchain_reorg_recheck`** | 裸 `runLoop` | ❌ | ❌ | 同上（`:1949, :1955, :1985, :1998`） |
| **`onchain_registration_rescan`** | 裸 `runLoop` | ❌ | ❌ | 日志 + `RegistrationRescanFailed`（这一个有 counter ✅） |

`onchain_recheck` 是 **pull 路径上真正给用户入账的那个循环**：watcher 首次看到一笔转账时确认数通常还不够阈值，是 `recheckPendingDeposits` 后续把它推到 `confirmed` 并 post journal 的。这个循环停了，存款就静默地永远不入账 —— 而它既没有存活指标，也没有失败指标，全部错误只走 `core.Logger`，**库默认 `NopLogger()`**。同理 `onchain_reorg_recheck` 是唯一的 reorg 探测器。

`core/metrics.go:147-152` 对 `job` 参数的描述是「the fixed, bounded job name already used for logging (e.g. "rollup", … **or an onchain job's name**)」—— 这句话对上表后三行不成立。

**建议修法**：把 `Onchain.runLoop` 的 tick 签名改成 `func(context.Context) error`（三个 recheck 函数内部已经在收集错误，只是丢掉了），在 `safeRunTick` 里按 `Worker.runLoop` 的形状发 `JobTickCompleted` / `JobTickFailed`；`LockedJob` 包装的那两个继续把计数交给 `LockedJob.Run`（`locked_job.go:107-113` 的注释已经把「不要双记」的约定写清楚了）。同时统一 label：`sweep:<N>` → `onchain_sweep:<N>`，并在 §14 把整族补上。

---

### Minor

**Minor-1 · §12 声明的 alert source `deposit.reorged` 事件不存在。** `docs/RUNBOOK.md:1384`。全仓 grep：该名字只出现在 RUNBOOK 与设计档；代码发的是 `service: onchain: deep reorg detected` 日志（`service/onchain.go:2008`）、`deposit.reorg_unresolved`（`:1961`）、`deposit.reorg_tx_returned`（`:1967`）、`deposit.failed_tx_returned`（`:1970`）四种日志前缀，加 `ledger_deposit_reorg_detected_total`。§14 自己说该 metric 才是 §12 的 alert source，两节矛盾。后三种日志前缀在 RUNBOOK 里也完全没有对应处置 —— 尤其 `deposit.failed_tx_returned`（代码注释称之为「the loudest case in the file」：用户被欠了钱且只能人工补）没有任何 runbook 条目。

**Minor-2 · review reason 文档不全，且其中一个会把 on-call 引进死路。** `docs/RUNBOOK.md:1586`（写 2 个）、§13:1470-1490（写 3 个）；实际 5 个（`service/onchain.go:1075-1108`）。漏的两个：`token_unconfigured`（token 被移出 `CreditTokens` 后，在途 booking 一律进 `review` —— 会真的出现在队列里）；`shallow_reorg_returned`（`onchain.go:1972` 用它发 `DepositReviewRequired`，但它**不是** booking 状态，`GET /deposits/reviews` 里查不到 —— on-call 按 §13「Work the queue」第 1 步会看到空队列，而该信号真正的含义是「一笔被自动拒付的存款其实在链上，只能人工补」，处置完全不同）。

**Minor-3 · shallow reorg 的 3 次连续观测窗口只有约 40 秒，且同源、内存计数。** `service/onchain.go:1796-1846`，默认 `shallowReorgMisses=3` × `recheckInterval=20s`。后果是终态且幂等键永久吸收后续 sighting。只有**成功返回** `NotFound` 才计数（传输错误直接 return 不计），所以一个「能应答但没追上」的节点连续 40 秒就足以永久拒付一笔真实存款。计数器在内存里（重启归零只会**延迟**，方向安全 ✅）。这不算 bug —— 闸门存在且方向对 —— 但默认值相对于「不可逆」的后果偏松，且 `WithShallowReorgMisses` 的 doc comment 没提这个乘出来的时间窗有多短。建议默认改为按**时间**而非次数（例如「首次 miss 起至少 N 分钟且至少 M 次」），并在文档里把两个默认值乘出来给读者看。

**Minor-4 · §14 自称是完整的 metric 参考，却漏掉 `job_tick_*` 全族。** `core/metrics.go:154-168` 三处写「see docs/RUNBOOK.md」；RUNBOOK 全文 grep `job_tick` = 0。§14 的标题恰恰是「this library ships none of the alerting」——它是消费方从零写 alert rule 时唯一的参考。更外露的一处：`observability/prometheus.go:296` 把这句指引写进了 **Prometheus help 文本**（"Use increase(...)==0 to alert on a stalled job (see docs/RUNBOOK.md)"），所以每个消费方的 `/metrics` 端点都在把读者指向一节不存在的内容。

**Minor-5 · DR §5 的验证命令与它自己记录的修复不一致。** `docs/DR.md:218-238` 的 `ledger-cli reconcile --full | jq -e '.overall_passed == true'` 既不带 `--pubkey-hex/--key-id`（所以注释里声明的 I-32 覆盖必然拿不到），也不断言 `full_coverage` / `skipped_checks`（违反 `core/reconcile_extra.go:66` 自己写的要求）。DR:205-211 已经记录了这个缺口在 26-09-02 修好并给出了新 flag，但 §5 的配方没跟着更新。

**Minor-6 · PITR 会永久丢掉 T 之后的 reorg 异常行，DR 没提。** `docs/DR.md:103-110` 只谈 push 路径的重放。`deposit_reorgs` 行只有在 booking 仍处于 `reorgRecheckWindow`（默认 500 块）内时才会被 `recheckConfirmedDeposits` 重建；更老的永久消失 —— 正是 migration 017 建这张表要解决的问题。§5 的验证清单也不检查未结异常队列。建议 §5 加一步「`ledger-cli reorgs list` 应为空，或与恢复点前的已知异常一致」，并在 §4 说明该表不会自愈。

**Minor-7 · CAPACITY.md 对链上路径零覆盖。** grep `chain|deposit|sweep|reorg|onchain` = 0 命中。没有 `eth_getLogs` 的 QPS/成本口径（`maxBlocksPerScan` 默认 2000 + `addressShardLen` 默认 500 + 每笔命中一次 `eth_getTransactionReceipt`，这些都是 provider 计费维度）、没有 sweep 频率与 gas 预算、没有 watcher lag 的 SLO。SLO 表最后一行止于 event delivery。

**Minor-8 · §1 的两条确认命令返回不同形状，下文只对其中一条成立。** `docs/RUNBOOK.md:47-52`。`POST /api/v1/reconcile` 返回 `{balanced, gap, details}`（`server/handler_reports.go:29`），没有 `checks[]`；§1 下文的检查名表与「Match the failing check's `name` to the entries in `checks[].findings`」只对 `ledger-cli reconcile --full` / `POST /reconcile/full` 成立。`docs/api.md:1022` 把这个区别讲得很清楚，RUNBOOK 没有。

**Minor-9 · `docs/RUNBOOK.md:441` 的代码块标为 `bash` 但内容是 SQL。** 粘进 shell 直接语法错。

**Minor-10 · §8「Trace a booking end-to-end」的 SQL 用内部数字 `id`，而 on-call 手上是 uid。** `docs/RUNBOOK.md:573-580`（`WHERE id = 12345`）。这个库的对外契约是 uid-only；告警、`ledger-cli trace --booking-uid`、`/deposits/reviews` 给的都是 uid。三条 SQL 都要求先做一次未文档化的 uid→id 换算。建议直接改成 `WHERE uid = '<booking-uid>'` + 子查询。

**Minor-11 · 两个死指标，其中一个的文档陈述了一个假事实。** `core/metrics.go:98` `ReservedAmount`、`:177` `PendingEvents`，生产调用点均为 0（已在 `observability/emission_coverage_test.go:54/61/196` 登记为 follow-up —— 这个 gate 的形状很好）。但 `PendingEvents` 的 doc comment 写着「**sampled once per delivery job tick**」，这句话不成立（它从不被采样）。`observability/prometheus.go:541` 也把它注册进了 registry，所以 `ledger_events_pending` 会以一个恒为 0 的 gauge 暴露给消费方 —— 一个「永远健康」的假指标比缺失更糟。建议：要么在 doc comment 里明写「no production call site yet」，要么干脆先不注册进 Prometheus registry。

**Minor-12 · job label 格式不一致且未文档化。** `LockedJob` 用 `onchain_watch:<chainID>`（`service/onchain.go:2785`）与 `sweep:<chainID>`（`:2795`），而同一个 job 的 `JobPanicked` 走 `runLoop` 的名字 `onchain_watch` / `onchain_sweep`（`:2677, :2698`）。于是同一个 job 在 `job_tick_*` 与 `job_panicked_total` 两族里 label 不同名；归集 job 的前缀还是 `sweep:` 而非 `onchain_`。`core/metrics.go:147-152` 说 job 名是「fixed, bounded … never a free-form string」，这里把 chainID 拼进了 label（基数受链数约束，可接受，但没写进任何文档）。

**其他观察（不计入发现，仅记录）**
- `service/onchain.go:1528` `RunReorgRecheckOnce` 与 `:1938` `recheckOpenAnomalies` 在 `ReorgRecorder != nil && Reader == nil` 时会对 nil `Reader` 解引用（`:1953`）。`Run` 的启动校验（`:2667`）让这个组合在正常路径下不可达，`safeRunTick` 也会 recover；只有直接调用导出方法的消费方会碰到。
- `postgres/deposit_reorg_store.go:61-66` `HasOpenReorg` 是死代码，且它的 doc comment 声称「service.Onchain uses it to keep rechecking a booking whose anomaly is still open」—— 该方法不在 `service.ReorgRecorder` 接口里，零调用者（那个行为实际由 `recheckOpenAnomalies` 实现）。
- `service/onchain.go:2439-2453`：`advanceSweep` 的 pending 分支对**首次派发**与**revive 后派发**都硬编码 `priorTxHash=""`。revive 路径因此只能靠 `Sweeper.lastFee`（内存，重启即空）拿到费用地板，绕开了 26-08-26 那个「从链上读 `priorTxHash` 真实费用」的修复 —— 而 RUNBOOK §15:1697-1706 正是把那个修复算作「underpriced forever 已闭环」的依据。`failed.ChannelRef`（该 nonce 上最后一次广播的持久 hash）在调用点就在手边却没被传下去。是否真的导致 underpriced 取决于 `PendingNonceAt` 是否仍报同一个 nonce —— `reviveFailedSweep` 的注释自己写了「if not, it is the same stale value and this retry re-attempts with current gas pricing」，而「current gas pricing 且无地板」正是 underpriced 的定义。**PLAUSIBLE**，未能构造出实证（需要一个能稳定复现 mempool 状态的 anvil 场景）。
- `service/onchain.go:2244` `resolveSweepCurrency` 用 `cfg.SweepTokens[token]` 直接查（未 `strings.ToLower`），而 `Scanner.tokenDecimals`（`chains/evm/scanner.go:75`）与 `IngestDeposit`（`onchain.go:891`）都会归一。`SweepPolicy.Validate`（`core/onchain.go:400`）也不归一 `Token`。一个 checksum 大小写的 `SweepPolicy.Token` 会让该 (chain,token) 的归集每 tick 失败（fail-closed ✅，但只有 `job_tick_failed_total{job="sweep:N"}` 可见）。golang.md：外部标识符只在边界归一一次。
- `chains/evm/factory.go:36` `packBatchSweep` 不校验 `common.IsHexAddress(tokenAddress)`；非法字面量会静默变成零地址。实际会在 `EstimateGas` 处失败（fail-closed），且 `VerifyTokenDecimals` 在被调用时会先拦住 —— 但那个调用是可选的。
- `ledger-cli` 的 usage 写 `balances  show all balances for a holder`，实现却强制要求 `--currency`（`cmd/ledger-cli/main.go:284`）。
- `ledger-cli trace` 对未设置的 `expires_at` 渲染成 `0001-01-01T08:05:43+08:05`（Go 零时间 + 本地 LMT 偏移）。凌晨三点看到这个会愣一下。
- `reconcile --full` 实跑有 15 项（含 `period_close_violations`），§1 的表列了 15 行但其中 `unauthorized_journals` 那一项在无 verifier 时不出现、`period_close_violations` 没有行。该表自己声明了不保证穷尽（「add a row here when you add a check」），所以只算漏更新。

**做得好的地方**（明确记录，避免只报缺陷造成的画像失真）
- **指标名零漂移**：RUNBOOK / DR / CAPACITY / INVARIANTS 里出现的每一个 `ledger_*` 名字都在 `observability/prometheus.go` 里真实注册。逐个核过，零个假名。
- **`observability/emission_coverage_test.go` 的双 gate 形状是对的**：一个查「有生产调用点」（AST 级，`*ast.CallExpr` + receiver 形状 —— 修掉了「注释里写 `s.metrics.X(1)` 就能骗过」的旧实现），一个查「有行为 pin」，缺口显式登记且「register may only shrink」。M-9 / Minor-11 是这套 gate 的**覆盖边界**问题（gate 问的是「有没有调用点」，不是「这个 job 有没有存活信号」），不是它失效。
- **fail-closed 的方向选择几乎处处正确**：balance 不可读绝不当零（`scanner.go:172, 253`）、receipt 与 log 视图不一致就中止而不猜 key（`reader.go:250-256`）、`AutoCreditCeiling` / `ReconcileFailureLimit` 没有安全默认值且启动即拒（`onchain.go:562, 617`）、`GasCeiling` 会拒绝像 wei 的数值（`core/onchain.go:411-416`）、sweep 每个 target 重新 CREATE2 派生并逐字比对（`sweeper.go:212-219`）、`sweep.orphaned_broadcast` 宁可锁死也不盲发（`onchain.go:2404`）。
- **`ledger-cli trace` / `balance` / `balances` / `solvency` / `journals` / `config-history` / `reconcile --full` / `rollup reset-claim` 全部实跑通过**，输出对 on-call 直接可用（trace 的 metadata 里 chain_id / tx_hash / txlog_seq / block_number 齐全，正是查区块浏览器要的四个值）。
- **`chains/evm` 的 `-tags e2e` 在本机 anvil 上实跑通过**，`go test -short ./service/... ./core/... ./presets/... ./channel/...` 全绿。

---

## 6. 我没能验证的

1. **`advanceSweep` revive 路径的 underpriced 实证**（上文「其他观察」第 3 条）。要证实需要一个 anvil 场景：广播一笔低价交易占住 nonce、耗尽 bump cap 转 `failed`、重启进程清空 `lastFee`、再触发 revive，并确认 `PendingNonceAt` 在那个时刻仍报同一个 nonce。`PendingNonceAt` 在「交易仍在 mempool」时的返回值依赖节点 txpool 实现（anvil ≠ geth ≠ 各家 provider），我无法用 anvil 的行为代表生产 provider。**因此只标 PLAUSIBLE。**

2. **C-1 的 forward-jump 端到端实证**。我实测确认了三件事：`chain_cursors` 零触发器、`ledger_app` 持有 `UPDATE`、`SetChainCursor` 的单调性只在 query 的 `WHERE` 里。「gap 永久且无检测」是从 `scanChainOnce` 的代码路径（`from = cursor+1`，无回看）与 reconcile 的 15 项检查范围推出的，**没有**跑一个「真存款落在跳过区间 → 15 项 reconcile 全绿」的完整场景。要跑需要 anvil + 真 factory 合约部署 + 完整 `Onchain.Run`，超出本轮时间。推理链每一步都有 file:line，但结论本身是推导而非实测。

3. **`ErrAccountFrozen` / `ErrAccountClosed` / `ErrPeriodClosed` 走 dead-letter 的实际后果**。这三个在 `core/errors.go:109-111` 被分类为不可重试 → `permanentIngestFailure` = true。我一开始判它们会导致真存款永久丢失，但读完 `advanceConfirmation`（`onchain.go:1030-1063`）后**自我推翻**：booking 在 journal 尝试之前就已经被创建并推进到 `confirming` 了，所以 `onchain_recheck` 循环会一直重试到账户解冻/期间重开。净效果只是**多出一条误导性的 dead-letter 行**（一笔最终会入账的存款被记成了「永久拒收」），不是资金损失。我没有实测这条路径，也没有量化它对 dead-letter 队列的噪声贡献 —— 但这是 C-2 提议的 replay 命令需要考虑的一个边界（重放一条已经自愈的 dead letter 必须是幂等 no-op；`IngestDeposit` 的幂等性让它天然如此）。

4. **`VerifyTokenDecimals` 在真实代理 token 上的行为**。它对读不出 `decimals()` 的情况返回 `ErrTokenDecimalsUnreadable`（正确，不默认成 0），但我只在 unit 级别看了代码，没有对着一个真实的 upgradeable proxy USDT 实测。它是可选调用（库无 binary），所以「消费方忘了调」这个风险面我也无法在库内验证。

5. **多 replica 下的 cursor 与 nonce 竞争**。`newWatchLockedJob` / `newSweepLockedJob` 的 per-chain advisory lock 我只读了代码，没有起两个进程实测。`LockedJob` 的 session 亲和性处理（`locked_job.go:33-66`，acquire 时 pin 住连接、unlock 失败就 Hijack + Close）看起来是对的，且这个仓库已有 `postgres/advisory_lock_shape_pin_test.go`。

6. **`escalateWatcherStall` 的 dead-letter 灌爆边界**。`uq_ingest_dead_letters_idempotency_key` + `ON CONFLICT DO NOTHING` 让它按 (chain,tx,seq) 有界（这一点我核了 store 代码），但 watcher 楔死时每 tick 都会对同一批 blocked sighting 调一次 `RecordDeadLetter`，写放大我没测。

7. **`observability/prometheus.go` 的 help 文本与非链上指标的 label**。链上侧的 label 我补核过并且是对的（`chain_cursor_lag_blocks` → `{chain_id}`，`deposit_review_required_total` → `{chain_id, reason}`，`job_tick_*` → `{job}`，与 §14 写的一致）。其余 30 余个指标的 label 集合与 help 文本我没有逐个核。

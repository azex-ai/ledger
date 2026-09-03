# R3 复核 · 链上资金路径与可运维性

- **复核对象**：main `5a11533`（代码等于 `0e7512a`）。复核分支 `r3-recheck-onchain-ops`，worktree `/Users/aaron/projects/_worktrees/ledger/r3-onchain-ops`。
- **原报告**：`docs/audits/2026-09-03-independent-review/onchain-ops.md`（基线 `c854c6e`，Critical 2 / Major 9 / Minor 12）。
- **判据**：零 Critical 复发。
- **方法**：全部用原报告的同一 PoC 重跑，外加 mutation 测试（把新增的 emission / 围栏改坏，看仓库自己的 pin 是否变红）。只读代码，未修改任何被评审文件。
- **实跑环境**：独占容器 `postgres:17.2-alpine`（端口 16500，`postgres.Migrate` 到 version 30），`ledger_app` 独立连接串做权限侧 PoC，`anvil` 在位。
- **实验后清理**：`.scratch/` 与临时容器已删除，分支上只提交本报告。

> ⚠️ 实验环境事故（自我披露）：第一次起容器时用了 `docker run -d --name r3recheck-pg ... >/dev/null 2>&1 && ...`，把 name 冲突的报错吞掉了，随后 `pg_isready` 连上的是**另一个 agent 同名容器**。因此有几条 `chain_cursors` INSERT/UPDATE 与一次 `ALTER ROLE ledger_app PASSWORD` 落在了别人的库上（该容器随后被其所有者重建，写入已不存在）。之后全部实验改用唯一名 + 空闲端口探测 + 校验 `docker run` 输出。同 worktree 的其他复核员若发现自己的 fixture 在 2026-09-03 17:5x 出现过 `chain_id` 911/913 的游标行，来源是这里。

---

## 1. 摘要

**零 Critical 复发。** 原报告两条 Critical 都从「无护栏 / 无信号」变成了「DB 层强制 + 应用层补偿 + 可查界面 + 可执行手册」，9 条 Major 全部闭合（其中 6 条我用 mutation 验过 pin 不是装饰）。

- **C-1**（cursor 无护栏无审计）：`ledger_app` 现在**无法**跳游标（>100,000 块即拒）、无法倒退、无法改 `chain_id`、无法 DELETE、无法调 rewind 门；`SET LOCAL` 预置 GUC 与 `ON CONFLICT DO UPDATE` 两条绕行也都被拦。倒退走 owner-only 的 `ledger_rewind_chain_cursor()`，强制 reason，写两条审计行（其中专用那条 `changed_by = session_user`，可归因到真人）。应用侧还加了 `WithRescanLookback`（默认 128 块）无条件回扫 + 「游标超过链头」告警。**残余：新链的第一次 INSERT 不受上限约束**（我实测把一条没有游标的链插到 88,888,888 成功），只留审计行——029 头注自己点名了这条威胁形态，但结论句「converts … into 'one statement skips at most one oversized window'」对 INSERT 分支不成立。
- **C-2**（dead-letter 无信号无重放）：新增 `DepositIngestDeadLettered{chain_id,reason}` 计数器（8 个有界 reason）、`DeadLetterBacklog` gauge、`ledger-cli dead-letters list/show`（含 `--unbooked-only`）、`GET /deposits/dead-letters`、`POST /deposits/dead-letters/{uid}/replay`、`Onchain.ReplayDeadLetter`、RUNBOOK §18 逐 reason 一行 triage。**残余两点**：`IngestDeposit` 自己那条 `ErrConflict` 分支直接调 `RecordDeadLetter`，**绕过**了带计数的 `recordIngestDeadLetter`——所以 webhook 推送路径上这个 canonical dead-letter 不发计数器（我实测 `DepositIngestDeadLettered = 0`）；而 `DeadLetterBacklog` 只在 `Reader != nil` 的 deep-reorg tick 里采样，webhook-only 部署两个信号都没有。
- **money-out C-2 的应用层围栏（I-69 `corroborateBeforeConfirm`）**：我用自己的 fake reader + `ledger_app` 裸 INSERT 重放了原攻击，三条都符合契约——伪造 booking 进 `review`（reason `onchain_unverified`）零 journal；真 tx 改金额同样被拦；`eth_getLogs` 宕机既不入账也不判罪，链恢复后同一笔正常确认。
- **仍未处理**：Minor-5（DR §5 的验证命令）、Minor-6（PITR 丢 `deposit_reorgs`）、Minor-7（CAPACITY 零链上覆盖）、Minor-9（`bash` 代码块里塞 SQL）、Minor-11（两个死指标的文档仍陈述假事实）。另有 1 条**新问题**：RUNBOOK 把 token-decimals × currency-exponent 这条新启动闸引用成 **I-69**，而它实际不属于任何 invariant（`docs/INVARIANTS.md` 里没有条目）。

计数：**已闭合 15 / 部分闭合（有残余）3 / 未处理 5 / 复发 0 / 新问题 1**。

---

## 2. 逐条复核表

### Critical

| 原编号 | 原结论 | 复核方法 | 复核结论 | 残余 |
|---|---|---|---|---|
| **C-1** | `chain_cursors` 零触发器 + `ledger_app` 裸 UPDATE；豁免清单三条理由全错；gap 永久、无检测、无恢复 | ① `ledger_app` 连接实跑 7 组写：跳 99,999,999 / 倒退 / 改 `chain_id` / 新链高位 INSERT / cap 边界 ±1 / DELETE / 直调 rewind；② 两条绕行：`SET LOCAL ledger.rewind_chain_cursor='on'` 后倒退与超跳、`INSERT … ON CONFLICT DO UPDATE`；③ owner 走 rewind 门并查 `config_table_changes`；④ 读 guard 函数体与 029 头注；⑤ mutation：关掉 `rescanLookback` | **部分闭合** —— 见下方 C-1 详述 | **新链首次 INSERT 无上限**（实测成功插到 88,888,888，只留审计行）；029 头注的结论句对 INSERT 分支过宽 |
| **C-2** | dead-letter 无 metric / 无 CLI / 无端点 / RUNBOOK 零提及 / 无重放路径 | ① 造真 dead letter（同 key 不同金额 → `ErrConflict`）并用自建 recording metrics 观察发射；② `ledger-cli dead-letters list/show/--unbooked-only`；③ `RunReorgRecheckOnce` 看 `DeadLetterBacklog`；④ `ReplayDeadLetter`；⑤ RUNBOOK grep；⑥ mutation：删掉 `DepositIngestDeadLettered` | **部分闭合** —— 见下方 C-2 详述 | `IngestDeposit` 的 `ErrConflict` 分支不计数（webhook 路径 = 唯一路径时无信号）；`DeadLetterBacklog` 仅在 `Reader != nil` 时采样 |

### Major

| 原编号 | 原结论 | 复核方法 | 复核结论 | 残余 |
|---|---|---|---|---|
| **M-1** | `auto_reverse` 靠单次 `TxIncluded` 就扣款，而后果更轻的 shallow 路径要 3 次 | 造真 confirmed 存款 → 令 tx 离链 → 逐次 `RunReorgRecheckOnce`，记录 reversal 数 / 异常行 / 计数器；再造第二笔验「命中重置连击」 | **已闭合**。1、2 次不冲正；第 3 次连续 miss 才 `ReverseJournal`；第 4 次不重复冲正；中间一次「在链上」把连击清零，再攒 3 次才冲。异常行与 `DepositReorgDetected` 在**第一次**观测就落地（报告不被阈值延迟，符合 I-69 rule 2）。`WithDeepReorgMisses` 可配 | — |
| **M-2** | `TokenConfig.Decimals` × currency `Exponent` 无交叉校验，真存款被静默写off | 造 exponent=2 的 currency + decimals=6 的 token，跑 `ValidateTokenPrecision()` 与 `Run()`；另测 `TokenConfig{Decimals:19/36}.Validate()` | **已闭合**。`Run()` 与 `ValidateTokenPrecision()` 都以点名 chain/token/currency 的长错误拒绝；`maxTokenDecimals` 从 36 收到 18 | 检查在 `Run()`，**不在** `EnableOnchain()`（实测 `EnableOnchain` 返回 nil）。webhook-only 消费方须自己调 `ValidateTokenPrecision`；已在方法注释里写明 |
| **M-3** | `sweep.orphaned_broadcast` 永久锁死出链通道，其承诺的 RUNBOOK §15 恢复步骤不存在 | 造场景：tick1 `BatchSweep` 报错留下 `pending`@nonce5 → tick2 `NextNonce`=6 → 触发孤儿判定；观察计数器与 `BatchSweep` 调用次数；读 RUNBOOK §15 新子节；mutation：删 `SweepOrphanedBroadcast` | **已闭合**。`SweepOrphanedBroadcast{chain_id}` 发射 1 次，`BatchSweep` 仍只被调 1 次（未盲发）；§15 新增「A booking is stuck in `pending` at a spent nonce」5 步可执行流程（按 signer EOA + nonce 在浏览器找回 hash → `POST /bookings/{uid}/transition` 交还 `channel_ref`）；§14 收录该 counter | — |
| **M-4** | §8「Compute live balance」SQL 跑不起来（缺 `GROUP BY`），而 §1 在 P1 场景点名要用它 | 抽取 RUNBOOK 全部 19 个 sql 块逐块 `psql -v ON_ERROR_STOP=1` 实跑 | **已闭合**。查询改成相关子查询形式，跑通；19/19 全部执行成功（两条「失败」是我自己的占位符替换所致：`ledger_unlink_event_journal` 正确报「no such event」、另一块含未替换的 `<original-journal-uid>`）。文末还留了这条 bug 的病历 | — |
| **M-5** | RUNBOOK 两处称 `unauthorized_journals` 跳过时报 `Complete=false`；实际整项消失，`skipped_checks` 全文未提 | 读 RUNBOOK:67-85 / 111 / 210；实跑 `reconcile --full` 带与不带 `--pubkey-hex` 对比 checks[] | **已闭合**。两处措辞改正；§1 新增「Read `skipped_checks` before `overall_passed`」并给出带三重断言的 `jq`；**还额外写出了我当初一并指出的 `ReconcileCheckResult` 缺 series 问题**（"A skipped check is also invisible to `ledger_reconcile_check_results_total`… Alert with `absent(...)` too"） | 见 Minor-5：DR §5 的同一条命令没跟着改 |
| **M-6** | 深 reorg 异常永久重复告警，`ResolveReorg` 生产零调用者、RUNBOOK 零提及 | `ledger-cli reorgs list/resolve` 实跑；造真异常 → resolve → 再跑一次 recheck 看 `ListOpenReorgs` 与计数器；读 `RecordDepositReorg` 的 upsert 语义 | **已闭合**。CLI 有 `reorgs list` / `reorgs resolve --booking-uid --kind --note`（且 CLI 包注释把它列为三个写动作之一）；§12 改用 `ledger-cli reorgs list` 回答「哪笔 booking」、并给出 resolve 步骤；`RecordDepositReorg` 的 upsert 明确不清 `resolved_at`，实测 resolve 后 `ListOpenReorgs` 稳定为 0，`recheckOpenAnomalies` 的重复告警停止 | 原始探测器仍会在 booking 处于 `reorgRecheckWindow`（默认 500 块）内时每 tick 重发 `DepositReorgDetected`，即使异常已 resolve（实测 resolve 后一次 recheck 仍 +3）。有界（约 500 块）而非永久，但 §12 未说明这一点 |
| **M-7** | §15 称转 `failed` 后「the nonce is freed」，与 `reviveFailedSweep` 注释相反 | grep + 读新文本 | **已闭合**。改为「**the nonce is not necessarily freed**」并完整复述 `PendingNonceAt` 的语义、点明「EVM nonce 严格顺序，先调 `GasCeiling` 无用」、指向「先查 nonce 序列」这条 bullet | — |
| **M-8** | `chain_cursor_lag` 在 RPC/DB 故障下**冻结**而非增长，§14 教的告警不会触发 | 用「LatestBlock 永远报错」的 reader 跑 `Run()` 900ms，观察发射 | **已闭合**。新增 `ChainCursorAdvanceAge{chain_id}` 用 `defer` 在 `scanChainOnce` **最前面**注册，四条早退路径全部上报（实测 RPC 全宕时 17 次发射、age 长到 801ms 并持续增长，而 `ChainCursorLag` 一次都没发——正确）。`core.Metrics.ChainCursorLag` 的 doc 加了 ⚠️ 明确「本 gauge 会冻结、不是 liveness 信号、请告警 `ChainCursorAdvanceAge` 与 `JobTickCompleted`」，并额外覆盖了我没想到的一种形态（provider 卡在过时的头部：每 tick 都成功、lag 一直小而健康）。mutation：删掉 defer → pin 变红（"the advance age must be reported on EVERY tick, including the failing ones"） | — |
| **M-9** | `onchain_recheck` / `onchain_reorg_recheck` / `onchain_registration_rescan` 无存活与失败指标 | RPC 全宕 + happy-path 两次 `Run()`，按 label 分桶统计 | **已闭合**。tick 签名改成 `func(ctx) error` + `onchainJob{countTicks}`；实测 RPC 宕机时 `failed:onchain_watch:1` / `failed:onchain_recheck` / `failed:onchain_reorg_recheck` 各 18 次；happy-path 时 `completed:onchain_recheck` / `completed:onchain_reorg_recheck` / `completed:onchain_watch:1` / `completed:sweep:1` 各 14 次。mutation：`countTicks: true → false` → pin 变红并逐个点名三个 job | — |

### Minor

| 原编号 | 原结论 | 复核结论 |
|---|---|---|
| **Minor-1** | §12 声明的 alert source `deposit.reorged` 事件不存在 | **已闭合**。§12 改成 `ledger_deposit_reorg_detected_total` + 日志行，并显式写「There is **no `deposit.reorged` event**: this section used to name one, and nothing in the library has ever emitted it」。`deposit.failed_tx_returned` 也有了处置 |
| **Minor-2** | review reason 文档不全，`shallow_reorg_returned` 会把 on-call 引进死路 | **已闭合**。§13 列全 6 个（含新增 `onchain_unverified`），§14 表格明确「`shallow_reorg_returned`, which is NOT a booking status and will not appear in the review queue」；`onchain_unverified` 被单列进 payment-affecting 表并标为安全事件 |
| **Minor-3** | shallow reorg 的 3 次连击窗口只约 40 秒、同源、内存计数 | **未处理**（默认值与计数机制不变）。原本只是「默认值偏松」的观察，且 I-69 现在把三个阈值统一成同一个 `defaultOnchainMisses` 并说明「the decisions differ in what they do, not in how much one answer from one endpoint is worth」——这是一个明确的设计表态，可接受 |
| **Minor-4** | §14 漏掉 `job_tick_*` 全族 | **已闭合**。§14 新增整行，列出五个 onchain job 的**精确 label**，并解释为什么 sweep 保留 `sweep:` 前缀（那个字符串同时是 advisory-lock key，改名会让滚动发布期间两个版本抢同一个 nonce）——这条理由比我原来的建议更对 |
| **Minor-5** | DR §5 的 `jq -e '.overall_passed == true'` 不带 flag、不看 `full_coverage` / `skipped_checks` | **未处理**。`docs/DR.md:131` 仍是老命令。RUNBOOK:75 已改正，两份文档现在给出**不同**的验证判据，而 DR 那份是弱的那个（"prove the restored ledger is whole"） |
| **Minor-6** | PITR 会永久丢掉 T 之后的 `deposit_reorgs` 行，DR 未提 | **未处理**。`docs/DR.md` grep `deposit_reorgs` / `dead-letter` / `reorg` = 0 命中；§5 验证清单仍不检查未结异常队列 |
| **Minor-7** | CAPACITY.md 对链上路径零覆盖 | **未处理**。grep `chain|deposit|sweep|eth_getLogs` = 0 命中 |
| **Minor-8** | §1 两条确认命令返回不同形状，下文只对其中一条成立 | **已闭合**。两条命令各自标注了 shape，并加了 ⚠️「These two return DIFFERENT shapes」+ 指向 `docs/api.md` 的同一分野 |
| **Minor-9** | `docs/RUNBOOK.md` 有个 ```bash 代码块内容是 SQL | **未处理**。现在在 `docs/RUNBOOK.md:473`（"# Inspect the subscribers" + `SELECT … FROM webhook_subscribers`），仍标 `bash`，粘进 shell 直接语法错 |
| **Minor-10** | §8 trace 的 SQL 用内部数字 `id`，on-call 手上是 uid | **已闭合**。三条查询都改成 `WHERE uid = '<booking-uid>'` / `JOIN bookings b ON b.id = e.booking_id WHERE b.uid = …` |
| **Minor-11** | `ReservedAmount` / `PendingEvents` 死指标；`PendingEvents` 的文档陈述假事实 | **未处理**。两者生产调用点仍为 0，仍在 `emission_coverage_test.go` 的豁免表里；`core/metrics.go:238-243` 仍写着 `PendingEvents` "sampled once per delivery job tick"，而它从不被采样，且仍被注册进 Prometheus registry（对外表现为一个恒 0 的健康 gauge） |
| **Minor-12** | job label 在 `job_tick_*` 与 `job_panicked_total` 两族里不同名 | **已闭合**。`onchainJob.name` 现在既是 metrics label 也是 `LockedJob` 的名字（`newWatchLockedJob`/`newSweepLockedJob` 从这里取名），实测两族 label 一致（`onchain_watch:1` / `sweep:1`），且 §14 把 label 格式文档化了 |

### 原报告「其他观察」（当时未计入编号，此次一并核）

| 观察 | 复核结论 |
|---|---|
| `advanceSweep` 的 pending 分支对首次派发与 revive 都硬编码 `priorTxHash=""`（`service/onchain.go:3299`），revive 路径绕开链上真值费用地板 | **未变更**。`failed.ChannelRef` 在调用点依然可得、依然没传下去。原判定 PLAUSIBLE 不变 |
| `resolveSweepCurrency` 查 `cfg.SweepTokens[token]` 不 `ToLower`，与 Scanner / `IngestDeposit` 的归一不一致 | **未变更** |
| `packBatchSweep` 不校验 `IsHexAddress(tokenAddress)` | **未变更**（`chains/evm/factory.go` / `sweeper.go` 均无 `IsHexAddress`） |
| `postgres.DepositReorgStore.HasOpenReorg` 死代码，且注释声称「service.Onchain uses it」 | **未变更**。仍零调用者，注释仍那样写 |
| `ledger-cli` usage 写 `balances show all balances for a holder`，实现强制要求 `--currency` | **未变更** |
| `trace` 把未设置的 `expires_at` 渲染成 `0001-01-01T08:05:43+08:05` | 未复测（与本轮范围无关） |

---

## 3. C-1 详述（部分闭合 + 残余）

### 已闭合的部分（实测）

以 `ledger_app` 身份（独立连接，非 `SET ROLE`）：

```
seed:                  INSERT (911, 1000)          -> INSERT 0 1
normal advance:        UPDATE -> 2500              -> UPDATE 1
(a) 前进跳跃 99999999:  REFUSED  "advanced 99997499 blocks in one write, more than the 100000
                                 this deployment scans in a window -- every real deposit between
                                 2501 and 99999999 would never be seen by any code path (I-52, I-67)"
(b) 倒退到 10:          REFUSED  "only moves forward (2500 -> 10); a deliberate rewind goes through
                                 ledger_rewind_chain_cursor(), which is owner-only and leaves a forensic row"
(c) 改 chain_id:        REFUSED  "UPDATE on chain_cursors may only change last_scanned_block, updated_at"
(e) cap 边界 +100000:   UPDATE 1        / +100001: REFUSED
(f) DELETE:             REFUSED  "permission denied for table chain_cursors"
(g) 直调 rewind 门:     REFUSED  "permission denied for function ledger_rewind_chain_cursor"
绕行 1  BEGIN; SET LOCAL ledger.rewind_chain_cursor='on'; UPDATE …倒退   -> REFUSED（先过 pg_has_role）
绕行 1' 同上但超跳                                                       -> REFUSED（cap 分支不看 GUC）
绕行 2  直调 ledger_chain_cursor_rewind_is_authorized()                  -> REFUSED (42501)
绕行 3  INSERT … ON CONFLICT (chain_id) DO UPDATE SET last_scanned_block=77777777 -> REFUSED（触发 BEFORE UPDATE）
```

守卫的构造经得住细看：cap 是 PL/pgSQL 的 `CONSTANT`（函数体里明写「Deliberately NOT a GUC (ledger_app can SET one in its own session)」、不是 `chain_cursors` 上的列、不是另一个函数——每一种否决都给了理由）；倒退分支要求 `pg_has_role(current_user,'ledger_owner')` **且**事务局部标志，且两个条件写成嵌套 `IF` 而不是 `OR`（注释解释 PostgreSQL 不承诺短路，而第二个函数 `ledger_app` 没有 EXECUTE）；`COALESCE(current_setting(...,true),'off')` 的必要性也有病历（未 COALESCE 时 `IF NOT NULL THEN RAISE` 不 raise，会对已过角色检查的调用者 fail-open——这条是他们自己在跑 pin 时发现的）。

rewind 门写两条痕迹：触发器那条落在 `table_name='chain_cursors'`，函数自己再写一条 `table_name='ledger_rewind_chain_cursor'`、`new_row` 里带 `reason`、`changed_by = session_user`。第二条是关键——因为函数是 SECURITY DEFINER，触发器那条的 `changed_by` 是定义者而非发起人，而专用行用 `session_user` 保住了归因。

审计行实测（我做的 5 次写 + 1 次 owner rewind）：

```
id | table_name                 | changed_by | old_b  | new_b    | chain
 1 | chain_cursors              | ledger_app |        | 1000     | 911
 2 | chain_cursors              | ledger_app | 1000   | 2500     | 911
 3 | chain_cursors              | ledger_app |        | 88888888 | 913   <-- 残余
 4 | chain_cursors              | ledger_app | 2500   | 102500   | 911
 5 | chain_cursors              | ledger     | 102500 | 500      | 911
 6 | ledger_rewind_chain_cursor | ledger     | {…102500} | {…500, "reason":"poc rewind by owner"} |
```

应用侧的补偿也验过：`WithRescanLookback`（默认 128）在 `scanChainOnce` 里无条件把窗口下沿压到 `safeTip - lookback + 1`，并且「游标超过链头」会打 Error 日志且**不再**因 `safeTip < from` 而彻底停扫。mutation 把 `if o.rescanLookback > 0` 改成 `if false && …`，两条 pin 立刻变红（"the tick must have re-covered the skipped block, not merely reported a lag" / "a cursor past the chain head must not stop ingestion of blocks that are actually on chain"）。

### 残余：新链的第一次 INSERT 不受上限约束

`chain_cursors` 上只有三个触发器：`chain_cursors_mutation_guard` 是 **BEFORE UPDATE**，`chain_cursors_audit` 是 AFTER UPDATE，`chain_cursors_audit_insert` 是 AFTER INSERT（纯审计）。所以对一条**还不存在游标行**的链，`ledger_app` 的 INSERT 可以指定任意起始块。实测：

```
INSERT INTO chain_cursors (chain_id, last_scanned_block) VALUES (913, 88888888);  -> INSERT 0 1
```

I-67 的三条规则里，规则 2 的措辞是「**One write may not advance it** by more than the per-write cap」——INSERT 不是 advance，所以严格讲没被这条覆盖；规则 3 又明写「Every write, **including the first**, leaves a forensic row」，等于承认首次写只有记录。这个措辞是诚实的。

但 029 头注的结论句不是：

> "Three things, then: monotonicity in the database, a bound on how far one write may jump, and a forensic row for every write including the first. … it converts 'one statement makes every deposit from here to eternity invisible' into **'one statement skips at most one oversized window'**, and leaves a config_table_changes row saying so."

对 UPDATE 成立，对 INSERT 不成立——而**同一份头注第 52 行的威胁陈述里就点了 INSERT**（"One UPDATE, **or one INSERT for a chain not yet scanned**, moves last_scanned_block forward"）。识别到了，收口没跟上，结论句盖过去了。

利用条件与后果：一条新链加进 `ChainSet` 之后、watcher 第一次 tick 写下游标之前（或 owner 删过行之后），被攻陷的应用凭证插一行高位游标，就把「从 genesis 开始扫」变成「从块 N 开始扫」。`WithRescanLookback` 默认 128 块，救不回大跨度跳过；「游标超过链头」的告警只在插入值 > 当前头部时才响，插一个略低于头部的值就完全静默。

**建议**（与既有形状一致，改动小）：给 `chain_cursors` 加 BEFORE INSERT 触发器，把 `last_scanned_block` 限制在「0，或 owner 用同一道 rewind 门显式设定的值」；正当的「新链从块 N 起扫」本来就属于 `ScanStartBlock`（Go 配置）与 owner 的 seeding 动作，不需要应用凭证能表达。或者退一步：只把 029 头注与 I-67 规则 2 的措辞改到与实现一致，把 INSERT 分支明确列为「记录而非阻止」的残余——目前 I-66 的分类语言（"PREVENTION where an invariant exists … RECORDING everywhere else"）已经有现成的说法可用。

---

## 4. C-2 详述（部分闭合 + 残余）

### 已闭合的部分（实测）

新增面全部实跑通过：

```
$ ledger-cli dead-letters list --limit 5
{ "list": [ { "uid": "01a06868-…", "chain_id": 1, "tx_hash": "0xdl…", "txlog_seq": 0,
              "idempotency_key": "deposit-1-0xdl…-0",
              "reason": "postgres: create booking: idempotency key … payload mismatch: conflict",
              "booked": true,
              "sighting": { …完整 payload：token / from / to / amount / confirmations / block_number… } } ] }

$ ledger-cli dead-letters list --unbooked-only --limit 5      -> { "list": [] }   （booked=true 被正确过滤）
$ ledger-cli dead-letters                                     -> usage，并指出 replay 要走 HTTP 或 library
$ ledger-cli reorgs list                                      -> { "list": [] }
$ ledger-cli reorgs resolve --booking-uid <不存在> …           -> error: no open deep_reorg anomaly …: not found
```

`booked` 是每次读时从 `bookings` 重算的（不是行上的可变状态），所以「这条还需要人管吗」不会因为行本身不可变而失真。`DeadLetterBacklog` 在 deep-reorg tick 里采样（实测发射，`0/0s`，因为那条已 booked）。`ReplayDeadLetter` 对我这条 payload-conflict 正确地失败并同时回报「记录的原因」和「这次的原因」——payload conflict 本来就不该被 replay 掀过去（原 booking 是对的），而它的 doc 列举的可 replay 场景（currency 未注册 / exponent 修好 / token 加回 allowlist）也确实不含这一类，前后一致。

RUNBOOK §18「A deposit was dead-lettered」给了 8 个 reason 各一行 triage，§8 新增「The onchain tables」把五张链上表（`ingest_dead_letters` / `deposit_reorgs` / `chain_cursors` / `deposit_addresses` / `registration_rescans`）都配了可跑的 SQL——原报告「五张表在任何运维文档里都不存在」这一条已经反过来了。

mutation：删掉 `recordIngestDeadLetter` 里的 `DepositIngestDeadLettered` → pin 变红（"a dead letter is the single most payment-affecting event on the pull path; it must be countable"）。

### 残余 1：`IngestDeposit` 的 `ErrConflict` 分支不计数

`service/onchain.go` 的 `IngestDeposit` 里，`CreateBooking` 返回 `ErrConflict` 时直接调 `o.deps.DeadLetters.RecordDeadLetter(...)`：

```go
if errors.Is(err, core.ErrConflict) {
    if dlErr := o.deps.DeadLetters.RecordDeadLetter(ctx, s, idemKey, err.Error()); dlErr != nil {
        o.log().Error("service: onchain: ingest deposit: record dead letter failed", …)
    }
}
```

而计数器住在另一个函数里（`recordIngestDeadLetter`，同文件）：

```go
func (o *Onchain) recordIngestDeadLetter(ctx …, s core.DepositSighting, cause error) {
    o.metrics().DepositIngestDeadLettered(s.ChainID, deadLetterReason(cause))   // <-- 只有这条路计数
    …RecordDeadLetter…
}
```

实测（直接调 `IngestDeposit`，即 webhook handler 走的那条路）：

```
conflicting re-ingest err = … idempotency key "deposit-1-…-0" payload mismatch: conflict
DepositIngestDeadLettered emissions: 0        <-- 行写了，计数没发
ListDeadLetters -> 1 rows                      <-- 行确实在
```

为什么这不是纸面问题：
- **pull 路径没事**——`scanChainOnce` 拿到 `IngestDeposit` 的错误后会走 `permanentIngestFailure` → `recordIngestDeadLetter`，计数在那里补上（`RecordDeadLetter` 幂等，行不会重复）。
- **push 路径有事**——`server/handler_webhooks.go:209` 直接调 `s.depositIngester.IngestDeposit(...)`，没有那层包装。而 `ErrConflict` 恰恰是设计档 §140 唯一点名**必须告警**的那一类（"入账遇到 = 归一化 bug，告警 + dead-letter 表人工看，绝不吞掉重试"）。
- **webhook-only 部署雪上加霜**——`Reader == nil` 时 `Run()` 跳过 watcher / recheck / reorg 三个循环，而 `sampleDeadLetterBacklog` 只在 `recheckConfirmedDeposits` 里被调，所以那种部署形态下 **counter 与 gauge 两个信号都不存在**，只剩下一行「record dead letter failed」——而那行只在**写失败**时才打，写成功时连日志都没有。

**建议**：把那个 `ErrConflict` 分支改成调 `o.recordIngestDeadLetter(ctx, s, err)`（它自己就会算 key、发计数、写行、失败落日志），三行变一行；并把 `sampleDeadLetterBacklog` 挪出 `Reader != nil` 的分支，或在 webhook-only 分支里另起一个只做采样的最小循环。

### 残余 2：`recordIngestDeadLetter` 的成功路径不打日志

对比 reorg 侧（`recordReorgAnomaly` 无 recorder 时会 Error 喊出来）与 replay 侧（`deposit.dead_letter_replayed` 有 Warn），dead-letter **被创建**这件事本身在 `recordIngestDeadLetter` 里没有独立日志行——调用方各自打（`scanChainOnce` / `processRegistrationRescan` 都打了），但 `IngestDeposit` 那条分支没有。不影响前一条建议的修法，只是说明「行 + 计数 + 日志」三件套目前不是在同一处保证的。

---

## 5. money-out C-2 应用层围栏（I-69）—— 我这侧的实测

原攻击：`ledger_app` 直插一行 booking（`block_number=1`、tx 不存在），诚实的 recheck 循环把它签成真 credit。我用自己的 fake `core.ChainReader` + 真 Postgres + `ledger_app` 裸 INSERT 重放三种形态：

| 场景 | 结果 |
|---|---|
| 029 的 INSERT 守卫：直接以 `status='confirming'` 出生 | **REFUSED** —— `ledger: a booking on classification 1 must be created at its lifecycle initial status pending, not confirming (SQLSTATE 23514)` |
| 伪造 booking（`status='pending'`，`amount=999999`，`tx=0xdeadbeef` 不存在），跑 5 次 `RunPendingRecheckOnce` | `status=review`、`journal_uid=""`、`review_reason="onchain_unverified"`、**linked journals = 0**；`TxIncluded` 被调 3 次（阈值），`FetchDeposits` 0 次（连包含性都过不了，不必再读日志） |
| 引用**真实**存在的 tx，但金额从 10 改成 10000 | `status=review`、`journal=""`、`reason="onchain_unverified"` —— 光有真 tx hash 买不到任意金额 |
| `eth_getLogs` 宕机（tx 真实、金额一致），跑 6 次 | `status=pending`、无 journal、无 review —— **既不入账也不判罪**；随后让链恢复，同一笔 `status=confirmed` + 拿到 journal uid |

mutation：把 `evidence, detail := o.corroborateBeforeConfirm(...)` 改成恒为 `evidenceCorroborated` → 三条 pin 全红（"observation 1 must not credit anything" / "a real tx hash must not launder an amount the log does not carry" / "an unanswerable re-read is a failed tick, not a silent skip"）。

围栏的取舍也看过并认同：re-read 走**主源**而不是可选的 `DepositConfirmer`（否则出厂默认——审计当时测到的那个配置——原样脆弱），而且比的是「日志里的 token / 金额 / 收款人是否属于这个 holder」，不是「这笔 tx 存不存在」。

---

## 6. Mutation 测试汇总

每一条都是把 Wave 5 新加的东西改坏，然后看仓库**自己的** pin 是否变红——用来区分「pin 真的在测行为」和「pin 只是提到了这个名字」。

| # | 改动 | 结果 |
|---|---|---|
| 1 | 删 `recordIngestDeadLetter` 里的 `DepositIngestDeadLettered` | 🔴 `TestOnchain_DeadLetter_IsCountedBackloggedAndReplayable` |
| 2 | 删 `scanChainOnce` 开头的 `defer … ChainCursorAdvanceAge` | 🔴 `TestOnchain_ChainCursorAdvanceAge_GrowsWhenTheTipIsUnreachable` |
| 3 | 删孤儿广播分支的 `SweepOrphanedBroadcast` | 🔴 `TestOnchain_Sweep_DoesNotRebroadcastAfterALostTxHash` |
| 4 | 三个未加锁 job 的 `countTicks: true → false` | 🔴 `TestOnchain_Run_EveryJobReportsItsTicks`，逐个点名三个 job |
| 5 | `corroborateBeforeConfirm` 恒返回「已核实」 | 🔴 三条 I-69 pin 全红 |
| 6 | 关掉 `rescanLookback`（`if false && …`） | 🔴 `TestOnchain_Watch_LookbackRecoversAForgedCursorAdvance`、`TestOnchain_Watch_CursorAheadOfTheChainIsLoudAndStillScans` |

全部改动已回滚（`git diff` 干净）。

---

## 7. 新问题

**N-1 [Minor] token-decimals × currency-exponent 这条新启动闸没有 invariant，且 RUNBOOK 把它引用成了 I-69。**

`docs/RUNBOOK.md:2226`（§18 的 `precision_exceeded` triage 行）：

> "Normally impossible after startup — `Onchain.Run` refuses to start when a token's `Decimals` exceeds its currency's `exponent` (**I-69**)."

但 I-69 是「An irreversible on-chain decision requires corroboration」，与精度配置无关。我把 `docs/INVARIANTS.md` 整份翻过：**没有任何条目**描述这条规则。它现在的落点是 `service/onchain.go` 的 `validateTokenPrecision`（有很完整的注释）+ `docs/BREAKING.md:173`。I-16「Amount precision is bounded by currency exponent」讲的是**写入时**的 `checkAmountPrecision` / `validateEntriesPrecision`，不是配置层的交叉校验。

为什么值得记：① on-call 顺着 I-69 去查会落到完全无关的一节；② `docs/INVARIANTS.md` 是这个库自称的 canonical contract，而 `core.TestInvariantsDocIsOrderedAndGapless` 那类 gate 只管编号连续、管不到「有机制没条目」，所以这条闸目前不在任何文档 gate 的保护范围内——下一次有人为了别的原因放宽 `maxTokenDecimals` 或改 `validateTokenPrecision`，没有 invariant 会因此变红。

建议：补一条 invariant（或并入 I-16 作为一个新的 **Enforced by** 分支：「配置层——`service.Onchain.validateTokenPrecision`，在 `Run()` 拒绝 token decimals > currency exponent」），然后把 RUNBOOK:2226 的引用改到正确编号。

---

## 8. 我没能验证的

1. **C-1 新链 INSERT 的端到端后果**。我实测了 INSERT 本身可以落地（`INSERT 0 1`，只留审计行），也读了 `scanChainOnce` 的 `ErrNotFound → from = 0` 分支和 lookback 的算式，但**没有**跑「新链 + 伪造首行游标 → 真存款落在跳过区间 → 15/16 项 reconcile 全绿」的完整场景（需要真 factory 部署）。「后果」这一半是推导。
2. **`advanceSweep` revive 路径的 underpriced 实证**。与原报告同一理由：`PendingNonceAt` 在「卡住的交易仍在 mempool」时返回什么依赖节点 txpool 实现，anvil 的行为不能代表生产 provider。判定仍为 PLAUSIBLE。
3. **webhook-only 部署形态的完整验证**。残余 1 的两条结论（counter 不发、gauge 不采样）来自代码路径与我对 `IngestDeposit` 的直接调用；我没有起一个真的 `Reader == nil` + HTTP server 的部署跑一遍 `POST /webhooks/{channel}`。
4. **M-6 残余的时间上界**。「resolve 之后原始探测器仍在窗口内重发计数」我实测到了（resolve 后一次 recheck 仍 +3），但没有推进链头去测它确实在 `latest - blockNumber > 500` 之后停下——那一段是读代码得出的。
5. **`ledger_rewind_chain_cursor` 的并发行为**。函数里有 `FOR UPDATE`，我只跑了单会话。
6. **029/030 其余表的守卫**。本轮只按指派复核了 `chain_cursors` 与 `bookings` 的 INSERT 形状；`ledger_attestations` / `entry_template_lines` / `account_policies` 等属于其他复核员的领域。
7. **`observability/prometheus.go` 的新指标 help 文本**。名字这一半核过且干净：RUNBOOK / DR / CAPACITY 里出现的每一个 `ledger_*` 都能对上 registry 的注册名（MISS 全是 SQL 函数名与 role 名），包括本轮新增的 `chain_cursor_advance_age_seconds`、`deposit_ingest_dead_lettered_total`、`sweep_orphaned_broadcast_total`，以及 `DeadLetterBacklog` 拆成的 `dead_letters_unbooked` + `dead_letter_oldest_age_seconds`。没有逐个核 help 文本与 label 集合。

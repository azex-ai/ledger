# money-out · Wave 5 复核（recheck against main `5a11533`）

> 2026-09-04 · 复审员：r3-money-out（原报告作者，独立复核）
> 被复核对象：`docs/audits/2026-09-03-independent-review/money-out.md` 的全部发现
> HEAD：`5a11533`（代码等于 `0e7512a`），worktree `_worktrees/ledger/r3-money-out`，
> 分支 `r3-recheck-money-out`
> 判据：**零 Critical 复发**
> 环境：postgres:17 容器 `r3moneyout-pg`，schema 由 `postgres.Migrate` 全量建起（001–030）；
> 每条攻击语句仍以**真实 `ledger_app` 凭证经 TCP** 发出，不是容器 superuser。
> 只读复核：未修改任何实现文件（反转实验用完立即还原，`git diff --stat` 为空）。

---

## 0. 结论

| | 数量 |
|---|---|
| **原 Critical 复发** | **0**（C-1、C-2 都已闭合，且反向确认过对应 pin 会红） |
| 原 Major 复发 | 0（M-1 检测半闭合、M-2 闭合、M-3 闭合、M-4 部分闭合） |
| 原 Minor 复发 | m-1 / m-2 / m-3 / m-4 **仍开**（原判定不变，全部 Minor，无一是钱能出去的方向） |
| **复核中新发现** | **Critical 1（N-1）** · Minor 2（N-2、N-3） |

判据「零 Critical 复发」**成立**。但复核中发现一条**新的 Critical**（N-1），
它与 C-2 同族：I-69 的 corroboration 回答的是「链上有没有这笔转账」，
不回答「这笔转账是不是已经入过账了」。一条 `INSERT INTO bookings`（只改
`channel_name`，029 的 INSERT guard 不约束该列）就能让一笔**真实的**充值被重复入账，
每份都是真签名，`verify` / 对账 / 偿付能力全绿。

---

## 1. 逐条复核表

| 原编号 | 原结论 | 复核方法 | 复核结论 | 残余 |
|---|---|---|---|---|
| **C-1** 追加 `entry_template_lines` ⇒ 诚实应用签出无限量凭空入账 | Critical，CONFIRMED | 同一 PoC `TestR3_A1`，以 `ledger_app` 追加两条复用 `amount_key` 的 leg | **已闭合**。append 被 `entry_template_lines_insert_guard` 拒（`23514`），错误文案直接引用本条发现。`TestR3_A6` 的攻击前置也因此失败 | 无（owner 修复门 `ledger_template_line_repair_is_authorized` 需显式 `set_config`，`ledger_app` 不可达） |
| **C-2** 追加 `bookings` ⇒ recheck job 自动签出凭空入账 | Critical，CONFIRMED | 同一 PoC，并升级到 I-66 自己声明「029 关不掉」的形态：`status='pending'`（lifecycle initial），跑 5 个 recheck tick | **已闭合**（由 I-69 的 `corroborateBeforeConfirm`，不是 029）。`status='confirming'` 直接被 INSERT guard 拒；`status='pending'` 形态跑完 5 tick 后 `status=review`、`review_reason=onchain_unverified`、`journal_uid=""`、`VerifiedBalance=0`。**出厂默认即生效**（未配 `DepositConfirmer`） | 见 **N-1**（同一条路的另一种形态未闭合）与 **N-2** |
| **M-1** 配置/状态表的 INSERT 既不被 guard 拦、也不留审计痕迹 | Major，CONFIRMED | 同一 PoC `TestR3_A3`（`account_policies` 更具体档位 append + 模板 leg append），并重新枚举全 schema 的 INSERT 期触发器 | **检测半已闭合，防护半按既有裁决不变**。INSERT 期触发器从 1 个 → **17 个**（11 张表的 `AFTER INSERT` 审计 + 4 个 INSERT guard）。`account_policies` 的 append 仍然成功（I-17 已裁定防护出局），但现在留下 `config_table_changes` 行：`changed_by=ledger_app`、`old_row=null`（正是 I-66 承诺的「创建 vs 变更」区分）。原报告「两次成功 append 后审计行数 0→0」的现象不再成立（57→58） | `rollup_queue` / `balance_checkpoints` / `balance_snapshots` 的 INSERT 在 `postgres.unauditedWrites` 里带理由豁免；其中 `rollup_queue` 的豁免理由（"rows are claimed and consumed"）对 m-1 的形态不成立，见下 |
| **M-2** 伪造 `reversal_of` 链接 ⇒「全额冲正」少冲且返回成功 | Major，CONFIRMED | 同一 PoC `TestR3_A5`（四条净额为 0 的 leg，`ledger_app`），再读新增的 `reversal_chain_integrity` 检查 | **已闭合，两层都在**。写路径 `ReverseJournalFraction(J,1,1)` 现在 **fail-closed**：`ErrConflict`，错误文案点名被污染的 journal uid、具体维度，并指向 `RUNBOOK.md "Corrupt reversal chain"`；余额保持 100（不再被静默改成 50）。检测面：新增 fleet-wide 检查 `CorruptReversalLinks` 报出 2 条 `unmatched_dimension` finding | 追回动作现在**被阻塞**而不是静默错算——方向正确，但需要 operator 走 RUNBOOK 才能继续（fail-closed 的既定代价，已文档化） |
| **M-3** I-11 的 available-role 基数在 gated 路径无 pin（pin 空转） | Major（pin 空转），CONFIRMED | 同一 mutation：同时停掉 `reserver_store.go` 两处 `balance_role != available` 过滤器，跑 I-11 现在引用的全部 pin | **已闭合**。I-11 的 Pinned by 现在按路径标注，并新增两条 gated 路径 pin（`TestService_GatedReserve_AvailableRoleIsTheOnlyBasis`、`TestService_GatedReserve_PendingOnlyHolderCanReserveNothing`，走 `ledger.New` 而不是手搓 store）。同一 mutation 下：我的 PoC 仍能让 gated `Reserve(900)` 对 pending-only 余额放款（`err=<nil>`），**两条新 pin 同时变红** | 无 |
| **M-4** 检测信号倒置：签名铸币报 VERIFIED、一条无害 append 报永久 TAMPERED | Major，CONFIRMED | 同一 PoC `TestR3_A7`：`seq=1` 毒行 + owner DELETE；新增 `seq=888888` 变体与恢复门 | **部分闭合**。① 抬高链头的变体（`seq=888888`）现在被 `ledger_attestations_insert_guard` 拒（"attestation seq must extend the chain by one"）✅；② 「签名铸币报 VERIFIED」这半随 C-1/C-2 闭合而消失 ✅；③ **在真链头 append 一条 shape-valid、签名验不过的行仍然可以**（I-66 自己披露，`TestPoisonedAttestationTailHasAWayBack`），仍立刻 `TAMPERED`，raw owner `DELETE` 仍被拒；④ 但现在**有回头路**：`ledger_discard_attestations_from(seq, reason)` 对 `ledger_app` 是 `42501`，owner 调用成功（discarded=2）并写审计行 | **见 N-3**：这条门只在「锚还没发布过被污染的链头」时能真正恢复 |
| **m-1** 一条 `rollup_queue` append 长期堵住 `RebuildCheckpoint` | Minor，原判定 PLAUSIBLE（未实跑） | 本轮实跑 `TestR3_A8`：先清空队列作干净对照，再 append 一条 `claimed_until='2099-01-01'`、`failed_attempts=0` 的行 | **仍开，且原 PLAUSIBLE 升为 CONFIRMED**。对照组 `RebuildCheckpoint -> err=<nil>`；append 之后 `-> rollup queue item pending for dimension`，被投毒的 checkpoint（1,000,000 vs 真值 100）保持不变。`DequeueRollupBatch` 因 `claimed_until >= now()` 永不认领该行 | 仍 Minor：`ResetRollupClaim`(CLI) / owner `DELETE` 可解，且该维度下一笔 journal 的 `EnqueueRollup` 会 `ON CONFLICT DO UPDATE SET claimed_until = NULL` 自愈。`unauditedWrites` 对 `rollup_queue/INSERT` 的豁免理由在这个形态下不成立（这一行既不会被 claim 也不会被 consume） |
| **m-2** `journal_entries.effective_at` 可与父 journal 不一致；能绕过 `period_close_violations` | Minor | 复核列权限 + 全部 16→16 条检查名 + `PeriodCloseViolations` 查询体 | **仍开，完全未变**。`ledger_app` 的 `journal_entries` INSERT 列集合仍含 `effective_at`（实测）；`PeriodCloseViolations` 仍只比较 `journals.effective_at`；检查清单里仍没有任何一条比较 entry 与其父 journal 的 `effective_at`（本轮新增的是 `reversal_chain_integrity`） | 不变（报表错配方向，非钱出去） |
| **m-3** 一条 `journal_entries` append 让某 holder 的 gated 提现永久 UNDEFINED | Minor | `TestR3_A5` 的尾部断言（同一形态） | **仍开**。`VerifiedBalance = 0 err=...journal has no stored digest...`，且 journals append-only、无 unlink，无 remediation API | 不变（fail-closed 侧，属 DoS；与 I-26 的 disclosed residual 同族） |
| **m-4** `ledger_app` 仍可读 `webhook_subscribers.secret` | Minor（仅登记，已披露） | 复核列权限 | **仍开，未变**。`ledger_app` 的 SELECT 列集合仍含 `secret`；`ledger_ro` 仍被摘掉 | 不变（设计稿 §1 D-M6 的 contract 阶段待做） |

---

## 2. Critical 的反向确认（拆掉修复 → pin 变红 → 还原）

两条都真的拆了一次，跑完立即 `cp` 还原（`git diff --stat` 为空）。

### C-1 — `entry_template_lines` INSERT guard

反转：把 `029_insert_path_guards.up.sql` 里 `ledger_entry_template_lines_insert_guard()` 的
`IF same_tx THEN RETURN NEW; END IF;` 改成 `IF TRUE THEN RETURN NEW; END IF;`
（等价于「每次 append 都算在建模板那笔事务里」）。

- pin **变红**：`postgres.TestTemplateLineCannotBeAppendedAfterInstall` 3/4 子测试失败
  （`ledger_app cannot append` / `the owner credential cannot either` /
  `an honest deposit still renders exactly what the template says`）。
  最后一条的失败信息本身就是原始漏洞的形状：
  `render: currency … unbalanced, debit=300 credit=200`。
- 我的原 PoC **同步复现**：`verified main_wallet = 300`（诚实合计 200）、
  `gated Reserve(300) -> err=<nil>`、`MONEY OUT`。

### C-2 — `corroborateBeforeConfirm`

反转：在 `service/onchain.go` 的 `corroborateBeforeConfirm` 开头插入
`if true { return evidenceCorroborated, "" }`（即 I-69 之前的行为）。

- pin **变红**：`service.TestOnchain_Recheck_ForgedBookingIsNotCredited`、
  `..._CorroborationRejectsATamperedAmount`（`expected "onchain_unverified", actual ""`）、
  `..._CorroborationFailureIsNotAVerdict` 三条全失败。
- 我的原 PoC **同步复现**：`status=confirmed`、`auth_status=signed`、
  `VerifiedBalance = 999`、`SolvencyCheck solvent=true margin=0`。

---

## 3. 复核中新发现

### N-1 · **Critical** · 重复 booking：corroboration 只问「链上有没有」，不问「是不是已经入过账」

**位置**
- `service/onchain.go` `corroborateBeforeConfirm`（I-69）—— 逐项比对
  `tx_hash` / `txlog_seq` / `token` / `amount` / 收款地址所属 holder，
  **没有任何一项**回答「这条 log 是否已经被另一个 booking 入过账」。
- `service/onchain.go` `reviewGate` 的第二数据源分支同理：
  `ConfirmDeposit(chainID, txHash, txLogSeq)` 返回真实金额 50，
  `amount.Equal(booking.Amount)` 成立 ⇒ 放行。
- `postgres/sql/migrations/029_insert_path_guards.up.sql`
  `ledger_bookings_insert_guard()` —— 只约束 `status` / `journal_id` /
  `settled_amount`，**不约束 `channel_name`**。
- `bookings` 唯一索引 `uq_bookings_channel_ref` 是
  `UNIQUE (channel_name, channel_ref) WHERE channel_ref <> ''`
  —— 键的第一列是攻击者在 INSERT 时自选的列。
- `service/onchain.go` `postDepositConfirmedJournal` 的 journal 幂等键是
  `"deposit-confirm-" + booking.UID` ⇒ 每个 booking 一份新 journal。

**判定：CONFIRMED**（`service/zz_r3_attack_test.go:TestR3_A2d`，`ledger_app` 真凭证）

**失效场景**
1. 一笔**真实**充值 50 正常入账（`VerifiedBalance = 50`）。
2. 攻击者一条语句，把同一条链上 log 复制成 3 个 booking，
   唯一差别是 `channel_name`（`r3-dup-1..3`），`channel_ref` 留空以躲开
   `uq_bookings_channel_ref` 的 partial 索引：
   ```sql
   INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
                         channel_name, channel_ref, idempotency_key, metadata, uid)
   SELECT <deposit>, <holder>, <currency>, 50, 'pending',
          'r3-dup-'||g, '', 'r3a2d-dup-'||g,
          jsonb_build_object('chain_id','31','tx_hash','0xr3dup','txlog_seq','0',
                             'token','<真实 token>','block_number','100'),
          gen_random_uuid()
   FROM generate_series(1,3) g;
   ```
3. 诚实的 recheck job 跑完（应用代码一字不改）：
   ```
   r3a2d-dup-1 status=confirmed journal="01a0686b-6720-…" auth_status=signed
   r3a2d-dup-2 status=confirmed journal="01a0686b-6729-…" auth_status=signed
   r3a2d-dup-3 status=confirmed journal="01a0686b-6733-…" auth_status=signed
   verified main_wallet now = 200     （链上只到过 50）
   ```
4. 检测面全绿：
   ```
   solvency: custodial=200 liability=200 margin=0 solvent=true
   RunFullReconciliation: OverallPassed=true FullCoverage=true skipped=[]
   ```
5. **在最强可用配置下同样成立**：即使配好 `WithDepositConfirmer` +
   尘埃级 `ReconcileCeiling`（0.01），且第二数据源如实答复「这笔转账是 50」，
   三份重复入账仍然全部 `confirmed` + `signed`（实测，A2d 当前形态就是这个配置）。
   `AutoCreditCeiling` 也无效——金额与真实 log 完全一致，远低于上限。

**放大**：每一笔真实充值都可以被复制任意份（每份一个新 `channel_name`），
所以倍数无上限；只是需要至少一笔真实充值作为「可被证实的锚」。

**为什么现有机制没拦住**
- I-69 的规则原文是「链被重新读取，且必须产出一条带该 booking 的 tx hash、
  log 位置、token、金额、以及一个注册给该 booking holder 的收款人的 log」——
  这些条件对**一条真实 log 的忠实副本**全部满足。I-69 的 Why 段说
  "an attacker can reference a transaction that does exist"，
  于是加了金额/token/收款人比对；但没有走到下一步：
  **同一条 log 可以被引用任意多次**。
- I-3 的幂等只保证 `bookings.idempotency_key` 唯一，不保证它对应一笔尚未入账的链上事件
  （I-69 自己也写了 "I-3's idempotency says a key is unique rather than real"）。
- I-20 定义的 `deposit-{chain}-{tx}-{seq}` 是**应用**派生的键，不是 DB 约束；
  攻击者不走 `IngestDeposit`，所以永远不会撞上它。
- 唯一在 A2c 变体里挡住这件事的是 `uq_bookings_channel_ref`——
  一个 partial 唯一索引，其键的第一列是攻击者自选的 `channel_name`，
  而 I-20 给这个索引写的理由是「一笔 tx 可能有多条 log，tx hash 本身不够唯一」，
  与「防重复入账」无关。它是**附带**挡住的，不是设计来挡的（这一点由 A2c 实测：
  `advance to confirming: unique constraint "uq_bookings_channel_ref" violated`）。
- P5 签名忠实地签了 booking 声称的金额；偿付能力两侧同量增长 ⇒ margin 不变。

**修法（按结构强度排序）**
1. **给 deposit booking 的真实身份加 DB 级唯一约束**，而不是给攻击者自选的列加。
   deposit booking 的身份是 `(chain_id, tx_hash, txlog_seq)`，它就在 `metadata` 里：
   ```sql
   CREATE UNIQUE INDEX uq_bookings_deposit_identity ON bookings (
       (metadata->>'chain_id'), (metadata->>'tx_hash'), (metadata->>'txlog_seq'))
   WHERE metadata ? 'tx_hash';
   ```
   这与 I-66 自己的论证同形（"a property of the only writer there is"）：
   `IngestDeposit` 对同一条 log 永远只写一个 booking，所以这条不变量对诚实写者恒真。
2. **在 `corroborateBeforeConfirm` 里加「已入账」检查**：找到那条 log 之后，
   若存在**另一个** booking 已经持有同一 `(chain_id, tx_hash, txlog_seq)`
   （尤其是已 `confirmed` 且带 `journal_id` 的），一律 `evidenceContradicted`。
   「这笔转账已经入过账」是账本自己拥有的事实，不需要问链。
3. 顺带把 `channel_name` 纳入 `ledger_bookings_insert_guard`
   （deposit 分类的诚实写者恒为 `onchain`）——最窄，且不覆盖「伪造行是第一行」的情形，
   所以只能作为 1 或 2 的补充。

---

### N-2 · Minor · 未获链上证实的 booking 可能永远进不了 review 队列，只剩每 tick 一条 Error 日志

**位置**：`service/onchain.go` `recheckOneDeposit` 的 `evidenceContradicted` 分支——
把 `pending` 推进到 `confirming` 时用 `ChannelRef = depositChannelRef(txHash, txLogSeq)`。

**判定：CONFIRMED**（`TestR3_A2c`，日志实录）

**失效场景**：攻击者伪造的 booking 引用一笔**已被真实 booking 占用**的
`(tx_hash, txlog_seq)`（这正是「引用一笔真实转账」最自然的写法），
并把金额改大。corroboration 正确判为 contradicted，连续 3 次后走 `routeToReview`——
但它必须先 `pending → confirming`，而该 transition 的 `channel_ref` 与真实 booking
撞上 `uq_bookings_channel_ref`：

```
WARN  … the chain does not corroborate this deposit … consecutive_misses 1 threshold 3
ERROR … deposit.onchain_unverified … consecutive_misses 3
ERROR … deposit.onchain_unverified … consecutive_misses 4
ERROR … deposit.onchain_unverified … consecutive_misses 5   ← 每 tick 重复，永不停
最终：status=pending  review_reason=""  journal=""
```

即：booking 永远停在 `pending`，`review_reason` 从未落库，
`evidenceMiss` 计数从不 `clear`，`ledger_deposit_review_required_total{reason=onchain_unverified}`
也拿不到这次事件。而 I-69 的 Enforced by 明确承诺
"the refusal is a queue an operator works (docs/RUNBOOK.md §13), not a log line"
—— 在这个（最可能的）攻击形态下，它**恰好只是一条日志**。

**钱是安全的**（什么都没入账），所以判 Minor。
**修法**：`routeToReview` 之前的这一步 transition 用一个不与既有 booking 冲突的
`ChannelRef`（例如 `depositChannelRef(...)+"#unverified-"+b.UID`），
或让 `routeToReview` 的失败也计入告警而不是被 `evidenceMiss` 的重复日志淹没。
顺带：N-1 的修法 1（deposit 身份唯一索引）会让这类伪造行在 INSERT 时就被拒，
从根上消掉这个分支。

---

### N-3 · Minor · `ledger_discard_attestations_from` 只在「锚尚未发布被污染的链头」时才是回头路

**位置**：`029_insert_path_guards.up.sql:774 ledger_discard_attestations_from`；
`service.AttestationService.catchUpAnchor`；I-56（锚的 head 不得回退，且是机器校验的属性）。

**判定：CONFIRMED**（`TestR3_A7` 与 `TestR3_A7b` 两个方向都实测）

- **在发布之前**发现毒行（`TestR3_A7b`）：`discard(1)` → 下一批 attestation →
  `verify = VERIFIED`。门确实是回头路 ✅
- **在发布之后**（`TestR3_A7`，也是默认 `AttestInterval=60s` 下更可能的顺序）：
  毒行已被 job 延链并 `catchUpAnchor` 发布到 seq 2；owner `discard(1)` 成功
  （discarded=2），但随后
  ```
  verify verdict = TAMPERED
    reason: anchor knows about seq 2 but the DB chain only reaches seq 1
  ```
  而 I-28 的判定表把 `anchorSeq > maxSeqSeen` 定为 `TAMPERED`，
  I-56 又（正确地）禁止把锚的 head 往回走。于是这条门把一种永久 `TAMPERED`
  换成了另一种永久 `TAMPERED`。

**钱是安全的**，判 Minor。**修法**：要么 `discard` 时把「已丢弃到 seq N」
作为一条可信的本地事实记录下来（`anchor_observations` 已经是 owner-written、append-only，
是天然落点），让 `VerifyLedger` 把 `anchorSeq > maxSeqSeen` 与
「有一条已记录的 discard 解释了这个落差」区分开；
要么在 RUNBOOK 里明确写出「毒行必须在下一个 `AttestInterval` 之前处置，
否则本期证明不可恢复」，把它变成一条有时限的操作纪律而不是一个看起来通用的门。

---

## 4. 结构性根因的复核

原报告 §1 的根因（「guard / audit 的覆盖面只有 UPDATE」）在新 HEAD 上重新枚举：

| 触发时机 | 原报告（001–028） | 现在（001–030） |
|---|---|---|
| 仅 `UPDATE` | 24 | 43 |
| 仅 `DELETE` | 13 | 19 |
| **含 `INSERT`** | **1**（只有 journal 平衡约束） | **24**（16 仅 INSERT + 7 `IUD` + 1 `IU`） |

按表看，新增的 INSERT 期触发器：
- **guard（拒绝）**：`entry_template_lines`、`bookings`、`reservations`、`ledger_attestations`
- **`AFTER INSERT` 审计**：`account_policies`、`bookings`、`chain_cursors`、`classifications`、
  `currencies`、`deposit_addresses`、`entry_template_lines`、`entry_templates`、`events`、
  `journal_types`、`reconcile_scan_cursors`、`reservations`
- 豁免（带书面理由，`postgres.unauditedWrites`）：`balance_checkpoints`、
  `balance_snapshots`、`rollup_queue`

所以「INSERT 既不被拦也不留痕」这条根因**已经不再成立**。
**N-1 不是这条根因的复发**——它是一条被 guard 放行（形状完全合法）、
被审计如实记录（`bookings_audit_insert` 会留行）、却在**应用语义**上仍然铸币的 INSERT。
换句话说：DB 层已经能说「这一行长得像诚实写者写的」，
但没有人问「诚实写者会不会写第二次」。

---

## 5. 我没能验证的

1. **HTTP 层**：N-1 是否也能从 `POST /bookings`（`ScopeWrite` key）发起——
   该端点让调用方自选 `amount` / `classification` / `channel_name`，
   而 recheck job 不区分来源。**没测**，但若成立，N-1 的门槛会从「DB 写凭证」
   降到「写 scope 的 API key」，严重性更高。这是我认为下一轮最该优先看的一点。
2. **`chains/evm` 真实 RPC**：所有 onchain 复核仍用仓库自带的 `fakeChainReader`。
   真实 `eth_getLogs` 下 `corroborateBeforeConfirm` 的窗口（它按
   `FetchDeposits(chainID, blockNumber, blockNumber, addrs)` 单块重读）
   在归档节点缺失历史块时会返回什么，没测。
3. **`anchors/r2`**：N-3 只在 `anchordev.LocalFileAnchor` 上测过。
4. **`cmd/ledger-cli`**：只走 Go API，没跑 CLI 与它的 exit code 语义。
5. **`registeredAddresses` 的缓存行为**：`corroborateBeforeConfirm` 依赖它，
   地址注册面很大时的成本与缓存失效没测。
6. **全量 `make test`**：本轮只跑了与发现相关的包内测试子集（含 Wave 5 的新 pin），
   没跑整套。

---

## 6. 附录：复现命令

```bash
cd /Users/aaron/projects/_worktrees/ledger/r3-money-out   # branch r3-recheck-money-out @ 5a11533
docker start r3moneyout-pg
export DATABASE_URL="postgres://test:test@127.0.0.1:15877/postgres?sslmode=disable"

# 原发现逐条复核
go test ./postgres/ -run 'TestR3_A1|TestR3_A3|TestR3_A4|TestR3_A5|TestR3_A8|TestR3_A9' -count=1 -v
go test ./service/  -run 'TestR3_A2'                                                  -count=1 -v
go test .           -run 'TestR3_A6|TestR3_A7'                                        -count=1 -v

# 新发现
go test ./service/  -run 'TestR3_A2c|TestR3_A2d' -count=1 -v      # N-1 / N-2
go test .           -run 'TestR3_A7b'            -count=1 -v      # N-3

# schema 事实（触发器计数、列权限）
docker exec r3moneyout-pg psql -U test -d postgres -c "CREATE DATABASE lg2;"
go run ./cmd/r3probe "pgx5://test:test@127.0.0.1:15877/lg2?sslmode=disable"
docker exec r3moneyout-pg psql -U test -d lg2 -c "
SELECT CASE WHEN (t.tgtype & 4)>0 THEN 'INSERT' ELSE '' END ||
       CASE WHEN (t.tgtype & 16)>0 THEN ' UPDATE' ELSE '' END ||
       CASE WHEN (t.tgtype & 8)>0 THEN ' DELETE' ELSE '' END AS events, count(*)
FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
JOIN pg_namespace n ON n.oid=c.relnamespace AND n.nspname='public'
WHERE NOT t.tgisinternal GROUP BY 1 ORDER BY 2 DESC;"
```

实验文件（全部 `zz_r3_*` 前缀，**不是 pin**，只在这个 worktree）：
`postgres/zz_r3_attack_test.go`（A1/A3/A4/A5/A8/A9）、
`service/zz_r3_attack_test.go`（A2/A2b/A2c/A2d）、
`zz_r3_detect_test.go`（A6/A7/A7b）、`cmd/r3probe/`（只调 `postgres.Migrate`）。

反转实验（§2、M-3）各自用完立即 `cp` 还原；`git diff --stat` 为空，
无任何实现文件被改动。

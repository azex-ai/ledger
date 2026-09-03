# 独立复审 · money-out —— 让钱离开系统 / 让账本多认一笔钱

> 2026-09-03 · 审计员：r3-money-out（首次接触本仓库，独立于既有 audit 与整改清单）
> 只读依据：`CLAUDE.md`、`docs/INVARIANTS.md`、`docs/plans/2026-08-21-tamper-evident-ledger-design.md`
> 未读：`docs/audits/**`、`docs/plans/2026-09-02-*`
> 实验环境：worktree `_worktrees/ledger/r3-money-out`（分支 `r3-money-out`），postgres:17 容器
> `r3moneyout-pg`，schema 由 `postgres.Migrate` 全量建起；每条攻击语句都以**真实
> `ledger_app` 凭证经 TCP 连接**发出，不是容器 superuser。
> 威胁模型：**DB 写凭证 `ledger_app` 已泄露，程序端可信**（设计稿 §1 第一行）。

---

## 0. 摘要

**最要紧的一条**：拿到 `ledger_app` 后，**一条 `INSERT`** 就能让诚实的应用进程用真实
Attestor 签出一笔凭空的入账，而全套防线一致报告"正常"。实测（`TestR3_A6`）：

```
RunFullReconciliation : OverallPassed=true  FullCoverage=true  SkippedChecks=[]
ledger-cli verify     : VERIFIED            (reasons=0, uncovered=0)
SolvencyCheck         : custodial=300 liability=300 margin=0 solvent=true
holder available      : 300                 （诚实充值合计 200）
```

这不是"绕过签名"，而是**让签名去认一件假的事实**。P5/I-26 的整个论证前提是"伪造记录拿不到
私钥"；下面两条路径不需要私钥——它们改的是**应用签名之前所读的输入**，而应用会忠实地把
错误结论签下来、锚定下来、并在 `verify` 里报 `VERIFIED`。

**结构性根因（一句话）**：本 schema 里**除 journal 平衡约束外没有任何 INSERT 期触发器**；
14 个 mutation guard 全是 `BEFORE UPDATE`，11 个审计触发器全是 `AFTER UPDATE`。于是对每一张
配置/状态表，`ledger_app` 的 **INSERT 既不被拦、也不留痕**。migration 003 的文件头把这一类
攻击描述得完全准确（"它不伪造任何东西，它让应用对错误的事实签一份正确的 journal"），
然后只关上了 `UPDATE` 这半扇门。

计数：**Critical 2 · Major 4 · Minor 4**。另有 **1 条 pin 空转**（I-11 在 gated 路径无钉子，
拆掉实现后 6 条被引用的 pin 全绿，且钱真的能出去）。

---

## 1. 结构性根因：guard / audit 的覆盖面只有 UPDATE

在实跑的 schema 上枚举全部非内部触发器（`pg_trigger`）：

| 触发时机 | 数量 | 内容 |
|---|---|---|
| `BEFORE UPDATE` (row) | 14 | 全部 mutation guard：`classifications` / `reservations` / `journals` / `bookings` / `events` / `account_policies` / `currencies` / `journal_types` / `entry_templates` / `entry_template_lines` / `deposit_addresses` + 3 张 blanket |
| `BEFORE UPDATE/DELETE` (blanket) | 22 | `ledger_block_mutation()`：append-only 表族 |
| `AFTER UPDATE` (audit) | 11 | `ledger_log_config_table_change()` / `ledger_log_reconcile_scan_cursor_change()` |
| **`AFTER/BEFORE INSERT`** | **1** | 只有 `trg_check_journal_currency_balance`（per-currency 借贷平衡，`journal_entries`） |

实测命令与输出见 §6 附录 A。推论，逐条实测过：

- **INSERT 不被拦**：`entry_template_lines`、`bookings`、`account_policies`、`reservations`、
  `journals`、`journal_entries`、`events`、`ledger_attestations`、`entry_attestations`、
  `reservation_*_receipts/legs`、`rollup_queue`、`balance_checkpoints` 全部对 `ledger_app`
  开放 INSERT，且无一条 INSERT 期检查（除 journal 平衡）。
- **INSERT 不留痕**：`config_table_changes` 是 `AFTER UPDATE` 写入的。`TestR3_A3` 里两次
  成功的配置表 append（`account_policies` + `entry_template_lines`）之后
  `count(config_table_changes)` **仍为 0**。
- 因此 **I-58 的第 1/2 条规则（"guard 放行的改动会被记录 / 记录不可被它所记录的角色写"）
  在语义上只覆盖 UPDATE**；I-58 正文与 pin（`TestPartialGuardTablesAreAudited`、
  `TestAccountPolicyEnforcementKnobChangeIsAudited`）都只谈 `AFTER UPDATE`。

---

## 2. 算式 × 项 × 可写性

对每一个"决定放多少钱 / 认多少钱"的算式，逐项回答：读哪张表哪些列、`ledger_app` 能否写、
能写会不会让结果偏高、签名在这一项上是否生效。

### 2.1 `Reserve`（gated，`RequireVerifiedBalance=true`）—— 库文档指定的提现闸
`postgres/reserver_store.go:541` `availableBase = min(V, E)`；`:580` `available = base − held`

| 项 | 来源 | `ledger_app` 可写？ | 偏高路径 | 签名覆盖 |
|---|---|---|---|---|
| V = Σ `VerifiedBalance(cls)` | `journal_entries` + 每条贡献 journal 的 `VerifyJournalAuth` | entries 可 INSERT（须平衡） | 伪造未签名 entries → V 直接 `ErrUnauthorizedJournal`（fail-closed）✅ | ✅ 每条 journal 逐笔验签 |
| V 的分类枚举 | `ListComputedBalancesForHolders` 的 `populated` CTE（`DISTINCT journal_entries`） | entries 可 INSERT | 只能"多算未授权分类" → 被 V 拒 ✅ | — |
| V 的角色过滤 | `classifications.balance_role` | UPDATE 受 guard（仅 `''→role`，且 `''→available` 有 entries 时被拒）；**INSERT 自由** | 新建分类无 entries，不进枚举 ✅ | — · ⚠️ **该过滤器本身无 pin，见 M-3** |
| E = Σ `RecomputeCheckpointFromEntries` | `journal_entries` + `classifications.normal_side` | `normal_side` UPDATE 被 guard 拒；entries 可 INSERT | E 会算进伪造 entries，但 `min(V,E)` 由 V 兜住 ✅ | ❌ E 无验签（设计如此） |
| held（signed 路径 I-65） | `reservations.reserved_amount`/`expires_at`（UPDATE 全被 guard 拒）− 已验签 discharge claim | claim 表可 INSERT | 伪造/无签名 claim → discharge 记 0 ✅（`TestReserve_SignedDischarge_*` 实证） | ✅ `CanonicalReservationDischargeDigest` |
| held（fallback I-49） | `SumUnexpiredReservationHolds` = Σ `reserved_amount` where `expires_at > now()` | `reserved_amount > 0` CHECK；`expires_at` 不可改 | 无法调低（负额被 CHECK 拒）✅ | — |
| account policy 闸 | `account_policies`（classification_id=0 档） | UPDATE 受 guard 但白名单含 3 个开关；**INSERT 自由** | 冻结/透支下限可被更具体的 INSERT 行覆盖（对 Reserve 需同档，受唯一键限制）· 见 M-1 | ❌ |
| **模板内容** | `entry_templates` + `entry_template_lines` | **INSERT 自由** | ⛔ **C-1：追加一条 line 就让每笔诚实充值多认 N 倍，且带真签名 → V 接受** | ⚠️ 签名覆盖"渲染结果"，不覆盖"模板定义" |

### 2.2 `Reserve`（ungated，默认）
`availableBase` = `sumBalancesByRoleWithQueries`（`checkpoint + delta`）；held = `SumActiveReservations`
（读 `status` / `settled_amount`）。两者都在 `ledger_app` 可写面内 —— 这是**已披露**的
（I-49 Why、I-11「How that sum is computed is not one query」）。本报告不重复记为缺陷，但提醒：
**gate 是 per-call opt-in，库不强制**，所以"默认配置下没有提现闸"仍然是消费方最容易踩的
一步（I-32 已声明 mechanism-not-policy）。

### 2.3 `ConfirmPending` —— 唯一的"把 pending 变成可花的钱"原语
`postgres/pending_store.go:439` `bal = min(V, E)`

| 项 | 来源 | 可写？ | 偏高路径 | 签名 |
|---|---|---|---|---|
| V | `VerifiedBalance(pending 维度)` | — | 伪造 pending entries → 整笔拒 ✅（I-64 step 2） | ✅ |
| E | `pendingBalanceFromEntries`（锁内纯 SQL） | entries 可 INSERT | 由 V 兜住 ✅ | ❌ |
| pending 的来源 | `deposit_pending` / `deposit_confirm_pending` 模板 | **模板 line 可 INSERT** | ⛔ **C-1 的同一条路**：被放大的 pending credit 是**真签名**的，V 会接受 | ⚠️ |
| 挂钩条件 | `ledger.New` 仅在配置了 `Attestor` 时接线 V | — | 未配 Attestor ⇒ 仅 E ⇒ 伪造 entries 通过（I-64 已披露） | — |

### 2.4 入账确认（`service.Onchain`）—— 库自己决定金额的唯一一处
`service/onchain.go:1769 recheckOneDeposit` → `:1020 advanceConfirmation` → `:1143 reviewGate`
→ `:1269 postDepositConfirmedJournal`

| 项 | 来源 | 可写？ | 偏高路径 | 签名 |
|---|---|---|---|---|
| 入账金额 | **`bookings.amount`** | UPDATE 被 guard 拒；**INSERT 自由** | ⛔ **C-2：伪造一行 booking，金额由攻击者定** | ⚠️ 签的是 booking 说的金额 |
| 确认数 | `latest − bookings.metadata.block_number + 1` | `metadata` **UPDATE 白名单内 + INSERT 自由** | 置 `block_number=1` 即"早已确认"，`TxIncluded` 分支根本不跑 | ❌ |
| 链上存在性 | `Reader.TxIncluded` | — | **只在 `confirmations < threshold` 时调用**（`onchain.go:1794`）；伪造行直接跳过 | ❌ |
| token 是否在册 | `metadata.token` ∈ `cfg.CreditTokens`（进程内配置） | metadata 可写 | 填公开的 token 地址即可 | ❌ |
| `AutoCreditCeiling` | 进程内配置（I-62 强制必填） | 不可写 | **只限单笔**；多插 N 行即绕过 | ❌ |
| 第二数据源 | `DepositConfirmer` + `ReconcileCeiling` | 不可写 | **可选**，`ReconcileCeiling` 默认 0 = 关闭，且只对**大于 ceiling** 的金额生效 | ❌ |
| 收款人 | `deposit_addresses.account_holder` | UPDATE 全列不可改；INSERT 受 `uq_(address)` / `uq_(account_holder)` 双唯一键约束 | 无法重指向既有地址 ✅（003 已修） | — |

### 2.5 冲正 / 冲正余量
`postgres/reversal_fraction_store.go:527 cumulativeReversedByDimension`

| 项 | 来源 | 可写？ | 偏高路径 | 签名 |
|---|---|---|---|---|
| 原 journal 的 entries | `journal_entries`（append-only, no UPDATE） | 不可改 | — | ✅ |
| 已冲正额 | **所有 `reversal_of = J` 的 journal 的全部 entries** | **journals + entries 可 INSERT** | ⛔ **M-2：伪造一条净额为 0 的"冲正"，`ReverseJournalFraction(J,1,1)` 少冲且返回 nil** | ❌ 该集合不验签 |
| 校验 | `validateReversalOfInput`（`:596`） | — | 只挂在 `postJournalWithQueries`，raw SQL 完全绕开 | — |

### 2.6 余额读面 / 偿付能力
- `GetBalance` / `GetBalanceBreakdown` / `min_balance` / `system_rollups` / `ExpirePendingOlderThan`
  前置过滤：读 `balance_checkpoints`（可写、无触发器）—— **已披露**（I-23 / I-49 / I-17 信任边界）。
- `SolvencyCheck`：两侧都从 entries 重算，实测拆掉后 pin 变红（§5 R6）✅。
  但对 C-1/C-2 **无效**：负债与托管资产同量增加，`margin=0 solvent=true`（实测）。

---

## 3. 发现

### C-1 · Critical · 追加一行模板 line ⇒ 诚实应用签出无限量凭空入账

**位置**
- `postgres/sql/migrations/003_config_table_guards.up.sql:96-97,145` —— guard 是
  `BEFORE UPDATE`，并 `REVOKE UPDATE`；**INSERT 权限保留**（应用安装模板时需要）。
- `core/template.go:119-172` `EntryTemplate.Render` —— 每次调用重读 lines，逐 line 生成 entry。
- `presets/templates.go:141-148` —— 出厂 `deposit_confirm` 只有两条 line，`amount_key` 都是
  `"amount"`；追加的 line 只要复用同一个 key，`Render` 完全察觉不到。
- `postgres/ledger_store.go:571 ExecuteTemplate` → `PostJournal` → `attestJournal`（真实签名）。

**判定：CONFIRMED**（`TestR3_A1`、`TestR3_A6`，均以真实 `ledger_app` 凭证发语句）

**失效场景**
1. 诚实充值 100 → `VerifiedBalance(main_wallet) = 100`。
2. 攻击者一条语句：
   ```sql
   INSERT INTO entry_template_lines (template_id, classification_id, entry_type, holder_role, amount_key, sort_order)
   VALUES (<deposit_confirm>, <main_wallet>, 'debit',  'user',   'amount', 98),
          (<deposit_confirm>, <custodial>,   'credit', 'system', 'amount', 99);
   ```
   —— 被接受（`REVOKE UPDATE` 不影响 INSERT，guard 是 `BEFORE UPDATE`），
   **`config_table_changes` 无任何记录**。
3. 应用代码一字不改，下一笔 100 的充值：`total_debit=200`，`auth_status=signed`。
4. `VerifiedBalance(main_wallet) = 300`（诚实合计 200）。
5. **gated `Reserve(300)` 返回 `nil`** —— 钱可以走了。
6. 检测面：`RunFullReconciliation` = `OverallPassed=true, FullCoverage=true, SkippedChecks=[]`；
   `verify` = **VERIFIED**；`SolvencyCheck` = `solvent=true, margin=0`。

**最小复现**：`postgres/zz_r3_attack_test.go:TestR3_A1_TemplateLineAppendMintsSignedMoney`
（+ 检测面 `zz_r3_detect_test.go:TestR3_A6_NothingDetectsTheTemplateLineMint`）。
放大倍数只受"追加几对 line"限制，无上限。

**为什么现有机制没拦住**
- I-26/P5 的签名认证的是 **`JournalInput` 的渲染结果**，不是"模板定义"。攻击不需要私钥。
- I-22 的配置表小节承认"信誉良好的签名 + 错误事实"这一类，但落地只做了**列级不可 UPDATE**；
  003 的文件头把攻击写成 `UPDATE deposit_addresses SET account_holder = ...`，于是修法也停在
  UPDATE。**INSERT 从未被讨论过。**
- I-58 的"被放行的改动会被记录"是 `AFTER UPDATE`，INSERT 无痕。
- I-12（钱守恒）成立：伪造 leg 是配平的。I-24（per-journal 平衡）成立。I-37（偿付能力）
  两边同量增长 → `margin` 不变。这正是设计稿 §2 M5 那句"solvency 两边同量增加，内部 margin 不变"
  的现实版，只是这次它**带着有效签名**。

**修法（按代价排序）**
1. **`entry_templates` 加 set-once 的 `lines_digest`**：`CreateTemplate` 在同一事务里写入
   lines 的 canonical 摘要，列纳入 UPDATE guard 的不可改集合；`renderTemplate` 每次重算 lines
   摘要并比对，不符即 `ErrConflict`。追加一行立刻破摘要——这是唯一能**阻止**而非仅记录的做法，
   且与既有 `to_jsonb` guard 风格一致。
2. **INSERT 期 guard**：`entry_template_lines` 加 `BEFORE INSERT` 触发器，拒绝向已存在
   `entry_template_lines` 行的 template 追加（安装是一次性写入）；或把模板安装收进
   `SECURITY DEFINER` 函数、对 `ledger_app` `REVOKE INSERT`（与 I-35 分区函数同一形状）。
3. **补检测**：把 `config_table_changes` 的触发器扩到 `AFTER INSERT OR UPDATE`（见 M-1）。

---

### C-2 · Critical · 追加一行 `bookings` ⇒ recheck job 自动签出凭空入账

**位置**
- `service/onchain.go:1769-1848 recheckOneDeposit` —— `confirmations = latest − metadata.block_number + 1`；
  `:1794` 的 `TxIncluded` 存在性检查**只在 `confirmations < cfg.Confirmations` 分支里**。
- `service/onchain.go:1020-1069 advanceConfirmation` —— 通过 `reviewGate` 后直接
  `postDepositConfirmedJournal`。
- `service/onchain.go:1269-1305` —— 入账金额取 `booking.Amount`，即 `bookings.amount` 列。
- `postgres/sql/migrations/006_threat_model_guard_coverage.up.sql` —— `ledger_bookings_guard`
  是 `BEFORE UPDATE`（`amount` insert 后不可改），**INSERT 无约束**。

**判定：CONFIRMED**（`TestR3_A2`，真实 `ledger_app` 凭证）

**失效场景**（一条语句，无任何链上事件）
```sql
INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
                      channel_name, channel_ref, idempotency_key, metadata, uid)
VALUES (<deposit>, <victim/attacker holder>, <currency>, 999, 'confirming',
        'onchain', '0xforged#0', 'deposit-7-0xforged-0',
        '{"chain_id":"7","tx_hash":"0xforged","txlog_seq":"0","token":"<公开 token 地址>","block_number":"1"}',
        gen_random_uuid());
```
诚实的 `RunPendingRecheckOnce` 之后实测：
```
status=confirmed  journal_uid=01a0673b-…  auth_status=signed
VerifiedBalance(main_wallet) = 999          （从未有过真实充值）
SolvencyCheck: custodial=999 liability=999 margin=0 solvent=true
```

**边界与放大**
- `AutoCreditCeiling`（I-62 强制必填）只封**单笔**上限；插 N 行即得 N × ceiling，无总量约束。
- **存在有效缓解**：配好 `WithDepositConfirmer` 且把 `ReconcileCeiling` 设成尘埃值时，同一攻击
  被路由到 `review`，`review_reason=reconcile_mismatch`（实测 `TestR3_A2b`）。
  但 `DepositConfirmer` 是**可选**依赖，`ReconcileCeiling` 默认 0 = 关闭，
  且 `core.TokenConfig.ReconcileCeiling` 的文档明写"留 0 是正当选择"。
  **出厂默认是脆弱的**：I-34 只给 `ReconcileFailureLimit` 设了启动 fence，
  I-62 只给 `AutoCreditCeiling` 设了 fence，**没有任何 fence 要求存在第二数据源**。

**为什么现有机制没拦住**
- I-21 的论证是"`review` 存在是因为单源 confirmed 是这条路的全部信任边界"；但该边界只覆盖
  **sighting 来源**，不覆盖 **booking 行本身**。攻击者不需要伪造 sighting，直接伪造 booking。
- I-25 列了 `bookings` 的不可改列（含 `amount`），但那是 UPDATE 语义；
  `bookings` 的 INSERT 是应用的正常动作，从未被区分。
- I-3 的幂等（`uq_bookings_idempotency`）只保证同 key 不重复，不保证 key 对应真实链上事件。
- 签名（I-26）忠实地签了 booking 声称的金额。

**修法**
1. **让第二数据源成为 auto-credit 的前置条件**（最小改动）：`Onchain.Run` 启动校验里，
   若某 token 的 `AutoCreditCeiling > 0` 而 `DepositConfirmer == nil || ReconcileCeiling <= 0`，
   拒绝启动（与 I-62/I-34 已有的两条 fence 同形状）。并把 `ReconcileCeiling` 的语义
   从"超过才查"改为"低于才免查，0 = 全查"——现在的方向让"全部核对"只能靠填尘埃值实现。
2. **给 booking 加签名声明**（更彻底，沿用 I-65 的论证）：`CreateBooking` 时对
   `(uid, holder, currency, amount, chain_id, tx_hash, txlog_seq)` 签名并落三列；
   `advanceConfirmation` 在开事务前验签，验不过一律 `review`。I-49 那句"只有两种信号能
   逃出这个威胁模型：时间与签名"在这里同样适用，而这条路径目前一个都没用。
3. `recheckOneDeposit` 无条件调一次 `TxIncluded`（而不只在低确认分支），至少让"链上根本没有
   这笔 tx"的伪造行被挡住——代价是每 tick 多一次 RPC。**注意这不是完整修法**：攻击者可以引用
   一笔真实存在的 tx hash 并自定 `amount`。

---

### M-1 · Major · 配置/状态表的 INSERT 既不被 guard 拦、也不留任何审计痕迹

**位置**：`003_config_table_guards.up.sql:85-101`（6 张表，全 `BEFORE UPDATE`）、
`006_threat_model_guard_coverage.up.sql`（7 张表，同）、
`020_audit_trail_integrity_and_coverage.up.sql:204`（审计触发器，`AFTER UPDATE ON %I`）。

**判定：CONFIRMED**（`TestR3_A3`）

**失效场景（除 C-1/C-2 之外的独立实例）**：应用通过 `SetPolicy` 正当冻结了某账户
（`(holder, currency, 0)` 档 `status=frozen`）。攻击者 append 一条**更具体**的档位行：
```sql
INSERT INTO account_policies (uid, account_holder, currency_id, classification_id,
                              status, min_balance, enforce_min_balance, note)
VALUES (gen_random_uuid(), <holder>, <currency>, <main_wallet>, 'active', -1000000, true, '');
```
`GetEffectiveAccountPolicy`（`postgres/sql/queries/account_policies.sql:44-55`）按具体度排序取
第一条 ⇒ 该维度上的冻结与透支下限双双失效。实测该行确实成为 effective policy，
且 **`config_table_changes` 行数不变（0 → 0）**。

**为什么现有机制没拦住**
- I-17 已把 policy 三个开关声明为"业务规则，非防篡改控制"，**prevention 出局是既有裁决**，
  本条不复议。
- 但 I-58 的补偿是"**看得见**"：`TestAccountPolicyEnforcementKnobChangeIsAudited` 钉的是
  `UPDATE` 之后有审计行。**同等效果的 INSERT 变体零痕迹**，所以 I-58 §Why 里那句
  "现在它被看见了"对这条路径不成立。
- I-58 规则 1 的"覆盖面是推导出来的，不是列举的"用的推导谓词是
  `pg_trigger` 里的 `BEFORE UPDATE` row trigger —— 谓词本身就把 INSERT 排除在外了，
  所以那条自我强制的 gate 永远不会因为漏了 INSERT 而变红。

**修法**：把 `ledger_log_config_table_change()` 的触发器改成
`AFTER INSERT OR UPDATE`（`OLD` 为 NULL 时 `old_state` 记 `'null'::jsonb`），并把
`TestPartialGuardTablesAreAudited` 的推导谓词从"带 partial guard 的表"扩成
"任何 `ledger_app` 持 `INSERT` 且参与钱/风控决策的表"。这是把 I-58 的三条规则从
UPDATE 语义提升到"任何改动"语义。

---

### M-2 · Major · 伪造 `reversal_of` 链接 ⇒ "全额冲正"少冲，且返回成功

**位置**
- `postgres/reversal_fraction_store.go:527-544 cumulativeReversedByDimension` ——
  把**任何** `reversal_of = J` 的 journal 的全部 entries 当作冲正历史累加，不验签、不校验方向。
- `postgres/reversal_fraction_store.go:596 validateReversalOfInput` —— I-51 的规则 1-3
  只在 `postJournalWithQueries` 上执行；raw SQL INSERT 不经过它。
- `journals.reversal_of` 列对 `ledger_app` 开放 INSERT。

**判定：CONFIRMED**（`TestR3_A5`）

**失效场景**：真实签名充值 100（journal J）。攻击者 append 一条 `reversal_of = J` 的 journal，
四条 leg 在每个维度上净额为 0（`CR main_wallet 50` / `DR main_wallet 50` /
`DR custodial 50` / `CR custodial 50`）——不动任何余额，但把 `main_wallet/debit` 维度上的
"已冲正额"抬到 50。随后平台执行全额追回：

```
entries-only balance after the forged link: 100   （确实没动钱）
ReverseJournalFraction(J,1,1) -> err=<nil>
reversal journal total_debit = 50                 （全额冲正 100 应为 100）
balance after "reverse everything remaining": 50  （应为 0）
```

**为什么现有机制没拦住**
- I-2 明说其下界（`num==den` 后净额为 0）"成立的前提是每条 `reversal_of` 链接的 journal
  真的是它所指向者的冲正 —— 那是 I-51"。而 I-51 的执行点只在应用写路径，
  **在本仓库的标准威胁模型（DB 写凭证）下不成立**，两条不变式都没有声明这一点。
- 16 条对账检查里没有任何一条校验冲正链的形状（`journal_dr_cr` 只看 per-journal 平衡，
  `accounting_equation` 只看全局等式）。这次少冲的 50 让全部检查保持绿色。

**缓解现状（诚实说明）**：伪造 journal 未签名，所以该维度的 `VerifiedBalance` 变
UNDEFINED —— **gated 提现会被拒**，钱走不出去。所以直接损失是"平台以为追回了、实际没追回"
（对平台不利的方向）而非"用户多提"。在 ungated 路径（默认）上则是真的能提。

**修法**：新增对账检查 `reversal_chain_integrity` —— fleet-wide 扫
`reversal_of` 非空的 journal，逐条跑 `validateReversalOfInput` 的三条规则
（被指向者不是冲正 / 同维度反向 / 不超余量），任何违反即 finding。
`ReverseJournalFraction` 的 `num==den` 分支另可在计算余量时**只累加带有效签名的冲正**，
与 I-65 同一逻辑。

---

### M-3 · Major（pin 空转）· I-11 的 available-role 基数在 gated 路径完全无钉子

**位置**：`postgres/reserver_store.go:333`（V 侧）与 `:377`（E 侧）——两处
`if core.BalanceRole(row.BalanceRole) != core.BalanceRoleAvailable { continue }`。

**判定：CONFIRMED**（反转实验，`TestR3_A4` + I-11 全部被引 pin）

**实验**：把上述**两处**过滤器同时停掉（`if false && …`），然后：

| | 结果 |
|---|---|
| 过滤器在：gated `Reserve(900)` 对一个只有 `pending` 余额 1000 的 holder | `insufficient balance`（正确） |
| 过滤器拆掉：同一调用 | **`err=<nil>` —— 未确认的充值变成可提现** |
| I-11 被引用的 6 条 pin（`TestReserverStore_Reserve_Concurrent_RejectsOverCommit`、`TestReserverStore_SettlePartial_RemainderStillHeld`、`TestGetBalanceBreakdown_RolesPlusHolds`、`TestReserve_AvailableBasisExcludesPendingLockedAndRoleless`、`TestReserve_PendingOnlyBalanceNotReservable`、`TestInstallPresets_BalanceRoleUpgradeAndConflict`） | **全部通过（ok）** |

**为什么**：I-11 那两条名字最贴切的 pin（`..._ExcludesPendingLockedAndRoleless`、
`..._PendingOnlyBalanceNotReservable`）走的是**ungated** `Reserve`，读
`sumBalancesByRoleWithQueries`；I-49 把 gated 路径换成了两个**全新**的求和函数，
但没有把角色基数的钉子跟着搬过去。于是 I-11「availability base 只含 `role=available`」这条
规则，在 I-49 指定为"决定多少钱可以离开"的那条路径上是**未被机器校验的**。

**修法**：给 gated 路径补一条 pin —— 同一 fixture 下 `RequireVerifiedBalance: true`
必须对 `pending`/`locked`/`memo`/role-less 余额返回 `ErrInsufficientBalance`，
并在 I-11 的 Pinned by 里标明它钉的是哪条路径。

---

### M-4 · Major · 检测信号被倒置：签名铸币报 VERIFIED，一条无害 append 报永久 TAMPERED

**位置**：`ledger_attestations` 对 `ledger_app` 开放 `INSERT`（无 UPDATE/DELETE），
`seq` UNIQUE，且 `ledger_attestations_no_update/_no_delete` 用 `ledger_block_mutation()`
对**所有角色**（含 `ledger_owner`）拒绝 DELETE。

**判定：CONFIRMED**（`TestR3_A7`）

**实测**：以 `ledger_app` append 一条 `seq=1` 的垃圾 attestation 行之后
```
honest RunAttestBatch -> err=<nil>          （诚实 job 继续从 seq 2 追加，prev_root 链到毒行）
verify verdict = TAMPERED
  invalid signature at seq 1 / batch_digest mismatch / root_hash does not match its own stored fields
DELETE as the migration credential -> ERROR: ledger: DELETE on ledger_attestations is not allowed (23514)
```
即：**一条 INSERT 就把 `verify` 永久钉在 TAMPERED**，连迁移凭证都删不掉
（要恢复必须以 owner 身份 `DISABLE TRIGGER`，属 DDL 级手术）。

**为什么这是缺陷而不是"检测正常工作"**：把它与 C-1/C-2 并读——
真正的铸币报 `VERIFIED`，一次零收益的骚扰报 `TAMPERED` 且不可清除。
这让 `verify` 的告警**在两个方向上都不可用**：
既会漏（签名铸币），又会被一条语句永久钉住误报（此后 operator 只能忽略它）。
I-27/I-28 讨论过"attestation 只存在于 DB 中时 owner 可截断"，
但没有讨论"**append** 一条无效 attestation"这个方向。

**修法**
1. `RunAttestBatch` 在追加 seq N+1 之前，先验证 seq N 的签名（`prev_root` 的来源必须是
   一条**验得过**的行）；验不过就拒绝延链并告警，而不是把毒行接进链里。
2. 给 `ledger_attestations` 加 `BEFORE INSERT` guard：`seq` 必须等于
   `max(seq)+1` **且** 该行签名可验（或干脆把插入收进 `SECURITY DEFINER` 函数、
   对 `ledger_app` `REVOKE INSERT`——写 attestation 的只有一个 job，
   与 I-35 分区函数同一形状）。
3. 提供一条 owner-only、写审计的"隔离毒行"过程（同 migration 027
   `ledger_unlink_event_journal` 的形状），否则这是不可逆的。

---

### m-1 · Minor · 一条 `rollup_queue` INSERT 可长期堵住 I-23 唯一的修复路径

`RebuildCheckpoint` 在 `CountPendingRollupForDimension > 0` 时返回 `ErrRollupPending`
（`postgres/sql/queries/integrity_checkpoint.sql:34-46`）。攻击者可 append 一行
`processed_at IS NULL, failed_attempts = 0, claimed_until = '2099-01-01'`：
`DequeueRollupBatch` 因 `claimed_until >= now()` 永不认领 ⇒ 永远 pending ⇒
被投毒的 checkpoint **无法用受信任入口修复**。
`failed_attempts >= 10` 的同类洞已被修过（该查询注释自己写着），这个变体没有。
可由 operator 用 `ResetRollupClaim` / owner DELETE 化解，故 Minor。
**判定：PLAUSIBLE**（依据查询谓词逐条比对，未实跑）。
修法：`CountPendingRollupForDimension` 忽略 `claimed_until` 明显越界的行，
或给 `rollup_queue.claimed_until` 加上界 CHECK。

### m-2 · Minor · `journal_entries.effective_at` 可与父 journal 不一致，且能绕过 `period_close_violations`

`ledger_app` 的 `journal_entries` INSERT 列权限包含 `effective_at`（实测），
而 I-14 声称"entry 的 `effective_at` 永远等于父 journal 的"——**无对账检查校验这条**
（16 条检查里没有任何一条比较两者）。
`PeriodCloseViolations`（`postgres/sql/queries/periods.sql:98-109`）只比较
`journals.effective_at` 与关账线，所以一条"journal 的 `effective_at` 在关账线之后、
entries 的 `effective_at` 在关账线之前"的伪造记录不会被报告，
而 `ListBalancesAt` / trial balance / `balance_snapshots` 读的是 **entry** 的 `effective_at`。
**判定**：CONFIRMED（权限与两条查询的读取列，均已核对原文）/ PLAUSIBLE（as-of 报表的实际错配未实测）。
修法：新增 `entry_effective_at_consistency` 对账检查（`je.effective_at <> j.effective_at`）；
`PeriodCloseViolations` 同时看 entries。

### m-3 · Minor · 一条 `journal_entries` INSERT 可让某 holder 的 gated 提现永久 UNDEFINED

在 `TestR3_A5` 中已实测：伪造 journal 一旦在某 `available` 维度留下 entry，
`VerifiedBalance` 永久返回 `ErrUnauthorizedJournal`（journals append-only，无 unlink），
该 holder 的 gated 提现从此全部被拒。这是 fail-closed 侧，方向正确，
但与 I-26 的 disclosed residual（`unsigned_tx_mode` 单向、无 remediation API，选项 (a)(b)(c) 记录在案）
是同一族问题，**主体是 entries 而不是 journal 的 auth 列**，I-26 未涵盖。
一条 INSERT 即可造成对单个 holder 的永久拒付。**判定：CONFIRMED**。
修法：与 I-26 选项 (b) 一并考虑——一张 append-only 的
`journal_quarantine(journal_id, reason, actor)`（owner-only 写入 + 审计），
让 `VerifiedBalance` 把已隔离的伪造 journal 排除在"贡献者"集合之外，
而不是永久把维度判为 UNDEFINED。

### m-4 · Minor（仅登记，已披露）· `ledger_app` 仍可读 `webhook_subscribers.secret`

实测列权限：`ledger_app` 对 `secret` 持 `SELECT`（`ledger_ro` 已被 007 摘掉）。
设计稿 §1 的威胁表第 5 行（D-M6）已把这条记为 expand 阶段未收口项，
`core.WebhookSigner` 的 contract 阶段（`REVOKE` / `DROP` 该列）留给已迁移密钥的部署。
**不作为新发现**，仅确认它在当前 schema 上仍然成立。

---

## 4. 与钱相关的不变式：pin 是否真的会红

按任务要求，在 worktree 里**真的把被测实现拆掉一次**，跑该不变式引用的 pin，实验后立即恢复
（`git status` 已确认所有实现文件干净，只余我新增的实验文件）。

| 不变式 | 反转的实现 | 跑的 pin | 结果 |
|---|---|---|---|
| I-49 `min(V,E)` | `reserver_store.go:541` → 只用 V | `TestReserve_RequireVerifiedBalance_*` | ✅ **红** `..._RechecksUnderLock`："Reserve must re-derive the available base under the balance lock" |
| I-49 保守 hold | `:556-566` 两个 gated 分支停掉 → 回落 `SumActiveReservations` | `TestReserve_RequireVerifiedBalance_Hold*`、`TestReserve_SignedDischarge_*` | ✅ **红** 4 个测试 / 6 个子测试（含 `HoldSurvivesStatusTamper/released_outright`、`NoAttestorKeepsConservativeHold`） |
| I-65 discharge 验签 | `reserver_discharge_auth.go:271` 去掉 `VerifyReservationDischargeAuth` | `TestReserve_SignedDischarge_ForgedClaimDischargesNothing` | ✅ **红** 全 3 个子测试（unsigned / garbage / 跨 reservation 重放签名） |
| I-64 `min(V,E)` | `pending_store.go:439` → 只用 E | `TestConfirmPending_*` | ✅ **红** `..._VerifiedBaseCapsEntriesForgedInTheWindow`。⚠️ `..._RefusesForgedPendingEntries` **仍绿**——它被 V 的授权分支拦下，不测金额上限；两条 pin 的分工在 I-64 正文里没写清 |
| I-32 / I-33 缓存判决 | `verified_balance_store.go:166` → 缓存 `authorized` 跳过实时验签 | `TestVerifiedBalance_*` | ✅ **红** `..._CachedAuthorizedVerdictDoesNotSkipTheLiveCheck`、`..._RefusesTamperedEntryAmount` |
| I-37 偿付能力只读 entries | `sqlcgen` 里 `GetSystemSideCustodialBalance` 改读 `balance_checkpoints` | `TestSolvencyCheck_*` | ✅ **红** `..._IgnoresTamperedCheckpoints`、`..._WithdrawFee_DoesNotManufactureDeficit` |
| **I-11 available 基数（gated 路径）** | `reserver_store.go:333` + `:377` 两处角色过滤 | I-11 全部 6 条被引 pin | ❌ **全绿**，而钱真的出去了 → **M-3** |

结论：I-49 / I-65 / I-64 / I-32 / I-33 / I-37 的钉子是**载重的**（拆掉即红，且红在正确的语句上）。
I-11 在 gated 路径上是**空转的**。

---

## 5. 我没能验证的

1. **`ledger_owner` / superuser 档次**：本报告只做 `ledger_app`。owner 能 `DISABLE TRIGGER` /
   `DROP`，设计稿 §1 已单列一行，我没有独立复核它的那些结论。
2. **`chains/evm` 的 e2e**（`//go:build e2e`）与真实 RPC 行为：C-2 用的是仓库自带的
   `fakeChainReader`。真实 watcher 是否会因为伪造 booking 的 `tx_hash` 不存在而在别处报错、
   从而在生产里更早暴露，我没有测。
3. **HTTP 层（`server/`）的 scope/capability 面**：I-38/I-34 的 API key 语义我只读了不变式，
   没有跑攻击。C-2 若能从 `POST /bookings` 发起（`ScopeWrite`），严重性会从"DB 凭证"降到
   "写 scope key"——**这一点我没验证**，但值得下一轮专门看：`POST /bookings` 允许消费方
   自定 `amount` + `classification`，而 recheck job 的信任模型不区分来源。
4. **`anchors/r2` 真实载体**：M-4 的毒丸只在 `anchordev.LocalFileAnchor` 上测过。
   R2 + Object Lock 下 `verify` 的判定可能不同（锚不会被污染，但 DB 侧毒行仍在）。
5. **C-1/C-2 在 `cmd/ledger-cli` 各子命令下的表现**：我只走了 `service.VerifyLedger` 与
   `RunFullReconciliation` 的 Go API，没跑 CLI（含它的 exit code 语义）。
6. **m-1 未实跑**（仅按 SQL 谓词推导）；**m-2 的 as-of 报表错配未实跑**。
7. **性能回归**：本轮所有修法建议（模板摘要、booking 签名、`AFTER INSERT` 审计）
   我都没有估算 `PostJournal` / `RunAttestBatch` 的成本影响。
8. **`dev_credit` 预设**：`svc.InstallDevCreditPreset` 之后是否存在从 `ledger_app`
   可达的铸币路径，我只做了静态判断（需要消费方显式安装 + 显式调用），未实测。

---

## 6. 附录 A —— 复现环境与命令

```bash
docker run -d --name r3moneyout-pg -e POSTGRES_PASSWORD=test -e POSTGRES_USER=test \
  -e POSTGRES_DB=postgres -p 15877:5432 postgres:17
docker exec r3moneyout-pg psql -U test -d postgres -c "CREATE DATABASE lg;"
go run ./cmd/r3probe "pgx5://test:test@127.0.0.1:15877/lg?sslmode=disable"   # = postgres.Migrate
docker exec r3moneyout-pg psql -U test -d lg -c "ALTER ROLE ledger_app PASSWORD 'app';"

# 触发器全表（§1 的依据）
docker exec r3moneyout-pg psql -U test -d lg -c "
SELECT c.relname, t.tgname, pg_get_triggerdef(t.oid)
FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
JOIN pg_namespace n ON n.oid=c.relnamespace AND n.nspname='public'
WHERE NOT t.tgisinternal ORDER BY 1,2;"

# 只保留 INSERT 期触发器 —— 结果只有 trg_check_journal_currency_balance
docker exec r3moneyout-pg psql -U test -d lg -c "
SELECT c.relname, t.tgname FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
JOIN pg_namespace n ON n.oid=c.relnamespace AND n.nspname='public'
WHERE NOT t.tgisinternal AND (t.tgtype & 4) > 0;"

# 攻击实验（worktree，非主仓；均以真实 ledger_app 凭证发语句）
cd /Users/aaron/projects/_worktrees/ledger/r3-money-out
export DATABASE_URL="postgres://test:test@127.0.0.1:15877/postgres?sslmode=disable"
go test ./postgres/ -run 'TestR3_A1|TestR3_A3|TestR3_A4|TestR3_A5' -count=1 -v
go test ./service/  -run 'TestR3_A2'                                -count=1 -v
go test .           -run 'TestR3_A6|TestR3_A7'                      -count=1 -v
```

实验文件（**只在 worktree，未进主仓**，全部是 `zz_r3_*` 前缀，非 pin）：
- `postgres/zz_r3_attack_test.go` —— A1 模板 line / A3 INSERT 无 guard 无审计 / A4 gated 角色基数 / A5 伪造冲正链接
- `service/zz_r3_attack_test.go` —— A2 伪造 booking / A2b 配了第二数据源的对照组
- `zz_r3_detect_test.go` —— A6 检测面全绿 / A7 attestation 毒丸
- `cmd/r3probe/` —— 只调 `postgres.Migrate` 的一次性 helper

所有反转实验（§4）用完立即 `cp` 还原；worktree `git status` 只余上述新增文件，
无任何实现文件被改动。主仓 `/Users/aaron/projects/ledger` 除本文件外未写入。

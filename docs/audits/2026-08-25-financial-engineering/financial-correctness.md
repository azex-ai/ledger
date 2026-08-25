# 审计报告 — financial-correctness（收窄范围：Q1 舍入 / Q2 精度 / Q3 余额等式 / Q4 符号约定）

## 摘要

最要紧的发现不在舍入，而在**符号**：`main_wallet` 被声明为 debit-normal（`presets/templates.go:62`），
`deposit_confirm` / `withdraw_lock` / `fx_*` 都按这个声明走（DR = 增加），但 **`transfer_out` /
`transfer_in` / `fee_charge` 三个已发货 preset 用的是相反的口语约定**（"CR 客户账户 = 给钱"），
方向整体反了 —— P2P 转账让**付款方余额增加、收款方减少**，收手续费让**用户可用余额增加**。
这三个模板的测试只断言 "debit 合计 == credit 合计"，而模板两条腿共用同一个 amount key，
该断言恒真，永远不可能红。

其次：`ReverseJournalFraction` 的累计冲销账按 (holder, currency, classification, entry_type)
聚合，但逐条 entry 比对——一张 journal 若在同一维度上有两条同向 entry，先部分冲销再
"冲销剩余全部"会**静默少冲**并返回成功。

Q1 舍入本身是这份代码里做得最扎实的部分（`core.Allocate` 用 big.Rat + 最大余额法，残差不丢），
真正的风险是"舍入残差归谁"这件事**没有任何机制强制**，只靠 caller 自觉 + 每币种借贷相等兜底。
Q2 精度校验我逐个写入口核过了，**没找到漏掉的入口**。
Q3 四个条件里三个成立（分区 / reversal / 并发 rollup），**effective_at 回溯这一个不成立** ——
不是实时余额错，而是已写入的历史快照永不失效。
Q4 同一个符号语义有 **7 处 Go + 10 处 SQL** 独立实现，valid 数据下一致，未知 normal_side 时三种
不同处置（报错 / 当 debit / 当 0）。

---

## 发现

### [Critical] 三个已发货 preset 的借贷方向与 `main_wallet` 的 normal_side 相反 —— 转账把钱转反、收费给用户加钱

- **位置**：
  - `presets/transfer.go:50-51`（`transfer_out`）、`presets/transfer.go:59-60`（`transfer_in`）
  - `presets/fee.go:40-41`（`fee_charge`）
  - 对照的权威定义：`presets/templates.go:62`（`main_wallet` = `NormalSideDebit`）+
    `postgres/sql/queries/checkpoints.sql:58`（`normal_side='debit' AND entry_type='debit' → +amount`）
- **判定**：CONFIRMED（读完 template → `EntryTemplate.Render` → `postJournalWithQueries` →
  余额查询的完整链路；符号规则从 SQL 的 CASE 表达式直接读出，未依赖任何注释）
- **失效场景**：
  1. **P2P 转账方向相反**。`transfer_out` = `DR main_wallet(user) / CR settlement(system)`。
     main_wallet 是 debit-normal，debit ⇒ **+**。付款方执行 `transfer_out` 100 → 付款方
     main_wallet **+100**；收款方执行 `transfer_in` 100（`CR main_wallet`）→ 收款方 **−100**。
     一笔 100 的转账结果：付款方 +100、收款方 −100，方向整体颠倒。
     交叉验证：同为 `main_wallet ↔ settlement` 两腿结构的 `fx_sell`
     （`presets/fx.go:55-56`，`CR main_wallet / DR settlement`）与 `fx_buy`
     （`presets/fx.go:64-65`，`CR settlement / DR main_wallet`）用的是**相反**的配对
     —— `transfer_out` 的 entry 组合等于 `fx_buy`（用户收钱），`transfer_in` 等于
     `fx_sell`（用户付钱）。两对模板必有一对是错的，而下面第 2 条确定了哪一对。
  2. **收手续费给用户加钱**。`fee_charge` = `DR main_wallet(user) / CR fees(system)`。
     收 2.50 的费 → 用户 main_wallet **+2.50**（`main_wallet` 带 `BalanceRoleAvailable`，
     即**可用余额、可被 Reserve 花掉**，见 `presets/templates.go:62`），同时 `fees` 收入 +2.50。
     两边同增 ⇒ 凭空造出 2.50 无资产背书的可花余额。
  3. **同仓内已有正确写法作为对照**（这是"哪一边是对的"的决定性证据）：
     - `presets/templates.go:202-205` `withdraw_fee` 收费的正确形状 =
       `DR fee_expense(user, debit-normal) / CR locked(user) / DR custodial / CR fee_revenue`
       → 用户侧 locked **−fee**、fee_expense +fee。
     - `postgres/balance_breakdown_test.go:75-78` 手写 fixture 把"付费"记成
       `DR fee_expense / CR main_wallet`，注释明写 "main_wallet −5"。
     两处都表明"用户付钱 = **CR** main_wallet"，`fee_charge` 的 `DR main_wallet` 因此是反的。
  4. 结构性原因（说明为什么 `fee_charge` 不是"少写一条腿"而是**表达不出来**）：在
     main_wallet=debit-normal / `fees`=credit-normal 这套约定下，两条腿的 DR-CR 组合必然让
     两个账户的**带符号余额同向变动**（deposit 就是靠这一点：用户 +、custodial + 都对）。
     要做到"用户 − 且平台收入 +"，必须像 `withdraw_fee` 那样用四条腿两对。任何 2-entry 的
     `main_wallet ↔ 收入账户` 模板都只能得到用户 +。
- **最小复现**：
  ```
  InstallTemplateBundle(TransferBundle())            // 或 FeeBundle()
  ExecuteTemplate("transfer_out", holder=1001, amount=100)
  GetBalance(1001, cur, main_wallet)  // => +100（期望 −100）
  ExecuteTemplate("fee_charge",  holder=1001, amount=2.50)
  GetBalance(1001, cur, main_wallet)  // => 再 +2.50（期望 −2.50）
  ```
- **为什么现有机制没拦住**：
  - `presets/transfer_test.go:69,77` 与 `presets/fee_test.go:63` 只调 `assertBalanced`
    （`presets/transfer_test.go:91-103`，断 debit 合计 == credit 合计）。模板两条腿共用同一个
    `AmountKey`（`presets/transfer.go:50-51` 都是 `"amount"`），所以 debit==credit **在模板结构上恒真**
    —— 把 `EntryType` 两边一起写错、写反、写成任意方向，这个断言都是绿的。这正是契约 §1.4
    第 1 条（"被测代码整个不执行会失败吗"）要抓的形状：这里被测的**方向语义完全没有被断言**。
    `presets/transfer_test.go:31` 甚至断言了 `main_wallet` 是 debit-normal，却没有用它去校验方向。
  - 全仓没有任何测试或 example 用 `transfer_out` / `transfer_in` / `fee_charge` 跑到真库并断言
    余额（grep 结果：除 `presets/` 自身外零引用；`docs/COOKBOOK.md` 也没有 transfer 配方）。
    对照 `withdraw_fee` 有 `postgres/presets_install_test.go:92-142` 的真库余额断言 —— 有断言的
    那条是对的，没断言的三条是错的。
  - `presets/fee.go:17-19` 注释自述 "analogous to consts.AccountClassificationFees in
    payments/backend"，`presets/transfer.go:36-42` 的注释也写着 `DR main_wallet (sender)`。
    从另一套（main_wallet 视作 credit-normal 的）账套照搬过来是最可能的成因 —— 此因果为
    **PLAUSIBLE**（我没有 payments/backend 可读），但行为本身 CONFIRMED。

### [Critical] 同维度多条 entry 时，"部分冲销后再冲销剩余全部"静默少冲并返回成功

- **位置**：`postgres/reversal_fraction_store.go:245-289`（`num == den` 剩余量分支）、
  `postgres/reversal_fraction_store.go:469-487`（`cumulativeReversedByDimension` 按维度聚合）、
  `postgres/reversal_fraction_store.go:349-355`（分数分支的逐条上限检查）
- **判定**：CONFIRMED（手算过下述两个场景；前置条件"一张 journal 可以在同一维度上有两条同向
  entry"从 `core/journal.go:130-171` 确认 —— `Validate` 只检查每币种借贷相等，**不去重、不禁止
  重复维度**）
- **失效场景**（金额给数字）：
  原始 journal J：
  ```
  DR main_wallet(H=1, USD) 60      CR settlement(H=-1, USD) 60
  DR main_wallet(H=1, USD) 40      CR settlement(H=-1, USD) 40
  ```
  （debit 两条同维度 K1，credit 两条同维度 K2；合计 100/100，`Validate` 通过）
  1. `ReverseJournalFraction(J, 1, 2)`：按 (currency, entry_type) 分组 → debit 组总额 100 →
     `scaleByFraction` → 50 → `core.Allocate(50, [60,40])` → [30,20]。credit 组同理 [30,20]。
     冲销 journal = 50/50，合法落库。此时
     `alreadyReversed[K1] = 30+20 = 50`、`alreadyReversed[K2] = 50`。
  2. `ReverseJournal(J)`（即 `num == den`，"冲销剩余全部"）：逐条算
     `remaining = e.Amount − alreadyReversed[key]`：
     - entry(60) → 60 − 50 = **10**（正确应为 30）
     - entry(40) → 40 − 50 = −10 → `!remaining.IsPositive()` → **跳过**（正确应为 20）
     - credit 侧对称 → 10 + 跳过
     冲销 journal = debit 10 / credit 10，**借贷相等 ⇒ `Validate` 通过 ⇒ 成功落库并返回 nil error**。
  3. 结果：原始 100 只冲销了 50 + 10 = **60**，剩余 **40 永久留在账上**，而调用方拿到的是
     "全额冲销成功"。用户被扣的 40 没有退回，且没有任何错误、日志或指标提示。
  - 非对称变体（只有 debit 侧重复维度）会走成 debit 10 / credit 50 → `ErrUnbalancedJournal`，
    即该 journal **此后永远无法被冲销**（fail-closed，但 `ErrUnbalancedJournal` 会让调用方以为
    是自己的入参错，而不是账本卡死）。两个分数冲销连做则会得到虚假的 `ErrConflict`
    （`reversal_fraction_store.go:352`：`already(50) + new(30) > original(60)`）。
- **最小复现**：
  ```
  j := PostJournal(两条 60/40 的 DR main_wallet + 两条 60/40 的 CR settlement)
  ReverseJournalFraction(j.UID, 1, 2, ...)   // OK，冲销 50
  ReverseJournal(j.UID, ...)                 // 返回 nil，但只冲销 10；累计 60 ≠ 100
  ```
- **为什么现有机制没拦住**：
  - `postgres/reversal_fraction_store.go:250-256` 的注释明确断言
    "**the per-currency remainder is equal on the debit and credit sides by subtraction**"
    —— 这个推理只在"每个维度键至多对应一条 entry"时成立，注释里没有这个前提，代码里也没有
    校验它。
  - `postgres/reversal_fraction_test.go` 的全部用例都用**每维度恰好一条** entry 的 journal：
    `seedFractionFixture`（`:22-45`，2 条 entry / 2 个维度）、
    `TestReverseJournalFraction_MultiCurrencyBalancesPerCurrency`（`:163-192`，4 条 entry /
    4 个维度）。因此 `core.Allocate` 在冲销路径上**从未被 len(weights) > 1 的输入测到过**
    ——分组分摊这条主要代码路径整个没有真库覆盖。
  - `docs/INVARIANTS.md` I-2 只承诺"累计冲销不超过原额"（上界），**不承诺"num==den 之后原额被
    完全冲销"**（下界）。少冲不违反任何写下来的 invariant，所以对账检查也不会报。

### [Major] 偿付能力（solvency）把 user-side 的 debit-normal 费用账当成负债 —— 每笔提现手续费让平台看起来少偿付 1 笔手续费

- **位置**：`postgres/sql/queries/platform_balances.sql:60-91`（`GetTotalUserSideBalance`：
  `WHERE account_holder > 0`，不按 `balance_role` 过滤、不区分 normal_side）、
  `postgres/platform_balance_store.go:153-179`（`solvencyCheckWithQueries`）、
  `presets/templates.go:73`（`fee_expense` = debit-normal，**HolderRoleUser**）、
  `presets/templates.go:202`（`withdraw_fee` 把它记在用户号上）
- **判定**：CONFIRMED（用仓内既有测试自己的数字算出来的）
- **失效场景**：`postgres/presets_install_test.go:55-142` 的标准流程（充值 500 → 锁 105 →
  `withdraw_fee` 5 → `withdraw_confirm` 100）跑完后，该测试自己断言的余额是
  `main_wallet=395`、`locked=0`、`fee_expense=5`、`custodial=395`。于是：
  ```
  Liability = Σ(holder>0) = main_wallet 395 + locked 0 + fee_expense 5 = 400
  Custodial = 395
  Margin    = 395 − 400 = −5      Solvent = false
  ```
  平台实际完全健康（用户可用 395，托管 395），但 `/platform/solvency` 报**资不抵债 5**，
  数额恰等于累计手续费。手续费越多，虚假缺口越大且单调增长。
  更严重的次生后果：这套报表的设计用途是"诚实暴露无背书的负债"
  （`presets/devcredit.go:13-24` 明写 solvency 就是这么用的）。累计手续费造成的漂移会把
  **真实的**缺口淹没 —— 缺口 −X 无法区分是 X 的无背书发行还是 X 的历史手续费。
- **最小复现**：装 withdrawal preset → 走一遍 deposit/lock/`withdraw_fee`/`withdraw_confirm`
  → `SolvencyCheck(cur)` → `Solvent == false && Margin == -fee`。
- **为什么现有机制没拦住**：
  - 唯一的 solvency 真库测试 `postgres/platform_balance_realtime_test.go:168-233`
    **自建了一套与已发货 preset 相反的符号约定**：`mw_solv` 建成 `NormalSideCredit`
    （`:180-183`）、`custodial` 建成 `NormalSideDebit`（`:185-188`），而 preset 里
    `main_wallet` 是 debit-normal、`custodial` 是 credit-normal（`presets/templates.go:62,64`）。
    它在自己那套自洽的小世界里通过，**完全没有验证实际发货的 preset 配置**，更没有任何
    分类带 role='' 的用户侧费用账参与。
  - 同一文件 `:20-105` 的另一个测试也把 `custodial` 建成 debit-normal（`:38-41`），
    并在 `:93` 断言 custodial = **−500**。这两个 fixture 说明"这套符号约定容易写反"本身
    已经在测试里发生了。
  - `GetBalanceBreakdown` 面向持有人时是**按 `balance_role` 过滤**的
    （`postgres/reserver_store.go:254` 注释、`postgres/balance_breakdown_test.go:86`
    明确 "Role-less classifications (fee_expense) appear nowhere"），而 liability 求和不过滤。
    同一个概念"用户余额"有两套互不一致的定义，只有前者有测试保护。

### [Major] effective_at 回溯记账不会让已写入的历史快照失效

- **位置**：`postgres/sql/queries/checkpoints.sql:166-186`（`ListBalancesAt`，按
  `effective_at < cutoff` 聚合）、`service/snapshot.go:120-155`（当日快照写一次）、
  `postgres/snapshot_extra_store.go:49-80`（`UpsertSnapshotSparse`：与"本日之前最近一条快照"
  相同就跳过写入）、`core/journal.go:96`（`effectiveAtFutureTolerance` 只限未来，**过去无下界**）
- **判定**：CONFIRMED（读完 snapshot job → sparse upsert → `GetLatestSnapshotBefore` 链路；
  未跑）
- **失效场景**：H 的 main_wallet 在 2/28 的 as-of 余额 100，2/28 的快照写入 100。3/3 补记一笔
  `EffectiveAt = 2026-02-27` 的 +50（在期间结账线之后，`postgres/ledger_store.go:873-881`
  的 I-15 闸不会拦）。此后 2/28 的**真实** as-of 余额是 150，但 `balance_snapshots` 里
  2/28 那行永远是 100，没有任何路径会重算它。`GetSnapshotBalances(H, cur, 2/28)` 返回 100。
  开启 sparse 时更糟：`GetLatestSnapshotBefore` 会以这条陈旧行为基准做"未变化就跳过"的判断。
  实时余额（I-5，按 id 而非 effective_at）不受影响 —— 错的只有历史报表，而历史报表正是
  `effective_at` 存在的唯一理由。
- **最小复现**：跑一天快照 → post 一笔 `EffectiveAt` = 前一天的 journal →
  `GetSnapshotBalances(前一天)` 与 `ListBalancesAt(前一天+1)` 对比，两者不一致。
- **为什么现有机制没拦住**：`docs/INVARIANTS.md` I-14（`:410-448`）和
  `docs/COOKBOOK.md:410-414` 都只说"`effective_at` 不影响实时余额，只改变 as-of 归属桶"，
  **没有任何一处说明已生成的 as-of 快照不会被重算**。I-14 的 Pinned by 列表里
  `TestRollupAdapter_ListBalancesAt_UsesEffectiveAt` 验的是查询读 effective_at，
  不是"回溯后快照被修正"。`service/snapshot_backfill.go` 提供了人工重算手段，但没有任何
  东西在回溯记账时触发它。

### [Minor] 同一个符号语义有 17 处独立实现，未知 normal_side 时有三种互不相同的处置

- **位置**（全部为独立实现，非共享一个函数）：
  - Go（7）：`core/account_policy.go:104-110`（自称 "the sole authority"）、
    `service/rollup.go:204-213`、`service/reconcile.go:806-811`、
    `service/reconcile.go:1036-1040`、`service/reconcile.go:1373-1377`、
    `postgres/ledger_store.go:1173-1181`、`postgres/trial_balance_store.go:78-81`、
    `postgres/reconcile_queries.go:127-131`
  - SQL（10 处表达式，3 种写法）：`checkpoints.sql:57-62`（4-way CASE，`ELSE 0`）、
    `checkpoints.sql:173-180`（3-way + 兜底当 debit）、`integrity_checkpoint.sql:24-29`、
    `integrity_checkpoint.sql:117-120`、`platform_balances.sql:44-50` /`:78-84` /`:112-118`
    （`OR` 合并式）、`holder.sql:49`（`entry_type = c.normal_side` 字符串比较式）、
    `reconcile.sql:66-73`（`MIN(c.normal_side)`）
- **判定**：CONFIRMED（一致性逐个核过：在 `normal_side ∈ {debit, credit}` 下 17 处**结论一致**）
- **失效场景**：分歧只出现在 normal_side 取到第三个值时，而此时四种处置互相矛盾：
  `service/rollup.go:211` **报错**（注释还写着 "a class of bug that has happened before"）、
  `postgres/ledger_store.go:1178-1180` **默认按 debit-normal**、
  `checkpoints.sql:62` **`ELSE 0`（该 entry 不计入余额）**、
  `checkpoints.sql:177-178` **兜底按 debit-normal**。
  今天不可达（`001_baseline.up.sql:169` 有 `CHECK (normal_side IN ('debit','credit'))`，
  且 `docs/INVARIANTS.md:1072` 记 normal_side 不可变），所以只是漂移面而非在案缺陷 ——
  但 17 份拷贝意味着"改一次符号规则要改 17 处"，且其中 `ELSE 0` 那一份是唯一会**静默少算钱**
  的形状。
- **为什么现有机制没拦住**：没有任何门禁要求这 17 处保持同构；`cmd/ledger-cli/main.go:421`
  的 `var _ = core.NormalSideDebit`（注释 "keep imported types referenced so the file fails
  fast if any drift"）说明作者意识到了漂移风险，但这行不检查任何东西 —— 它只是压编译错误。

### [Minor] `journal_entries.id` 单独不唯一，I-5 的单调性完全依赖序列

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:325-338`
  （`id BIGSERIAL` + `PRIMARY KEY (id, created_at)`，`PARTITION BY RANGE (created_at)`）
- **判定**：CONFIRMED（schema 事实）；后果为 **PLAUSIBLE**（我没有构造真实触发路径）
- **失效场景**：schema 注释（`:280-284`）称这个 PK 是 "a uniqueness backstop beyond trusting
  the sequence" —— 实际上不是：`(id, created_at)` 允许**不同分区里出现同一个 id**。余额等式
  `Σ(id > last_entry_id)` 只按 id 过滤，所以任何 id ≤ watermark 的 entry 都**永久不计入余额**，
  而 `SumGlobalDebitCreditByCurrency`（`checkpoints.sql:157-164`）与
  `reconcile.sql:51-76` 会照常统计它 —— 两套账因此可以在不违反任何 id 区间不变式的情况下分叉。
  现实触发面：备份/PITR 恢复后序列回退，或（既有威胁模型内）持 `ledger_app` 凭证显式指定 id
  的直接 INSERT。
- **为什么现有机制没拦住**：`docs/INVARIANTS.md` I-5 的 "Load-bearing prerequisite" 段
  （`:186-193`）把单调性归因于 advisory lock（对的），但没提"id 唯一性并未由 schema 保证"这一半。

### [Minor] I-5 的载荷前提只有散文守卫，没有机器门禁

- **位置**：`docs/INVARIANTS.md:186-193`（"Any future write path that inserts entries without
  `acquireBalanceLocks` silently reopens this visibility race — **do not add one**"）、
  唯一 choke point `postgres/ledger_store.go:889`（`acquireBalanceLocks`）→
  `postgres/ledger_store.go:950`（唯一的 `InsertJournalEntry` 调用点）
- **判定**：CONFIRMED（grep 全仓：`InsertJournalEntry` 在非生成、非测试代码中只有一处调用点，
  该处在 `acquireBalanceLocks` 之后 —— **今天是成立的**）
- **失效场景**：不是当前缺陷，是缺护栏。第二个 entry 写入路径一旦出现（且忘了取锁），
  rollup 的 `MAX(id)` watermark 就会跨过一条尚未提交的低 id entry，该 entry 的金额**永久**
  不进任何余额（`SumEntriesSince` 的 REPEATABLE READ 只保证 SUM 与 MAX 同快照，
  **不解决提交顺序与 id 顺序不一致**，见 `postgres/rollup_adapter.go:176-194` 的注释所覆盖的
  窗口比它自称的更窄）。这条整个靠人记住。
- **为什么现有机制没拦住**：这正是 `working-agreements.md` §5 说的形状 —— 能做成机器检查
  （一条断言 `InsertJournalEntry` 的调用点数 == 1 且位于 choke point）却写成了文档里的祈使句。

### [Minor] `presets/fx.go` 的文档与自己的代码符号相反

- **位置**：`presets/fx.go:27-30`（"Net effect": `settlement (CCY-A): +qtyA` / `(CCY-B): -qtyB`）
  与 `presets/fx.go:45-49`（"`DR main_wallet (user) CR settlement (system)`"）
  vs 实际代码 `presets/fx.go:55-56`（main_wallet = **Credit**、settlement = **Debit**）
- **判定**：CONFIRMED
- **失效场景**：`settlement` 是 credit-normal（`presets/transfer.go:16`），fx_sell 对它记 debit
  ⇒ 实际 `settlement(CCY-A) = **−**qtyA`、`settlement(CCY-B) = **+**qtyB，与文档的
  `+qtyA / −qtyB` 恰好相反。而文档紧接着要求消费方"自行把 settlement 余额与外部托管头寸对账"
  （`:32-34`）—— 照文档做的人会把方向核反。同时这份注释也是上面那条 Critical 的传染源：
  按注释"修正"代码就会把 fx 也改成转账那一版的错误方向。
- **为什么现有机制没拦住**：`presets/fx_test.go` 与其余 preset 测试同型，只断 balanced。

### [Minor] 精度报错信息把 currency uid 冒充 code

- **位置**：`postgres/precision.go:28-31`（`dimCurrency{UID: e.CurrencyUID, Code: e.CurrencyUID, ...}`）
- **判定**：CONFIRMED
- **失效场景**：journal 路径的 `ErrPrecisionExceeded` 文案里 `currency %s (%s)` 两个位置都是
  uid，币种 code 永远缺失；而 reserve / booking 路径（`postgres/reserver_store.go:101` 等）传的是
  真 `dimCurrency`，文案里有 code。同一个哨兵错误在不同入口给出不同信息量，排障时误导。

---

## 四个问题的直接回答

### Q1. 舍入残差归谁？

**全部舍入点**（grep `Round|Truncate|Shift|Div` 于非测试 Go 后逐个打开核对）：

| 位置 | 模式 | 残差归谁 |
|---|---|---|
| `core/money.go:39-51` `Round` | 4 模式可选（`RoundHalfUp` 默认 / `RoundHalfEven` / `RoundDown` / `RoundUp`），未知模式回落 HalfUp | 由调用方决定 |
| `core/money.go:59-61` `ConvertAt` | 调用方传入 | **无归属机制**：注释（`:55-58`）说残差"expected to land on a settlement account"，`expected` 而非强制；账本不参与 |
| `core/money.go:82-166` `Allocate` | floor + 最大余额法，全程 `big.Rat` 精确有理数（无 `decimal.Div`/无 float） | **不产生残差**：`Σshares == total` 精确成立；leftover 逐个"最小单位"分给截断余数最大的份额，同余数按输入序（`:139-144`），确定可复现 |
| `core/money.go:91` `Truncate` | 仅作相等性判定，拒绝超精度的 `total`（不静默舍入） | — |
| `postgres/reversal_fraction_store.go:492-495` `scaleByFraction` | 中间 `DivRound` 保 `exponent+12` 位保护位（`:21`），再 `core.Round(..., RoundHalfUp)` | 组总额层面舍入一次，再由 `Allocate` 精确拆分 → **组内不产生残差** |
| `postgres/precision.go:16` `Truncate` | 仅相等性判定 | — |
| `chains/evm/sweeper.go:244` `big.Int Div` | gas bump 分母 | 非账本金额 |

**多腿分摊时残差落在哪条腿**：只有冲销路径做分摊，落法是"截断余数最大的腿各得一个最小单位"
（`core/money.go:139-147`）—— 不是落在固定某条腿，也不是落在 settlement。

**"每腿各自舍入后总和 ≠ 原额"的路径**：
- **正常记账路径不存在** —— `core.EntryTemplate.Render`（`core/template.go:132-171`）把
  `params.Amounts[AmountKey]` **原样**放进 entry，`Render` 与 `postJournalWithQueries` 全程
  **不做任何舍入**。多腿模板（如 `settle_with_fee`，`presets/settlement.go:46-50`：
  `gross_amount` / `net_amount` / `fee_amount`）要求调用方自己算准
  `net + fee == gross`，否则 `core/journal.go:160-170` 的每币种借贷相等检查直接
  `ErrUnbalancedJournal`。**残差问题被 fail-closed 地推给了调用方**——这是当前设计的实际形态，
  账本本身不吸收也不分配任何残差。
- **冲销路径**理论上安全（Allocate 精确），但有上面那条 Critical 的同维度缺陷。

**`presets/fx.go` 的"settlement 吸收净额"声称 —— 亲手算过两条腿**：
FX 的两条腿**各自是单币种、两行、共用同一个 `AmountKey "amount"`**
（`presets/fx.go:54-57` 与 `:62-66`）。所以：
1. **单腿内不存在任何舍入** —— 两行金额字面相同，debit == credit 恒精确成立。
   "有舍入时是否还守恒"这个问题在腿内**不成立前提**。
2. 真正的舍入发生在**账本之外**：调用方用 `ConvertAt(qtyA, rate, expB, mode)` 算 `qtyB`。
   账本收到的是两个**独立**的、已经各自合法的单币种 journal。
3. 因此 "settlement 吸收净额" 是**恒真但空洞**的：settlement 拿到的就是
   `−qtyA`（CCY-A）与 `+qtyB`（CCY-B）（注意符号与其文档相反，见上面 Minor），
   无论 `qtyB` 是不是 `qtyA × rate` 的正确舍入结果。**账本没有任何机制校验两条腿的汇率关系**
   ——`fx_sell` 与 `fx_buy` 之间没有共享 idempotency 根的强制（`presets/fx.go:16-19` 只说
   "ideally the same idempotency-key root"）、没有 rate 校验、也不保证两腿都落地
   （一腿成功一腿失败 = 用户钱少了但没拿到对价，且每腿各自 balanced，对账查不出）。
   `presets/fx.go:22-26` 声称"per-currency balance validation 能抓住任何 rate-quote bug"
   —— **这个声称不成立**：每币种借贷相等对任意 `qtyB` 都成立，rate 错 100 倍也照样通过。
   （此条我记为 Q1 的答案而非独立发现，因为跨腿原子性属 territory B/C 的编排范畴。）

### Q2. 精度校验能被绕过吗？—— 逐个写入口核过，**没找到漏掉的入口**

`currencies.exponent` 侧先关掉了"声明精度 > 存储精度"这条路：
`core/interfaces.go:361-363` 限 `[0,18]`，`001_baseline.up.sql:161`
`CHECK (exponent >= 0 AND exponent <= 18)`，且**没有任何 UPDATE exponent 的查询**
（`postgres/sql/queries/currencies.sql` 只有 Create / Get / Deactivate / List）。所以
声明精度永不超过 `NUMERIC(30,18)`，不存在"声明 25 位、Postgres 静默舍到 18 位"。

**全部金额写入口**（按 `INSERT INTO` 扫 `postgres/sql/queries/*.sql`，再回查 Go 调用点）：

| 写入口 | 校验 | 结论 |
|---|---|---|
| `journal_entries`（唯一 INSERT 调用点 `postgres/ledger_store.go:950`） | `validateEntriesPrecision`（`postgres/ledger_store.go:850`，逐条按该 entry 币种 exponent） | ✅ 覆盖 |
| `reservations.reserved_amount`（Reserve） | `postgres/reserver_store.go:101` | ✅ |
| `reservations.settled_amount`（Settle） | `postgres/reserver_store.go:397` | ✅ |
| `reservation_settlement_legs.amount`（SettlePartial） | `postgres/reserver_store.go:510` | ✅ |
| `bookings.amount`（Create） | `postgres/booking_store.go:124` | ✅ |
| `bookings.settled_amount` + `events.settled_amount`（Transition） | `postgres/booking_store.go:239-248`（`input.Amount.IsZero()` 时跳过，但那条路径不改写金额：`:285-287` 保留原值） | ✅ |
| pending confirm/cancel（`postgres/pending_store.go:212-273`） | 无自己的检查，但**必经** `ledger.PostJournal` | ✅ 间接覆盖 |
| 冲销（`ReverseJournal*`） | 金额由 `core.Round(..., exponent)` 生成，且必经 `PostJournal` | ✅ 双重 |
| `balance_checkpoints` / `balance_snapshots` / `system_rollups` | 全部是 entry 的派生聚合，不是独立输入 | 不适用 |
| `deposits` / `withdrawals` 两张表的 `InsertDeposit`/`InsertWithdrawal` | **Go 侧零调用点**（grep 确认），死 SQL | 不可达 |

行为是**拒绝而非静默舍入**（`postgres/precision.go:11-23` 明确 "never rounds or truncates"），
返回 `core.ErrPrecisionExceeded`。唯一瑕疵是上面那条报错文案 Minor。

### Q3. 余额等式在四种条件下还成立吗？—— 三成立、一不成立

1. **分区表：成立**。`journal_entries` 按 **`created_at`**（写入日期，非 `effective_at`）
   月分区（`001_baseline.up.sql:325-338`），`id` 是**跨分区共享的全局 `BIGSERIAL``，
   `idx_entries_account_id (account_holder, currency_id, classification_id, id)` 建在分区父表
   上（`:370`），`id > watermark` 的过滤跨分区语义正确；有 DEFAULT 分区（`:360`）
   兜住时间窗外的写入，不会因缺分区而失败。回溯记账（effective_at 早）拿到的仍是**新的、更大**
   的 id，所以照样被 delta 计入。
   **理由中的裂缝**（记为上面的 Minor）：`id` 单独不唯一（PK = `(id, created_at)`），
   等式的单调性只靠序列本身。
2. **effective_at 回溯：实时余额成立，历史快照不成立**。等式按 `id` 推进，与 `effective_at`
   无关，回溯记账不影响实时余额（`docs/COOKBOOK.md:410-412` 也这么说，我核对了
   `checkpoints.sql:47-75` 的 delta 条件，确实只有 `je.id > cp.last_entry_id`）。
   **但**从同一批 entry 派生的 as-of 视图（`ListBalancesAt` → `balance_snapshots`）会因回溯
   而永久失真 —— 见上面的 Major。
3. **reversal（含部分冲销）：等式本身成立**。冲销只 append 新 entry（`journals.reversal_of` +
   翻转的 entry），拿新 id，走同一个 choke point，因此等式对它没有例外。累计守恒的上界由
   `GetJournalForUpdate` 行锁（`postgres/sql/queries/journals.sql:22-27`）+
   `reversal_fraction_store.go:349-355` 的逐条上限检查在**调用方事务的隔离级别**下保证
   （不依赖 SERIALIZABLE，靠原 journal 行的 `FOR UPDATE` 串行化）。
   **下界不保证** —— 见上面第二条 Critical。
4. **并发 rollup：成立，但理由是"寄生"的**。rollup **自己不取任何锁**；
   `SumEntriesSince`（`postgres/rollup_adapter.go:176-194`）只用 REPEATABLE READ 保证
   SUM 与 `MAX(id)` 同快照。它之所以不漏账，靠的是**写入方**在分配 id 之前就取了
   `pg_advisory_xact_lock('balance:holder:currency')`（`postgres/ledger_store.go:889`
   → `:132-140`）并持到 COMMIT，使得**同一 (holder, currency) 内 id 顺序 == 提交顺序**，
   于是快照里"看得见 id=N"必然意味着"该 pair 所有 < N 的 id 都已提交可见"。
   我另外核了 checkpoint 写入本身的两条竞态并确认无误：
   (a) `processItem`（`service/rollup.go:166-215`）读 cp 与求和是**成对**的
   （`sinceEntryID` 取自同一个 cp），所以基准余额与 delta 区间永远匹配；
   (b) `UpsertBalanceCheckpoint` 的 `WHERE last_entry_id < EXCLUDED.last_entry_id`
   （`checkpoints.sql:15-19`）让落后的写入者无法回退 watermark。
   两个 worker 的四种交错我手推过，结果都正确或被 guard 拒绝。
   **风险是该前提没有机器门禁** —— 见上面的 Minor。

### Q4. 符号约定在几个地方被独立实现？

**17 处**（Go 7 + SQL 10，清单见上面的 Minor 条目），另加**第 18 个独立实现层**：
每个 preset 模板的 `EntryType` 选择本身就是在手工实现符号约定 —— 而这一层**已经漂移了**
（三个模板用了相反的约定，见第一条 Critical）。

17 处纯计算实现在合法数据下**结论一致**；不一致有两处：
① 未知 `normal_side` 的处置有报错 / 当 debit / 当 0 三种；
② `system 对手方（负 holder）` 这一维**根本没有参与符号计算** ——
`core.SystemAccountHolder`（用于取负）只决定 holder 落在哪个命名空间
（`core/template.go:150-157`），余额符号完全由 `(normal_side, entry_type)` 二元决定。
`account_holder < 0` 只在**分组/筛选**语义里出现
（`platform_balances.sql:34,64,95`、`reconcile.sql:63`），从不参与取正负。
这一点是干净的，也是为什么第一条 Critical 只需要看两个维度就能定论。

---

## 移交

- **[→ B 并发]** FX 两条腿（`fx_sell` / `fx_buy`）没有跨腿原子性、没有共享幂等根的强制
  （`presets/fx.go:16-19` 只是"ideally"）：一腿成功一腿失败时用户付出而未获对价，且每腿各自
  balanced、对账不可见。
- **[→ B 并发]** `acquireBalanceLocks`（`postgres/ledger_store.go:132-140`）的 key 是
  `fmt.Sprintf("balance:%d:%d", holder, currencyID)` 走单参数 advisory lock；key 空间碰撞与
  `hashtextextended` 的派生属你的题。I-5 的正确性直接压在它身上。
- **[→ E 测试可信度]** `presets/*_test.go` 的 `assertBalanced` 是一个**结构上恒真**的断言
  （模板两腿共用同一 amount key），却是三个 preset 的唯一保护 —— 建议把这个 helper 整体
  过一遍。另 `postgres/platform_balance_realtime_test.go:38-41,180-188` 用了与已发货 preset
  **相反**的 normal_side fixture，测的不是产品配置。
- **[→ E 测试可信度]** `core.Allocate` 在冲销路径上从未被 `len(weights) > 1` 的真库输入覆盖
  （所有 `postgres/reversal_fraction_test.go` 的 fixture 每维度一条 entry）。
- **[→ F 结构]** `postgres/sql/queries/deposits.sql` / `withdrawals.sql` 全部查询在 Go 侧零调用点
  （死 SQL，v1 遗留）；`cmd/ledger-cli/main.go:420-421` 的 `var _ = strings.TrimSpace` /
  `var _ = core.NormalSideDebit` 是 `golang.md` 明令禁止的"先造再丢"形状，且注释声称的
  "fails fast if any drift" 并不成立。
- **[→ F 结构]** `001_baseline.up.sql:284-292` 自述 `journal_entries` 上有两条语义重复的
  `account_holder <> 0` CHECK，保留待单独评审 —— 已记录，不是我的发现。
- **[→ D 威胁模型]** `journal_entries.id` 单独不唯一（PK = `(id, created_at)`）在
  "DB 写凭证已泄露"模型下是一个**规避检测**的原语：显式指定一个 ≤ watermark 的 id 插入，
  该行不进任何余额读，却进全局借贷合计与对账查询。

## 我没能验证的

- **没有跑任何测试或 SQL**（territory 要求只读，且 testcontainers 需要起库）。所有 CONFIRMED
  都是完整读链路 + 手算，未经执行验证。三条 Critical/Major 的数字我建议 Team Lead 用
  `postgres/presets_install_test.go` 的现成 fixture 各写一个断言实测 —— 尤其是
  solvency 的 `Margin == -5`，那是用该测试自己的断言值算出来的，最容易复核。
- **`float64` 全仓排查没做**（不在我这次的四个问题内，属 territory A 的其他必答项）。
- **`core.Allocate` 的 `leftover.Int64()` 窄化**（`core/money.go:150`）：注释称
  leftover 有界于 `len(weights)`，我接受了这个推理但没有独立证明
  （若 `total` 的精度断言被绕过，`absUnits` 可能极大）。
- **`checkpoint_integrity_store` / `verified_balance_store` / `period_close` 的数学**没有细看
  （`RebuildBalanceCheckpoint` 的无条件覆盖我只读了 SQL 注释声称的前置条件，没有核对
  `CountPendingRollupForDimension` 的实际调用是否真在锁内）。
- **`balance_trends_store` / `holder_store` 的聚合**只核了符号表达式，没核时间窗与去重。
- **多币种 journal 在冲销时各币种用各自 exponent** 的组合（`reversal_fraction_store.go:311-315`）
  我只核了单条路径，没穷举"同币种多分类 + 跨币种"的交叉。
- **`transfer_out` / `fee_charge` 是否有仓外消费方**无从判断 —— 仓内零引用，
  但这是个对外发布的库（`ledger-react` / go module tag），实际影响面取决于下游。

# 复盘 — 对账 check #2 的两个覆盖盲区（系统侧永不扫描 / 不完整仍报通过）

> 2026-08-21 · 发现于一次 codex 对抗式评审（起因：审计「攻击者拿到 DB 写权限
> 能做什么」）· 设计 SoT `docs/plans/2026-08-21-tamper-evident-ledger-design.md` §0.1 / §2
> money path: checkpoint 直接决定余额读数（I-5）。对账 check #2 是**唯一**能发现
> checkpoint 被篡改的机制——审它是否真的在看。

---

## 结论（先说）

**check #2 的机制是对的，但它的覆盖面有两个洞，合起来使「我们每小时在对账」这句话
在最关键的维度上是假的。**

- 🔴 **B1**：keyset 游标从 `(0, 0)` 起步，谓词是 `account_holder > after_holder`。
  系统 holder 按定义是用户 holder 的**负数**（`core/system_account.go:6-26`），
  所以**每一次运行的第一页就永久排除了全部负数 holder**。
  整个系统 / 托管侧的 checkpoint 从未被验证过——而伪造的托管余额正是藏在那里。
- 🔴 **B2**：扫描被 `Check2ScanLimit`（默认 **5000** 对）截断或超时时，只追加一条
  "incomplete" finding，`result.Passed` **保持 true**。同源问题：check #8
  （`pending_journal_timeout`）因 schema 特性缺失**从未运行**，也报 `Passed: true`。
- 两个洞叠加的后果：一个只改系统侧 checkpoint 的攻击者，既不会被 B1 扫到，
  即使扫到也可能落在 B2 的截断之外，而报告仍然显示绿色。

一句话：**检测器在跑，但它没在看该看的地方，而且不承认自己没看。**

---

## 🔴 B1 — 系统侧 checkpoint 永不被扫描

- **位置**：`service/reconcile.go:364-381`（游标初始化为零值）
  + `postgres/sql/queries/reconcile.sql:126-137`（`account_holder > sqlc.arg(after_holder)`）。
  该 SQL 的注释当时还写着 "pass (0, 0) for the first page"，把错误的用法固化成了文档。
- **为什么会发生**：keyset 分页的惯例是从 0 起步，因为主键通常是正整数。
  这个账本的 holder 维度**故意跨越零**（正 = 用户，负 = 系统对手方，I-9），
  惯例在这里恰好失效。写分页时没有把这个领域事实带进来。
- **攻击场景**：`UPDATE balance_checkpoints SET balance = balance + N WHERE account_holder = -<holder>`。
  托管侧负债凭空增加，用户侧读数不变，check #2 完全不感知。
- **修法**：游标起点改 `math.MinInt64`。`currency_id` 来自 `BIGSERIAL` 恒 ≥ 1，
  因此 `(MinInt64, MinInt64)` 起步无遗漏点。
- **Pinned by**：
  - `service.TestFullReconciliation_Check2ScansSystemHolderCheckpoints`（DB 层，走真实 SQL 谓词；
    只污染系统侧 `+777`，断言必被抓到）
  - `service.TestCheck2GlobalBalance_ScansNegativeSystemHolders`（单元；mock 精确复刻同一谓词）
- **反向验证**：临时回退游标改动后，DB 那条只扫到 **1 个** pair（应为 2），
  系统侧 `+777` 的篡改**完全未被发现**；单元那条只扫到 2/4。修复后两条均通过。

## 🔴 B2 — 未运行 / 未扫完被折算成通过

- **位置**：`service/reconcile.go:443-458`（`partialReason != ""` 只 append finding，不改 `Passed`）
  + `:655-668`（check #8 跳过却 `Passed: true`）。
- **为什么会发生**：`CheckResult` 只有一个布尔 `Passed`，无法表达
  「跑完了且没问题」/「跑完了有问题」/「根本没跑完」三种状态。
  二值类型逼着代码把第三种折进第一种。原注释甚至写着
  "instead of silently reporting success as if full coverage had been verified"——
  **意图是对的，类型不支持，于是注释成了未兑现的承诺**。
- **后果**：默认 `Check2ScanLimit = 5000`，任何超过 5000 个 (holder, currency) 对的部署
  都只验证同一个前缀（finding 文案自己承认 "the next scheduled run rescans from the top"），
  而报告持续显示通过。
- **修法**：
  - `core.CheckResult` 加 `Complete bool`——**零值 = 未完成**，故一个忘记设置它的
    新 check 会被读成「未验证」而不是「已验证」（fail-closed by construction）。
  - `core.ReconcileReport` 加 `FullCoverage bool`：仅当每个 check 都 Complete 时为真。
    `OverallPassed` 语义**不变**（只报「发现了违规」），避免破坏既有消费方；
    两者文档写明：**`OverallPassed` 单独不构成健康证明，须 `OverallPassed && FullCoverage`**。
  - `ReconcileCheckResult` metric 改报 `Passed && Complete`，使任何基于该 metric 的
    告警 fail-closed——跳过或截断的 check 不可能在看板上显示绿色。
- **Pinned by**：
  - `service.TestCheck2GlobalBalance_ScanLimitReportsPartialCoverage`（`Complete=false`）
  - `service.TestFullReconciliation_Check2ReportsPartialScanOnScanLimit`（DB 层，含 `FullCoverage=false`）
  - `service.TestFullReconciliation_AllPass`（断言 check #8 `Complete=false` 且拉低 `FullCoverage`）
  - `service.TestFullReconciliation_EmitsMetricsPerCheck`（断言跳过的 check 不发绿色 metric）

---

## 契约影响

`CheckResult` / `ReconcileReport` 是序列化契约。加字段属 expand（`deployment.md`），
消费方容忍未知字段（`api-contract.md` §8），已同步：

- `docs/openapi.yaml`（`ReconcileReport.full_coverage`、`checks[].complete`，均带说明）
- `web/packages/ledger-react/src/client/schema.ts`

## 未解决（归入设计稿 P2）

`Check2ScanLimit` 之外的队尾仍然永不被扫——修复需要**持久化 resume cursor**（要 migration）。
B2 修完后，这个缺陷会以「持续 `FullCoverage=false`」的形式**诚实地暴露**出来，
而不再被静默折算成通过。这是刻意的中间状态：先让谎言停止，再补齐覆盖。

## 教训

1. **跨越零的维度会让分页惯例失效。** holder 维度故意有负数半区（I-9），
   任何以 `> 0` 或 `> cursor(0)` 起步的遍历都要显式论证是否覆盖负半区。
2. **二值类型会逼出谎言。** 「通过 / 未通过」装不下「没跑完」，
   于是代码把第三态折进第一态，注释写着相反的意图。缺的是类型，不是纪律。
   零值应当指向最保守的那一态。
3. **注释与 SQL 里的用法示例会把 bug 固化成文档。** `-- pass (0, 0) for the first page`
   让下一个读者以为那是对的。修 bug 时必须一并修掉指向它的注释
   （同类：`reconcile.go` 的 `journal_dr_cr` 注释声称 DB constraint 仍在 enforce，
   而它已被 migration 018 删除——见设计稿 §2 M1）。
4. **「检测器在跑」不等于「检测器在看」。** 覆盖面必须是可断言的一等信号，
   而不是埋在 finding 文案里的一句话。

# 防伪造记录与防篡改 — 账本完整性设计

> 2026-08-21 · Status: **IMPLEMENTED**（2026-08-25 收口）
> P0–P7 全部落地并合入 main，另加两波后续：Wave 2 提现时 verified-balance 闸
> （`ReserveInput.RequireVerifiedBalance`）、Wave 3 attestation 批量摊销授权判定
> 与充值复核的职责分离。契约与逐项分工见
> `docs/plans/2026-08-21-integrity-hardening-contracts.md`；行为契约见
> `docs/INVARIANTS.md` I-22..I-34（13 条）。
> 未收口的只剩一项：`ledger_ro` 仍是全库明细 SELECT，§3 想要的聚合视图未做（见 §14 末与 RUNBOOK 待办）。
> ⚠️ 本稿描述的 migration 编号（042–054）在 2026-08-25 的 baseline 压平中已不存在，
> schema 现由单一 `001_baseline` 定义；编号在此仅作历史索引。
> **起因**：把「数据库写凭证已经泄露」当作前提来设计，而不是当作要防的事。
> 要回答的问题因此不是「怎么防泄露」，而是
> **「攻击者拿到 DB 写权限后，直接改余额或插入一条伪造的账目记录，账本层面怎么防」**。
> 本稿经 codex 对抗式评审（2026-08-21）修订。原稿以 batch Merkle 为主线的优先级排序**被推翻**：
> Merkle 降为可选末位，**per-journal 授权签名升为主线**。评审确认的缺陷清单见 §2。

## 0. 决策记录

| 决策点 | 结论 |
|---|---|
| 问题定义 | 防**伪造记录 / 改余额**（完整性 integrity）。**不是**防泄露（保密性 confidentiality）—— 两者的防御工具几乎无交集，见 §1 |
| 主线机制 | **per-journal 授权签名**：对 canonical posting intent 签名，签名与 journal 同事务落库。这是唯一能在**写入时**确立真实性的机制 |
| Merkle 定位 | **降为可选末位（P7）**。对 canonical batch 做签名 hash 已足以检测「整批是否被改」；Merkle 的增量价值只是 inclusion proof 与**定位**（是哪几条被改了） |
| batch digest 与外部锚 | **同一 phase 交付（P6）**，不可拆。尾部截断**不产生 seq gap**（删掉最后 3 条，seq 1..97 依然连续），只有外部锚能钉住 head |
| 余额真相 | checkpoint 是**不可信缓存**。提现路径全量重算，不读 checkpoint |
| 平衡检查 | **回到 DB 层**（deferred constraint trigger，per-journal 一次聚合，不是 004 那个 per-row O(N²) 实现） |
| 签名密钥位置 | **要求只有一条：不与 DB 凭证同处一个失效域**——永不进 DB，且不与 `DATABASE_URL` 同一个 secrets bundle（`custody.md`：凭证不落可失陷面）。具体载体（本地密钥 / sidecar / HSM / 云 KMS）是 **deployment 选择**，已在 `Attestor` port 之后，不是设计决策。单体部署下进程内本地 ed25519 即满足 |
| 签名 port 命名 | 新 `core.Attestor`。**不复用 `core.Signer`**（`core/interfaces.go:437`，那是 EVM sweep tx 签名，另一把密钥、另一个爆炸半径） |
| canonical digest 的空间 | **uid-space**（`JournalInput` 原始形态），不是内部 id-space。理由见 §7.2 —— 这同时解决了「事务内禁外部调用」的红线冲突 |
| 前提条件 | DB role least-privilege 是**一切 DB 层强制的前提**。没有它，trigger 可被 `DROP`，I-2 在「app 凭证泄漏」这一档就已失效 |

### 0.1 P0 — 已实施（2026-08-21）

评审发现的两个**现存 bug**，已修复并验证，与本设计其余部分解耦：

| 缺陷 | 修法 | 文件 |
|---|---|---|
| 对账 check #2 的 keyset 游标从 `(0,0)` 起，`account_holder > cursor` 谓词**永久排除全部负数 holder** —— 即整个系统/托管侧从未被验证过 | 游标起点改 `math.MinInt64` | `service/reconcile.go`、`postgres/sql/queries/reconcile.sql`（注释） |
| 扫描被 `Check2ScanLimit`（默认 5000）截断或超时时仍标 `Passed=true`；check #8 **从未运行**（schema 特性缺失）也报 `Passed=true` | `CheckResult` 加 `Complete bool`（零值 = 未完成，fail-closed）；`ReconcileReport` 加 `FullCoverage bool`；`ReconcileCheckResult` metric 改报 `Passed && Complete`，使告警 fail-closed | `core/reconcile_extra.go`、`service/reconcile.go` |

契约面同步（`api-contract.md` §8，加字段 expand-safe）：`docs/openapi.yaml`、
`web/packages/ledger-react/src/client/schema.ts`。

**Pinned by**（两条钉子都验证过「未修复时确实失败」）：
- `service.TestCheck2GlobalBalance_ScansNegativeSystemHolders` — 未修复时只扫到 2/4 个 pair
- `service.TestFullReconciliation_Check2ScansSystemHolderCheckpoints` — DB 层钉子，走真实 SQL 谓词；
  未修复时只扫到 1 个 pair，系统侧 `+777` 的篡改**完全未被发现**
- `service.TestCheck2GlobalBalance_ScanLimitReportsPartialCoverage` — 断言 `Complete=false`
- `service.TestFullReconciliation_EmitsMetricsPerCheck` — 断言跳过的 check 不发绿色 metric

`make test` 全绿，`golangci-lint` 0 issues，`sqlc diff` 干净。

> ⚠️ **P0 未解决 C4b**：`Check2ScanLimit` 默认 5000，且 finding 文案自己写着
> "the next scheduled run rescans from the top" —— 超过 5000 个 (holder, currency) 对的部署
> 永远只验证同一个前缀，**队尾永不被扫**。修复需要持久化 resume cursor（要 migration），
> 归入 P2。P0 之后这个缺陷会以「持续 `FullCoverage=false`」的形式**诚实地暴露出来**，
> 而不再被静默折算成通过。

## 1. 威胁模型

| 攻击者能力 | 现状能做什么 | 本设计后 |
|---|---|---|
| **app DB 凭证**（Metabase session 泄漏 / SQL injection / 泄漏的 `DATABASE_URL`） | 改 checkpoint 即改余额；插入借贷不平衡的 entries；改 `normal_side` 翻转余额计算方向 | P1 后无 DDL；P2 后 checkpoint 不被信任；P3/P4 后 DB 层拦截；P5 后伪造 journal 无有效签名 |
| **DB owner / superuser** | `DROP TRIGGER` 后任意改写历史；`DROP` 分区静默丢整月 | 改写 → 签名/digest 验不过；丢分区 → 覆盖空洞；两者在 verify 中暴露 |
| **DB 备份 / 只读副本** | 读到全部账务明细 | 不变（见 non-goal 1） |
| **app 进程 + KMS 调用权限** | 铸造合法签名 | ⚠️ **不防**。见 non-goal 2 |

**Non-goals（明确不做，不要事后当缺口报）**：

1. **保密性**。这次事件若是「数据被读走」，那么复式记账与 Merkle **一条都不解决** ——
   它们证明数据没被改，不阻止数据被读。对症的是 P1 的只读 role、审计日志、缩小可查询面、
   凭证轮换、保留期收缩。本文只解决完整性 / 真实性 / 不可抵赖。
2. **app 进程 + 签名能力同时失陷**。攻击者能调签名就能给伪造记录签名。唯一缓解是留痕
   与限速，不是密码学保证 —— 而留痕放在哪里是**消费方**的部署选择（同 §14）。
3. **「从未被伪造」不能只靠事后机制**。batch digest / Merkle 会如实签署它看到的 DB 内容，
   包括窗口内刚被插入的伪造行。这正是 P5（写入时签名）必须先于 P6/P7 的原因。

## 2. 评审确认的缺陷清单

以下每条都经独立回查（文件:行号为回查依据），非推测。

### Critical

| # | 缺陷 | 依据 |
|---|---|---|
| C1 | **纯 SQL 可插入借贷不平衡的 entries**。004 的 DB 层 `trg_check_journal_currency_balance` 被 018 删除，检查移到应用层 `VerifyJournalBalanced`；`journals.chk_journal_balance` 只约束汇总列，与 entries 内容无关；018 的 append-only trigger 只拦 UPDATE/DELETE，**不拦 INSERT**。019–041 无重建 | `004_ledger.up.sql:33-70`、`018:143-165`、`journals.sql:128-137`、`ledger_store.go:608-618`、`037:7-10` |
| C2 | **checkpoint 篡改不自愈**。`newBalance = currentBalance.Add(delta)` 纯增量累加；`GetBalance` 无条件信任 checkpoint；`balance_checkpoints` 无 mutation trigger | `service/rollup.go:197-215`、`checkpoints.sql:6-18`、`ledger_store.go:747-812`、`005_checkpoints.up.sql:1-10` |
| C3 | 对账永远扫不到 system holder（负数） | ✅ **P0 已修** |
| C4 | 扫描不完整 / 检查被跳过仍标 `Passed=true` | ✅ **P0 已修**（C4b resume cursor 归入 P2） |

### Major

| # | 缺陷 | 依据 |
|---|---|---|
| M1 | **两笔互相抵消的不平衡 journal 可绕过 check #1**。名为 `journal_dr_cr` 的 check 实际调用全局会计等式，非逐 journal 验证。其注释声称「per-journal balance is enforced by DB constraints」—— 而那个 constraint 已被 018 删除，注释成了过期的错误依据 | `service/reconcile.go:323-350`、`checkpoints.sql:142-149` |
| M2 | **P6 不可拆成「先 digest 后锚」**。尾部截断不产生 seq gap；`verify` 第一步就是从外部锚取可信 head。attestation 只存在于失陷 DB 中时，owner 可截断最新 attestation 与对应 entries 而不留内部痕迹 | 本文 §8 |
| M3 | **Merkle 非必要条件**。签名 batch digest 已能检测整批变化；Merkle 只多给 inclusion proof 与定位 | 本文 §9 |
| M4 | `balance_snapshots` 仅在同日期重跑时自愈（worker 只反复生成 yesterday，旧日期不自愈）；`system_rollups` 直接聚合 checkpoints，**继承 checkpoint 污染** | `checkpoints.sql:151-170`、`snapshots.sql:1-5`、`worker.go:202-209`、`checkpoints.sql:177-184`、`system_rollup.go:62-78` |
| M5 | **账本内部现有不变量无法区分真/伪入账**（但这不是理论上的不可能）。genuine deposit 就是 `DR user.main_wallet / CR system.custodial`，直接 SQL 可插入完全相同的 balanced pair。I-9 只规定 holder 正负映射；I-12 只断言总 DR=CR；I-15/I-17 只在 `postJournalWithQueries` 内执行，直接 SQL 全部绕过。solvency 两边同量增加，内部 margin 不变 | `presets/templates.go:59-65,115-121`、`core/system_account.go:17-26`、`INVARIANTS.md:332-342`、`ledger_store.go:514-545`、`platform_balance_store.go:116-126,153-179` |

> **M5 是整份评审最有价值的一条**，因为它同时给出了反例：per-journal 授权签名可在
> **不依赖任何外部真值**的前提下拒绝伪造记录。这直接推翻了原稿「只能靠外部对账」的判断，
> 并把签名从「事后 batch」前移到「写入时 per-journal」。→ P5。

### 新发现的攻击路径（原稿完全漏掉，全部纳入 P4）

| # | 路径 | 依据 |
|---|---|---|
| A1 | **`classifications.normal_side` 是普通可变列**（只有 CHECK 约束值域）。改它直接翻转 rollup 的余额计算方向 | `002_classifications.up.sql:1-8`、`rollup.go` 的 normal_side switch |
| A2 | **`classifications.balance_role` 同理**。改它能把 `locked` 资金重新归入 `available` | `032_balance_role.up.sql:14-16` |
| A3 | **`reservations` 无 mutation trigger**。直接插入或放大 `reserved_amount` 可在**不产生任何 journal** 的情况下改变 available 余额 | `006_reservations.up.sql:1-18`、`reservations.sql:64-78` |
| A4 | **`journals.event_id` 的 "set-once" 保证不存在**。033 的函数体不比较 `event_id`，而 018:137-140 的 trigger 是无条件 `BEFORE UPDATE FOR EACH ROW` —— **没有 WHEN 子句**。033 注释里说的「WHEN clause below still permits」指向一条不存在的机制；该列甚至没有 FK。posting provenance 可被反复改写 | `033:13-32`、`018:137-140`、`014_journal_event_id.up.sql:1-2` |
| A5 | **`period_closes` 文档称 append-only，表上没有 trigger**。可改写「何时关账」的证据，而 I-15 只在应用层检查 | `026_period_closes.up.sql:1-15` |
| A6 | **`DROP TABLE` / detach 月分区可删整段历史**。仓库**没有任何 migration 创建 role**，而 `docs/RUNBOOK.md:419-437` 已经假定 `ledger_app` 存在并给出应急 `REVOKE`/`GRANT` —— runbook 在指挥一个不存在的东西 | `037:37-51`、`RUNBOOK.md:419-437` |

> ⚠️ **A4 的连带影响**：原稿 §4.3 推荐的「set-once 标记列」方案是拿「`journals.event_id`
> set-once 先例」类比论证的 —— **那个先例是假的**。新论证见 §8.2。

### 文档缺陷

- **I-1 高估了自己**：`INVARIANTS.md:21-34` 仍声称 deferred DB trigger 在执行，而 018 已删除它。
  这条过期声明正是 M1 那句错误注释的同源问题 —— 它会误导下一次同类分析。
  **P3 的交付物之一是修正 I-1**，明确「DB 不再 enforce per-journal/per-currency balance」。
- 原稿写的 I-23/I-24/I-25 是**未来设计项**，当前 `INVARIANTS.md` 最后一条是 I-21。
  实现与测试落地前不得写成已有保证。

## 3. P1 — DB role least-privilege

一切 DB 层强制的前提。三个 role，落在 `001_baseline`：

| Role | 权限 | 用途 |
|---|---|---|
| `ledger_owner` | 表 owner，DDL | 仅 migration 运行时 |
| `ledger_app` | 目标表 `SELECT/INSERT/UPDATE`；**无** DDL / `DROP` / `TRUNCATE` / `ALTER` / trigger 管理权 | `ledgerd` 与库模式消费方的 `DATABASE_URL` |
| `ledger_ro` | 全库 `SELECT`（优先只给聚合视图，不给明细表） | 报表 / BI / Metabase —— **这是那次事故应该走的 role** |

- `journal_entries` 的 `UPDATE` **不授予** `ledger_app`（现有代码从不 UPDATE entries）。
- `REVOKE ALL ON SCHEMA public FROM PUBLIC`，逐表显式 GRANT；`ALTER DEFAULT PRIVILEGES` 只给 owner。
- 分区表：GRANT 需覆盖父表 + 既有分区，并验证 `PartitionService` 新建分区继承权限。
- **同时修 A6 的文档债**：`RUNBOOK.md` 假定的 role 由本 migration 真正创建。
- **一步到位，不分阶段**（W4 拍板，见 `2026-08-21-integrity-hardening-contracts.md` Wave 4）：
  这个库没有历史用户，全新安装没有在座连接需要保护，所以 role 创建 + GRANT + `REVOKE ALL ON
  SCHEMA public FROM PUBLIC` + ownership 转移全部落在 `001_baseline` 一个 migration 里，不需要
  `deployment.md` 的 expand → migrate → contract 三个 release。

## 4. P2 — checkpoint 不可信化 + 提现门 + 外部资产对账

checkpoint **必须**能 UPDATE（rollup 的正常工作），所以不能用 trigger 保护 —— 这是它与
journal 的本质区别。唯一可行的是**可重算性**。

- **`RecomputeBalance`**：忽略 checkpoint，从 entry 0 全量重算。慢，但只在钱要离开系统时走。
  提现 / 大额路径强制走它。
- **`RebuildCheckpoint`**（受信任入口）：锁定 dimension → 全量重算 balance 与 watermark →
  **覆盖**现有 checkpoint。当前 upsert 只在 watermark 前进时写入且仍是增量结果（`checkpoints.sql:6-18`），
  无法修复污染。
  ⚠️ **检测与修复必须分离**：reconcile 只检测 + fail-closed，**不自动覆盖** ——
  攻击仍在进行时自动覆盖会毁掉取证证据。修复走显式 operator 动作。
- **C4b：持久化 resume cursor**。否则 `Check2ScanLimit`（默认 5000）之外的队尾永不被扫，
  P0 之后表现为持续 `FullCoverage=false`。
- **M4：`system_rollups` / `balance_snapshots` 纳入 entries-based 校验**。
  `system_rollups` 的校验必须**直接对 entries 重算**，不能拿 checkpoint 当「独立」基准
  （它正是污染来源）。snapshot 修复支持显式 date-range rebuild。
- **外部资产对账（solvency）**：托管钱包链上余额 vs 账本负债。这是 M5 场景下**唯一**
  能发现「凭空铸币」的手段（P5 落地前）。`platform_balance_store.go:116-126` 明确把与真实
  custody position 的比较留给消费方 —— 本 phase 要把它变成 ledger 侧的一等检查，
  并正确处理 crypto-deposit 设计 §5.4 的 unattributed 台账（否则对账信号失真）。

## 5. P3 — 平衡检查回到 DB 层

修 C1 + M1。

- 恢复 per-journal / per-currency 的 **deferred constraint trigger**，但用 004 之后学到的
  实现方式：**per-journal 一次聚合查询**，不是 004 的 per-row O(N²)。
  （应用层的 `VerifyJournalBalanced` 保留 —— 它给出更好的错误信息；DB 层是兜底。）
- **M1**：新增真正的 fleet-wide per-journal 检查，替换 `journal_dr_cr` 名下的全局等式：
  ```sql
  SELECT journal_id, currency_id
  FROM journal_entries
  GROUP BY journal_id, currency_id
  HAVING SUM(CASE WHEN entry_type='debit' THEN amount ELSE -amount END) <> 0;
  ```
  （全局等式检查保留为独立一项，两者语义不同，不可互替。）
- **修正 `INVARIANTS.md` I-1** 与 `reconcile.go:325-329` 的过期注释。
- 长期方向（本期不做，记录）：唯一 `SECURITY DEFINER` posting function +
  撤销 `ledger_app` 对 `journals`/`journal_entries` 的直接 `INSERT`。

## 6. P4 — mutation guard 清单

A1–A5 共享同一个根因：**参与余额计算的非-journal 表都没有保护**。一次成体盘点。

| 目标 | 措施 |
|---|---|
| `classifications.normal_side`（A1） | 加入 anti-update 保护列表。改它等价于重述全部历史 —— 若确需变更，走显式迁移而非 UPDATE |
| `classifications.balance_role`（A2） | 已有 `SetBalanceRole` 作为受控入口（`core/interfaces.go`，注释已说明这是 expand-style 升级）。DB 层加 guard，只允许经该入口的跃迁 |
| `reservations`（A3） | 加 mutation guard：`reserved_amount` 与 dimension 列 insert 后不可改；状态机跃迁走白名单 |
| `journals.event_id`（A4） | **补真正的 set-once**：加 FK，并把 `event_id` 纳入 033 的比较列表但放行 `NULL → 非NULL` 的单次跃迁（即 033 注释所描述、但从未实现的那个语义） |
| `period_closes`（A5） | 加 no-UPDATE / no-DELETE trigger，兑现文档声称的 append-only |
| 分区 DROP（A6） | 主要靠 P1 的 role 分离；覆盖完整性由 P6 的外部锚兜底 |

每条都要有对应的 invariant + pin test（§10）。

## 7. P5 — per-journal 授权签名（主线）

这是唯一在**写入时**确立真实性的机制，也是 M5 反例的落地。

### 7.1 结构

migration `043`：`journals` 加三列
```sql
ALTER TABLE journals
    ADD COLUMN auth_digest    BYTEA NOT NULL DEFAULT '',
    ADD COLUMN auth_signature BYTEA NOT NULL DEFAULT '',
    ADD COLUMN auth_key_id    TEXT  NOT NULL DEFAULT '';   -- KMS key version，支持轮换
```
三列一并纳入 033 的 anti-update 保护列表（新增 journals 列**必须**同步该函数 —— 033 已把这条写成规则）。

```go
// core/interfaces.go
//
// Attestor abstracts the key that authorizes a posting. The private key lives
// in a KMS/HSM and never enters the database or the app config: a DB
// compromise must not yield the ability to mint a valid authorization.
// Deliberately distinct from core.Signer (EVM sweep transactions) -- different
// key, different blast radius, never the same instance.
type Attestor interface {
    Sign(ctx context.Context, digest []byte) (signature []byte, keyID string, err error)
}

// AuthVerifier only needs the public key, so verification can run entirely
// outside the database host -- that independence is the whole point.
type AuthVerifier interface {
    Verify(ctx context.Context, digest, signature []byte, keyID string) error
}
```

### 7.2 canonical digest 在 uid-space，不在 id-space

⚠️ **红线冲突与解法**：`financial.md` 禁止在 DB 事务内做外部调用，而 KMS 签名是外部调用。
若 digest 基于内部 id，就必须等 `resolveEntries` 之后（事务内）才能算 —— 直接违规。

解法：digest 基于 **uid-space 的 `core.JournalInput` 原始形态**：

```
payload = journal_type_uid || idempotency_key || actor_id || source || event_uid
        || effective_at(RFC3339 UTC) || reversal_of_uid
        || sorted(entries: account_holder, currency_uid, classification_uid,
                           entry_type, amount_scaled)
digest  = SHA256(0x01 || len-prefixed fields...)
```

于是流程变成：**事务外**算 digest → **事务外**调 KMS 签名 → **事务内**只写入
`(auth_digest, auth_signature, auth_key_id)`。红线不破。

附带好处：uid-space 是对外契约（`project_v04_uid_contract`），比内部 id 稳定；
验证时从 DB 行 join 回 uid 即可重建，不依赖内部 id 是否被重排。

`amount_scaled` 沿用定点编码规则：定标到**恰好 18 位小数**的定点整数，二进制补码
big-endian **固定 16 字节**（`NUMERIC(30,18)` 值域装得下）。**禁止** `decimal.String()`
（尾零/指数表示不确定）、禁止 float、禁止变长编码。编码变更 = 破坏性变更，
新版本用新的 domain separator（不复用 `0x01`）。

### 7.3 幂等与可用性

- **幂等重放**：same key + same payload ⟹ digest 相同 ⟹ **签名可直接复用**，不重签。
  这与三态幂等（`feedback_idempotency_tristate`）天然吻合：payload 不同 ⟹ digest 不同 ⟹
  本就该 `ErrConflict`。
- **签名失败**：**不做 fail-open/fail-closed 配置**（2026-08-21 Aaron 拍板：过度设计）。
  单体部署下签名是进程内本地操作，失败就是 error 往上抛（`discipline.md` §6）。
  将来若换远程签名 adapter，重试/降级是**那个 adapter 内部的事**，不进 port 语义。
  但**必须**避免"以为在签其实没签"（`working-agreements` §3）：`Attestor` 为 nil ⟹
  特性整体关闭且三列留空（验证侧判为"特性未启用"，不是"验签失败"）；配了密钥就必须
  加载成功，否则启动报错。二者取其一，实现里写清楚。
- **验证点**：提现门、对账新增 check、`ledger-cli verify`。伪造 journal 无签名或验不过 ⟹ 拒绝。

### 7.4 边界（诚实说明）

per-journal 签名防的是**拿到 DB 写权限**的攻击者。拿到 **app 进程 + 签名能力**的攻击者
可以铸造合法签名（§1 non-goal 2）。这不削弱本机制 —— 它把攻击门槛从「一条 SQL」
抬到「拿下应用运行时」，并留下 KMS audit log。

### 7.5 `RunInTx` 缺口 —— 本节的 Critical 修正（2026-08-21）

⚠️ **§7.2 只规定了「digest 在 uid-space、签名在事务外」，从没处理它怎么和 `RunInTx` 组合。**
而 `RunInTx` 是这个库的旗舰特性，也是 CLAUDE.md 为 Event-Journal 原子性（I-10）推荐的唯一模式。

首版实现在 tx 模式（`s.pool == nil`，即调用方已开事务）下**故意跳过签名** —— 理由正确：
事务已经被别人开了，没有安全点可以调外部签名而不违反 `financial.md`。但后果致命：

```
service/onchain.go:779  postDepositConfirmedJournal → TxComposer.RunInTx
```

那是**入账过账路径** —— 正是 P5 存在的理由（§2 的 M5：伪造入账 = 凭空铸币）。
所以 P5 首版形态**恰好不保护它被设计来保护的那条路**。

更糟：未签名状态与「Attestor 未配置」**不可区分**。验证时无法分辨
「签名上线前的历史 journal」/「走 `RunInTx` 所以没签」/「攻击者直接 INSERT 的行」——
整个验证故事在这一类上塌掉（`working-agreements` §3：不可区分就是静默失败）。

**修法（两件，Lead 拍板，board #12）**：

1. **预授权 API**：`svc.Authorize(ctx, input) (AuthorizedJournal, error)` 在**事务外**算 digest
   并签名；`JournalWriter` 加 `PostAuthorized(ctx, AuthorizedJournal)`。
   `postDepositConfirmedJournal` 在开 `RunInTx` **之前**调 `Authorize`。

   ⚠️ **原稿这里写「输入是已知的」，错了一半**（2026-08-21 由 #13 的实现者发现并上报）：
   对 `journalMeta`（actor / metadata）成立，对 **`EventUID` 不成立**。
   `core.TransitionInput`（`core/booking.go:66-80`）没有 event-uid 字段，
   event uid 由 `postgres/booking_store.go:138/327` 的 `newUID()` 在**锁住 booking 行之后、
   事务内**才 mint —— `Authorize` 在事务外根本拿不到它。

   **裁决：把 `EventUID` 从 canonical digest 里整体移除**（见 §7.6）。
2. **`auth_status` 枚举列**（migration `051`）：`signed` / `unsigned_no_attestor` /
   `unsigned_tx_mode` 三态，让「为什么没签」可区分。
   该列由 045 的通用 `to_jsonb` 比较自动保护（不在 mutable 白名单内），无需再碰那个函数。

**范围红线**：**不新增任何配置旋钮** —— 无 policy、无 threshold、无 mode。
只有两个方法 + 一个枚举列。这一条是刻意的：本设计已经因为围绕部署变量建配置而返工过一次
（§14 开头那段）。

### 7.6 为什么 `EventUID` 不进 digest（2026-08-21 裁决）

被否决的方案是「签名时 `EventUID` 留空、事后只填 FK 不重签」。它会**破掉验证**：
`core/auth.go:260` 的 `VerifyJournalAuth` 是拿 `JournalInput` **重算** digest 再比对存储值。
签名覆盖的是 `EventUID=""`，而落库的行有 `event_id` ——
验证方从行重建 input 时会带上真实 EventUID ⟹ 重算 digest ≠ 存储 digest ⟹
**每一笔走 `RunInTx` 的入账 journal 都会被判 `ErrUnauthorizedJournal`**。

假阳性比缺失签名更糟：它让所有合法入账看起来像伪造，验证结论整体不可用。
要救它就得引入**第二种 digest 形状 + 一个区分标记**，而编码是本设计里**唯一改不掉**的东西
（§11 golden vectors 那条），现在分叉是最贵的错误。

**所以 `EventUID` 对所有 journal 一律不进 digest。** 理由不是「不得不」，而是它本来就不该在里面：

- 签名认证的是 **posting intent** —— 谁、何时生效、哪些账户、多少钱、幂等键、是否冲正。
  「哪个 event 触发了它」是 **provenance 元数据**，不是意图的一部分。
- event↔journal 的绑定**已经有比签名更强的保证**：I-10 的同事务（两者同写或同不写）
  + 045 的 set-once FK。签名加不了原子性。
- 统一一种形状 ⟹ 不需要标记、不需要版本化重建逻辑、验证是确定性的。
- 附带好处：**不需要动 `core.TransitionInput`**，P5-fix 的范围红线（两个方法 + 一个枚举列）守住。

**残留代价（disclosed limitation，写进 I-26）**：拿到 DB 写权限的攻击者可以给一条原本没有
event 的 journal 设上 `event_id`（045 允许 NULL→非NULL 一次），从而伪造 provenance 归属。
它**无法移动任何资金** —— 金额与账户在签名覆盖范围内。

**测试要求**：golden vector 不是删掉「event_uid 已设」那条，而是改成断言
**「设了 `event_uid` 不改变 digest」** —— 把这个决定钉成测试，将来有人想加回去会先撞上它。

## 8. P6 — 签名 batch digest + 外部锚（同一个 phase）

覆盖 P5 管不到的两件事：**行是否被删除**、**历史是否被改写**。

### 8.1 结构

migration `044`：`ledger_attestations`
```sql
CREATE TABLE ledger_attestations (
    id          BIGSERIAL PRIMARY KEY,
    uid         UUID   NOT NULL UNIQUE,
    seq         BIGINT NOT NULL UNIQUE,   -- 断号 = 中段截断
    entry_count BIGINT NOT NULL,
    batch_digest BYTEA NOT NULL,          -- 本批 entries 的 canonical 摘要
    prev_root    BYTEA NOT NULL,          -- 链接 seq-1；创世为 32 字节 0
    root_hash    BYTEA NOT NULL,          -- 被签名的摘要
    signature    BYTEA NOT NULL,
    key_id       TEXT   NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```
本表同样挂 no-UPDATE / no-DELETE trigger（复用 018 的 `ledger_block_mutation()`）。
**空批照样出一条**（`entry_count = 0`），否则「静默不产出」与「job 没跑」不可区分
（`working-agreements` §3）。

### 8.2 覆盖完整性：水位线（原稿论证已失效，重新论证）

I-5 的载重前提只保证「commit 序 = id 序」**在同一 `(holder, currency)` 内**成立。
跨 pair 无全局序 ⟹ 存在「id 较小但 commit 较晚」的 entry。若 batch 取 `to_entry_id = MAX(id)`，
迟到的小 id entry 会**永久漏在覆盖之外，且 seq 连续性检查发现不了**。

原稿推荐「`journal_entries` 加 set-once 标记列」，理由是类比 `journals.event_id` 的
set-once 先例 —— **A4 证明该先例不存在**。改为推荐**旁表**：

```sql
CREATE TABLE entry_attestations (
    entry_id BIGINT NOT NULL,
    seq      BIGINT NOT NULL REFERENCES ledger_attestations(seq),
    PRIMARY KEY (entry_id)
);
```

理由（比原稿的类比论证更强）：
1. **不需要放宽 `journal_entries` 的 no-UPDATE trigger** —— 那是当前仅存的几条硬保证之一，
   为了一个标记列去开口子不值得（A4 恰好说明「开了口子的 guard」会怎样腐化）。
2. 旁表自身可以是 append-only + trigger 保护。
3. 「是否被覆盖」成为可查询的显式事实（`LEFT JOIN ... WHERE seq IS NULL`），
   不依赖 id 区间推理；迟到的小 id entry 会被下一批捡走。
4. 代价：一次 join、一张表。分区表 UPDATE 的 IO 成本反而省下了。

### 8.3 外部锚（与 8.1/8.2 同一 phase，不可延后 —— M2）

- **库交付 `Anchor` port + 本地文件实现**（dev/test/example 用）。**载体是消费方的部署决定，
  本设计不规定。**（2026-08-21 Aaron 纠正：ledger 是库，没有自己的部署 ——
  规定消费方用哪个云、哪个账号、什么 retention，不是库该做的事。）
- **库要规定的是「锚必须具备的性质」**，这三条是消费方选载体时的验收条件：
  1. **在 DB 凭证触不到的地方** —— 泄漏 `DATABASE_URL` 的同一次事故不能连锚一起带走。
     这一条排除了「同一个数据库里的另一张表」，也排除了同一套凭证能写的存储。
  2. **写入后不可改**（append-only / 不可变），否则攻击者改锚即可让篡改自洽。
  3. **可独立读取** —— 验证方不经过账本自己的服务就能拿到 head。
- 满足这三条的载体有多种（对象存储的不可变模式、独立账号下的存储、公链、
  甚至一台不同信任域的机器）。**选哪个由消费方按自己的 threat model 与合规要求定**，
  库只需要一个满足 port 契约的 adapter。
- 内容（几十字节）：`{seq, root_hash, signature, key_id, anchored_at}`。
- 失败处理：本地重试队列 + 告警，**不阻塞 journal 写入**（锚定是旁路，不能成为账本可用性依赖）。
  连续失败超阈值只影响 verify 判定与提现门。

### 8.4 验证路径（`ledger-cli verify`）

```
1. 从外部锚拉最新 head          —— 不信 DB
2. 校 attestation 链：seq 连续 / prev_root 链接 / 每条签名用公钥验
3. 从 DB entries 重算 batch_digest 比对；LEFT JOIN 找未覆盖的 entry
4. 抽样/全量验 per-journal auth_signature（P5）
5. 校 checkpoint 可重算性（P2）
6. 输出 VERIFIED | DRIFT | TAMPERED | NOT_RUN
```

**红线**：`NOT_RUN ≠ VERIFIED`。KMS 不可达 / 公钥缺失 / 外部锚拉不到 / 超时 ⟹ 一律 `NOT_RUN`，
**fail-closed**。这与 P0 刚修的 `Complete`/`FullCoverage` 语义是同一条纪律的两处落地。

## 9. P7 — Merkle tree（**在范围内**，Aaron 2026-08-21 拍板）

> 原稿把这一节列为「仅当需要 inclusion proof 或定位时才做」。**两样都需要**，所以 P7 转正。

签名 batch digest（P6）已能回答「这批有没有被改」。P7 要交付的是它答不了的两件事，
**它们是两个独立能力，各有各的工作量，不要当成 Merkle 的免费副产品**：

### 9.1 定位（localization）—— 给 on-call 用

P6 只能告诉你「seq 137 这批对不上」。一批可能有几千条 entry。
Merkle 让你沿树下降，把不一致收敛到具体叶子：

- 从根开始比对左右子树 hash，只沿不匹配的分支下降 —— O(k log n)，k = 被改条数。
- 输出必须是**具体 entry id 列表**，不是「某个区间里有问题」。
- 接进 `ledger-cli verify`：`TAMPERED` 判定要附上被改的 entry 清单。

### 9.2 Inclusion proof —— 给第三方用

证明「某笔 journal 确实在账本里」而**不暴露其余账目**，且验证方**不需要访问数据库**：

- 生成：给定 entry id → 返回 audit path（兄弟 hash 序列 + 叶子索引 + 所属 seq 的 root）。
- 验证：`VerifyInclusion(leaf, path, index, root) bool` —— **纯函数，零依赖**，
  第三方可以只拿这个函数 + 外部锚上的 root 独立验证。这是「不需要访问数据库」的兑现。
- 红线：audit path **不得泄露其他 entry 的内容**（兄弟节点只给 hash，不给 payload）。

### 9.3 实现约束

- 采纳 **RFC 6962**（Certificate Transparency）规范，不自创：
  `leaf = SHA256(0x00 || payload)`、`node = SHA256(0x01 || l || r)`，
  奇数叶子**不复制末节点**（防 CVE-2012-2459 类二义性）。
- `ledger_attestations.batch_digest` 换成 `merkle_root`，其余结构不变 ——
  P6 刻意留了这个接口位。migration `048`。
- **编码换 domain separator**：P6 的 batch digest 与 Merkle root 不是同一个东西，
  不要复用同一个 separator（`0x02`），否则历史 attestation 与新的无法区分。
- 叶子的 payload 复用 P5 的 `EncodeAmount` 与字段顺序纪律（同一套 golden vectors 约束），
  **不要另起一套编码**。
- 必须对拍 **RFC 6962 的公开测试向量**，不只测自洽。

### 9.4 两处规格补正（2026-08-21，源：P7 实现者主动披露）

P7 的实现者交付时 flag 了两件它没有单方面解决的事。两件都不是留白，是**我的规格漏了前提**。

#### (1) `merkle_root` 必须进签名域 —— 否则 inclusion proof 的根没有外部背书

首版把 `merkle_root` 加成新列，但它**不是 `AttestationRootHash` 的输入** ⟹
外部锚上的 head 根本没有 attest 这个 root。第三方拿到的 root 只受
append-only trigger + ACL 保护 —— 那是**数据库内部**的保护，
而 inclusion proof 的全部意义就是「验证方不需要信任数据库」。

那样交付出去的能力是「证明某笔在一棵**我们说是这样的**树里」，不是
「在一棵被签名并锚定到 DB 之外的树里」。

**修正**：新版 root hash 把 `merkle_root` 绑进去，separator `0x11`（契约 §2.6）。
P6 已有的 attestation 无 `merkle_root`，保持 v1（`0x03`）；048 之后新建的用 v2。
verify 处理两种版本 —— 与 auth digest V1/V2 同一模式。**不改 `0x03` 的语义**，
那是历史行的验证规则。

#### (2) 定位必须自包含 —— §9.1 少写了一个存储前提

§9.1 要求「`TAMPERED` 判定要附上被改的 entry 清单」。实现者指出：只存聚合 root、
不存第二棵树时，DB 自己**不可能**产出这个清单 —— 这是对的，而且它选择了
**没有外部参照时诚实报告「没有清单」，而不是编一个**。

但 schema 可以改。**在 `entry_attestations` 存每条 entry 的 `leaf_hash`**（32 字节/entry）：
定位时从 entries 重算叶子、与存储叶子逐一比对，直接得到 entry id 清单。

关键是**存储的叶子本身被签名保护**：改一个存储叶子会改变由它算出的 root，
与签名的 root_hash 不符 ⟹ 被检出。所以「entry 和存储叶子一起改」无法逃过。
CT 就是这么做的（它存整棵 log）。

**为什么这不是可选优化**：这条能力的用户是 **on-call**。一个「要求 operator 事先已有
可信外部快照」的能力，在真正需要它的时刻（刚发现被篡改）恰好最可能没有那份快照。
`ReferenceEntries` hook 保留作为第二条路（与异地副本对照）。

#### 附：RFC 6962 对拍缺口已由 Lead 补上

实现者诚实标注了「本环境无网络，未能对拍官方 RFC 6962/CT 测试向量」，
并且**没有编造「官方」数据**。Lead 用一个按 §2.1 递归定义独立写的第三实现，
在 CT 经典 8-entry 测试 log 上对拍 n=0..8 全部通过
（`core.TestMerkleTree_RFC6962TestLogRoots`）。
价值在于：实现者的 Go 与其 Python 出自同一人，能抓转录/结构错误，
抓不到两边共享的**规范误读**。

## 10. 新增不变量（按 `INVARIANTS.md` §How to add 流程）

| ID | 规则 | 归属 |
|---|---|---|
| I-22 | `ledger_app` role 无 DDL / DROP / TRUNCATE 权限 | P1 |
| I-23 | checkpoint / system_rollups / balance_snapshots 可由 entries 精确重算，drift = 0；检测不自动修复 | P2 |
| I-24 | 每个 journal 在 DB 层 per-currency 借贷平衡（不依赖应用层） | P3 |
| I-25 | 参与余额计算的非-journal 表（classifications 的 normal_side/balance_role、reservations、period_closes）insert 后不可任意改 | P4 |
| I-26 | 签名特性启用时，每个 journal 携带有效授权签名；验签失败的 journal 不可提现 | P5 |
| I-27 | attestation 链完整：seq 连续、prev_root 链接、签名有效、**每条 entry 恰好被一个 attestation 覆盖** | P6 |
| I-28 | 最新外部锚 head 与 DB attestation 链一致 | P6 |

⚠️ 每条都必须先有 pin test 才能写进 `INVARIANTS.md`（P0 的四条已满足该标准）。

## 11. 测试策略

- **Golden vectors（最高优先级）**：`payload` 编码 —— 金额定标（18 位边界、负零、最大值）、
  时间编码、字段顺序、空 entries。任何 diff = 破坏性变更。
- **篡改测试**（testcontainers 真 PG，用 owner role 绕过 trigger 制造篡改）：
  改 entry amount → batch digest 不符；插入无签名 journal → P5 拒绝；`DROP` 分区 → 覆盖空洞；
  改 checkpoint → I-23 fail；改 `normal_side` → I-25 fail；伪造 attestation → 验签不过。
- **迟到 entry 测试**（§8.2 的失效模式，**必须有**）：并发两个不同 `(holder,currency)` 的 journal，
  人为让小 id 后 commit，断言其最终被覆盖且不重复覆盖。
- **两笔抵消的不平衡 journal**（M1）：断言新 per-journal check 抓到，旧全局等式抓不到。
- **`NOT_RUN` 测试**：公钥缺失 / 外部锚不可达时 `verify` 返回 `NOT_RUN` **且提现被拒**，不是放行。
- **性能回归**：`postgres/benchmarks_test.go` 的 `PostJournal` bench。
  已实测（2026-08-21，M3 Max + testcontainers loopback）：`PostJournal` 2.55–2.62 ms、
  `ReserveSettle` 2.24 ms、`GetBalance` 0.74 ms；`postJournalWithQueries` 对一个 2-entry
  journal 已做 **16 次串行 DB 往返**（`9 + D + D' + 2N`，典型 deposit/withdrawal 15–20 次）。
  单体部署下一次本地签名是微秒级，**不在这个噪声之上** —— 延迟不是本设计的门槛。
  仍需回归确认没有意外退化。

## 12. 分阶段与回滚（`deployment.md`）

| Phase | 变更性质 | 回滚 |
|---|---|---|
| P0 | 已完成。加字段（expand-safe）+ 修游标 | 回退 commit |
| P1 role | 落在 `001_baseline`，一步到位（W4 拍板，全新安装无在座连接需要保护） | 回退 `001_baseline` 的 down 脚本 |
| P2 | 新增 check + 受信任 rebuild 入口（只报告不自动改） | 关 job |
| P3 | DB trigger 恢复 = 行为变更（此前能写入的不平衡数据会被拒）。**上线前必须先跑一次全量 per-journal 检查并清理存量违规** | 删 trigger |
| P4 | trigger 新增；若存量数据依赖某列可改，先盘点 | 删 trigger |
| P5 | 三列 expand-safe（default ''）；Attestor 为 nil ⟹ 行为与今天一致。**提现门要求签名是行为变更**，单独 release | 关掉提现门 |
| P6 | 旁路，失败不阻塞写入 | 停 job |
| P7 | 换列语义，走新 domain separator | 停 job |

**Fence**：删除历史 attestation 行 = 永久失去该期证明，事后无法补签（KMS 会签，
但签的是「现在的 DB 内容」，不是当时的）。任何清理动作打 `breaking-rollback-fence`。

## 13. 本期不做

- 保密性 / 字段级加密（§1 non-goal 1）。
- 防「app + KMS 同时失陷」（§1 non-goal 2）。
- 除本地文件外的任何 `Anchor` adapter（生产载体由消费方按 §5 的三条性质自选并实现）。
- 第三方自助验证 endpoint。
- `bookings` / `events` 进 attestation（先只覆盖 journals + entries）。
- 签名密钥轮换自动化（`auth_key_id` 列已留，轮换走 runbook）。
- `SECURITY DEFINER` posting function + 撤销 app role 的直接 INSERT（P3 记录为长期方向）。

## 14. 消费方契约（原「待拍板」，2026-08-21 重写）

> **本节曾经是一份「等 Aaron 拍板」清单，那个框架是错的。**
> `ledger` 是**库**（`projects.md` 登记为共享库，被产品 import）。它没有自己的部署，
> 所以「用哪个云账号」「retention 多长」「阈值填多少」「什么时候切 `DATABASE_URL`」
> 全部是**消费方的决定**，不是库的未决项。
>
> 我在 KMS 那一轮已经被纠正过一次（见本节原有的 KMS 说明），却没有把教训推广到外部锚 ——
> 同一个错误犯了两次：**把部署变量当成设计门槛，并列进自己的待办**。
>
> 本节现在只列**消费方接入时必须自己提供/决定的东西**，以及库对每一项的契约。
> 库侧没有未决项。

| 消费方必须提供 | 库的契约 | 不提供的后果 |
|---|---|---|
| `core.Attestor` 实现（签名密钥） | port 已定；库附本地 ed25519 实现可直接用 | nil ⟹ 签名特性整体关闭，`auth_status` 记为 `unsigned_no_attestor`，**不会静默假装签了** |
| `core.AuthVerifier`（公钥侧） | 只需公钥，可在 DB 之外独立运行 | 缺失 ⟹ `verify` 返回 `NOT_RUN`，**不折算成通过** |
| `core.Anchor` 实现 | port 已定；库附本地文件实现（dev）。载体须满足 §5 的三条性质 | nil ⟹ attestation 只存在于 DB 内，尾部截断无法检出；`verify` 报 `NOT_RUN` |
| 提现门阈值等策略值 | 注入配置；缺省即启动报错，不默认放行 | 启动失败（刻意的：这类默认值等于信任边界敞开） |
| 接入前跑一次全量 per-journal 扫描 | 库提供该扫描（`journal_dr_cr` check + `integrity_balance.sql`） | 存量若有不平衡 journal，balance trigger 会让上线即拒写 |
| 跑 `001_baseline` 的连接能 `CREATE ROLE`（superuser 或 `CREATEROLE`） | baseline 一步创建 role + GRANT + `REVOKE ALL ON SCHEMA public FROM PUBLIC` + ownership 转移，无需消费方协调发布节奏；`ledgerd` serving pods 用 `ledger_app` 凭证 | 用不能 `CREATE ROLE` 的连接跑 baseline 会在建 role 那步直接报错，不存在「部分应用」的中间态 |

### 一条库对部署方的硬约束（不是选项）

签名密钥**不得与 `DATABASE_URL` 存放在同一个 secrets store / env bundle**。否则泄漏 DB
凭证的同一次事故会连密钥一起带走，整套签名失效。代码里无法强制（库看不见消费方的
secrets 布局），所以写在 `Attestor` 的 doc comment 里。

### 库侧真正剩下的（都不阻塞消费方接入）

- `ledger_ro` 目前是全库明细 `SELECT`，§3 想要的聚合视图尚未提供 —— 已在 RUNBOOK 记为待办。
  影响：BI 角色比理想状态宽，但仍远小于一个 superuser session。
- pin 的族引用（`Foo_*`）只要一个成员存活即算通过（`core.TestInvariantsDocPinsAllExist`
  的已披露局限）。

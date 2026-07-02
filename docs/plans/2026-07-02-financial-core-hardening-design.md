# ledger v0.3 — Financial-Core Hardening Design（期末关账 / 舍入策略 / 账户策略 / 部分操作）

**Created**: 2026-07-02
**Status**: Draft — pending Aaron review
**Repo**: `github.com/azex-ai/ledger`
**前置**: 2026-07-02 深度 review（四区域缺口分析）+ 同日修复批次（migration 001-024、`fix-core/storage/service/http` 已合入 main）

---

## 0. 背景与约束

深度 review 结论：并发正确性内核扎实（advisory-lock 串行化、幂等三段式、checkpoint+delta），
但对照 TigerBeetle / Modern Treasury / Formance 这一代账本系统，**记账语义表达力**有一批
结构性缺口。缺陷类问题已在上一批修复；本文档设计剩余的「代际能力」。

**关键约束变化（2026-07-02 Aaron 拍板）**：

> 当前无外部用户，允许直接强制迁移，不考虑数据兼容性。

因此本设计**全部采用单步破坏性迁移**（不走 deployment.md 的 expand/migrate/contract
三段式），Go API 允许直接破坏签名。但 `financial.md` 硬约束不放松：append-only journal、
per-currency 借贷平衡、幂等、decimal 精度依然是底线。

**目标**：把 ledger 打磨成可长期服务多项目的金融账本基础库。每个能力都要落成
`docs/INVARIANTS.md` 里可测试的新不变量。

### Non-goals（本轮不做）

- 分布式/多库部署（单 PostgreSQL 仍是唯一后端）
- 计息（interest accrual）、分期、复杂金融产品
- UI 层的 maker-checker 工作流（仅在 backlog 段落记设计方向）

---

## 1. Effective Date — 业务日期与入账日期分离

### 问题

`journals` 只有 `created_at`（入账时间）。回溯记账（月末收到上月账单、迟到的链上确认）
无法表达业务归属期；所有按时间的报表只能按写入时间切。这也是期末关账（§2）的前置。

### 设计

**Schema（migration 025）**：

```sql
ALTER TABLE journals
    ADD COLUMN effective_at TIMESTAMPTZ NOT NULL DEFAULT now();
-- 反规范化到 entries，让 as-of 聚合不需要 join journals
ALTER TABLE journal_entries
    ADD COLUMN effective_at TIMESTAMPTZ NOT NULL DEFAULT now();
-- 存量数据强制回填（无外部用户，直接 UPDATE）
UPDATE journals SET effective_at = created_at;
UPDATE journal_entries je SET effective_at = j.effective_at
    FROM journals j WHERE je.journal_id = j.id;
-- as-of / trial-balance 查询走 effective_at
CREATE INDEX idx_entries_currency_effective
    ON journal_entries (currency_id, effective_at);
```

**core**：

- `JournalInput.EffectiveAt time.Time`（零值 → `now()`，与 No NULL 策略一致）
- `TemplateParams.EffectiveAt` 同步透传
- 校验：**不允许未来时间**（> now + 5min 时钟容差 → `ErrInvalidInput`）。
  未来记账（scheduled posting）是另一个 feature，本轮不做。
- 反冲 journal 的 `effective_at` 默认 = 反冲发生时刻（now），**不继承原 journal**——
  这是关账后修正的标准做法（修正记在当期，见 §2）。

**语义边界（重要）**：

- **实时余额语义不变**：`checkpoint + delta` 仍按 entry id（提交顺序）滚动。
  `effective_at` 只影响**报表口径**，不参与余额时序——否则回溯记账会让 checkpoint
  机制失效。
- as-of 查询（`ListBalancesAt`、balance trends、daily snapshots、§2 trial balance）
  切换到 `effective_at` 口径。`ListBalancesAt` 的 `WHERE created_at < $1` 改为
  `WHERE effective_at < $1`。
- daily snapshot 的「当日」定义改为 effective 口径；backfill 逻辑同步调整。

### 新不变量

**I-14: Effective date 一致性** — `journal_entries.effective_at` 恒等于所属
journal 的 `effective_at`；effective_at 不晚于 created_at + 时钟容差。
Pinned by: 迁移后 schema 测试 + PostJournal 集成测试。

---

## 2. Accounting Period Close — 期末关账

### 问题

没有关账线：任何历史时点的报表都可能被后续回溯写入改变，「截止某期的对账」无从谈起。
审计/财报场景必须有「某日期之前的账已封版」的保证。

### 设计

**Schema（migration 026）**：

```sql
-- append-only 关账线历史。生效线 = created_at 最新的一行（latest-row-wins），
-- 允许 reopen（写一行更早的 close_before），全程留审计痕迹。
CREATE TABLE period_closes (
    id           BIGSERIAL PRIMARY KEY,
    close_before TIMESTAMPTZ NOT NULL,  -- effective_at < close_before 的记账被拒绝
    note         TEXT NOT NULL DEFAULT '',
    actor_id     BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_period_closes_created ON period_closes (created_at DESC);
```

**语义**：

- `PostJournal`（含模板/批量/反冲路径）在事务内读取生效关账线，
  `effective_at < close_before` → 新哨兵 **`ErrPeriodClosed`**。
  读取成本：单行索引查询（表极小、变更极低频），不做进程内缓存（避免多副本失效问题）。
- **关账后如何修正**：不允许改历史（I-2 不动摇）。修正 = 在当期反冲 + 重新入账
  （反冲 journal 的 effective_at = now，落在开放期间）。写入 COOKBOOK 菜谱。
- **Reopen**：写入一行更早的 `close_before` 即可（latest-row-wins）。自用阶段允许；
  等有外部消费者后可以再收紧为单调不可逆 + 需要双人复核。

**接口（core，消费方定义）**：

```go
type PeriodCloser interface {
    ClosePeriod(ctx context.Context, input ClosePeriodInput) (*PeriodClose, error) // append 一行
    ActiveCloseLine(ctx context.Context) (time.Time, error)                        // 零值 = 从未关账
    ListPeriodCloses(ctx context.Context, limit int) ([]PeriodClose, error)
}
```

**HTTP**：`POST /api/v1/periods/close`、`GET /api/v1/periods/closes`。

### Trial Balance（试算平衡表 — 关账的验收工具）

`GET /api/v1/reports/trial-balance?currency_id=&as_of=`：

- 按 classification 聚合 `effective_at <= as_of` 的 debit/credit 总额，
  按 normal side 计算净额，输出总借=总贷校验行。
- 实现：`journal_entries` 上的 SQL 聚合（走 §1 新索引）。大账本下这是重查询，
  加与 reconcile check #2 相同的 timeout 保护；后续可基于 daily snapshot 加速（backlog）。
- facade：`TrialBalanceReader`，同时给 `ledger-cli` 加 `trial-balance` 子命令。

### 新不变量

**I-15: 关账线之前的账本不可变** — 不存在 `effective_at` 早于生效关账线且
`created_at` 晚于该关账线创建时间的 journal。
Pinned by: 集成测试（关账 → 回溯记账被拒 → reopen → 通过；反冲落当期）。

---

## 3. Currency Exponent & Rounding Policy — 货币精度与舍入策略

### 问题

`currencies` 只有 id/code/name/is_active。JPY(0 位)、USD(2 位)、USDT(6 位)、
wei(18 位) 的合法小数位无处声明；`NUMERIC(30,18)` 是存储精度不是业务精度——
一笔 `0.001 JPY` 的 entry 今天可以入账。库也没有提供任何舍入/分摊原语，
调用方各自手搓 = 各自出 bug。

### 设计

**Schema（migration 027）**：

```sql
ALTER TABLE currencies
    ADD COLUMN exponent SMALLINT NOT NULL DEFAULT 18
    CHECK (exponent >= 0 AND exponent <= 18);
-- 存量 currency 保持 18（最宽松，不破坏现有数据）；部署方自行 UPDATE 收紧。
```

**校验（store 层，PostJournal / Reserve / Pending 全部写路径）**：

- entry.Amount 的小数位数 > 该 currency 的 exponent → 新哨兵 **`ErrPrecisionExceeded`**。
- 判定：`amount.Equal(amount.Truncate(exponent))`（不允许静默截断/舍入——舍入是
  调用方的显式决策，库只拒绝）。
- currency 元数据在写路径本来就要校验存在性，增量成本 ≈ 0。

**舍入与分摊原语（新文件 `core/money.go`，纯函数，零依赖）**：

```go
type RoundingMode int
const (
    RoundHalfUp   RoundingMode = iota // 默认：四舍五入
    RoundHalfEven                     // 银行家舍入
    RoundDown                         // 向零截断（平台让利方向明确时用）
    RoundUp                           // 远离零
)

// Round 把 d 舍入到 exponent 位小数。
func Round(d decimal.Decimal, exponent int32, mode RoundingMode) decimal.Decimal

// Allocate 把 total 按 weights 分摊成 len(weights) 份，每份 ≤ exponent 位小数，
// 且 Σ份额 == total（largest-remainder 法，绝不丢分尾差）。
// 典型用途：100 元按 3 人分摊、手续费按比例拆到多个收入科目。
func Allocate(total decimal.Decimal, weights []decimal.Decimal, exponent int32) ([]decimal.Decimal, error)

// ConvertAt 按 rate 换汇并舍入到目标币种精度。FX preset 文档改为要求显式调用。
func ConvertAt(amount, rate decimal.Decimal, targetExponent int32, mode RoundingMode) decimal.Decimal
```

- FX preset（`presets/fx.go`）语义不变（settlement 吸收净差），文档补「换汇必须先
  `ConvertAt` 再入模板，尾差走 settlement 科目」的菜谱。
- COOKBOOK 加「舍入决策表」：什么场景用什么 mode，谁承担尾差。

### 新不变量

**I-16: 金额精度受币种约束** — 任何已提交 entry 的金额小数位数 ≤ 所属币种 exponent。
Pinned by: 单测（Round/Allocate 性质测试，含 Σ==total 模糊测试）+ 集成测试
（超精度 entry 被拒）。

---

## 4. Account Policy — 账户冻结与余额约束

### 问题

账户是隐式维度（无账户实体），因此没有任何 per-account 控制：不能冻结、不能关闭、
不能声明「此账户不许透支」。今天全库唯一的余额强制点在 Reserve 时刻——直接
`PostJournal` 可以把任何账户推成负数而无感（风控上不可接受）。

### 设计

保持「账户是隐式维度」的核心设计（这是本库的差异化优点，不引入重量级账户实体），
用**可选的 policy 覆盖行**表达控制：无 policy 行 = 今天的行为（active、不强制余额）。

**Schema（migration 028）**：

```sql
CREATE TABLE account_policies (
    id                  BIGSERIAL PRIMARY KEY,
    account_holder      BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id         BIGINT NOT NULL DEFAULT 0,  -- 0 = 该 holder 的所有币种
    classification_id   BIGINT NOT NULL DEFAULT 0,  -- 0 = 所有 classification
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'frozen', 'closed')),
    -- 余额下限：0 = 不许透支；负值 = 透支额度（credit limit = -min_balance）；
    -- 正值 = 防尘底线（financial.md 的 $0.10 规则可声明在这里）
    min_balance         NUMERIC(30,18) NOT NULL DEFAULT 0,
    enforce_min_balance BOOLEAN NOT NULL DEFAULT false,
    note                TEXT NOT NULL DEFAULT '',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_holder, currency_id, classification_id)
);

-- 策略变更是运营配置（非资金），可 UPDATE；但每次变更留 append-only 审计行
CREATE TABLE account_policy_changes (
    id         BIGSERIAL PRIMARY KEY,
    policy_id  BIGINT NOT NULL REFERENCES account_policies(id),
    old_state  JSONB NOT NULL,
    new_state  JSONB NOT NULL,
    actor_id   BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**语义**：

| status | 减少余额方向的 entry | 增加余额方向的 entry | Reserve |
|---|---|---|---|
| `active` | ✅ | ✅ | ✅ |
| `frozen` | ❌ `ErrAccountFrozen` | ✅（入金仍可到账） | ❌ |
| `closed` | ❌ `ErrAccountClosed` | ❌ | ❌ |

- 「减少/增加余额」按 classification 的 normal side 判定（credit-normal 用户余额：
  debit=减少；debit-normal 资产账户：credit=减少）。
- 匹配优先级：精确 (holder,currency,classification) > (holder,currency,0) >
  (holder,0,0)。取最具体的一行生效。
- `enforce_min_balance`：entry 提交前计算 balance-after ≥ min_balance，
  违反 → 复用 `ErrInsufficientBalance`（消费方已有处理路径，不新增哨兵）。

**强制点与并发**：`PostJournal` 已对每个 touched (holder,currency) 持有
tx-scoped advisory lock（串行化写入），在锁内追加：

1. 读 policy（单行索引查询，无 policy 即跳过——常见路径零额外读）
2. 若有 min_balance 强制：锁内做 checkpoint+delta 余额读（与 Reserve 的
   TOCTOU-safe 检查同构）

Reserve 路径同样加 status 检查（复用已持有的锁）。

**接口 + HTTP**：

```go
type AccountPolicyStore interface {
    SetPolicy(ctx context.Context, input AccountPolicyInput) (*AccountPolicy, error)
    GetPolicy(ctx context.Context, holder, currencyID, classificationID int64) (*AccountPolicy, error)
    ListPolicies(ctx context.Context, holder int64) ([]AccountPolicy, error)
}
```

`PUT /api/v1/accounts/{holder}/policy`、`GET /api/v1/accounts/{holder}/policies`。

### 新不变量

**I-17: 账户策略强制** — 不存在违反其维度生效策略（frozen 方向阻断 / closed 全阻断 /
min_balance 下限）的已提交 entry。
Pinned by: 集成测试矩阵（3 status × 2 方向 × 有/无 min_balance）+ 并发测试
（冻结与在途 journal 竞争）。

---

## 5. Partial Operations — 部分冲销与部分结算

### 5a. 部分冲销（Partial Reversal）

**问题**：`ReverseJournal` 只支持整笔反冲。部分退款、手续费保留的退款做不了，
COOKBOOK 又明确劝阻手拼反向 journal。

**设计**：

- 保留 `ReverseJournal(ctx, journalID, reason)`（整笔，语义不变）。
- 新增比例式部分冲销：

```go
// ReverseJournalFraction 反冲原 journal 的 num/den 比例。
// 每条 entry 按比例缩放后用 largest-remainder 法按 (currency, side) 修正尾差，
// 保证反冲 journal 自身 per-currency 平衡（复用 §3 Allocate）。
ReverseJournalFraction(ctx, journalID int64, num, den int64, reason string) (*Journal, error)
```

  不提供任意 per-entry 金额的部分冲销（任意金额组合可能不平衡，把校验负担甩给
  调用方 = 邀请 bug；真有任意需求时调用方直接 PostJournal 一笔对冲分录并在
  metadata 里引用原 journal）。

- **Schema（migration 029）**：drop `uq_journals_reversal_of` 唯一索引
  （一笔 journal 允许多次部分反冲）。
- **累计守恒**：对原 journal 行 `SELECT ... FOR UPDATE` 串行化并发反冲，
  校验 Σ(已反冲比例) + 本次 ≤ 1，违反 → `ErrConflict`。
- **I-2 修订**：「corrections via reversal only」不变；「至多反冲一次」改为
  「累计反冲金额 per entry 不超过原额」（新 pin 测试替换旧测试）。

### 5b. 部分结算（Partial Settlement）— 激活 `settling` 死状态

**问题**：Reservation FSM 声明了 `active→settling→settled`，但没有任何接口能进入
`settling`——review 定性为半成品。LLM 计费等场景需要「一笔预留、多次扣减」。

**设计**：

```go
// SettlePartial 从预留中结算一部分。首次调用使状态进入 settling。
// 累计 settled ≤ reserved（沿用已有 chk_settled_lte_reserved + Go fail-fast）。
SettlePartial(ctx, reservationID int64, amount decimal.Decimal) error

// FinalizeSettlement 结束结算：状态 → settled，剩余额度隐式释放。
FinalizeSettlement(ctx, reservationID int64) error
```

- 现有 `Settle`（一次性全额结算）语义不变。
- `settled_amount` 从单值改为**累计值**（列不变，语义改为累加；UpdateReservationSettle
  查询改为 `settled_amount = settled_amount + $2`，schema 无需迁移）。
- **过期语义**：`settling` 状态过期时自动 finalize（已结算部分保留、剩余释放），
  区别于 `active` 过期的整笔 release。expiration worker 补这条分支 + 文档。
- HTTP：`POST /api/v1/reservations/{id}/settle-partial`、`.../finalize`。

---

## 6. 杂项清理（借破坏性窗口一次做掉）

1. **Metadata 类型统一**：`Booking.Metadata` / `TransitionInput.Metadata` /
   `Event.Metadata` 从 `map[string]any` → `map[string]string`，与 journal/pending
   对齐。消除幂等 payload 深比较的不稳定源。DB JSONB 不变，纯 Go 类型收紧。
2. **`Settle`/`Release` 补 Input struct**：`SettleInput{ReservationID, Amount}` +
   `Validate()`，与其他写路径的「Input + Validate」套路对齐（v0.x 破坏签名 OK）。
3. **channel 入站 nonce 防重放**：`webhook_nonces (nonce PK, seen_at)` 表 +
   时间窗内唯一校验，替代「纯靠下游幂等兜底」；配 TTL 清理（时间窗 2×5min 之外可删，
   这是防重放缓存不是账本数据，允许删除）。
4. **lifecycle JSON 加 `version` 字段**（恒 1）：为未来演化留钩子，成本一行。

---

## 7. Backlog（本轮不实现，记设计方向）

| 项 | 方向 | 触发条件 |
|---|---|---|
| 分区自动化 | 月度具名分区 + worker 预建下月 + detach 归档；存量 default 分区数据一次性搬迁 | entries 行数 > 千万级 |
| Maker-checker | 手工 journal 走 `pending_approvals` 表 + 二人复核 + 审批即 PostJournal | 有第二个运营人员时 |
| Per-key holder scoping | API key → 允许的 holder 范围映射，读端点鉴权 | 服务模式对外暴露时 |
| 对账单导出 | `GET /accounts/{holder}/statement?period=` → CSV；基于 §1 effective 口径 | 有对外结算需求时 |
| Classification 层级 | `parent_id` + 五大类账户类型 → 完整 chart of accounts | 需要合并报表时 |
| Snapshot 加速 trial balance | trial balance 从全表聚合改为 snapshot + delta | trial balance 查询变慢时 |

---

## 8. 实施计划

每个 Phase = 一个独立 worktree 批次（沿用本次修复批的多 agent 流水线），
含迁移 + core + store + service + HTTP + openapi + INVARIANTS.md 更新 + 测试。

| Phase | 内容 | 迁移号 | 预估规模 |
|---|---|---|---|
| **P1** | §1 effective date + §2 period close + trial balance | 025-026 | 大 |
| **P2** | §3 currency exponent + money 原语 | 027 | 中 |
| **P3** | §4 account policy | 028 | 大 |
| **P4** | §5 partial reversal + partial settlement | 029 | 中 |
| **P5** | §6 杂项清理 | 030 | 小 |

依赖：P4 依赖 P2（Allocate）；P1/P2/P3 相互独立可并行，但迁移号需预分配（如上表）。
全部完成后 tag `v0.3.0`，CHANGELOG 单条大版本条目。

**测试标准**（每 phase 的 Done）：

- 新不变量各有至少一个 pin 测试（写进 INVARIANTS.md 的 Pinned by）
- `go test -race ./...` 全绿 + `golangci-lint` 0 issue + `sqlc diff` 干净
- 金融 TDD 断言（financial.md）：端到端余额变化、全局借贷平衡、操作后 Locked 归零

---

## 9. 待拍板的设计决策（Aaron review 点）

1. **冻结方向性**（§4）：`frozen` = 只封「减少余额」方向、入金仍可到账；
   `closed` = 双向全封。推荐此两档设计（符合风控惯例：冻结不该挡住用户入金）。
2. **min_balance 三合一**（§4）：负值=透支额度、0=禁透支、正值=防尘底线，
   一个字段表达三种策略。推荐（避免 credit_limit/overdraft/dust 三个字段的组合爆炸）。
3. **effective_at 拒绝未来时间**（§1）：未来记账（scheduled posting）视为独立
   feature 不在本轮。推荐拒绝（容差 5min）。
4. **period close 允许 reopen**（§2）：latest-row-wins + 审计留痕。自用阶段推荐允许；
   有外部消费者后再收紧。
5. **部分冲销只做比例式**（§5a）：不做任意 per-entry 金额组合。推荐（守恒校验
   简单可靠；任意对冲走普通 PostJournal）。

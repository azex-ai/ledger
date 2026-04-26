# Ledger v2 — Classification-Driven Architecture

> 从硬编码业务类型到科目驱动的通用账本引擎。

**Created**: 2026-04-22
**Status**: Design
**Supersedes**: `2026-04-17-design.md`（v1 基础架构）

---

## 1. 设计动机

v1 将 deposit、withdrawal 作为独立领域概念，各自拥有表、store、handler。这导致：

- 新增科目（如内部划转、手续费结算）需要新建表 + store + handler + 状态机代码
- 事件/通知系统缺失，外部系统只能轮询 ledger 获取状态变更
- 科目和操作逻辑紧耦合，library 用户难以扩展

v2 的核心变化：**科目（Classification）是一切的主体**。deposit 和 withdrawal 只是两个出厂预设的科目配置。

---

## 2. 核心原则

### 2.1 科目驱动

Ledger 面向科目，不面向具体业务类型。所有操作（充值、提现、划转、手续费...）统一为"某科目下的一次操作（Operation）"。

### 2.2 单向数据流

```
外部系统 ──命令──▶ Ledger ──事件──▶ 订阅方
              (写入)            (通知)

✅ 外部 push 数据进来（API / channel adapter 回调）
✅ Ledger 向外 emit 事件（通知状态变更）
❌ Ledger 不会主动查询或调用外部系统
```

Ledger 是纯粹的状态容器 — 接收命令、校验、记账、发出事件。"去链上查确认数"、"调银行接口"等逻辑不属于 ledger。

### 2.3 Event-Journal 原子性

事件不是状态变更的"通知" — 事件是 journal 存在的**原因**。

```
┌────────── 一个 DB 事务 ──────────┐
│  Event:   deposit.confirmed      │ ← 为什么记这笔账
│  Journal: debit/credit entries   │ ← 账务事实
│  Operation: status → confirmed   │ ← 状态更新
│                                  │
│  event.journal_id ←→ journal_id  │
└──────────────────────────────────┘
```

每次 mutation = 状态变更 + 记账 + 事件写入（同一个事务）。投递是唯一可插拔的部分。

### 2.4 No NULL

所有数据库列 NOT NULL，使用有意义的默认值：

| 类型 | 默认值 | 语义 |
|------|--------|------|
| 金额 | `0` | 未设置/未结算（状态字段判断） |
| 字符串 | `''` | 未设置 |
| 外键 ID | `0` | 未关联（不加 FK 约束） |
| 时间 | `'epoch'` | 不适用/未设置 |
| JSONB | `'{}'` | 空 |

UNIQUE 约束使用部分索引：`WHERE column != ''` / `WHERE column != 0`。

---

## 3. 架构总览

### 3.1 Package 结构

```
ledger/
├── core/                     ← 纯领域层（零外部依赖）
│   ├── classification.go     ← Classification + Lifecycle
│   ├── operation.go          ← Operation（统一的科目操作实例）
│   ├── event.go              ← Event（事件 = journal 的原因）
│   ├── journal.go            ← Journal, Entry, JournalInput + 平衡校验
│   ├── template.go           ← EntryTemplate（journal 构造的语法糖）
│   ├── reserve.go            ← Reservation（跨科目的锁定机制）
│   ├── checkpoint.go         ← BalanceCheckpoint, RollupQueueItem, Snapshot
│   ├── interfaces.go         ← 所有 Store 接口
│   ├── errors.go             ← 领域错误
│   └── engine.go             ← Engine 配置（Logger, Metrics 注入）
│
├── channel/                  ← Channel Adapter 接口 + 示例（依赖 net/http）
│   ├── adapter.go            ← ChannelAdapter 接口定义
│   └── onchain/              ← demo: 链上充值确认适配器
│       └── evm.go
│
├── postgres/                 ← PostgreSQL 适配器
│   ├── sql/
│   │   ├── migrations/       ← schema migrations (embed.FS)
│   │   └── queries/          ← sqlc query files
│   ├── sqlcgen/              ← generated (do not edit)
│   ├── store.go              ← LedgerStore（Journal + Balance）
│   ├── operation.go          ← OperationStore（统一操作管理）
│   ├── reserve.go            ← ReserverStore
│   ├── event.go              ← EventStore（outbox 写入）
│   └── migrate.go
│
├── service/                  ← 业务编排 + 后台任务
│   ├── rollup.go             ← Checkpoint 物化
│   ├── expiration.go         ← 过期操作/预留清理
│   ├── reconcile.go          ← 对账引擎
│   ├── snapshot.go           ← 日快照
│   ├── system_rollup.go      ← 系统余额汇总
│   ├── worker.go             ← 后台 job 协调器
│   └── delivery/             ← 事件投递（可插拔）
│       ├── callback.go       ← 同步 callback（library 模式）
│       └── webhook.go        ← HTTP 投递 + 重试（service 模式）
│
├── server/                   ← HTTP API（standalone 模式）
│   ├── routes.go
│   ├── handler_operations.go ← 统一操作 handler（按 classification 路由）
│   ├── handler_journals.go
│   ├── handler_balances.go
│   ├── handler_webhooks.go   ← 入站 webhook 接收
│   └── ...
│
├── presets/                  ← 出厂预设科目配置
│   ├── deposit.go            ← deposit 科目 lifecycle + 建议 templates
│   └── withdrawal.go         ← withdrawal 科目 lifecycle + 建议 templates
│
├── cmd/ledgerd/              ← standalone 服务入口
├── web/                      ← Next.js 管理后台
└── examples/
    └── onchain-deposit/      ← 链上充值完整 demo
```

### 3.2 层级关系

```
cmd/ledgerd/main.go
  └── server/ (HTTP API)
        └── service/ (编排 + worker)
              └── postgres/ (DB adapter)
                    └── core/ (纯领域)

channel/ (adapter 接口，平行于 server/)
presets/ (科目配置，依赖 core/)
service/delivery/ (投递实现，依赖 core/ + postgres/)
```

---

## 4. Core 领域模型

### 4.1 Classification + Lifecycle

```go
// core/classification.go

type Status string

// Lifecycle 定义科目的状态机。纯粹的状态转换规则，不绑定任何副作用。
type Lifecycle struct {
    Initial     Status
    Terminal    []Status
    Transitions map[Status][]Status // from → []validTargets
}

// Validate 检查 lifecycle 的自洽性：
// - Initial 必须存在于 Transitions 的 key 中
// - Terminal 不能有出边
// - 所有 target 必须在某处定义
func (l *Lifecycle) Validate() error { ... }

// CanTransition 校验 from → to 是否合法
func (l *Lifecycle) CanTransition(from, to Status) bool { ... }

// IsTerminal 检查状态是否为终态
func (l *Lifecycle) IsTerminal(s Status) bool { ... }

type Classification struct {
    ID         int64       `json:"id"`
    Code       string      `json:"code"`
    Name       string      `json:"name"`
    NormalSide NormalSide  `json:"normal_side"`
    IsSystem   bool        `json:"is_system"`
    IsActive   bool        `json:"is_active"`
    Lifecycle  *Lifecycle  `json:"lifecycle"`   // nil = 纯标签科目
    CreatedAt  time.Time   `json:"created_at"`
}
```

Lifecycle 只回答"从 A 能不能到 B"。转换时记什么账、发什么事件，由调用方编排。

### 4.2 Operation

统一替代 v1 的 Deposit / Withdrawal。所有科目共用同一个模型。

```go
// core/operation.go

type Operation struct {
    ID               int64              `json:"id"`
    ClassificationID int64              `json:"classification_id"`
    AccountHolder    int64              `json:"account_holder"`
    CurrencyID       int64              `json:"currency_id"`
    Amount           decimal.Decimal    `json:"amount"`           // 计划/请求金额
    SettledAmount    decimal.Decimal    `json:"settled_amount"`   // 实际/结算金额（0 = 未结算）
    Status           Status             `json:"status"`
    ChannelName      string             `json:"channel_name"`
    ChannelRef       string             `json:"channel_ref"`
    ReservationID    int64              `json:"reservation_id"`   // 0 = 未关联
    JournalID        int64              `json:"journal_id"`       // 0 = 未记账
    IdempotencyKey   string             `json:"idempotency_key"`
    Metadata         map[string]any     `json:"metadata"`
    ExpiresAt        time.Time          `json:"expires_at"`       // epoch = 不过期
    CreatedAt        time.Time          `json:"created_at"`
    UpdatedAt        time.Time          `json:"updated_at"`
}

type TransitionInput struct {
    OperationID int64
    ToStatus    Status
    ChannelRef  string              // 可选，转换时设置渠道引用
    Amount      decimal.Decimal     // 可选，转换时设置结算金额
    Metadata    map[string]any      // 可选，合并到 operation.metadata
    ActorID     int64               // 谁触发的转换
}
```

### 4.3 Event（事件 = Journal 的原因）

```go
// core/event.go

type Event struct {
    ID                 int64              `json:"id"`
    ClassificationCode string             `json:"classification_code"`
    OperationID        int64              `json:"operation_id"`
    AccountHolder      int64              `json:"account_holder"`
    CurrencyID         int64              `json:"currency_id"`
    FromStatus         Status             `json:"from_status"`
    ToStatus           Status             `json:"to_status"`
    Amount             decimal.Decimal    `json:"amount"`
    SettledAmount      decimal.Decimal    `json:"settled_amount"`
    JournalID          int64              `json:"journal_id"`       // 0 = 该事件未产生 journal
    Metadata           map[string]any     `json:"metadata"`
    OccurredAt         time.Time          `json:"occurred_at"`

    // 投递状态（不属于 core 语义，由 delivery 层使用）
    DeliveryStatus     string             `json:"-"`
    Attempts           int                `json:"-"`
}
```

事件类型由 `ClassificationCode + FromStatus + ToStatus` 三元组自然描述。不需要预定义事件类型枚举 — 科目是动态配置的，事件类型也是动态的。

### 4.4 Journal（不变，继承 v1）

Journal、Entry、EntryTemplate 保持 v1 设计不变。它们是 ledger 的基础记账能力，与科目系统正交。

```go
// core/journal.go — 与 v1 相同

type Journal struct {
    ID             int64
    JournalTypeID  int64
    IdempotencyKey string
    TotalDebit     decimal.Decimal
    TotalCredit    decimal.Decimal
    Metadata       map[string]any
    ActorID        int64               // 0 = system
    Source         string
    ReversalOf     int64               // 0 = 非冲正
    EventID        int64               // v2 新增：触发此 journal 的事件 ID（0 = 手动记账）
    CreatedAt      time.Time
}
```

`EventID` 是 v2 唯一新增字段 — 建立 event → journal 的因果链。手动 PostJournal（不经过状态机）时 EventID = 0。

### 4.5 Reservation（保留，跨科目机制）

Reservation 不属于某个科目 — 它是"锁定资金"的通用机制。任何科目的操作流程都可以关联 reservation。保持 v1 设计不变。

### 4.6 Interfaces

```go
// core/interfaces.go

// === 写入 ===

// JournalWriter — 纯记账能力
type JournalWriter interface {
    PostJournal(ctx context.Context, input JournalInput) (*Journal, error)
    ExecuteTemplate(ctx context.Context, code string, params TemplateParams) (*Journal, error)
    ReverseJournal(ctx context.Context, journalID int64, reason string) (*Journal, error)
}

// Operator — 科目操作的状态管理
type Operator interface {
    CreateOperation(ctx context.Context, input CreateOperationInput) (*Operation, error)
    Transition(ctx context.Context, input TransitionInput) (*Event, error)
}

// Reserver — 资金锁定机制（跨科目通用）
type Reserver interface {
    Reserve(ctx context.Context, input ReserveInput) (*Reservation, error)
    Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error
    Release(ctx context.Context, reservationID int64) error
}

// === 读取 ===

type BalanceReader interface {
    GetBalance(ctx context.Context, holder, currencyID, classificationID int64) (decimal.Decimal, error)
    GetBalances(ctx context.Context, holder, currencyID int64) ([]Balance, error)
    BatchGetBalances(ctx context.Context, holderIDs []int64, currencyID int64) (map[int64][]Balance, error)
}

type OperationReader interface {
    GetOperation(ctx context.Context, id int64) (*Operation, error)
    ListOperations(ctx context.Context, filter OperationFilter) ([]Operation, error)
}

type EventReader interface {
    GetEvent(ctx context.Context, id int64) (*Event, error)
    ListEvents(ctx context.Context, filter EventFilter) ([]Event, error)
}

// === 后台任务 ===

type RollupWorker interface {
    ProcessBatch(ctx context.Context, batchSize int) (int, error)
}

type Reconciler interface {
    CheckAccountingEquation(ctx context.Context) (*ReconcileResult, error)
    ReconcileAccount(ctx context.Context, holder, currencyID int64) (*ReconcileResult, error)
}

type Snapshotter interface {
    CreateDailySnapshot(ctx context.Context, date time.Time) error
    GetSnapshotBalance(ctx context.Context, holder, currencyID int64, date time.Time) ([]Balance, error)
}

// === 元数据管理 ===

type ClassificationStore interface {
    Create(ctx context.Context, input ClassificationInput) (*Classification, error)
    Deactivate(ctx context.Context, id int64) error
    Get(ctx context.Context, id int64) (*Classification, error)
    GetByCode(ctx context.Context, code string) (*Classification, error)
    List(ctx context.Context, activeOnly bool) ([]Classification, error)
}

type TemplateStore interface {
    Create(ctx context.Context, input TemplateInput) (*EntryTemplate, error)
    Deactivate(ctx context.Context, id int64) error
    Get(ctx context.Context, code string) (*EntryTemplate, error)
    List(ctx context.Context) ([]EntryTemplate, error)
}

type CurrencyStore interface {
    Create(ctx context.Context, input CurrencyInput) (*Currency, error)
    Get(ctx context.Context, id int64) (*Currency, error)
    List(ctx context.Context) ([]Currency, error)
}

// === 投递（可插拔）===

type EventDeliverer interface {
    Deliver(ctx context.Context, event Event) error
}
```

**关键变化 vs v1**：

- `Depositor` + `Withdrawer` → 统一为 `Operator`
- `Transition()` 返回 `*Event`（事件是原子产物）
- 新增 `EventReader` 和 `EventDeliverer`
- Journal 新增 `EventID` 字段

### 4.7 独立命令原则

Ledger 暴露的命令彼此独立，调用方自由编排：

```
Transition(opID, toStatus)     → 纯状态变更 + 产生 Event
PostJournal(entries)            → 纯记账
ExecuteTemplate(code, params)   → PostJournal 的语法糖
Reserve(input)                  → 纯资金锁定
```

原子性由调用方在 service 层保证（同一个 DB 事务），但这些命令在概念上是独立的。

---

## 5. 数据库 Schema

### 5.1 分类扩展

```sql
-- classifications 表增加 lifecycle 列
ALTER TABLE classifications ADD COLUMN lifecycle JSONB NOT NULL DEFAULT '{}';
```

`lifecycle = '{}'` 表示纯标签科目（无状态机）。有状态机的科目存储：

```json
{
  "initial": "pending",
  "terminal": ["confirmed", "failed", "expired"],
  "transitions": {
    "pending": ["confirming", "failed", "expired"],
    "confirming": ["confirmed", "failed"]
  }
}
```

### 5.2 统一 Operations 表（替代 deposits + withdrawals）

```sql
CREATE TABLE operations (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_id BIGINT NOT NULL,
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    amount            NUMERIC(30,18) NOT NULL,
    settled_amount    NUMERIC(30,18) NOT NULL DEFAULT 0,
    status            TEXT NOT NULL,
    channel_name      TEXT NOT NULL DEFAULT '',
    channel_ref       TEXT NOT NULL DEFAULT '',
    reservation_id    BIGINT NOT NULL DEFAULT 0,
    journal_id        BIGINT NOT NULL DEFAULT 0,
    idempotency_key   TEXT NOT NULL,
    metadata          JSONB NOT NULL DEFAULT '{}',
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_operations_idempotency UNIQUE (idempotency_key)
);

-- channel_ref 幂等：只对非空值做唯一约束
CREATE UNIQUE INDEX uq_operations_channel_ref
    ON operations (channel_name, channel_ref)
    WHERE channel_ref != '';

-- 查询索引：某用户某科目下的操作
CREATE INDEX idx_operations_holder_class
    ON operations (account_holder, classification_id, status);

-- 过期扫描
CREATE INDEX idx_operations_expires
    ON operations (expires_at)
    WHERE expires_at != 'epoch';
```

**字段语义**：

| 字段 | 含义 | deposit 场景 | withdrawal 场景 |
|------|------|-------------|----------------|
| `amount` | 计划/请求金额 | 预期充值额 | 申请提现额 |
| `settled_amount` | 实际/结算金额（0=未结算）| 实际到账额 | 实际扣款额 |
| `channel_name` | 外部渠道标识 | "evm", "tron" | "evm", "bank" |
| `channel_ref` | 外部渠道引用 | tx hash | tx hash |
| `reservation_id` | 关联预留（0=无）| 通常不用 | 锁定资金的 reservation |
| `journal_id` | 关联 journal（0=未记账）| 确认时的记账 | 确认时的记账 |
| `metadata` | 科目特有数据 | `{"confirmations": 12}` | `{"review_required": true}` |

### 5.3 Events 表（Outbox）

```sql
CREATE TABLE events (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_code TEXT NOT NULL,
    operation_id        BIGINT NOT NULL DEFAULT 0,
    account_holder      BIGINT NOT NULL DEFAULT 0,
    currency_id         BIGINT NOT NULL DEFAULT 0,
    from_status         TEXT NOT NULL DEFAULT '',
    to_status           TEXT NOT NULL,
    amount              NUMERIC(30,18) NOT NULL DEFAULT 0,
    settled_amount      NUMERIC(30,18) NOT NULL DEFAULT 0,
    journal_id          BIGINT NOT NULL DEFAULT 0,
    metadata            JSONB NOT NULL DEFAULT '{}',
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- 投递状态
    delivery_status     TEXT NOT NULL DEFAULT 'pending',
    attempts            INT NOT NULL DEFAULT 0,
    max_attempts        INT NOT NULL DEFAULT 10,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_delivery_pending
    ON events (next_attempt_at)
    WHERE delivery_status = 'pending';

CREATE INDEX idx_events_operation
    ON events (operation_id);
```

### 5.4 Webhook Subscribers（仅 standalone 模式）

```sql
CREATE TABLE webhook_subscribers (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    url              TEXT NOT NULL,
    secret           TEXT NOT NULL DEFAULT '',
    filter_class     TEXT NOT NULL DEFAULT '',     -- 空 = 所有科目
    filter_to_status TEXT NOT NULL DEFAULT '',     -- 空 = 所有状态
    is_active        BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 5.5 Journals 表（v2 变更）

```sql
-- journals 表增加 event_id 列
ALTER TABLE journals ADD COLUMN event_id BIGINT NOT NULL DEFAULT 0;

CREATE INDEX idx_journals_event ON journals (event_id) WHERE event_id != 0;
```

### 5.6 Reservations 表（No NULL 适配）

```sql
CREATE TABLE reservations (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_holder  BIGINT NOT NULL,
    currency_id     BIGINT NOT NULL,
    reserved_amount NUMERIC(30,18) NOT NULL CHECK (reserved_amount > 0),
    settled_amount  NUMERIC(30,18) NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'settling', 'settled', 'released')),
    journal_id      BIGINT NOT NULL DEFAULT 0,
    idempotency_key TEXT NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '15 minutes',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_reservations_idempotency UNIQUE (idempotency_key),
    CONSTRAINT chk_settled_non_negative CHECK (settled_amount >= 0),
    CONSTRAINT chk_settled_lte_reserved CHECK (settled_amount <= reserved_amount)
);
```

### 5.7 删除的表

v2 删除 `deposits` 和 `withdrawals` 表，统一到 `operations`。

---

## 6. 事件系统

### 6.1 原子写入

每次 `Operator.Transition()` 在同一个 DB 事务中完成：

```
1. 加载 Operation + Classification（含 Lifecycle）
2. Lifecycle.CanTransition(from, to) 校验
3. UPDATE operations SET status = to, ...
4. INSERT INTO events (原因记录)
5. 如果调用方同时提供了 JournalInput：INSERT INTO journals + journal_entries
6. UPDATE journals SET event_id = events.id（建立因果链）
```

注意：步骤 5 的 journal 不是由 Transition 自动触发的 — 是调用方在同一个事务中显式调用 PostJournal / ExecuteTemplate 的结果。Transition 只负责状态变更 + 事件写入。

### 6.2 投递机制

**Library 模式 — 同步 callback**：

```go
ledger := core.NewEngine(
    core.WithEventHandler(func(ctx context.Context, e core.Event) error {
        if e.ClassificationCode == "deposit" && e.ToStatus == "confirmed" {
            // 通知业务系统
        }
        return nil
    }),
)
```

callback 在事务提交后同步调用。失败不影响事务（事件已持久化在 events 表中）。

**Service 模式 — 异步 HTTP 投递**：

Worker 轮询 events 表中 `delivery_status = 'pending'` 的记录：

1. 匹配 subscriber 的 filter（classification_code, to_status）
2. 构造 payload，HMAC-SHA256 签名
3. HTTP POST 到 subscriber URL
4. 成功 → `delivery_status = 'delivered'`
5. 失败 → `attempts++`，指数退避算 `next_attempt_at`
6. 超过 `max_attempts` → `delivery_status = 'dead'`（死信，可手动重试）

**签名格式**（复用 payments 的方案）：

```
X-Ledger-Signature: t=<timestamp>,v1=<hmac_sha256_hex>
X-Ledger-Event-ID: <event_id>
X-Ledger-Timestamp: <unix_timestamp>
```

### 6.3 事件过滤

订阅方按三元组过滤：`(ClassificationCode, FromStatus, ToStatus)`。

```go
// "所有 deposit 确认"
filter_class = "deposit", filter_to_status = "confirmed"

// "任何科目失败"
filter_class = "", filter_to_status = "failed"

// "所有事件"
filter_class = "", filter_to_status = ""
```

### 6.4 对账用途

events 表同时是审计日志 — 每条 journal 都可追溯到触发它的 event：

```sql
-- 查看某笔 journal 的触发原因
SELECT e.classification_code, e.from_status, e.to_status, e.amount
FROM events e
JOIN journals j ON j.event_id = e.id
WHERE j.id = ?;

-- 查看某科目的事件流水
SELECT * FROM events
WHERE classification_code = 'deposit'
ORDER BY occurred_at;

-- 对账：计划金额 vs 实际金额
SELECT classification_code,
       SUM(amount) AS expected,
       SUM(settled_amount) AS actual,
       SUM(amount - settled_amount) AS gap
FROM events
WHERE to_status IN ('confirmed', 'settled')
GROUP BY classification_code;
```

---

## 7. Channel Adapter（入站 Webhook）

### 7.1 接口定义

```go
// channel/adapter.go
// 注意：不在 core/ 中，因为依赖 net/http

type CallbackPayload struct {
    OperationID    int64               // ledger 侧的 operation ID（可选，用于匹配）
    ChannelRef     string              // 外部唯一引用（如 tx hash）
    Status         string              // 渠道侧的状态判断结果：confirming / confirmed / failed
    ActualAmount   decimal.Decimal     // 实际金额（0 = 未知）
    Metadata       map[string]any      // 渠道特定数据
}

type ChannelAdapter interface {
    // Name 返回渠道标识，如 "evm", "tron", "bank_transfer"
    Name() string

    // VerifySignature 校验回调签名的合法性
    VerifySignature(header http.Header, body []byte) error

    // ParseCallback 解析回调 payload 为统一格式
    ParseCallback(header http.Header, body []byte) (*CallbackPayload, error)
}
```

### 7.2 Server 层集成

```go
// server/handler_webhooks.go

// POST /api/v1/webhooks/:channel
// channel adapter 注册在 server 启动时
func (s *Server) handleWebhookCallback(w http.ResponseWriter, r *http.Request) {
    channelName := chi.URLParam(r, "channel")
    adapter, ok := s.channels[channelName]
    if !ok {
        httpx.Error(w, "unknown channel")
        return
    }

    body, _ := io.ReadAll(r.Body)

    if err := adapter.VerifySignature(r.Header, body); err != nil {
        httpx.Error(w, "signature verification failed")
        return
    }

    payload, err := adapter.ParseCallback(r.Header, body)
    if err != nil {
        httpx.Error(w, "invalid callback payload")
        return
    }

    // 转换为 ledger 命令
    _, err = s.operator.Transition(r.Context(), core.TransitionInput{
        OperationID: payload.OperationID,
        ToStatus:    core.Status(payload.Status),
        ChannelRef:  payload.ChannelRef,
        Amount:      payload.ActualAmount,
        Metadata:    payload.Metadata,
    })
    // ...
}
```

数据流：`HTTP 回调 → ChannelAdapter.Parse() → TransitionInput → Operator.Transition() → Event 持久化`

### 7.3 Demo: EVM 链上充值适配器

```go
// channel/onchain/evm.go

type EVMAdapter struct {
    // 外部系统（如 block scanner）通过 webhook 通知 ledger
    // ledger 不直接和链交互（单向数据流原则）
    SigningKey []byte
}

func (a *EVMAdapter) Name() string { return "evm" }

func (a *EVMAdapter) VerifySignature(header http.Header, body []byte) error {
    sig := header.Get("X-Signature")
    mac := hmac.New(sha256.New, a.SigningKey)
    mac.Write(body)
    expected := hex.EncodeToString(mac.Sum(nil))
    if !hmac.Equal([]byte(sig), []byte(expected)) {
        return fmt.Errorf("channel: evm: signature mismatch")
    }
    return nil
}

func (a *EVMAdapter) ParseCallback(header http.Header, body []byte) (*channel.CallbackPayload, error) {
    var raw struct {
        TxHash        string `json:"tx_hash"`
        OperationID   int64  `json:"operation_id"`
        Amount        string `json:"amount"`
        Confirmations int    `json:"confirmations"`
        Status        string `json:"status"` // "confirming" | "confirmed" | "failed"
    }
    if err := json.Unmarshal(body, &raw); err != nil {
        return nil, fmt.Errorf("channel: evm: parse: %w", err)
    }

    amount, err := decimal.NewFromString(raw.Amount)
    if err != nil {
        return nil, fmt.Errorf("channel: evm: invalid amount: %w", err)
    }

    return &channel.CallbackPayload{
        OperationID:  raw.OperationID,
        ChannelRef:   raw.TxHash,
        Status:       raw.Status,
        ActualAmount: amount,
        Metadata: map[string]any{
            "confirmations": raw.Confirmations,
            "tx_hash":       raw.TxHash,
        },
    }, nil
}
```

---

## 8. 预设科目

### 8.1 Deposit 预设

```go
// presets/deposit.go

var DepositLifecycle = &core.Lifecycle{
    Initial:  "pending",
    Terminal: []core.Status{"confirmed", "failed", "expired"},
    Transitions: map[core.Status][]core.Status{
        "pending":    {"confirming", "failed", "expired"},
        "confirming": {"confirmed", "failed"},
    },
}

// 建议的 journal templates（使用方可以不用）
// "deposit_credit": 确认到账时
//   DR system:deposit / CR user:deposit
```

### 8.2 Withdrawal 预设

```go
// presets/withdrawal.go

var WithdrawalLifecycle = &core.Lifecycle{
    Initial:  "locked",
    Terminal: []core.Status{"confirmed", "failed", "expired"},
    Transitions: map[core.Status][]core.Status{
        "locked":     {"reserved"},
        "reserved":   {"reviewing", "processing"},
        "reviewing":  {"processing", "failed"},
        "processing": {"confirmed", "failed", "expired"},
        "failed":     {"reserved"}, // retry
    },
}
```

### 8.3 注册方式

```go
// Library 模式：代码注册
classStore.Create(ctx, core.ClassificationInput{
    Code:       "deposit",
    Name:       "Deposit",
    NormalSide: core.Credit,
    Lifecycle:  presets.DepositLifecycle,
})

// Standalone 模式：API 注册（lifecycle 作为 JSON 传入）
// POST /api/v1/classifications
// { "code": "internal_transfer", "name": "Internal Transfer",
//   "normal_side": "debit", "lifecycle": { "initial": "pending", ... } }
```

---

## 9. HTTP API 变更

### 9.1 删除的端点

```
DELETE  /api/v1/deposits/*          → 统一到 /api/v1/operations/*
DELETE  /api/v1/withdrawals/*       → 统一到 /api/v1/operations/*
```

### 9.2 新增/替换的端点

```
# Operations（统一）
POST   /api/v1/operations                      — 创建操作
POST   /api/v1/operations/:id/transition        — 状态转换
GET    /api/v1/operations/:id                   — 查单个
GET    /api/v1/operations                       — 分页查询（classification, status, holder 筛选）

# Webhooks（入站）
POST   /api/v1/webhooks/:channel                — 接收外部渠道回调

# Events（出站）
GET    /api/v1/events                           — 事件流水查询
GET    /api/v1/events/:id                       — 单个事件详情

# Webhook Subscribers（standalone 模式）
POST   /api/v1/subscribers                      — 创建订阅
GET    /api/v1/subscribers                      — 列出订阅
DELETE /api/v1/subscribers/:id                   — 删除订阅

# Classifications 扩展
POST   /api/v1/classifications                  — 新增（支持 lifecycle JSON）
```

### 9.3 保持不变的端点

Journals、Entries、Balances、Reservations、Templates、Currencies、Reconciliation、Snapshots、System 端点保持 v1 设计不变。

---

## 10. 完整数据流示例

### 10.1 链上充值确认

```
1. 外部 block scanner 检测到链上交易
2. scanner POST /api/v1/webhooks/evm
   { "tx_hash": "0xabc...", "operation_id": 42, "amount": "100.5", "confirmations": 12, "status": "confirmed" }

3. EVMAdapter.VerifySignature() 校验签名
4. EVMAdapter.ParseCallback() 解析为 CallbackPayload

5. 在一个 DB 事务中：
   a. Operator.Transition(op=42, to="confirmed", amount=100.5, channel_ref="0xabc...")
      - 校验 Lifecycle: confirming → confirmed ✅
      - UPDATE operations SET status='confirmed', settled_amount=100.5, channel_ref='0xabc...'
      - INSERT INTO events (classification='deposit', from='confirming', to='confirmed', amount=100.5, journal_id=...)
   b. 调用方 ExecuteTemplate("deposit_credit", {holder, currency, amount=100.5})
      - INSERT INTO journals + journal_entries
      - UPDATE journals SET event_id = <new_event_id>
   c. COMMIT

6. 事件投递（异步）：
   - Worker 读取 events 表中 delivery_status='pending' 的记录
   - 匹配 subscriber filter
   - HTTP POST 到 subscriber URL（HMAC 签名）
   - 更新 delivery_status
```

### 10.2 Library 模式使用

```go
import (
    "github.com/azex-ai/ledger/core"
    "github.com/azex-ai/ledger/postgres"
    "github.com/azex-ai/ledger/presets"
)

// 初始化
engine := core.NewEngine(
    core.WithLogger(slogAdapter),
    core.WithEventHandler(func(ctx context.Context, e core.Event) error {
        log.Printf("event: %s %s→%s", e.ClassificationCode, e.FromStatus, e.ToStatus)
        return nil
    }),
)

store := postgres.NewStore(pool, engine)
store.Migrate(ctx)

// 注册科目
store.Classifications().Create(ctx, core.ClassificationInput{
    Code:      "deposit",
    Name:      "Deposit",
    NormalSide: core.Credit,
    Lifecycle:  presets.DepositLifecycle,
})

// 创建操作
op, _ := store.Operations().Create(ctx, core.CreateOperationInput{
    ClassificationCode: "deposit",
    AccountHolder:      userID,
    CurrencyID:         usdtID,
    Amount:             decimal.NewFromFloat(100),
    IdempotencyKey:     "dep-001",
    ChannelName:        "evm",
})

// 状态转换（由外部系统的确认触发）
event, _ := store.Operations().Transition(ctx, core.TransitionInput{
    OperationID: op.ID,
    ToStatus:    "confirmed",
    Amount:      decimal.NewFromFloat(100),
    ChannelRef:  "0xabc...",
})

// 记账（调用方自行编排，可以用 template 也可以手动构造）
store.Journals().ExecuteTemplate(ctx, "deposit_credit", core.TemplateParams{
    HolderID:       userID,
    CurrencyID:     usdtID,
    Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromFloat(100)},
    IdempotencyKey: "dep-001-journal",
    EventID:        event.ID, // 建立因果链
})
```

---

## 11. Migration 路径（v1 → v2）

1. **新增 events 表**
2. **新增 operations 表**
3. **迁移 deposits → operations**（classification_id = deposit 科目 ID）
4. **迁移 withdrawals → operations**（classification_id = withdrawal 科目 ID）
5. **classifications 表增加 lifecycle 列**，填入 deposit/withdrawal 的 lifecycle JSON
6. **journals 表增加 event_id 列**（默认 0）
7. **reservations 表 No NULL 适配**（settled_amount 默认 0 等）
8. **rollup_queue 表 No NULL 适配**（processed_at 改为 status 字段或默认 epoch）
9. **删除 deposits、withdrawals 表**
10. **更新 server handlers**：deposit/withdrawal 端点重定向到 operations
11. **更新 web 前端**：统一操作管理界面

---

## 12. 不变的部分

以下 v1 设计保持不变：

- Journal + Entry 模型（append-only，借贷平衡）
- EntryTemplate 系统（journal 构造的语法糖）
- BalanceCheckpoint + Rollup 机制
- Reservation 系统（跨科目锁定）
- 三维账户模型 `(AccountHolder, CurrencyID, ClassificationID)`
- 对账引擎（CheckAccountingEquation, ReconcileAccount）
- 日快照 + 系统余额汇总
- Observability 接口（Logger, Metrics）
- Concurrency 策略（advisory lock, SELECT FOR UPDATE, SKIP LOCKED）
- Partition 策略（journal_entries 按 created_at）

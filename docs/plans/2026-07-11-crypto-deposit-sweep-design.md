# Crypto Deposit + Sweep — 可选默认方案设计

> 2026-07-11 · Status: IMPLEMENTED + merged to main (2026-07-13) — backend + M3 + full frontend. §1-8 core, §9 M3 all shipped. Follow-ups (mi2/mi5 hardening, docs/frontend.md, openapi CI) tracked on the bus board.
> 目标：把 azex 的多链共享地址加密充值方案（CREATE2 Persistent Proxy）收进 ledger 库，
> 作为**可选默认方案**——消费方装了就有全套充值 + 归集，不装则 ledger 保持纯账本。
> 合约侧不变（azex-contracts：DepositFactory / DepositProxy，见其 CLAUDE.md）；本设计只覆盖 ledger 侧。

## 0. 决策记录

| 决策点 | 结论 |
|---|---|
| 边界范围 | 全量含归集（编排 + 契约进 ledger；RPC/签名在 adapter） |
| 重依赖布局 | `chains/evm/` 独立 go module（挟 go-ethereum），仿 `internal/postgrestest` 模式，go.work 串联 |
| 签名注入 | `Signer` port + 库附默认本地私钥实现；后续可换 KMS/HSM 不动编排 |
| CREATE2 salt | `salt = bytes32(account_holder)`，纯函数派生，无 nonce 生成器 |
| 入账 token | 白名单：USDT / USDC only（本期） |
| 归集 token | 多币种含 native，独立于入账白名单（`credit_tokens` ≠ `sweep_tokens`） |
| 深 reorg | `ReorgPolicy` 可配：`manual`（默认）/ `auto-reverse` |

## 1. 模块布局

```
ledger (根 module，轻依赖)
├── core/          # 新增: DepositAddress 类型、CREATE2 派生(纯 keccak, x/crypto/sha3)、
│                  #       AddressRegistry / ChainScanner / Sweeper / Signer 等 port
├── postgres/      # 新增: deposit_addresses 注册表 store + chain_cursors 游标 store
├── service/       # 新增: onchain 编排 —— EnsureDepositAddress、IngestDeposit、
│                  #       watcher job + sweep job（挂 service/worker.go）
├── presets/       # 新增: CryptoDepositBundle（deposit lifecycle 复用 + sweep lifecycle 新增）
└── chains/evm/    # ★独立 go module: Watcher/ChainScanner/Sweeper adapter + 默认本地私钥 Signer
```

结构原则不破：**core 和 service 只见 port，不见 RPC**。链上轮询、发 `factory.batchSweep`
全部在 `chains/evm` adapter；消费方在自己的 composition root 插 adapter。
不用 crypto 的消费方（如 armatrix）根 go.mod 不变，不拉 geth。

命名注意：现有 `channel/onchain` 是 webhook（外部推）入口；`chains/evm` watcher 是自己拉的入口。
两条 ingestion 路径汇到同一个 service 编排函数。

## 2. 地址派生与签发

- 派生纯函数：`addr = create2(factory, salt = bytes32(holder), initHash)`，EIP-55 输出。
  同一 holder 跨所有 EVM 链同一地址（factory 各链同地址部署）。
- `factory` / `initHash` 来自消费方注入的 `ChainSet` 配置。
- 签发入口：`svc.Onchain().EnsureDepositAddress(ctx, holder)` —— 派生 + upsert 注册表 + **注册回扫**
  （对该地址做一次历史 `eth_getLogs` 全量回扫，堵死「游标前充值丢失」缝隙，见 §5-2b）。
  服务模式：`POST /api/v1/holders/{holder}/deposit-address`。

```sql
deposit_addresses (
  id BIGSERIAL, uid,
  account_holder BIGINT UNIQUE,   -- 一个 holder 一个地址
  address TEXT UNIQUE,            -- EIP-55
  factory TEXT, init_hash TEXT,   -- 派生指纹，审计用
  created_at
)

chain_cursors (
  chain_id BIGINT PRIMARY KEY,
  last_scanned_block BIGINT,
  updated_at
)
```

注册表只做反向查询（address→holder）与审计，不参与派生。

## 3. 入账数据流

两条路径产出同一个 `DepositSighting{chain_id, tx_hash, txlog_seq, token, from, to, amount, confirmations}`：

1. **Watcher（拉，默认）**：`chains/evm` 按 `ChainSet` 轮询各链 ERC-20 `Transfer` 日志，
   过滤 `to ∈ 注册表`。地址集大时按 provider topic 上限（~1k）分片查询。
2. **Webhook（推，可选）**：沿用 `channel/onchain` HMAC webhook，解析成同一 sighting。

汇合点 `IngestDeposit(sighting)`：

- 首个确认 → `CreateBooking`（classification=deposit），幂等 key =
  **`deposit-{chain_id}-{tx_hash}-{txlog_seq}`**。`txlog_seq` 是该 tx 内部第几个命中我们地址的
  Transfer（tx 内确定），**不用块级 log_index**（reorg 后必变 → 重复入账）。
- booking payload 只含稳定身份字段（chain_id/tx_hash/token/to/amount）；confirmations、
  见到时块高等易变字段走 Transition metadata —— 否则双路径触发三态幂等 ErrConflict。
- 确认数 ≥ 该链阈值 → `RunInTx`：`Transition(confirmed)` + `ExecuteTemplate("deposit_confirm")`，
  EventID 原子互链（即 examples/crypto-deposit 手工流程的编排化）。
- 阈值前 tx 消失（浅 reorg）→ `Transition(failed)`，无 journal。确认阈值是 reorg 防线。
- token→currency 映射走 `ChainSet`（每链 token 合约地址 → currency code + **decimals**，
  adapter 边界归一为 decimal.Decimal）。未注册 token 忽略 + 日志。
- 白名单限定标准 USDT/USDC → 豁免 fee-on-transfer 面值多记问题（写进约束：扩白名单时必须重审）。
- 原生币充值本期**不入账**（日志轮询看不见 native 转账），但会被归集扫进 treasury（§4 无主资金）。
  这是显式产品决策。
- pending booking 的确认数推进：watcher 除向前扫新块外，维护「未终态 booking 重查」循环。

## 4. 归集数据流

- **编排**：service 层 sweep job，依赖 `ChainScanner`（枚举注册地址各链各 token 余额，含 native）
  与 `Sweeper`（构造 + 发送 `factory.batchSweep`，内部经 `Signer` 签名）。
- **策略**（`SweepPolicy` 注入）：每链每 token 最小阈值（须显著大于单地址归集 gas 成本，防灰尘
  攻击经济学，可随 gas 价调整）、gas 上限（超过跳过本轮，默认 20 gwei 惯例）、批量上限、间隔。
- **token 集合**：`sweep_tokens` 静态 allowlist，**永不自动发现**；每 token 单独成 batch，
  隔离恶意/异常 token 的爆炸半径。native/非白名单归集进 treasury，不给任何 holder 记账，
  发 `sweep.unattributed` 事件（token、金额、来源地址），人工处置。
- **账本记法**：遵循「渠道/托管资金移动不进账本」——sweep **只走 booking + event，无 journal**。
  新增 `sweep` classification lifecycle：`pending → sent → confirmed / failed(→retry→pending)`。
  一个 batch 一个 booking，`channel_ref` = 归集 tx hash，metadata 记 chain/token/地址数/总额。
- **幂等（先记后发）**：booking key = `sweep-{chain_id}-{token}-{signer_nonce}`。
  nonce 构造时锁定并**持久化**（不每次问 RPC），重试/gas-bump 复用同一 nonce → 同一 booking。
  sweep job 经 advisory lock 全局单飞；**一把 sweeper key 只允许一个部署使用**（runbook 红线）。

## 5. 安全模型

1. **RPC 是入账路径的 trusted oracle（最大信任边界）**。伪造 Transfer 日志 = 凭空铸钱。
   缓解分级：设计留双 provider 对账 port（金额超阈值需两个独立源一致才 confirm）；
   至少大额充值进人工复核队列。归集侧 RPC 说谎无资金损失（白扫/不扫）。
2. **地址可预算（salt=holder，factory 公开）**：(a) 灰尘攻击 → 归集阈值防线（§4）；
   (b) 注册前充值 → `EnsureDepositAddress` 注册回扫堵死（§2）。
3. **nonce 并发**：advisory lock 单飞 + nonce 持久化 + 单部署单 key（§4）。
4. **无主资金 vs 偿付性对账**：treasury 链上余额 > 账本负债是预期状态；
   `ledger-cli reconcile` / solvency 检查必须认识 unattributed 类别并单列台账，
   否则对账信号失真（掩盖真实短缺）。
5. **权限钉死**：webhook 通道限定 deposit classification，不得 transition sweep booking；
   sweeper key ≠ factory owner key，owner key（改 treasury、48h timelock）永不进 ledger 消费方配置。
6. **正面不变量（写进文档）**：归集目的地是合约状态（`factory.treasury`），不来自 ledger 配置
   —— ledger 侧全线被攻破也无法把钱导向别处。ledger 侧最坏情况 = 烧 gas + 强制归集（无害）。

## 6. 错误处理

- **深 reorg（confirmed 后回滚）**：`ReorgPolicy` 可配 ——
  - `manual`（**默认**）：watcher 定期复核最近 N 块内 confirmed booking 的 tx 存在性，
    消失则发 `deposit.reorged` 事件 + 告警，on-call 按 RUNBOOK 走 reversal journal。
  - `auto-reverse`：检测到即自动开 reversal journal + `Transition(reversed)`。
    误判（RPC 抖动/节点落后）= 自动扣用户钱，选它需消费方显式承担该风险。
- **ErrConflict**：入账遇到 = 归一化 bug，告警 + dead-letter 表人工看，绝不吞掉重试。
- **watcher 落后 / RPC 宕**：游标不动即落后，暴露 `chain_cursor_lag` metric
  （接 observability/prometheus.go），超阈值告警；恢复从游标续扫，不丢。
- **sweep tx 卡住**：同 nonce gas-bump 重发（同 booking，metadata 记 bump 次数）；
  超重试上限 → `failed` + 告警，人工介入（failed 有 retry 边，沿用 withdrawal 先例）。
- **sweeper EOA gas 不足**：低于水位线发告警事件，本轮跳过而非半途失败。

## 7. 测试策略

- core 派生纯函数单测：跨链地址一致性 vectors，与 azex-contracts forge 测试**对拍同一组 vector**。
- service 编排：testcontainers 真 Postgres（repo 惯例，无 mock DB）。
- `chains/evm`：anvil 本地链，watcher→入账→归集端到端。
- invariants pin 新增两条：sweep 不产生 journal；入账幂等在 reorg 导致 log_index 变化下不重复。

## 8. 本期不做

- TRON / 非 EVM 链（合约侧已明确不支持）。
- 原生币入账（只归集不记账）。
- KMS/HSM Signer adapter（port 已留，默认本地私钥实现先行）。
- 双 provider 对账的第二实现（port 留口，本期单 provider + 大额人工复核）。
- 地址轮换 / 一 holder 多地址（salt=holder 锁定一对一）。

---

## 9. M3 补偿控制 — 入账信任模型加固（2026-07-11 增补，Aaron 拍板「两者都做」）

> 背景：安全评审 M3 指出 §5-1「RPC 作为入账 oracle 是最大信任边界」的**唯一补偿控制**在
> 首版实现中缺失——单次 confirmed sighting 即过账，RPC 被攻破/webhook secret 泄漏 = 无上限铸币。
> 决策：分层防御，阈值门 + 双 provider 对账，**两者汇聚到同一个 `review` 人工审核状态**。

### 9.1 核心结构：两个机制，一个 review 队列

deposit lifecycle 新增 `review` 状态。`confirming → confirmed` 的过账路径（`advanceConfirmation`
的 confirming case，过账前）插入一道闸：

```
confirming (确认数达阈值)
  ├─ 金额 ≤ AutoCreditCeiling 且 对账通过 → confirmed（过账，同今天）
  └─ 金额 > AutoCreditCeiling 或 对账不一致 → review（不过账，emit deposit.review_required）
review
  ├─ 人工 approve → confirmed（补过账，走同一 RunInTx + EventID 互链）
  └─ 人工 reject  → failed（无 journal）
```

新 lifecycle：`pending → confirming → confirmed | failed | expired | review`；`review → confirmed | failed`。
`review` 非终态。**过账动作只在真正 confirmed 时发生**——review 期间用户余额不增，账本不动。

### 9.2 阈值门（threshold gate）

- 配置：`ChainConfig` 每 (chain, currency) 加 `AutoCreditCeiling decimal.Decimal`（**注入配置，不硬编码数值**；
  0 或缺省 = 无上限 = 关闭该机制，opt-in 语义与整个 onchain 子系统一致）。Aaron 部署时定具体数值。
- 判定在 adapter 边界之后、过账之前，纯 service 逻辑，无外部调用。

**§9.2 addendum（M3.1 secure-by-default，2026-07-11 安全评审 MJ1 拍板，见
`docs/bugs/2026-07-11-m3-security-review.md`）**：上面「0 或缺省 = 关闭」的
opt-in 默认值被推翻——0（从未设置）现在是 `service.Onchain.Run` 的**启动错误**，
不是「关闭该机制」。消费方必须显式二选一：正数 ceiling，或哨兵值
`core.UnboundedAutoCredit`（明确接受无上限单源信任）。理由：这一项的默认值直接
等于「信任边界敞开」，与整个 onchain 子系统其余部分（`DepositConfirmer`/
`ReconcileCeiling`/watcher/sweep 均可无害地保持 nil/0）不同类——0 default 会让
「已加固」的 M3 反而制造一种虚假的安全感。`ReconcileCeiling` 不受影响，仍是
opt-in（0 = 不对账，但不影响铸币上限本身）。

### 9.3 双 provider 对账（reconciliation）

- 新 port（消费方定义，`core/interfaces.go`）：
  ```go
  // DepositConfirmer 是入账对账的第二独立数据源。消费方用第二个 RPC endpoint
  // 装配一个 core.ChainReader 的等价实现即可（chains/evm 的 Reader 已满足，
  // 只是连不同的 provider）——无需新 adapter 代码，只是第二个实例 + 不同 URL。
  type DepositConfirmer interface {
      // ConfirmDeposit 复核一笔已达确认阈值的 sighting：返回该 provider 观测到的
      // 金额与是否 canonical 包含。service 比对它与主 sighting 是否一致。
      ConfirmDeposit(ctx context.Context, chainID int64, txHash string, txLogSeq int32) (amount decimal.Decimal, included bool, err error)
  }
  ```
- 配置：`ReconcileCeiling decimal`（同样 per (chain,currency)，注入）——金额 > 此值才触发对账
  （小额不值得双查 RPC 成本；可与 AutoCreditCeiling 独立配置，也可设成同值）。
- 判定：主 sighting 金额 vs 第二源金额必须精确相等 **且** included=true，否则 → review（不是直接 failed，
  因为不一致可能是某一源暂时落后，交人工判定）。对账 RPC 调用**在 RunInTx 之外**（红线：DB 事务内禁外部调用）。
- `DepositConfirmer` 为 nil（消费方没配第二源）→ 对账机制关闭，只走阈值门。两个机制各自 opt-in。

### 9.4 人工审核面

- 存储：复用 booking 的 `review` 状态即是队列（无需新表）。查询 = `ListBookings(status=review, classification=deposit)`。
- HTTP：
  - `GET /api/v1/deposits/reviews`（列出待审，分页，uid-only）
  - `POST /api/v1/deposits/{uid}/review/approve`（→ confirmed + 补过账）
  - `POST /api/v1/deposits/{uid}/review/reject`（→ failed，body 带 reason，脱敏入事件）
  - 端点在 ScopeWrite API-key 鉴权组内（与现有写端点一致）；approve/reject 幂等（同 uid 重复调返回当前状态，非 review 态返 bizcode 冲突）。
- 事件：`deposit.review_required`（金额/原因：over_ceiling | reconcile_mismatch）、`deposit.review_approved`、`deposit.review_rejected`——供消费方接告警/工单。

### 9.5 不变量与测试

- 新 invariant I-21：**review 期间账本零变动**——review 态 booking 的 `journal_uid` 恒空，
  只有 approve→confirmed 才产生 journal（pin：testcontainers 驱动 over-ceiling deposit 到 review，断言无 journal；approve 后才有）。
- 测试：阈值门（金额跨 AutoCreditCeiling 两侧分别走 confirmed / review）；对账一致→confirmed、不一致→review；
  approve 补过账走 RunInTx + EventID 互链；reject 无 journal；review approve/reject 幂等。
- 安全回归：伪造单源 sighting 大额（>ceiling）**必进 review 不自动过账**——这正是 M3 要堵的铸币路径。

### 9.6 分阶段（deployment.md：lifecycle 是 expand 变更）

- `review` 状态是 lifecycle 的**新增**（expand-safe）：老 booking 不受影响，新增转移边不破坏现有 confirmed/failed 路径。
- AutoCreditCeiling/ReconcileCeiling 缺省 = 关闭 → 未配置的消费方行为与今天完全一致（向后兼容）。
  **（M3.1 addendum 推翻此条，见 §9.2 addendum：`AutoCreditCeiling` 缺省改为启动即报错，
  不再是「行为不变」的向后兼容默认——这是本次安全评审后的刻意破例，理由同上。）**

# 发版就绪评估 — 2026-08-27

> **本文件是评估，不是发版决定。** 版本号与发不发由 Aaron 拍板。
> 数据来源：`docs/audits/2026-08-25-financial-engineering/TODO.md` §10（23 条破坏性变更条目）、
> `CHANGELOG.md` 的 Unreleased 段（293 行）、`git log origin/main..HEAD`（95 个未推送提交）。

## 1. 质量门现状（2026-08-27 实跑）

| 门禁 | 结果 |
|---|---|
| `go test -race ./...` | 16 包全绿，无 FAIL |
| `npm test`（`@azex/ledger-react`） | 37 文件 / 174 测试全过 |
| `make sqlc-diff` | 绿 |
| `make openapi-check` | 绿 |
| `npm run codegen:check` | 绿 |
| `go vet` / `golangci-lint` | 绿 |

⚠️ `sqlc-diff` 曾在 main 上连续红过九次合并而无人发现（Team Lead 的 Done 判据漏列了它）。
本轮起它是硬性判据。

## 2. 这一波实际改了什么

| 阶段 | 产出 |
|---|---|
| 三波整改 | 审计的 74 条 Major/Minor，8 个域并行 |
| DR 演练 | 首次实跑；**证伪**了「PITR 会让序列回退」这条被写进 migration 注释的断言 |
| 独立对抗评审 | 0 Critical / 8 Major / 11 Minor —— 全部收口 |
| web 首轮审计 | 2 Critical / 7 Major —— 全部收口（该包此前从未被审过） |

不变式 `I-34 → I-44`，migration `005 → 013`。

## 3. 破坏性变更：23 条，分四类

逐条原文在 `TODO.md` §10。按「消费方要做什么」归类：

### 3.1 必须改代码才能编译（Go 库消费方）

- `Settle` / `Release` / `FinalizeSettlement` / `Transition` 的**幂等键必填**，前三者签名从裸参数改为 Input 结构体
- `CheckpointIntegrityStore.RebuildCheckpoint` 返回类型改为 uid 版 `core.BalanceCheckpoint`；
  id-keyed 的工作类型移到 `service.BalanceCheckpoint`
- `NewReserverStore` 签名、`journals.event_id` 的 Go 类型（更早的变更）
- 新建**非系统分类**必须显式声明 `balance_role`（含新的 `memo` 值）——`Validate` 会拒绝空值

### 3.2 必须改数据（最重的一类，别漏）

- **`transfer_out` / `transfer_in` / `fee_charge` 的借贷方向修正** ——
  **消费方需要冲销既有 journal**。这是 C7，实测过：转账 100 元原本让付款方 **+100**
- **分数冲销的聚合口径修正** —— 需要检查含重复维度的既有冲销

### 3.3 HTTP / wire 行为变化

- 四个 `core` 哨兵的 HTTP 表现（`ErrUnauthorizedJournal` 从 500/19999 改为 422/14010 且不再可重试）
- `next_cursor` 现在真的序列化为 JSON `null`
- **`GET /holder/transactions` 的 `kind` 语义第三次变**：内部 code → uid → 稳定词汇
  （`deposit`/`withdrawal`/`transfer`/`fee`/`adjustment`/`other`）
- `POST /reservations/{uid}/settle|finalize|release` 请求体新增必填 `idempotency_key`
- `/system/health`、`/system/ready` 失败响应改用统一信封
- `POST /journals/template` 可能新增 403（仅当部署方设置了保护清单）

### 3.4 TS 类型收紧（8 个 schema 补 `required:`）

`Balance` / `Reservation` / `Booking` / `DepositAddress` / `Event` / `ReconcileResult` /
`SystemRollup` / `BalanceBreakdown`。**Go 侧零改动，wire 格式与昨天逐字节相同**——
break 只发生在重新生成 TS 类型的消费方身上，且 optional→required 对读取代码是安全方向。

## 4. 发版前仍然敞着的三件事

这三条**不阻塞**发版，但发出去的版本带着它们。列在这里是为了「知道自己在发什么」。

1. **锚定载体未选型** —— `anchordev` 自己的包注释写着 DEV / TEST ONLY。
   防篡改链（P5 签名 → P6 批量存证 → **锚定** → 验证）的最外环在生产形态下没有实现。
   签名那半（`authdev`）是刻意做成生产可用的、有威胁模型依据；锚这半不是。
   载体候选与取舍见对话记录：Object Lock 分账号 / 公链 / 第三方时间戳 / 另一失效域的库。
2. **没有任何真实消费方** —— 44 条不变式、benchmark、capacity 文档全部是**对着测试**验证的，
   不是对着流量。本次审计最主线的失效形态正是「能力在测试里工作，因为测试自己把线接好了」，
   而一个没有消费方的库结构上无法发现这一类问题。
3. **DR 演练的 RTO/RPO 是玩具数据集的数字** —— `docs/DR.md` 已注明不能外推到生产规模。

## 5. 版本号（Aaron 拍板）

- 当前：Go module 与 `@azex/ledger-react` 都在 `0.5.1`，要求**同发**
- 23 条破坏性变更里有两条要求**消费方改数据**（§3.2），不只是改代码
- 建议在 CHANGELOG 顶部单独立一节「升级前必读」，把 §3.2 那两条放在最前——
  它们是唯一「不看就会算错钱」的

# 全代码库深度审计（第二轮）— Phase 0 契约

> 2026-09-02 · Status: **CLOSED**（十份报告 2026-09-02 全部交付，lead 复核记录见 `docs/audits/2026-09-02-deep-audit/lead-verification.md`；Aaron 当日拍板全量修复，Wave 1–4 整改已全部合入 —— 处置表见 `docs/audits/2026-09-02-deep-audit/README.md`，整改契约 `2026-09-02-remediation-contracts.md`，随后的独立复审见 `2026-09-03-independent-review-contract.md`）。原文：Status: OPEN · 基线 commit `fd87cae`（0.6.0 之后，含 R2 anchor / 符号权威收敛 / 三轮整改）。
> 方法沿用 `2026-08-25-financial-audit-contract.md`（§1 纪律与 §3 产出格式**原文有效**，本文不复述），
> 只重定 territory：上一轮之后代码变了一大块（防篡改链接线、I-42 符号权威、`journal_entries.id` 序列化、
> `anchors/r2` 新模块、web 整改），且上一轮自己承认漏了「可运维性」与「链上资金路径」两个维度，本轮补成独立 territory。
> 报告落点：`docs/audits/2026-09-02-deep-audit/<territory>.md`。

## 0. 本轮与上一轮的关系

- 上一轮 8 Critical + 40 Major + 34 Minor，处置状态见 `docs/audits/2026-08-25-financial-engineering/README.md`。
  **不要重报已关闭条目**，但要**证伪它们的修复**：每个 territory 都有一段「上轮修复复核」——
  对落在自己 territory 里的上轮修复，问三件事：修复是否真的接到真实路径（不是只在测试里接好）；
  回归 pin 若把修复拆掉会不会红；修复有没有在别处复制出同形缺陷。
- 上一轮的方法教训继续生效：§1.4「有测试不等于被验证」、§1.5「不要编造具体值」、§1.1 只读。
- 上一轮 lead 发现的共同盲区（「篡改之后能不能复盘」）已部分修复（四张配置表加了 trigger）——
  本轮 D territory 负责核它是否覆盖完整。

## 1. 十个 territory（按问题切）

| # | territory | 拥有的问题 | agent 类型 |
|---|---|---|---|
| A | financial-correctness | 金额类型/精度/舍入/符号；借贷平衡；余额推导；偿付能力；I-42 符号权威是否真的唯一 | code-reviewer |
| B | concurrency | 锁与键空间；隔离级别；TOCTOU；幂等三态；事务边界；worker claim/lease；goroutine 退出 | code-reviewer |
| C | tamper-evident | P5 签名 / P6 存证 / Merkle 验证 / `VerifiedBalance` / anchors（r2 + anchordev + anchortest）/ 密钥管理 | security-reviewer |
| D | threat-model | DB 三角色 ACL 与 trigger 覆盖；HTTP 鉴权与 API key scope；webhook 入站；dev_credit 门禁；篡改后可复盘性；用户面泄露 | security-reviewer |
| E | consumer-surface | facade `ledger.go` 每个导出方法的误用面；`RunInTx` 克隆；Worker 接线；examples 是否示范正确用法；README/docs 契约 | code-reviewer |
| F | test-credibility | 非空洞性；消费方对等性；互斥测试；INVARIANTS `Pinned by` 是否真能证伪；CI 是否真的跑到（含子模块） | code-reviewer |
| G | onchain-money-path | `chains/evm` + `channel/onchain` + `service/onchain.go`：扫链 fail-open/closed；确认数；reorg；CREATE2 派生；sweep 签名；AutoCreditCeiling；rescan | code-reviewer |
| H | structure-and-contract | 六边形边界；port 位置；schema/索引/分区与查询匹配；`api-contract.md` 逐端点；openapi ↔ Go ↔ TS 三方一致；`deployment.md` 演化能力 | system-design-reviewer |
| I | operability | metrics 是否可告警；RUNBOOK/DR 与代码是否一致；worker 三 job 的可观测；`cmd/ledger-cli` 取证能力；日志有没有泄露金额/密钥 | code-reviewer |
| J | web-ledger-react | `@azex/ledger-react` 三消费面（root / heroui / headless）：金额展示、四态、幂等 key、skin parity、类型与 openapi 一致、用户面泄露 | code-reviewer |

看到不属于自己的问题 → 写进「移交」段，不深挖。

## 2. 本轮附加纪律

- **允许跑命令验证**（Docker 已起，testcontainers 可用），但 `go test` 一律 `timeout: 600000`，
  且只跑与自己 territory 相关的包；不许改任何非报告文件。
- **拆掉再看**：证伪一条 pin 时可以在**内存里**推演「删掉这段实现哪个测试会红」，
  不要真的改源码；实在要跑，先 `git stash` 级别的可逆操作，跑完立即 `git checkout -- .` 恢复并在报告注明。
- 每条发现的 `file:line` 以 `fd87cae` 为准。

## 3. Team Lead 职责（同上一轮 §4）

逐条复核 CONFIRMED；Critical/Major 停下给 Aaron 拍板；交叉比对十份报告找共同盲区；合并去重统一定级。

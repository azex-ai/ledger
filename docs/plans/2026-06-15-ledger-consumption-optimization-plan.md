---
created: 26-06-15
source: conversation
context: ledger
tags:
  - status/exploring
  - type/decision
related:
  - "[[26-06-12-Harness-Taxonomy-Migration]]"
---

# Ledger 消费优化方案 — 让其他项目更好地引入 ledger

> 目标：把 `azex-ai/ledger` 从「下游难引、引了易炸部署」改造成「pin 一个 tag 就能用、本地联调零摩擦、永不把本地路径带进部署」的内部共享库。
> 触发：harness-evolve all-history 扫描发现，ledger 跨项目消费反复炸部署（跨 armatrix + yugen/azex、跨月）。

## 问题陈述（证据）

ledger 定位为跨项目内部库（决策 `ledger/.claude/.../ledger-module-positioning.md`，obs #12662，26-05-29），被 azex / yugen / caste / armatrix 的 Go 后端引用。但消费体验有结构性缺陷，**反复炸部署**：

| 证据 | 日期 | 现象 |
|---|---|---|
| #16760 | 06-11 | armatrix 部署：`go.mod` 含 `replace github.com/azex-ai/ledger => /Users/aaron/ai/ledger` 绝对本地路径，Railway 容器内不存在 → `go mod download` 挂 |
| #16775 / #16785 | 06-11 | 同错连撞（「Third」「Fifth Deployment Confirms Identical Build Path Error」）|
| #16766 | 06-12 | yugen/azex：去掉 replace 后又暴露「Invalid Placeholder Version」（伪版本 `v0.0.0-…-hash` 不可用）|
| #16769 / #16771 | 06-12 | 重部署 → Module Download Succeeded, Compilation Failed |

**两个根因**：
1. **ledger 没有任何 semver tag**（`git -C ai/ledger tag` 为空）。它自己 README L42-68 推荐的发布消费方式 `go get github.com/azex-ai/ledger@<tag>` **不可执行**——没 tag 可 pin。下游被迫用 commit 伪版本（要求 commit 已 push 且可达），或临时塞本地 `replace`（README 明确警告不要用）。
2. **下游没采用文档里的 `go.work`**：armatrix / yugen 的 server 目录均无 `go.work`。本地联调时一旦手改 committed `go.mod` 加 replace，就跟着 commit 进部署。

> 注：README **已有** `Local Development with go.work` 段，文档不缺；缺的是 ① 库的发布物（tag）② 下游对该模式的采用 ③ 一道防止本地 replace 进部署的护栏。

## 方案：三层修复

### 层 1 — 库侧：建立发布物与发布流程（ledger 仓库）
让 `go get …@<tag>` 从空头支票变成可执行路径。

- 给 `azex-ai/ledger` 打首个 semver tag（建议 `v0.1.0`，承认尚未稳定的 0.x 语义）。
- 约定发布流程并写进 ledger README 的「Consuming from another Go module」段（与现有 go.work 段并列）：CI 绿 → 打 tag → 更 CHANGELOG → 下游 `go get …@v0.x.y`。
- 后续按 `deployment.md` 的演化纪律：破坏性 API 变更走 minor/major bump + CHANGELOG，下游按需升级。

### 层 2 — 本地联调契约：标准化 go.work（下游仓库）
消费方本地改 ledger + 调用方时，**用 `go.work`，绝不动 committed `go.mod`**。

- 为 armatrix / yugen（及后续 caste / azex 真用到时）在 server 父目录放 `go.work`（git-ignored，README 已说明用法）。
- committed `go.mod` 永远 pin 到一个**已发布 tag**（层 1 产出），不是伪版本、不是本地 replace。

### 层 3 — 护栏：禁止本地 replace 进部署（下游仓库 + 可选 CI）
- 下游加一道轻量检查（pre-commit 或 CI 一步）：`go.mod` 出现 `replace … => /Users/` 或 `=> ../` 绝对/相对本地路径即 fail，提示「本地联调请用 go.work」。
- 与 `feedback_stop_repeating_failed_command` 配套：部署 build 连撞 2 次同错先查 `go.mod` replace + 本地结构，不盲目重跑。

## Phase 拆解与验收

| Phase | 动作 | 验收 |
|---|---|---|
| P1 库发布 | ledger 打 `v0.1.0` + README 加发布消费段 + CHANGELOG | `go get github.com/azex-ai/ledger@v0.1.0` 在干净环境可拉取 |
| P2 下游迁移 | armatrix + yugen server 加 `go.work`，`go.mod` 改 pin `@v0.1.0` | 两项目本地 `go build` 绿；committed `go.mod` 无任何本地 replace |
| P3 护栏 | 下游加 replace-本地路径检查（pre-commit/CI） | 故意提交一条 `replace => /Users/...` 被拦截 |
| P4 验证 | 各项目走一次真部署（Railway/容器） | 部署不再因 ledger 依赖失败；构建上下文 zero local-path 依赖 |

## 边界 / 非目标
- 不改 ledger 内部领域逻辑（双分录、reserve/settle 等不动）——只动**消费契约与发布物**。
- React (`@azex/ledger-react`) 与 service 模式的消费路径本次不重做（炸的是 Go lib 路径）；如未来 npm 侧也踩坑再单列。
- 不在本方案里执行——这是方案，按 Aaron 排期分 phase 落地。

## See Also
- ledger README `Local Development with go.work`（L42-68）— go.work 用法已文档化
- `~/.claude/rules/deployment.md` — expand→migrate→contract / 契约演化
- `~/.claude/rules/financial.md` — ledger 领域不变量（本方案不触碰）
- `memory/feedback_stop_repeating_failed_command.md` — 同错连撞即停、先查 replace/本地结构
- harness-evolve runlog `26-06-15-1750-last-9d.md` + 本次 all-history 重扫

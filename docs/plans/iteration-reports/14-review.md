# 第 14 轮独立域评审

结论：**PASS**。本次 diff 未发现需要阻断合并的问题。

## 范围与职责

- 评审基线：`c5d0f1e06f71cdf2352b502574ba1efeb2a455c9` → `a29d202679c33e88dbf8432660ee7e46d0aa5c11`。
- 独占 worktree：`codex/ledger20-t14-review`。只读六个变更文件及理解其调用契约所需的上下文，没有修改业务源码。
- Input → Output → Handoff → Gate/Review → Memory：Task 14 diff、冻结的 Capture 契约及 demo 迁移裁决 → 本域评审报告 → root → Codex 第二意见及串行集成 gate → 本报告记录项目证据与适用边界。

## 核对结果

1. `examples/credits-topup/main.go:315` 的正金额路径直接调用 `svc.Capture`，保留原事件 base key，由 facade 派生 `:settle` / `:charge`，并传入实际 reservation UID、金额、模板及 Partial 标记。旧的手工 settlement 与 charge 事务组合已删除；`credits_spend` 模板仍使该用户的 available 分类按金额减少。Capture 的事务、锁并集及幂等语义没有在示例层被绕过。
2. 零金额 full 路径仅以 `:release` 释放预留，不创建消费 journal；零金额 partial 安全拒绝。流式正金额使用 Partial Capture，结束时单独 FinalizeSettlement，释放剩余 hold。失败回归明确检查 `ErrAccountFrozen`、reservation 仍 active 及 settlement receipt 回滚；未把部分提交认证为成功。
3. quote metadata 先复制再添加 `usage_event_id`；helper 不写 Capture 的保留字段。新增 full / partial 回归检查原 map 不变、quote 与宿主字段保留、Capture 保留字段、unsigned journal 状态及同 key 重放后的余额、hold 和 journal 数量。
4. `main.go:287` 的 legacy guard 同时检查 journals 与 reservations，并由 setup、scenario 在各自业务写入前调用。旧 `credits-demo*` 事件被拒绝；当前 `credits-demo-v3-capture:` 事件可重放。实现没有重命名既有事件或修改旧 journal。bootstrap migration 先于 guard，与冻结裁决及 README 一致。
5. 旧 journal / reservation 两类测试分别确认拒绝后六个账务表行数、余额与 hold 不变；当前 v3 测试先重新 setup 再重放 scenario，检查没有额外 journal。新 namespace 明确仅适用于独立的新 demo 数据库；文档未把换 key 描述为宿主升级旧事件的办法。
6. README、实现报告及公共余额断言一致区分分类账面余额、held 与 `GetBalanceBreakdown.available`。流式两次消费后的账面余额分别为 932.875 / 912.875，held 为 90 / 70，可用余额均为 842.875；Finalize 后可用余额为 912.875。

## 验证证据与限制

- 本席独立完成 diff、相关实现与测试断言的静态评审；`git diff --check c5d0f1e06f71cdf2352b502574ba1efeb2a455c9..a29d202679c33e88dbf8432660ee7e46d0aa5c11` 通过。
- PostgreSQL 动态证据沿用实现者在 `14.md` 记录的 `go test -race ./examples/credits-topup/... -count=1 -timeout 5m`，PASS，12.979s；每个 case 使用独立数据库及 `ledger_app` 运行角色。**该命令并非本席运行**。未发现需要额外 PG 复现的疑点，未重复占用 PG slot。
- 本报告只覆盖 Task 14 的示例接入与迁移边界；不声称完成真实链上充值、外部 provider、signer 或全仓集成验收。root 负责第二意见及后续集成 gate。

阻断项：无。待修订项：无。

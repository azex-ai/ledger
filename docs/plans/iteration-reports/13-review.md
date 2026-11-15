# 第 13 轮独立域评审

结论：**PASS**。本次 diff 未发现阻断项或待修订项。

## 范围与职责

- 基线：`09d270fac54b4c6de50f4434ca72e1ef7703d7f4` → `1a585d015e69a7f3e7f44139e1be8e635441cce8`。
- 独占 worktree：`codex/ledger20-t13-review`；仅写本报告，未修改源码或提交。
- 完整阅读新增的 `examples/signed-capture` 实现、测试、README 与 `13.md`；核对其调用的 Authorize / PostAuthorized、锁、事务、验签、verified balance 与 settlement 契约，没有扩大为全库评审。
- Input → Output → Handoff → Gate/Review → Memory：冻结的签名组合边界与本次 diff → 独立金融域意见 → root → Codex 第二意见及集成 gate → 本报告保留证据与限制。

## 评审结果

1. **外部调用与事务边界正确。** `capture.go:51` 先执行顶层 `AuthorizeTemplate`，随后完成新授权验签或重放历史验证；`RunInTx` 从第 75 行才开始。事务内仅依次执行 `LockForTemplates`、`PostAuthorized`、`Settle` / `SettlePartial`。既有 API 的 transaction-bound 分支不会调用 signer / verifier。
2. **重放兼容分支严格符合裁决。** 第 55 行要求 `StatusSigned` 且 verifier 存在；第 58 行仅在 Digest、Signature、KeyID 全空时进入历史验证。新签名与部分缺损材料进入 `VerifyJournalAuth`，后者检查 digest 一致性及签名材料完整性。重放所涉两个模板维度逐一调用 `VerifiedBalanceReader`；事务内仍由 `PostAuthorized` 在锁下核对原 key/payload。README 明确该验证覆盖完整历史，其他 unsigned / bad-signature journal 也会导致保守拒绝，未将其描述为单笔 journal 查询。
3. **原子性与锁范围保持。** 两个事件 key 由同一不可变事件 ID 派生；`LockForTemplates` 同时覆盖 journal key、settlement key 和固定模板的用户/系统余额维度。charge 与 settlement 使用同一金额。callback 的每个错误都向上传播，提交失败或 settlement 失败时返回 nil journal，避免对回滚结果发布成功回执。既有余额锁及 reservation 行锁负责串行化并发写入。
4. **幂等契约与宿主责任清楚。** 固定 EffectiveAt、金额、Partial、reservation 关联和双 key 的重放边界写入注释及 README。模板、可信 reservation 和跨事件/跨操作去重明确由宿主掌握；此 private example helper 没有声称接受任意客户端 reservation 或提供通用 Capture 的输入防线。
5. **签名与可用金额没有混淆。** 正式模板将用户 main_wallet 减少 20，系统 custodial 同币对应减少；journal 使用已验证的签名授权，事务内 discharge 则没有签名。VerifiedBalance 为 80，普通 full / partial hold 为 0 / 40；新 verified reserve 仍计入未到期原始 hold 60，所以 21 拒绝而 20 可用。README 没有承诺完整生命周期均已签名，也没有承诺与外部 provider 的原子提交。
6. **测试断言覆盖实际经济效果。** full / partial 检查余额、普通 hold、reservation 状态与持久化 discharge 的空签名字段；固定时间重放检查相同 UID、signer 不再调用及七类表行数不变。失败场景检查 nil receipt、金额与 hold 不变、reservation 仍 active。61 大于原始 hold 60、却小于余额 100 的用例能触发“journal 可写但后续 settlement 拒绝”的真实回滚路径。未验证历史、连续 partial event 与 80 − 60 门槛均有数据库断言。
7. **运行说明与 executable 一致。** `run()` 包含 migration、runtime role 校验、真实 facade 调用、同次运行重放和门槛断言；随机开发密钥仅限全新示例数据库，README 如实说明其密钥保管、重启及生产适用边界。

## 验证证据与限制

- 本席独立完成静态实现及测试评审；`git diff --check 09d270fac54b4c6de50f4434ca72e1ef7703d7f4..1a585d015e69a7f3e7f44139e1be8e635441cce8` 通过。
- 动态证据沿用实现者 `13.md`：真实 PostgreSQL 17 上 `go test -race -timeout 5m ./examples/signed-capture/... -count=1`，12 个叶测试 PASS，6.183s。数据库按测试隔离，金融写入以 `ledger_app` 执行；测试 signer/verifier 每次通过独立连接检查 `pg_stat_activity` 中其他打开事务。
- **上述 PG 测试并非本席运行。** 未发现需要额外复现的新疑点，未重复执行同一 suite，也未占用 PG slot。作者记录的 compile-only 与 vet 证据不替代 PG 验收。
- 本报告只评价示例在冻结契约下的组合方式；没有独立执行全仓集成、生产 signer、链或外部 provider 验收。root 负责第二意见与后续集成。

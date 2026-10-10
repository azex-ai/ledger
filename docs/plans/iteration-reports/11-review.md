# 第 11 轮独立域评审

结论：**PASS**。本 diff 未发现 blocker，无需修复波次。

范围：`f13d0ae48ad6052e80f91ac57d23e11378566271..6e62ceffd504ff643d14a48a1d82c42fe3dfd98e` 的 4 个文件；独占分支 `codex/ledger20-t11-review`。读取全部 diff、新增单元/真实数据库测试及 Exchange 完整事务体。未修改业务源码，未提交。本报告占 T2 域 reviewer 一席，主控另给 Codex 第二意见。

输入 → 输出 → 交接 → 验证 → 记忆：两腿 available 经济效果契约与实现 diff → 本报告 → root 裁决/集成 → 独立 helper race 测试和 diff 检查、实现者 PG 证据 → Exchange/Capture 不同模板约束已记录在实现注释与 `11.md`，不新增全局规则。

## 核验

- `holderLegNet` 先拒绝每条腿的其他 currency、其他 user 或其他 system counterpart，再仅累计目标 holder 的 available 分录。系统对手方不会计入用户净额；holder 已由 Exchange 验证为正数，派生 counterpart 不存在 MinInt64 取负问题。
- 每个用户 entry 都按该 classification 的 NormalSide 调用 `core.SignedAmount`。算法未硬编码“credit 一定减少”，debit-normal、credit-normal 及多个 available 分类的组合保持正确。
- 非 available 净额以 ClassificationUID 分组。此前已固定 holder/currency，因此该键与完整 `(holder,currency,classification)` 维度等价；不同 pending 分类、pending/locked 或 memo 不能相互抵消。只有同分类精确净零允许通过，符合本轮契约。
- 调用点仍分别比较卖出 `-Quantity`、买入 `Quote.TargetAmount`；unexpectedChange 与错误金额任一成立都会拒绝。空 entries 也不可能匹配已验证为正的 quantity/target。未知用户分类返回错误，系统分类由已落库 entry 的外键保证存在。
- 锁预取、Reserve/Settle、模板 batch、quote metadata、派生幂等 key 与 transaction/clone 分支未改。检查仍在同一事务提交前针对实际 journal entries 执行；普通调用整体回滚，宿主事务须返回错误的既有要求保持。
- `classificationRoles(ctx, tx)` 签名和实现保持，Capture 的复用不会因本 diff 受影响。Capture 禁止非 available entry，而 Exchange 允许同维度净零，两者没有被合并成错误的共同语义。

做得好的地方：新增 12 个真实模板都保持双分录平衡，直接检验“账平但经济效果错误”的核心问题；测试在拒绝后核验两币余额、所有自定义分类、hold 与六类持久化记录，并复用原 key 成功重试，证明完整回滚。

## 验证

本席独立执行：

```sh
go test -race -timeout 2m . -run '^TestExchangeHolderLegNet_RequiresAvailableAndExpectedScope$' -count=1
git diff --check f13d0ae..6e62ceffd504ff643d14a48a1d82c42fe3dfd98e
```

结果：helper 测试 PASS，`github.com/azex-ai/ledger 1.356s`；diff 检查 PASS。该测试不使用 PostgreSQL，覆盖合法 normal_side、多个 available 分类、同 pending 分类净零、跨分类抵消拒绝及 holder/currency/counterpart 范围。

真实 PostgreSQL 证据沿用实现者 `11.md`：修复前 12 个平衡模板均因错误接受而 RED；修复后全部 Exchange `-race` 回归 PASS（13.195s），包含既有 quote、幂等、方向、事务回滚、funding 与 lock 集合测试。本席未重复该 PG 套件，不将实现者运行表述为独立运行；无需要新增 PG 证伪的未决点。无运行中测试或本席占用的 PG 资源。

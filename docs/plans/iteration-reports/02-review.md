# 第 2 轮独立域评审

结论：**PASS**。此次 diff 未发现阻塞项；无需修复波次。

评审范围：`97e33362abd436c072114ab863079ac65a78fad1..ae0c1b7d68a8d4e8883b2be8b1f984d9a6303fe4`，共 5 个文件。评审者在独占 worktree `codex/ledger20-t02-review` 审阅全部 diff、完整策略执行函数、直接调用点和相关既有测试。未修改业务源码，未提交。本报告占 T2 域 reviewer 一席；Codex 第二意见及分支最终验收由主控另行记录。

输入 → 输出 → 交接 → 验证 → 记忆：Task 2 冻结契约、实现 diff 与 `02.md` → 本报告 → root 裁决和集成 → 独立真实 PostgreSQL 回归 → I-17 已记录按币种汇总规则，本次无需重复增加规则。

## 核验结果

| 核验点 | 证据与结论 |
|---|---|
| 异币种不能抵消冻结扣减 | `postgres/account_policy_enforce.go:23` 定义独立 `(policyID, currencyID)` 键；`:101` 使用已解析分录的真实 currency ID 汇总；`:108` 对每个键分别拒绝负值。USD 扣减与 PTS 增加即使命中同一 holder-wide policy，也不会进入同一净额。 |
| 同币种跨分类的 pending 确认仍允许 | 键未引入 classification；同币 pending 减少和 available 增加继续合并。`postgres/account_policy_store_test.go:317` 扩展的两种 wildcard 冻结均确认 pending 清零、available 增加，并拒绝随后真实扣减。 |
| closed 与 min_balance 语义保持 | closed 仍在 `postgres/account_policy_enforce.go:91` 逐条拒绝；min_balance 仍在 `:117` 使用原有 `(holder,currency,classification)` 净额。策略查找与优先级未改。相关既有状态矩阵、优先级及三类余额下限测试通过。 |
| 拒绝无部分入账 | 策略检查仍位于 `postgres/ledger_store.go:1247`，在 journal/entry 写入前。新测试检查 journals、journal_entries、balance_checkpoints、rollup_queue 行数及用户/系统双方两币余额不变。 |
| 新测试能够检出原缺陷 | 四个跨币种 case 明确要求 `ErrAccountFrozen`，覆盖相等/较大 PTS 数量与两种分录顺序。原 `map[policyID]` 会把这些数量汇为非负，因此无法满足断言；解冻后使用同一 idempotency key 成功则证明拒绝不是 journal 本身无效或 key 已被占用。本次未重复运行实现者已记录的修复前 RED。 |

做得好的地方：改动只收窄冻结汇总维度，没有改变借贷方向、数据库模型或既有锁序；测试同时验证错误、余额、持久化副作用与解冻后的成功路径，未把“复现成功”误当成“修复成功”。

## 独立验证

在审核 worktree 中，将 `DATABASE_URL` 指向主控为本任务准备的临时 PostgreSQL 17；`internal/postgrestest` 为测试创建独立数据库。执行：

```sh
go test -race -timeout 5m ./postgres -run '^TestLedgerStore_(Frozen_CrossCurrencyCannotOffsetDecrease|ConfirmPending_SucceedsWhileFrozen|AccountPolicy_)' -count=1
git diff --check 97e33362abd436c072114ab863079ac65a78fad1..HEAD
```

结果：测试 PASS，`github.com/azex-ai/ledger/postgres 5.796s`；diff 检查 PASS。测试已结束，重 PostgreSQL 测试资源已释放给主控。

边界：本次仅评审 Task 2 的冻结汇总修复，不代表全仓验收。I-17 中既有 holder-wide 策略更新的跨币种并发锁限制没有因本次改动消失，实现报告对此披露准确。

# 第 6 轮独立域评审

结论：**PASS**。此次 diff 未发现 blocker，无需修复波次。

范围：`ab3a5d1844dd0a6c8fda85118ce4bc6a2d6f3696..8f52e084a22e7941a1233e09526611e6182b9b60`，6 个变更文件；在独占分支 `codex/ledger20-t06-review` 审阅全部 diff，并比对现有 client 方法、`types.ts`、生成 DTO 和 server client 委托。未改业务源码，未提交。本报告占一次 T2 域 reviewer 席位，主控另给 Codex 第二意见。

输入 → 输出 → 交接 → 验证 → 记忆：SDK safe-integer 契约、实现与 `06.md` → 本报告 → root 裁决/集成 → 独立 build 和 client 测试 → `BREAKING.md` 已记录 numeric SDK 边界和重试语义，无需新增全局规则。

## 核验

| 检查项 | 结果 |
|---|---|
| 数字身份边界 | `holder-boundary.ts:12` 使用 `typeof === "number"` 与 `Number.isSafeInteger`，覆盖正负安全边界、非整数和非有限值；不夹断、不替换、不从舍入后的大 number 推断原始 holder。 |
| 请求完整性 | `:53` 的 body 路径、`:93` 的 balances/holders path 与 `:99` 的 holder query 覆盖现有公开方法的 16 类入口；preview 单独选择 holder_id，batch 遍历所有 holder，journal 遍历所有 entry。 |
| 响应完整性 | `:65` 覆盖现有 DTO 的单行、分页 list、journal/preview entries、balance classifications、batch 外层 holder_id/内层 balances 与 reconciliation details。检查发生在 `client.ts:152` 返回 data 前，后续 `.then(d => d.list)` 不能绕过它。 |
| Promise 语义 | 请求检查位于 `client.ts:107` 的 async request 内；无效 numeric holder 返回 rejected Promise，fetch 尚未调用。原有 JSON.stringify 的其他输入异常不属于本次承诺。 |
| metadata 例外 | 仅沿声明的 DTO 路径遍历，未递归扫描同名任意字段；metadata、模板 amount key、actor_id 保留既有处理。缺失字段/非数组结构也没有被误宣称为完整 schema 验证。 |
| 测试有效性 | 通过公开方法验证行为；响应使用原始 JSON 字面值分别保留相邻大 int64，避免测试构造阶段先舍入。任一后续行异常时拒绝整页；请求端断言零 fetch。 |
| 集成与兼容说明 | `createServerLedgerClient` 委托同一实现；`BREAKING.md` 明确安全整数子集、既有大 int64 的宿主路径及响应拒绝不能回滚已提交操作。模板预览测试补合法 holder 前置输入，未减弱 totals 断言。 |

做得好的地方：统一 request 边界使所有现有消费入口共享同一拒绝语义；选择器避免误把 metadata 中的业务标识当成 ledger holder。

## 独立验证

以下在本 reviewer worktree 执行，node_modules 为本目录独立安装：

```sh
npm ci --no-audit --no-fund
npm run -w @azex/ledger-react build
npm test -w @azex/ledger-react -- test/client
git diff --check ab3a5d1..8f52e084a22e7941a1233e09526611e6182b9b60
```

结果：安装、ESM/DTS build、diff 检查通过；client 测试 **3 文件 / 122 项 PASS**，Vitest 总耗时 781ms。另用 Node 导入本次构建的 `dist/server.js`，调用 `createServerLedgerClient().getBalances(Number("9007199254740993"))`，独立确认返回 Promise、以 `LEDGER_UNSAFE_HOLDER` reject、fetch 调用次数为 0。

未重复全套 346 项测试，未用 PostgreSQL；全包测试和既有 gate 告警沿用实现报告证据。此范围不包含尚待 Task 7 的 UI 字符串解析，也未把基线中的 expires_in 契约作为本任务新问题。未来新增 holder-bearing endpoint 仍需更新选择器与对应方法回归，`06.md` 已准确披露该维护边界。

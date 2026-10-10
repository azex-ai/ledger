# 第 10 轮独立域评审

结论：**PASS**。本次 diff 未发现阻断项或待修订项。

## 范围与职责

- 基线：`c93481e42db724045c2ec829e47e6b5c0e1f615a` → `46ac7b699ecae278ee99e91369774cb4d369f669`。
- 独占 worktree：`codex/ledger20-t10-review`。完整阅读 `examples/configured-ledger/**` 与 `10.md`，只读核对相关 presets、Render 和 Exchange 契约；未修改源码、未提交。
- Input → Output → Handoff → Gate/Review → Memory：Task 10 冻结范围、原币账本边界与本次 diff → 独立金融域意见 → root → 第二意见及串行集成 gate → 本报告记录证据与适用限制。

## 核对结果

1. **配置复用符合目标。** 示例直接使用 `CurrencyInput`、`TemplateBundle`、`TemplateParams` 和 `FixedRate`，安装既有 Deposit / Fee / FX bundles。POINTS 通过相同配置类型声明 credit-normal 的系统 `points_issued`，复用 debit-normal / available 的 `main_wallet`；没有另建 DSL、核心 API、SQL 或依赖。安装后的 NormalSide 与 BalanceRole 有实际数据库断言。
2. **经济方向与数量正确。** USDC 用户 wallet 为 `100 − 25 − 1 = 74`，CREDITS 为 `1000 − 20 = 980`，POINTS 为 100，ROSE 为 2。四行 fee 为 DR user fee_expense / DR system custodial / CR user main_wallet / CR system fees，产生 memo expense +25 与费用收入 +25，同时减少 wallet 和 custodial 各 25。系统最终各分类分别为 USDC custodial 75、fees 25、settlement −1；CREDITS settlement 980；POINTS points_issued 100；ROSE settlement 2。源码、字面量 oracle 和 README 一致，没有混合币种或分类余额来宣称守恒。
3. **平衡与经济效果分别验收。** `main.go:250` 从持久化分录逐 journal、逐 CurrencyUID 计算 DR − CR；`main.go:213` 的业务 oracle 则逐 holder/currency/classification 检查独立字面量预期。oracle 不从待验收模板、rate 或 quote 反推期望。测试还检查全部 hold 为零，以及各币种 `GetBalanceBreakdown.available` 等于 wallet 数量，防止 memo expense 增加被误算为可消费余额。
4. **反向 fee 负例确实拒绝经济错误。** `main_test.go:98` 只克隆测试候选并反转四行方向，保留已安装 preset。候选先经 Render、输入验证和真实事务内记账，再证明 journal 平衡但用户 wallet 为 125、expense 为 −25、系统 custodial 为 125、fees 为 −25。独立预期 75 / 25 / 75 / 25 返回 `errEconomics`，经 callback 传播后，journal/reservation 数量及四项余额恢复到原始状态。
5. **错误 gift rate 负例没有认证缺陷。** `main_test.go:149` 用合法 rate 0.5 得到 10 ROSE，先核对两个 journal 各自平衡及用户/系统实际余额，再由独立的 2 ROSE 要求拒绝。外层 RunInTx 传播错误，回滚 Exchange 的两个 journal 与 reservation；用户及系统 CREDITS / ROSE 余额、持久 journal/reservation 数量和源币 hold 均恢复。
6. **重放与提交边界如实表达。** 完整正常场景产生七笔 journal、两个已完成 reservation；重装匹配配置后重放相同事件 ID，测试确认 UID、余额和数量保持。已提交 gift 更换 rate 返回 `ErrConflict`，无额外效果。README 明确配置版本仅用于新事件，不能改旧事件 ID 绕过冲突；普通 executable 是提交后的验收，不能自动撤销已提交错误配置，只有负面候选测试显式使用外层事务回滚。
7. **宿主边界明确。** 示例只使用本地充值与积分发行 fixture；原币数量、USD 展示估值、配置兑换与提现资格分离。raw fee journal 不管理 hold，生产并发收费的授权和 reservation 组合由宿主负责。文档没有将 settlement 余额当作外部托管证据，也没有声称连接市场或执行真实交易。

## 验证证据与限制

- 本席独立完成静态实现、经济方向及测试断言评审；`git diff --check c93481e42db724045c2ec829e47e6b5c0e1f615a..46ac7b699ecae278ee99e91369774cb4d369f669` 通过。
- 动态证据沿用作者 `10.md`：真实 PostgreSQL 17 上 `go test -race ./examples/configured-ledger/... -count=1 -timeout 3m`，三个测试 PASS，2.970s；每个 case 独立数据库，应用写入以 `ledger_app` 执行并检查 runtime role。作者另有 vet 通过记录。
- **上述 PG 测试及 vet 并非本席运行。** 未发现需要额外复现的新疑点，未重复执行相同 suite，也未占用 PG slot。
- 本报告不声称独立运行 executable、全仓集成或外部托管/交易验收。root 负责第二意见与集成 gate。

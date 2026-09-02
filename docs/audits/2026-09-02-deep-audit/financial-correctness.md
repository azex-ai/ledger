# 审计报告 — financial-correctness（第二轮 · 基线 `fd87cae`）

## 摘要

**上一轮的结论「引擎对、配置的方向反了」在本轮依然成立，而且 C7 只修了三个 preset 中的三个 —— 同一个符号错误还活在另外两个已发货 preset 里。**
`capital_injection` 对 credit-normal 的 `custodial` 记 DR：实测注入 1000 平台资本后 `Custodial = −500`、`margin = −1000`、`Solvent = false`，
即「唯一能改善偿付能力的动作」把偿付能力打成资不抵债。`checkout_settlement_net` 同型：实测 gross 100 / net 97 / fee 3 之后 `margin = −3`，每笔结算累计一次。
两者的 pin（`presets/capital_test.go:73`、`presets/settlement_test.go:71`）**断言的正是产生错误符号的那两个 EntryType**——修方向会让测试变红，测试在保护缺陷。

第二条是 C8 的复活：C8 的修复只加固了 `ReverseJournalFraction` 内部的推导，而 `PostJournal` 对 `ReversalOfUID` **零校验**。
实测：存入 100 → 提交一张净额为 0 的 journal 并标 `reversal_of = J`（四条腿全合法、逐币种借贷相等、余额不动）→ `ReverseJournalFraction(J, 1, 1, "reverse everything remaining")` 返回 **nil**，
只冲销 50，账上**剩 50**，而 `CheckAccountingEquation` 与 `ReconcileAccount` 全部 `balanced=true gap=0`。C8 原始形态一字不差地回来了，走的是另一扇门。

第三条是 I-42「符号权威只剩一处」**不成立**：`balance_trends.sql:42-43` 把 `credit` 当 inflow、`debit` 当 outflow，
**完全不看 `normal_side`**——这是第 18 处独立实现，上一轮的清单和 15d110e 的收敛都没数到它，而且它是唯一**结论就是错的**那一处（不是漂移风险）。
实测同一行 JSON：`balance=395 inflow=105 outflow=500`，用户存入 500 被报成流出 500。

第四条是 M-4 修复的副作用：`fee_expense` 从 `balance_role=''` 改标 `'memo'` 之后，`memo <> ''` 让它进入了 holder 交易流水的聚合范围，
`withdraw_fee` 的两条 holder 腿净额归零被过滤 —— **提现手续费在用户账单里彻底消失**。实测：余额 total=395，账单只有「入 500 / 出 100」，净 400，**5 无从解释**。
而 `holder.sql:20-22` 的注释恰好预言了这个后果，pin `holder_store_test.go:59` 仍然按**改标之前**的 `''` 造 fixture，所以一直是绿的。

金额类型/精度这一块反而是干净的：`float64` 在金额路径上零出现（`postgres/convert.go:120-129` 已从 warn-and-continue 改成 reject），
`EncodeAmount` 的 scale 18 与 `NUMERIC(30,18)` 的存储精度一致（签名精度问题已闭），`core.Allocate` 拒负权重、拒全零权重、残差精确不丢。
上轮的两条 Major（solvency 把 memo 账当负债、`effective_at` 回溯快照不重算）**主体已修**，但后者的修复只装在一个读入口上（见 [Major] 快照自愈两处漏）。

---

## 发现

### [Critical] `capital_injection` / `capital_withdraw` 的 `custodial` 腿符号相反 —— 注入平台资本反而让偿付能力变成资不抵债

- **位置**：`presets/capital.go:35`（`capital_injection` → `custodial` **Debit**）、`presets/capital.go:45`（`capital_withdraw` → `custodial` **Credit**）
  - 权威约定：`presets/templates.go:71`（`custodial` = `NormalSideCredit`）+ `presets/templates.go:167` 一带的 `deposit_confirm`（**CR** custodial 表示托管资产**增加**）
  - 消费点：`postgres/sql/queries/platform_balances.sql:99-127`（`GetSystemSideCustodialBalance`，硬编码 `c.code = 'custodial'`）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（数字来自实跑，`InstallDefaultPresets` + `InstallExtendedPresets`，exponent=2）：

  ```
  deposit_confirm   holder=1005 amount=500      # 正常入金
  capital_injection holder=1005 amount=1000     # 平台注资 1000

  实测：
    user  main_wallet   500
    sys   custodial    -500      ← 期望 1500
    sys   equity       1000
    SOLVENCY custodial=-500 liability=500 margin=-1000 solvent=false
  ```

  正确结果应为 `custodial=1500 liability=500 margin=+1000 solvent=true`。**margin 的误差是 2000**，方向完全反转。
  `capital_withdraw` 对称错误：抽走平台资本会让 `custodial` **增加**，即「把钱拿走」被记成「托管变多」。
  次生后果与上一轮 M-4 同型且更重：`Solvent=false` 是本库唯一的「无背书发行」告警信号，注资一次就把它永久钉在 false 上，
  真实缺口再也无法从虚假缺口里分辨出来。
- **最小复现**：
  ```go
  svc.InstallExtendedPresets(ctx)
  svc.JournalWriter().ExecuteTemplate(ctx, "capital_injection",
      core.TemplateParams{HolderID: h, CurrencyUID: cur.UID,
          Amounts: map[string]decimal.Decimal{"amount": d("1000")}, IdempotencyKey: "k"})
  svc.SolvencyChecker().SolvencyCheck(ctx, cur.UID)   // => Custodial 减少 1000
  ```
- **为什么现有机制没拦住**：
  1. **pin 认证了缺陷**。`presets/capital_test.go:73` 是
     `assert.Equal(t, core.EntryTypeDebit, injJournal.Entries[0].EntryType)  // custodial DR`
     —— 把方向改对，这一行立刻红。更刺眼的是同一文件 `:30` 已经断言了
     `assert.Equal(t, core.NormalSideCredit, custodial.NormalSide)`：**测试知道 custodial 是 credit-normal，却仍然断言注资要 DR 它**。
     这和上一轮 C7 里 `presets/transfer_test.go:31` 的形状一字不差（断言了 normal_side，却不用它校验方向）。
  2. `presets/capital_test.go:72` 的 `assertBalanced`（`presets/transfer_test.go:91-104`）在两腿共用同一个
     `AmountKey "amount"` 时**结构上恒真**，方向语义完全未被断言 —— 上一轮已把这个 helper 点名为恒真断言，本轮它仍是这两个 preset 的唯一保护。
  3. `postgres/solvency_test.go` **只有一个测试**（`:42 TestSolvencyCheck_WithdrawFee_DoesNotManufactureDeficit`），只覆盖 withdrawal 路径。
     `capital_*` / `checkout_settlement_*` / `fx_*` / `fee_charge` 全部没有偿付能力断言。
  4. 成因推断（**PLAUSIBLE**）：`presets/capital.go:10` 的注释 "Equity is credit-normal (A = L + E); increases on the right side"
     说明作者用的是标准会计约定（资产 = debit-normal，DR 资产 = 增加）。而本库把 `custodial` 声明为 **credit-normal**，
     `deposit_confirm` 靠 **CR** 让它增加。照标准约定写就必然写反 —— 与 C7 完全同源。

---

### [Critical] `PostJournal` 对 `ReversalOfUID` 零校验 —— C8「冲销剩余全部静默少退」原型复活，且所有对账检查照常全绿

- **位置**：
  - 缺校验处：`postgres/ledger_store.go:931-945`（`input.ReversalOfUID` 只做「uid 能否解析成一行」，**不校验** entries 是否为被引用 journal 的翻转、不校验被引用 journal 本身是不是冲销、不校验累计额上界）
  - 受害处：`postgres/reversal_fraction_store.go:518-536`（`cumulativeReversedByDimension`：把**任何** `reversal_of = J` 的 journal 的 entries 无条件当作 J 的既有冲销记账）
  - 消费处：`postgres/reversal_fraction_store.go:270-318`（`num == den` 分支用 `originalByDim − alreadyReversed` 算「剩余」）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（实跑输出原样抄录，exponent=2）：

  ```
  after original deposit 100         balance = 100
  net-zero journal tagged reversal_of err = <nil>  balance = 100
  ReverseJournalFraction(1,1) err = <nil>
    reversal journal posted: total_debit=50 total_credit=50
  after "fully reversed"             balance = 50  <= expected 0
  CheckAccountingEquation balanced=true gap=0
  ReconcileAccount balanced=true gap=0
  ```

  中间那张「净额 0」的 journal 是四条完全合法的 entry，逐币种借贷相等，holder 余额一分不动：
  ```
  CR main_wallet(h)  50      DR main_wallet(h)  50
  DR custodial(-h)   50      CR custodial(-h)   50
  ```
  它使 `alreadyReversed[(h,cur,main_wallet,debit)] = 50`（credit 腿被翻回 debit 键）
  与 `alreadyReversed[(-h,cur,custodial,credit)] = 50`，于是「冲销剩余全部」把 100 的剩余算成 50。
  调用方拿到 `nil error`，账上留下 **50**，且**每一层防御都说没问题**：借贷平衡、逐币种平衡、
  `CheckAccountingEquation`、`ReconcileAccount` 全部 `balanced=true gap=0`。
- **最小复现**：见上（`PostJournal` 原始 → `PostJournal{ReversalOfUID: J, 四条净零腿}` → `ReverseJournalFraction(J, 1, 1)` → 读余额）。
- **触发面**：**库模式专有**。HTTP 写路径 `server/handler_journals.go:17-28`（`postJournalRequest`）**不接受** `reversal_of_uid`，
  所以服务端消费方打不到；但 `core.JournalInput.ReversalOfUID`（`core/journal.go:83`）是导出字段，
  库模式（CLAUDE.md 声明的首选消费方式）下 `svc.JournalWriter().PostJournal(...)` 直接可达。
  这不是攻击者专用路径：一个自己实现更正流程、为可审计性给更正 journal 标上 `reversal_of` 的消费方（这正是该字段的用途）就会撞上。
- **为什么现有机制没拦住**：
  - C8 的修复（`b49cb20`）把「按维度聚合」补进了 `reversalEntriesFor` 与 overshoot 检查两处，
    但它假设 **`reversal_of = J` 的 journal 全部由冲销 API 生成**。这个前提在代码里没有任何一处被强制。
    修复关的是推导路径，输入的门没关 —— 契约要问的「修复有没有在别处复制出同形缺陷」，答案是：不是复制，是**同一个缺陷从另一条路走进来**。
  - `ReverseJournal` / `ReverseJournalFraction` 都有 `original.ReversalOf.Valid → ErrConflict` 的护栏
    （`postgres/ledger_store.go:762`、`postgres/reversal_fraction_store.go:152`），`PostJournal` 却没有对称护栏。
    实测 `PostJournal{ReversalOfUID: <一张冲销 journal>}` 同样被接受（余额 0 → 100），冲销链完全没有完整性约束。
  - `postgres/reversal_fraction_test.go` 的全部 fixture 都通过冲销 API 生成 `reversal_of` 行，
    因此「手写 reversal_of」这条输入形态**零覆盖**。
  - `docs/INVARIANTS.md` I-2 承诺的是「累计冲销不超过原额」（上界）。本例累计 = 50 + 50 = 100 = 原额，**上界没被违反**，
    所以 I-2 及其 pin 在这个失效上必然是绿的。

---

### [Major] I-42 的「符号权威只剩一处」不成立：`balance_trends.sql` 是第 18 处独立实现，且方向是错的 —— 存入 500 被报成流出 500

- **位置**：`postgres/sql/queries/balance_trends.sql:42-43`
  ```sql
  COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0::numeric END), 0) AS inflow,
  COALESCE(SUM(CASE WHEN je.entry_type = 'debit'  THEN je.amount ELSE 0::numeric END), 0) AS outflow
  ```
  `daily_flows` CTE（`:35-51`）**完全没有 JOIN `classifications`**，因此这两列不看 `normal_side`。
  外泄面：`core/audit.go:18-19` → `postgres/balance_trends_store.go:121-139` → `server/handler_trends.go:18-19,77-78` → `docs/openapi.yaml:2788-2789`。
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（实跑，`main_wallet` = debit-normal + `BalanceRoleAvailable`，即本库的正典用户钱包）：

  ```
  deposit_confirm 500 → lock_funds 105 → withdraw_fee 5 → withdraw_confirm 100

  TREND 2026-09-02 balance=395 inflow=105 outflow=500
  ```
  同一行 JSON 里：`balance` 是 normal_side-aware 的正确值 395（存入 500 让它上升），
  而 `inflow=105 / outflow=500` 把 500 的**入金**报成**流出**、把 105 的**流出**报成**流入**。
  对每个 debit-normal 的角色分类（`main_wallet`、`locked`）方向全反；只有 credit-normal 的
  `pending` 方向偶然正确。任何按 inflow/outflow 画图或做现金流分析的消费方拿到的是镜像结果。
- **最小复现**：装 deposit preset → `deposit_confirm 500` → `GET /holders/{h}/trends?...` → `outflow == 500`。
- **为什么现有机制没拦住**：
  1. **I-42 / I-43 的「Enforced by」是一张硬编码文件清单**（`docs/INVARIANTS.md:3612-3620`：
     `checkpoints.sql` / `integrity_checkpoint.sql` / `platform_balances.sql` / `holder.sql` / `reconcile.sql`），
     `balance_trends.sql` 从不在其中。pin（`postgres.TestLedgerSignedAmount_AgreesWithCoreSignedAmount` 等，
     `postgres/normal_side_sign_test.go`）只验证**两个 SQL 函数与两个 Go 函数结论一致**，
     **没有任何机械门禁扫描 `postgres/sql/queries/*.sql` 找残留的裸符号表达式** —— 我 grep 确认全仓无此类 gate。
     所以 15d110e 的 commit message 数出「9 处 SQL 表达式」、上一轮审计数出「10 处」，两次都漏掉同一个文件，
     正是因为两次都是**人工枚举**。这就是 `working-agreements.md` §5 的形状：能做成机器检查却写成了文档清单。
  2. **仓内唯一断言 Inflow/Outflow 的测试用的是空账户**：`postgres/balance_trends_store_test.go:150-151`
     （`assert.True(t, p.Inflow.IsZero())` / `p.Outflow.IsZero()`），在一个**没有任何 entry** 的 holder 上断言两者为零。
     方向语义从未被断言过。而同文件 `:17-121` 的 `TestBalanceTrends_GapFill` 已经把触发这个 bug 的 fixture
     完整搭好了（`:31-34` 建 debit-normal + available 的 `wallet_trend`，`:57` 记 DR 500），
     **只差一行 Inflow/Outflow 断言就能红**。
  3. **同仓已有正确写法作为反证**：`postgres/sql/queries/holder.sql:17` 明写
     "an entry increases the holder's balance when its entry_type equals the classification's normal_side. Positive net = \"in\""，
     `:76` 用 `ledger_signed_amount(...)` 实现。也就是说本库两个用户可见表面（交易流水 / 余额趋势）
     对同一笔存款给出**相反**的方向，且只有前者受保护。
- **附带同型残留（同一条 I-42 缺口）**：`postgres/sql/queries/reconcile.sql:42,48`
  （`SettlementNettingViolations`）也是裸 `CASE WHEN entry_type='debit' THEN amount ELSE -amount END`，
  不看 `normal_side`。`HAVING ... != 0` 对零点判定不受符号影响（所以不误报/漏报），
  但 `net_balance` 会以**相反符号**呈现给运维：`settlement` 是 credit-normal，
  `service/reconcile.go:1070-1071` 把它原样写进 Finding 的 `Detail`（`net=%s`），
  运维排查 settlement 泄漏时看到的方向是反的。定级 Minor，并入本条一起改。

---

### [Major] `checkout_settlement_gross` / `_net` 的 `main_wallet` 腿符号相反 —— 结算让平台每笔虚增一个手续费的资不抵债，且 wire 上 `kind=deposit` 却 `direction=out`

- **位置**：`presets/settlement.go:42`（`_gross`：`main_wallet` **Credit** `gross_amount`）、
  `presets/settlement.go:51-52`（`_net`：`custodial` **Debit** `gross_amount` / `main_wallet` **Credit** `net_amount`）
  - 自我矛盾的文档：同文件 `:29-31`（"gross_amount — total received from customer, **debited from custodial**" /
    "net_amount — merchant's net **receipt**, credited to main_wallet"）与 `:13-16`
    （`HolderTxKindDeposit`，"from the receiving holder's perspective this is external funds **landing in** their spendable balance"）
  - 第三种读法：`presets/transfer.go:43` 又写着 "checkout_settlement credits it because **the holder is paying**"
    —— 同一个模板在仓内有两种互相矛盾的解读，而代码与两者都不自洽
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（实跑，exponent=2）：

  ```
  deposit_confirm 1000 → checkout_settlement_net gross=100 net=97 fee=3

    user  main_wallet   903        (−97)
    sys   custodial     900        (−100)
    sys   fees            3        (+3)
    SOLVENCY custodial=900 liability=903 margin=-3 solvent=false
    TX kind=deposit  label="Payment"  dir=out  amount=97
  ```
  两个独立问题：
  1. **偿付能力**：`custodial` 掉 `gross`（100）而 `liability` 只掉 `net`（97），`margin` 每笔结算净减 **fee**，
     单调累积。一个日跑 1 万笔、平均 fee 3 的部署每天虚增 3 万缺口，直接淹没任何真实缺口
     —— 与上一轮 M-4 被判 Major 的理由完全相同（虚假漂移把真实缺口淹掉）。
  2. **用户面自相矛盾**：journal type 标 `HolderTxKindDeposit`（`presets/settlement.go:18`），
     文档说这是「外部资金进入用户可支配余额」，而 wire 上实际是 `dir=out`。
     消费方（`@azex/ledger-react` 的 `kindLabels` 按 `kind` 打标签，`docs/INVARIANTS.md` I-44 就是为这个设计的）
     会把一笔 97 的**流出**渲染成「Payment / 存款类」。
  3. 结构性原因：在 `main_wallet`=debit-normal / `custodial`=credit-normal / `fees`=credit-normal 这套约定下，
     「商户收到 net、平台留 fee、托管增 gross」这三件事**无法用 3 条腿表达**（借方只有 net，贷方要 gross+fee，差 2×fee）。
     要正确表达必须像 `withdraw_fee`（`presets/templates.go:213-219`）那样用 4 条腿两对。
     当前 3 腿模板的任何符号组合都得不到「用户 + 且托管 + 且收入 +」。
- **最小复现**：装 extended presets → `deposit_confirm 1000` → `checkout_settlement_net{gross:100,net:97,fee:3}` → `SolvencyCheck` → `margin == -3`。
- **为什么现有机制没拦住**：
  - `presets/settlement_test.go:71-74` / `:111-114` 断言的就是产生错误符号的那组 EntryType，
    并且 `:74` 的注释 `// merchant` 明确把 CR 的那条腿称为商户腿。修方向 → 测试红。
  - `presets/settlement_test.go:68,108` 的唯一守卫是 `assertBalanced`（恒真，见上）。
  - `postgres/solvency_test.go` 只有 withdrawal 一个用例，checkout 路径零偿付能力断言。
  - `presets/settlement_test.go:89` 只断言 `gross == net + fee`（调用方算术），不断言方向。

---

### [Major] M-4 改标 `fee_expense` 为 `memo` 之后，提现手续费从用户账单里消失 —— 余额少 5 而流水里没有这 5

- **位置**：
  - 改标处：`presets/templates.go:84`（`fee_expense` 从 `balance_role=''` 改为 `core.BalanceRoleMemo`，`dcb9113`）
  - 未同步的过滤器：`postgres/sql/queries/holder.sql:33` 与 `:88`（`balance_role <> ''` —— `'memo' <> ''` 为**真**，memo 因此被纳入聚合）
  - 归零后被丢弃：`postgres/holder_store.go:131`（`if net.IsZero() { continue }`）
  - 该文件自己的预言：`postgres/sql/queries/holder.sql:20-22`
    "Role-less holder-side trackers (fee_expense, ...) are bookkeeping detail: **including them would net a fee charge to zero and hide it from the user**."
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（实跑，标准提现流程，与 `postgres/presets_install_test.go` 同一条路径）：

  ```
  deposit_confirm 500 → lock_funds 105 → withdraw_fee 5 → withdraw_confirm 100

  shipped fee_expense: normal_side=debit balance_role="memo" is_system=false
  breakdown total=395 | statement lines=2 net=400 | unexplained=-5
        withdrawal  "Withdrawal"   out 100
        deposit     "Deposit"      in  500
  ```
  `withdraw_fee` 的两条 holder 腿（`presets/templates.go:216` `fee_expense` **DR** 5 → 带符号 **+5**；
  `:218` `locked` **CR** 5 → 带符号 **−5**）在 `memo` 被纳入聚合后净额恰为 **0**，整行被 `net.IsZero()` 丢掉。
  用户看到的对账结果：余额 395，账单「入 500、出 100」= 净 400，**5 消失且无任何行可解释**。
  改标之前（`balance_role=''`）只有 `locked` 腿参与，净额 −5 → 显示为一笔 5 的流出。
  余额本身仍然正确（`memo` 被 `postgres/ledger_store.go:1356` 的 `BalanceRoleNone` 跳过，
  且 `sums["memo"]` 这个键从不被读取），错的只有**用户可见的流水**。
- **最小复现**：`InstallDefaultPresets` → 上面四步 → `svc.HolderReader().ListHolderTransactions(ctx, h, "", 50)` → 只有 2 条，没有 fee 行。
- **为什么现有机制没拦住**：
  - **pin 用的是改标之前的 fixture**：`postgres/holder_store_test.go:59` 是
    `postgrestest.SeedClassification(t, pool, "fee_expense", "Fee Expense", "debit", false)`
    —— `SeedClassification`（`internal/postgrestest/postgrestest.go:167-176`）的 INSERT **不写 `balance_role`**，
    落到默认 `''`。同文件 `:37` 的字段注释还写着 `feeExp string // role-less holder-side tracker`，
    `:98` 写着 `// (2) Fee charge: wallet -5, role-less fee_expense +5 — the role filter`，
    `:146` 断言 `"fee stays visible despite role-less counter-entry"`。
    **测试测的是产品已经不再使用的配置**，所以它永远绿 —— 契约 §1.4 第 2 条（「测试做的准备工作，真实调用方也会做吗」）的正例。
  - M-4 的落地把 `NOT IN ('', 'memo')` 补进了 `platform_balances.sql:79`（偿付能力侧），
    **没有同步 `holder.sql` 的三处 `<> ''`**。两个语义（「什么算负债」与「什么算用户可见的钱」）
    原本共用 `<> ''` 这一个谓词，M-4 只分叉了其中一侧。
  - 同源附带影响（同一处 `<> ''`，定级 Minor）：
    `postgres/sql/queries/holder.sql:118`（`ListHolderCurrencies`）会因为只有 `fee_expense` 有记账
    就把一个币种列进用户的币种列表；`postgres/sql/queries/reconcile.sql:247`（未标 `holder_kind` 的巡检）
    的扫描面也随之扩大。

---

### [Major] `fees` 收入账被两个已发货模板写入相反符号 —— 收了两笔 30 的手续费，账上读出 0

- **位置**：`presets/fee.go:54`（`fee_charge` → `fees` **Debit**）vs `presets/settlement.go:53`（`checkout_settlement_net` → `fees` **Credit**）
  - `fees` = `NormalSideCredit`（`presets/fee.go:26`）
  - 自述为已知未决：`presets/fee.go:39-46`（"⚠️ The counterpart's direction is a narrower, still-open question... Both cannot be right."）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（实跑，同一币种同一 holder）：

  ```
  deposit_confirm 1000
  fee_charge              amount=30                          # fees DR 30 → -30
  checkout_settlement_net gross=100 net=70 fee=30            # fees CR 30 → +30

  fees revenue account: system-side=map[custodial:1295 fee_revenue:5 fees:0]
  trial balance fees: debit=30 credit=30 net=0
  ```
  平台实际赚了 **60**，`GetPlatformBalances` 与 `TrialBalance` 都报 **0**。
  `presets/fee.go:39-46` 承认了「两者必有一错」，但**没有写出这个数值后果**（两笔同额手续费互相抵消到零），
  也没有任何测试覆盖它，所以维护者读注释只会觉得这是个「呈现口味问题」。
  一个既做直接收费又做 checkout 结算的部署，其聚合手续费收入 = `checkout_fees − direct_fees`，
  这个数字既不是收入也不是任何有意义的量。
- **最小复现**：见上（同一 currency 上各跑一次 `fee_charge` 与 `checkout_settlement_net`，读 `GetPlatformBalances`）。
- **为什么现有机制没拦住**：`presets/fee_test.go` / `presets/settlement_test.go` 都只做 `assertBalanced`（恒真）；
  无任何测试对 `fees` 的累计余额做断言。`presets/fee.go:44-46` 把它挂成「territory A flagged... Resolve them together」
  的待办，但没进 `TODO.md`，也没有门禁。
  ⚠️ 修的时候注意：`fee_charge` 的 holder 腿（`presets/fee.go:53`，CR main_wallet）是 **C7 修对了的**，实测
  `margin=+2.5 solvent=true`，**不要跟着 counterpart 一起翻**。

---

### [Major] `effective_at` 回溯的快照自愈只装在一个读入口上，另一个用户可见入口仍返回过期值；且新维度完全不被自愈

- **位置**：
  - 自愈实现（只此一处）：`postgres/rollup_adapter.go:411-496`（`GetSnapshotBalances` + `snapshotDimensionIsStale`）
  - 漏一：`postgres/sql/queries/balance_trends.sql:25-34` 的 `snapshots` CTE 直接 `FROM balance_snapshots`，
    完全绕过 Go 侧自愈；`postgres/balance_trends_store.go:96-131` 只覆盖**今天**一行（live override）
  - 漏二：`postgres/rollup_adapter.go:442`（`for i, r := range rows`）—— 自愈是**行驱动**的，
    某个维度在快照写入时若没有 entry 就不存在快照行，循环永远看不到它
  - 声明：`docs/INVARIANTS.md:561-580`（I-14 "As-of reads self-heal against retroactive posting"）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（实跑）：

  ```
  # 业务日期 3 天前记一笔 DR main_wallet 100；为「2 天前」生成日快照
  snapshot written for 2026-08-31
    before backdated write   GetSnapshotBalances -> main_wallet=100
    before backdated write   GetBalanceTrends    -> main_wallet=100

  # 现在补记一笔 effective_at = 3 天前的 journal：main_wallet +50，并新开 pending +70
    after backdated write    GetSnapshotBalances -> main_wallet=150          ← 自愈生效
    after backdated write    GetBalanceTrends    -> main_wallet=100          ← 仍然过期
    GROUND TRUTH ListBalancesAt: main_wallet=150
    GROUND TRUTH ListBalancesAt: pending=70                                  ← GetSnapshotBalances 完全没有这一行
  ```
  1. `/holders/{h}/trends`（`server/handler_trends.go:24`）对**除今天以外的每一天**返回未自愈的缓存值。
     I-14 那段文字说的 "only reads through `GetSnapshotBalances` are guaranteed correct" 从字面上是诚实的，
     但它没有点出**第二个读 `balance_snapshots` 的入口存在且是用户可见的 HTTP 端点**。
  2. 回溯记账新开的维度（`pending=70`）在 `GetSnapshotBalances` 的输出里**根本不出现**。
     as-of 头寸少了 70，而不是给出一个错的数 —— 缺行比错值更难发现。
     稀疏模式（`postgres/snapshot_extra_store.go:49-89`，余额未变则不写行）会让「无行」成为常态，
     进一步扩大这个漏洞面。
- **最小复现**：见上（`CreateDailySnapshot(d)` → post `EffectiveAt = d-1` 的 journal 且引入一个新分类 → 对比
  `GetSnapshotBalances(d)` / `GetBalanceTrends(d..d)` / `ListBalancesAt(d+1)`）。
- **为什么现有机制没拦住**：`postgres/snapshot_staleness_test.go` 只经 `GetSnapshotBalances` 验证，
  且 fixture 的维度在快照时都已有 entry。没有任何测试比对 `GetBalanceTrends` 与 `ListBalancesAt`；
  `snapshot_integrity` 对账检查（`postgres/sql/queries/integrity_checkpoint.sql:107-140`）
  按注释只扫**最近一个** `snapshot_date`，历史日期靠人工 `BackfillSnapshots`。

---

### [Major] 装了 FX preset 之后，买入币种的 `SolvencyCheck` 永久为 `false`

- **位置**：`postgres/sql/queries/platform_balances.sql:99-127`（`GetSystemSideCustodialBalance` 硬编码
  `c.code = 'custodial'`，`settlement` / `equity` 不计入）+ `presets/fx.go:35-48`（settlement 吸收 FX 净额）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**（实跑，EEE → FFF，rate 0.9）：

  ```
  deposit 100 EEE → fx_sell 100 EEE → fx_buy 90 FFF

  EEE: user main_wallet=0   custodial=100  settlement=-100
       SOLVENCY custodial=100 liability=0  margin=100 solvent=true
  FFF: user main_wallet=90  custodial=0    settlement=90
       SOLVENCY custodial=0   liability=90 margin=-90 solvent=false
  ```
  这是**账务上完全健康**的 FX 头寸（平台持 100 EEE、欠用户 90 FFF），
  但 FFF 的偿付能力报表读作「资不抵债 90」。`presets/fx.go:46-48` 只说
  "Reconciling settlement balances against external custody figures is the caller's responsibility"，
  **没有说 `SolvencyCheck` 会因此永久返回 false**。后果与上一轮 M-4 同型：
  唯一的「无背书发行」告警信号被结构性钉在 false，真实缺口不可分辨。
- **最小复现**：装 FX bundle → 走一次 `fx_sell`/`fx_buy` 对 → `SolvencyCheck(买入币种)` → `Solvent == false`。
- **为什么现有机制没拦住**：无任何 FX 偿付能力测试（`postgres/solvency_test.go` 只有 withdrawal 一例）。
  `presets/fx_test.go` 与其余 preset 测试同型，只 `assertBalanced`。
- **附带（Minor，同一处）**：`GetSystemSideCustodialBalance` 把「什么是托管资产」硬编码成
  `code = 'custodial'`。一个把托管分类命名成别的 code 的消费方会得到 `Custodial = 0` 与永久资不抵债，
  且没有任何报错提示这个耦合。`core.SolvencyChecker`（`core/platform_balance.go:74-81`）的接口文档
  写的是 "Custodial is the total of system-side \"custodial\" classification balances"，
  但没说这个字符串是**不可配置**的。

---

### [Minor] `journal_entries.id` 的唯一性由 ACL 保证，而库从不校验自己连的是哪个角色

- **位置**：`postgres/sql/migrations/008_journal_entries_id_sequence_only.up.sql`（把 `ledger_app` 的
  table-level INSERT 换成不含 `id` 的 column-level INSERT）+ `postgres/migrate.go:47`（`Migrate` 只收一个 URL）
- **判定**：CONFIRMED（schema/代码事实）；后果 **PLAUSIBLE**
- **失效场景**：I-42 关掉「显式指定 id」这条路的手段是 ACL，而 ACL **只约束 `ledger_app` 这个角色名**。
  库模式下消费方把任意 `*pgxpool.Pool` 交给 `ledger.New`，没有任何一处断言这个连接的 `current_user`
  是 `ledger_app` —— 用 owner / superuser 连（`Migrate` 本身需要 `CREATE ROLE`，所以开发与很多部署会这么连）时
  I-42 的保护完全不在。余额等式 `Σ(id > watermark)` 的单调性因此依赖一条部署纪律，
  而这条纪律只写在 `README.md:75` 与 `docs/RUNBOOK.md`，代码里没有对应的机器检查。
- **为什么现有机制没拦住**：`postgres/roles_test.go` 以 `ledger_app` 连库实测拒绝（这部分是真的），
  但没有任何测试或运行时断言覆盖「消费方用了别的角色」这一情形。
  → 属 D territory 的 ACL 面，我只记录它对余额等式的承重作用。

---

## 上轮修复复核

对落在本 territory 的上轮修复，逐条问三件事：① 修复接到真实路径了吗 ② 拆掉修复哪个 pin 会红 ③ 有没有在别处复制出同形缺陷。

| 上轮 | 修复 | ① 接到真实路径 | ② 拆掉会红的 pin | ③ 同形缺陷 |
|---|---|---|---|---|
| **C5** `Settle` 释放冻结不记账 | `6eecc7c` 改 example | ✅ 语义未改（实测 T4：finalize 后 `main_wallet` 仍是 100，记账仍归调用方），`examples/billing` 进了 `RunInTx` | 无（这是 example 修复，不是代码修复） | ⚠️ 语义本身仍是陷阱：实测 `FinalizeSettlement` 之后 breakdown 直接回到 `avail=100 locked=0`，**settle 掉的 10 完全不可见**。调用方忘了在同一个 tx 里 post journal，这 10 就静默变回可花额度，没有任何信号。属 E territory 的消费方表面，移交 |
| **C6** `'' → available` 把历史余额变成可提现 | `305d6f3` migration 004 upgrade guard | ✅ **实测生效**：我尝试 `SetBalanceRole(fee_expense, '')` 被拒（`SQLSTATE 23514`，"only the '' -> <role> upgrade is allowed"） | migration 004 的 pin | 无 |
| **C7** 三个 preset 借贷方向反了 | `d5c7357` | ✅ 实测 `transfer_out/in` 正确（付款方 100→60、收款方 0→40、`margin=0`）；`fee_charge` holder 腿正确（100→97.5、`margin=+2.5`） | `presets/transfer_test.go` 的方向断言 | ❗ **是。修复只覆盖了被点名的三个模板**：`capital_injection`/`capital_withdraw`（Critical）与 `checkout_settlement_gross`/`_net`（Major）是同一个「照标准会计约定写、与本库 credit-normal 声明相反」的错误，`d5c7357` 没有做全仓符号复核，两者的 pin 还在认证错误方向 |
| **C8** 冲销剩余全部静默少退 | `b49cb20` 按维度聚合 | ✅ **实测生效**：60+40 同维度 → 100 → 50 → **0**，完全守恒；`ReverseJournal` 在有冲销史时正确拒绝 | `postgres/reversal_fraction_test.go` 的余额断言（该 pin 断言的是 holder 余额而非内部形状，写得对） | ❗ **是。**`PostJournal` 对 `ReversalOfUID` 零校验，C8 原型经 `cumulativeReversedByDimension` 完整复活（Critical）。修复关的是推导，输入的门没关 |
| **I-42** `journal_entries.id` 仅由序列产生 | `4eab202` migration 008 | ✅ ACL 层，覆盖 `pg_partition_tree` 全部现有分区；`InsertJournalEntry` 不列 `id` | `postgres.TestJournalEntries_DuplicateIDAcrossPartitions_Rejected` | ⚠️ 保护只绑 `ledger_app`，库不校验自己的连接角色（上面的 Minor） |
| **I-43** 符号权威收敛成一处 | `15d110e` `core.Sign` + migration 009 | ⚠️ **部分**。Go 侧我 grep 全仓复核：`service/rollup.go:239` 与 `service/reconcile.go:1023` 剩下的 `ns == core.NormalSideDebit` 是**分桶/异常判定**不是符号计算（且都在 `core.Delta` 校验之后），**Go 侧确实只剩一处权威**。SQL 侧**不是**：`balance_trends.sql:42-43`（Major）与 `reconcile.sql:42,48`（Minor）是两处残留裸实现 | `core.TestSign_*` / `postgres.TestLedgerSignedAmount_*` —— 拆掉 `core.Sign` 的拒绝分支会红（这些 pin 有效） | ❗ 权威**函数**是唯一的，但「所有调用点都用它」这件事**没有机器门禁**，只有 `docs/INVARIANTS.md:3612-3620` 的硬编码文件清单。第 18 处漏检就是这么发生的 |
| **M-4** 偿付能力把 memo 账当负债 | `dcb9113` + migration 011 `BalanceRoleMemo` | ✅ **实测生效**：标准提现流程 `custodial=395 liability=395 margin=0 solvent=true`（上一轮实测是 `margin=-5 solvent=false`）；`ClassificationInput.Validate` 拒绝新建非系统分类留空 `balance_role` | `postgres/solvency_test.go:42` | ❗ **是，反向的。**`fee_expense` 改标 `'memo'` 之后 `holder.sql` 三处 `balance_role <> ''` 没有同步，提现手续费从用户账单消失（Major）。M-4 只分叉了「负债」这一侧的谓词 |
| **effective_at 回溯快照** | `docs/INVARIANTS.md:561-580` 读边界自愈 | ⚠️ **只接了一个读入口**。`GetBalanceTrends` 绕过（Major） | `postgres/snapshot_staleness_test.go` | ❗ 自愈是行驱动的，新维度不被覆盖（同一条 Major） |
| **`convert.go` float64 warn-and-continue** | 改成 reject | ✅ `postgres/convert.go:120-129` 现在返回 `ErrPrecisionExceeded` | — | 无 |
| **精度报错把 uid 冒充 code** | `postgres/precision.go:27-45` 解析真 code | ✅ | — | 无 |
| **`presets/fx.go` 文档与代码符号相反** | 已改 | ✅ **实测一致**：doc 说 `settlement(CCY-A) = -qtyA / (CCY-B) = +qtyB`，实测 `-100 / +90` | — | 无 |
| **签名精度** | `059c22a` / `884adcc` `effective_at` 截到微秒 | ✅ 那条是**时间**精度。**金额**精度我另核：`core.EncodeAmount`（`core/auth.go:74-95`）定标到 scale 18，与 `NUMERIC(30,18)` 的存储 scale 一致，且对表示形式不敏感（归一化尾零），>18 位小数拒绝而非舍入 —— 签名精度与存储精度**一致** | `core/auth_test.go` 的显式纳秒时间戳用例 | 无 |

---

## 必答项的直接回答

### 有没有任何路径让金额碰到 `float64`？

**金额路径上零出现。** grep 全仓非测试 Go（`float64|float32|ParseFloat|%f|Float64()|InexactFloat64`）后逐个打开核对：

| 位置 | 判定 |
|---|---|
| `postgres/convert.go:120-129` | **fail-closed**：`anyToDecimal` 的 float64 分支返回 `ErrPrecisionExceeded`（上轮的 warn-and-continue 已改） |
| `observability/prometheus.go:37-42` `decimalToFloat` | 仅 Prometheus gauge（协议本身是 float64），不回流账本；不算金额路径 |
| `observability/prometheus.go:211,335,390-395,426,442` | 全是**计数/桶边界/延迟**，非金额 |
| `chains/evm/sweeper.go` `big.Int Div` | gas bump 分母，非账本金额 |

JSON 侧：`core` 的金额字段全是 `decimal.Decimal`，`server/` 的 wire struct 全部 `string`（`handler_trends.go:17-19` 等）。
日志格式化侧无 `%f` 于金额。`decimalToNumeric`/`numericToDecimal`（`postgres/convert.go:23-49`）走 `big.Int` 系数+指数，不经 float。

### `NUMERIC(30,18)` 与 `currencies.exponent`：声明精度 vs 存储精度

- `exponent` 被双重锁在 `[0,18]`：`core/interfaces.go` 的 `CurrencyInput.Validate` + `001_baseline.up.sql:161` 的 `CHECK`。
  `postgres/sql/queries/currencies.sql` **没有任何 UPDATE exponent 的查询**（只有 Create/Get/Batch/List），
  `003_config_table_guards.up.sql:41` 的注释也把 `currencies.exponent` 列为「immutable」。
  所以声明精度**永不超过**存储 scale，不存在「声明 25 位、Postgres 静默舍到 18 位」。
- **超精度是拒绝，不是舍入**：`postgres/precision.go:13-25` 明写 "never rounds or truncates"，返回 `core.ErrPrecisionExceeded`。
  调用点 8 处（`ledger_store.go:929` 逐 entry / `reserver_store.go:101,481,594` / `booking_store.go:124,255`），
  与上轮核过的入口集合一致，未见新增未覆盖入口。
- **签名 digest 的精度与存储一致**：`core/auth.go:44-48` 的 `amountScale = 18`，注释就明确以 `NUMERIC(30,18)` 为上限；
  `EncodeAmount`（`:74-95`）把任意 Decimal 定标到 18 位后编成 16 字节二补码，
  对 `100.5` 与 `100.500000000000000000` 得到同一字节串 —— 所以「签名时的表示」与「从库里读回后的表示」digest 相同。
  >18 位小数直接 `ErrInvalidInput`。**这一项没有发现问题。**
  （`:51-57` 的注释 "10^30 scaled by 10^18 needs ~100 bits" 措辞不准 —— `NUMERIC(30,18)` 的整数部分只有 12 位，
  最大定标值约 10^30，需要约 100 bit，结论对、推导写反了。纯注释瑕疵，不单列。）

### I-42 符号权威：真的只剩一处了吗？

**Go 侧是，SQL 侧不是。**

- **Go（grep `NormalSideDebit|NormalSideCredit` 全仓非测试，逐个打开）**：只有两处 `ns == core.NormalSideDebit`
  不在 `core.Sign` 家族里 —— `service/rollup.go:239`（判断「负余额对 debit-normal 账户是否异常」，
  这是另一个命题，不是符号计算）与 `service/reconcile.go:1023`（把已由 `core.Delta` 算好的 net 分进
  debit-normal / credit-normal 两个桶；`Delta` 已在 `:1009` 拒绝非法值，所以 `else` 分支安全）。
  两者都不是符号实现的重复。**Go 侧收敛成立。**
- **SQL**：`postgres/sql/queries/*.sql` 里 `normal_side` 相关的符号表达式全部走 `ledger_signed_amount` /
  `ledger_signed_delta`（`checkpoints.sql:57,167`、`holder.sql:76`、`integrity_checkpoint.sql:24,123`、
  `platform_balances.sql:44,90,119`、`reconcile.sql:66,116`）。**但另有两处裸实现绕过了整个机制**：
  - `balance_trends.sql:42-43` —— **第 18 处，而且结论是错的**（见 Major）。
  - `reconcile.sql:42,48` —— 呈现符号相反（见同一条 Major 的附带）。
  另有若干裸 `CASE WHEN entry_type='debit' ... ELSE -amount`（`integrity_balance.sql:24,36,39`、
  `journals.sql:143`）是**逐 journal 借贷平衡检查**，`debit − credit` 与 `normal_side` 无关，不属此类，已核对确认。
- **未知 `normal_side` 的处置是否唯一且 fail-closed**：**是。**`core.Sign`（`core/account_policy.go:124-134`）
  对非法 `normalSide` / `entryType` 一律返回 `ErrInvalidInput`，三个 wrapper 全部透传；
  SQL 侧 `ledger_reject_unknown_normal_side` `RAISE EXCEPTION`（`22023`）。
  上轮那个会**静默少算钱**的 `ELSE 0` 形态已经消失。唯一保留的 NULL 分支
  （`WHEN p_entry_type IS NULL THEN NULL`）是 LEFT JOIN 占位行，有专门 pin，语义正确。
  **前提是走了这套函数** —— `balance_trends.sql` 没走，所以它对未知值的处置是「静默按 debit 当流出」，
  即那个已被消灭的形态在这一处仍然活着。

### M-4 修复后的偿付能力等式对每个 preset 都成立吗？（逐 preset 实跑）

| preset | 实测 | 成立？ |
|---|---|---|
| withdrawal（deposit 500 / lock 105 / withdraw_fee 5 / withdraw_confirm 100） | `custodial=395 liability=395 margin=0 solvent=true` | ✅（上轮 Major 已修） |
| `fee_charge` 2.50（deposit 100） | `custodial=100 liability=97.5 margin=+2.5 solvent=true` | ✅ |
| `transfer_out`/`transfer_in` 40 | `custodial=100 liability=100 margin=0 solvent=true`；`settlement` 分类级净额 0 | ✅ |
| `checkout_settlement_net` gross100/net97/fee3 | `custodial=900 liability=903 **margin=-3 solvent=false**` | ❌ Major |
| `capital_injection` 1000 | `custodial=-500 liability=500 **margin=-1000 solvent=false**` | ❌ Critical |
| `fx_sell`/`fx_buy` 100→90 | 卖出币种 `margin=+100`；**买入币种 `margin=-90 solvent=false`** | ❌ Major |
| `spread` | 只有一个分类、无模板（`presets/spread.go:21-27`），不参与 | n/a |

**走一遍具体数字（`checkout_settlement_net`，按要求举例）**：
```
初始： main_wallet(user)=1000  custodial(sys)=1000  fees(sys)=0
       Liability = 1000（main_wallet 是 available，计入）
       Custodial = 1000（GetSystemSideCustodialBalance 只取 code='custodial'）
       margin = 0

checkout_settlement_net gross=100 net=97 fee=3：
  custodial   DR 100  → credit-normal，DR 减少 → 1000 − 100 = 900
  main_wallet CR  97  → debit-normal，CR 减少 → 1000 −  97 = 903
  fees        CR   3  → credit-normal，CR 增加 →    0 +   3 =   3
  借方 100 = 贷方 97+3 ✅ 逐币种平衡，journal 合法落库

结果： Liability = 903    Custodial = 900    margin = 900 − 903 = **−3**    solvent = false
```
缺口恰等于 `fee_amount`，每笔结算累加一次。等式在 `fee_amount = 0` 时（即只用 `_gross`）退化为
`custodial −gross / main_wallet −gross`，margin 不变但**双方同向减少**，
即商户「收款」让商户余额下降 —— 方向问题依旧。

### 余额 = `checkpoint.balance + SUM(entries > last_entry_id)` 在各条件下

| 条件 | 结论 |
|---|---|
| **分区表** | ✅ 成立。`journal_entries` 按 `created_at` 月分区，`id` 是跨分区共享的 `BIGSERIAL`，`idx_entries_account_id` 建在父表；`id > watermark` 跨分区语义正确 |
| **`journal_entries.id` 改为仅由序列产生（`4eab202`）** | ✅ **加强**了这条等式。上轮那个「同一 id 出现在两个分区 → 该行永久不进余额但进全局合计」的分叉面在 ACL 层关掉了。⚠️ 保护绑 `ledger_app` 角色，库不校验自己的连接角色（Minor） |
| **`effective_at` 回溯** | ✅ 实时余额成立（等式按 `id` 推进，与 `effective_at` 无关；实测回溯记账后 `GetBalance` 立即反映）。⚠️ as-of 派生视图仍有两处漏（Major） |
| **reversal（含部分冲销）** | ✅ 等式成立（冲销只 append 新 entry、取新 id、走同一 choke point）。上界由原 journal 的 `FOR UPDATE` + 逐维度 overshoot 检查保证，实测 C8 场景守恒。❌ **下界不保证** —— 经 `PostJournal(ReversalOfUID)` 可被破坏（Critical） |
| **并发 rollup** | ✅ 成立（未复跑，采信上轮的推导 + `postgres/` rollup 测试全绿）。理由仍是「寄生」的：rollup 自己不取锁，靠写入方在分配 id 前取 `pg_advisory_xact_lock('balance:holder:currency')` 使同一 pair 内 id 顺序 == 提交顺序。这个前提仍**只有散文守卫**，无机器门禁（上轮 Minor，未修） |

### 部分冲销累计守恒（C8 修复）与「同 journal 内同维度两条 entry 一正一负」

- **C8 修复本身正确**，实跑守恒（60+40 同维度：100 → 50 → 0）。
- **「一正一负」这个边界不构成问题**：`entryDimKey`（`postgres/reversal_fraction_store.go`）
  **包含 `entryType`**，所以同 (holder, currency, classification) 上的一条 debit 与一条 credit 是**两个不同维度**，
  各自独立结算。我手推了 `DR mw 100 / CR mw 30 / CR settlement 70` 这张 journal 的
  `1/2` + `1/1` 两步：三个维度分别 50+50 / 15+15 / 35+35，累计恰为 100/30/70，且每步逐币种平衡。守恒成立。
- **真正的破口不在这个边界，在输入**（Critical，见上）。

### `core.Allocate` 分摊与 FX 净额吸收

实跑 `core.Allocate`：

| 输入 | 输出 | 判定 |
|---|---|---|
| `Allocate(0.03, [1,1,1], 2)` | `[0.01 0.01 0.01]` sum=0.03 | ✅ 残差精确分配，不丢不造 |
| `Allocate(10, [0,0], 2)` | `ErrInvalidInput` "weights must not all be zero" | ✅ fail-closed |
| `Allocate(10, [-5,15], 2)` | `ErrInvalidInput` "weight[0] must not be negative" | ✅ **拒负权重** |
| `Allocate(-10, [5,5], 2)` | `[-5 -5]` sum=-10 | ✅ 负 total 正确（不会「为负」意外，符号跟随 total） |

**残差归谁**：`core/money.go` 的最大余数法 —— 截断余数最大的份额各得一个最小单位，同余数按输入序，确定可复现。
不落固定某条腿、不落 settlement。全程 `big.Rat`，无 float、无 `decimal.Div`。
**不会破坏借贷平衡**：冲销路径按 `(currency, entry_type)` 分组，同一 `scaleByFraction` + `Allocate`
作用在两个相等的组总额上必得相等结果。

**FX 净额吸收**：文档与代码符号现已一致（实测 `settlement(EEE)=-100 / settlement(FFF)=+90`，
与 `presets/fx.go:37-38` 相符），上轮那条 Minor 已闭。但「settlement 吸收净额」这个说法仍然
**恒真但空洞**：两条腿各是单币种两行共用同一个 `AmountKey`，腿内不存在舍入；
`qtyB` 是否等于 `qtyA × rate` 的正确舍入，账本**不校验也无法校验**（`presets/fx.go:29-33` 声称
per-currency balance validation 能抓 rate-quote bug —— 这个声称不成立，逐币种平衡对任意 `qtyB` 都成立）。
上轮已记，本轮复核仍然如此，不重复记为发现。新增的是它对偿付能力的后果（Major）。

### `balance_role` 三桶 `total = available + locked + pending` 守恒吗？（实跑每条路径）

```
T4 after deposit 100      avail=100  locked=0   pending=0  total=100  a+l+p=100  held=0
T4 after reserve 30       avail=70   locked=30  pending=0  total=100  a+l+p=100  held=30
T4 after settlePartial 10 avail=80   locked=20  pending=0  total=100  a+l+p=100  held=20
T4 after finalize         avail=100  locked=0   pending=0  total=100  a+l+p=100  held=0
```
**Reserve / SettlePartial / FinalizeSettlement 每条路径都守恒**，`total == a+l+p` 恒成立，
`held` 与 locked 的搬移一致（`postgres/ledger_store.go:1320-1322`：`available = Σavailable − held`、
`locked = Σlocked + held`）。`memo` 角色**不进任何桶**（`:1356` 跳过 `BalanceRoleNone`；
`sums["memo"]` 这个键被写入但从不被读，是死键但无害）。
pending 桶经 `pending` 分类（credit-normal + `BalanceRolePending`）参与，未见不守恒路径。

⚠️ 但注意最后一行：`finalize` 之后 `avail` 回到 **100**，`raw main_wallet balance = 100` ——
**settle 掉的 10 在账本上不存在**（记账归调用方，C5 的既定语义）。三桶守恒，只是守恒在一个「什么都没发生」的状态上。

---

## ⚠️ 流程事故（已自行消失，但记录在案）

**审计期间（约 10:30 SGT）工作树是脏的，`core` 包在基线上是红的 —— 不是我改的**（我全程只写了本报告一个文件）：

```
$ git status --porcelain
 M core/onchain.go

$ git diff core/onchain.go
 func (c TokenConfig) AutoCreditCeilingConfigured() bool {
-	return c.AutoCreditCeiling.IsPositive() || c.AutoCreditCeiling.Equal(UnboundedAutoCredit)
+	return true
 }

$ go test ./core/... -count=1
--- FAIL: TestTokenConfig_AutoCreditCeilingConfigured
    zero_value_(never_set):                  expected false, actual true
    arbitrary_negative_is_not_the_sentinel:  expected false, actual true
FAIL	github.com/azex-ai/ledger/core
```

某个 territory（`AutoCreditCeiling` 属 **G · onchain-money-path**）在做本轮契约 §2「拆掉再看」时把实现桩成
`return true`。**我写报告期间它被恢复了**，收尾复核时 `git diff core/onchain.go` 已空、
`go test ./core/ -run TestTokenConfig_AutoCreditCeilingConfigured` → `ok`。所以此刻无需处理。

仍然记录，因为它暴露一个流程缺口：**契约 §2 允许「拆掉再看」，但没有要求把窗口做成互斥的**。
在那个窗口里跑 `go test ./core/...` 的任何 agent 会看到一个与自己无关的红 ——
或者更糟，把它当成自己的发现上报。建议下轮契约加一条：拆实现必须在 worktree 里做，
或至少在 `bus checkpoint` 里声明「我现在持有 <file> 的临时修改」。

附带一个正面信息：这条恰好证明那条 pin 是有效的（拆掉实现确实会红），可作为 G 的证伪证据。

我这一侧的 territory-相关测试全程绿：
`go test ./postgres/ -run 'Reversal|Reverse|Rollup|Solvency|SignedAmount|SignedDelta|Snapshot|Checkpoint|Holder|Trend'` → `ok 10.679s`，
`go test ./presets/...` → `ok`。**本报告的每一条发现都是「测试全绿、行为错误」。**

---

## 移交

- **[→ E consumer-surface]** `FinalizeSettlement` / `Settle` 之后 breakdown 直接回到 `avail=100 locked=0`，
  settle 掉的金额**完全不可见**；调用方漏了在同一 tx 里 post journal，这笔钱就静默变回可花额度，无任何信号。
  C5 的处置是「修 example」，但 API 形状本身仍是陷阱（`core/interfaces.go:155-163` 的文档没说「本方法不记账」）。
- **[→ E consumer-surface]** `core.JournalInput.ReversalOfUID`（`core/journal.go:83`）是导出字段、有文档、
  无任何校验，且误用后果是静默丢钱（我的 Critical）。这是「一个只读文档和签名的人会怎么用错」的典型。
- **[→ F test-credibility]** `presets/*_test.go` 的 `assertBalanced`（`presets/transfer_test.go:91-104`）
  在上一轮已被点名恒真，本轮仍是 `capital_*` / `checkout_settlement_*` / `fx_*` / `spread` 的唯一保护。
  另有两处 pin **主动认证缺陷**（`presets/capital_test.go:73-74`、`presets/settlement_test.go:71-74,111-114`），
  修方向会让它们红 —— 与上一轮反转的三处同型，建议同样反转而非删除。
- **[→ F test-credibility]** `postgres/holder_store_test.go:37,59,98,146` 整套 fixture 用的是
  **M-4 之前**的 `fee_expense`（`balance_role=''`），与已发货 preset 的 `'memo'` 不一致；
  `:146` 的断言 `"fee stays visible despite role-less counter-entry"` 在产品配置下为假。
  建议查一遍还有多少 `SeedClassification`（不带 role）在测产品早已不用的配置。
- **[→ F test-credibility]** `postgres/balance_trends_store_test.go:150-151` 只在**空账户**上断言 Inflow/Outflow；
  同文件 `TestBalanceTrends_GapFill` 已有能触发 Major 的完整 fixture，只差一行断言。
- **[→ F test-credibility]** `postgres/solvency_test.go` 只有 1 个测试、只覆盖 withdrawal。
  建议每个已发货 preset 一条偿付能力断言 —— 本轮两条方向性缺陷都是这个空白直接导致的。
- **[→ H structure-and-contract]** `docs/INVARIANTS.md:3612-3620`（I-43 "Enforced by"）是一张**硬编码文件清单**，
  不是机器检查。建议加一条 gate：扫 `postgres/sql/queries/*.sql`，任何同时提到 `entry_type` 与
  `amount` 的 `CASE` 表达式若不在白名单（逐 journal 平衡检查）内就 fail。这条能一次性拦住第 18 处及未来的第 19 处。
- **[→ H structure-and-contract]** `docs/openapi.yaml:2788-2789` 的 `inflow`/`outflow` 没有任何 description，
  语义在 wire 上完全未定义（`core/audit.go:14` 的 "net credit/debit" 是机械描述，不是方向语义）。
- **[→ H structure-and-contract]** `presets/settlement.go:29-31` / `:13-16` 与 `presets/transfer.go:43`
  对同一个 `checkout_settlement` 给出两种互相矛盾的语义解读，代码与两者都不自洽。
  这是一个「契约没定」而非「实现有 bug」的问题，需要先拍板语义。
- **[→ D threat-model]** I-42 的 `id` 保护只绑 `ledger_app` 角色名，库从不断言自己连的是谁
  （`postgres/migrate.go:47` 只收 URL，`ledger.New` 只收 pool）。余额等式的单调性承重在这条部署纪律上。
- **[→ D threat-model]** 正面情报：C2 的配置表守卫（`003_config_table_guards`）**实测比预期更严** ——
  我尝试 `UPDATE entry_template_lines SET classification_id = ...`（一个合法的运维改配置动作）
  被拒（`SQLSTATE 23514`，"UPDATE on entry_template_lines may only change nothing"）。
  这意味着**运维无法在不改代码的情况下调整任何模板腿**（连修上面那两条方向缺陷的运行时 workaround 都做不了）。
  安全上是对的，可用性上值得你确认一下这是不是有意为之。
- **[→ J web-ledger-react]** `@azex/ledger-react` 若渲染 trends 的 inflow/outflow，方向是反的；
  若渲染 holder transactions，提现手续费缺行且 `checkout_settlement` 会出现 `kind=deposit` + `direction=out` 的组合。

---

## 我没能验证的

- **并发 rollup 与余额等式**没有复跑并发场景，采信上一轮的推导 + `postgres/` rollup 测试全绿。
  两个 worker 交错、`UpsertBalanceCheckpoint` 的 `last_entry_id` 单调守卫、
  `SumEntriesSince` 的 REPEATABLE READ 我只读了代码与注释，没有构造真实竞态。
- **`capital_withdraw` 的符号错误只做了代码推导，没有实跑**（`capital_injection` 是实跑的）。
  它与 `capital_injection` 严格对称（`presets/capital.go:44-45` 两腿互换），
  所以「抽走资本会让 custodial 增加」这个结论我判 CONFIRMED-by-symmetry，但没有实测数字。
- **`withdraw_fee` 手续费消失的「改标之前对照组」没能实跑**：我尝试用一个 `balance_role=''` 的克隆分类
  重指模板腿，被 C2 的配置表守卫拒绝（见上面的移交）。该结论因此建立在读代码上：
  `holder.sql:33,88` 的 `<> ''` + `'memo' <> ''` 为真 + 两条腿带符号 `+5/−5` 净额 0 +
  `holder_store.go:131` 的 `net.IsZero()` 过滤。四步都是确定性事实，但**归因**（「是 M-4 改标导致的」）
  未经反向实测。
- **`SettlementNettingViolations` 的符号后果**只读了 SQL 与 `service/reconcile.go:1070-1071` 的
  Finding 拼装，没有实跑一个非零 settlement 净额去看运维实际看到的字符串。
- **稀疏快照模式（`UpsertSnapshotSparse`）下的 as-of 读语义**没有实跑。
  我的实测用的是 dense 模式（`s.sparse == nil`）。稀疏模式下「无快照行」是常态，
  我推断这会放大「新维度不被自愈」的漏洞面，但没有验证稀疏模式的读路径是否另有 merger 补齐
  （`core.LiveBalanceMerger` 存在，我没有追它的调用点）。
- **多币种 journal 在冲销时各币种用各自 exponent** 的交叉组合（同币种多分类 + 跨币种同时）
  仍未穷举，与上一轮同。
- **`checkout_settlement_gross`（2 腿版）没有实跑**，只跑了 `_net`。方向结论从 `presets/settlement.go:41-42`
  的两行直接读出（`custodial` DR / `main_wallet` CR，两者都是「减少」），推导确定但无数字。
- **`server/` HTTP 层的偿付能力 / trends 端点没有经 HTTP 实测**，全部走库模式的 Go API。
  我核了 handler 的字段映射（`server/handler_trends.go:71-80`）是直传，但没有起 server 打请求。
- **`docs/COOKBOOK.md` 的配方是否引导消费方走到上述任一缺陷**没有逐条读。

---

## 复现环境（供 lead 复核）

我的所有实测在**仓外**进行，未修改仓库任何文件（除本报告）：

- 独立容器：`docker run -d --name auditA-pg -e POSTGRES_PASSWORD=test -e POSTGRES_USER=test -p 5441:5432 postgres:17`
  （与 territory D 的 `auditD-pg:5439` 及共享 `dev-postgres:5432` 均隔离）
- 独立 module：`/tmp/auditA/`（自带 `go.mod`，`replace github.com/azex-ai/ledger => /Users/aaron/projects/ledger`，`GOWORK=off`）
- 每个场景前 `DROP SCHEMA public CASCADE; CREATE SCHEMA public;` 再 `ledgerpg.Migrate(dsn)`
- 全部走 facade（`ledger.New` → `InstallDefaultPresets` / `InstallExtendedPresets` → `JournalWriter()` / `Reserver()` / `SolvencyChecker()` / `HolderReader()` / `BalanceTrends()`），即**消费方真实路径**，没有任何测试专用接线
- 容器可以直接删：`docker rm -f auditA-pg`

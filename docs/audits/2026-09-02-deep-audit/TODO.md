# 第二轮审计 TODO — 2026-09-02（全量修复）
> 源：`docs/audits/2026-09-02-deep-audit/index/*.jsonl`（十份报告的机器可读索引，227 条）。
> 契约：`docs/plans/2026-09-02-remediation-contracts.md`。**按任务归属排列，不按报告排列**——一条落在哪个任务由它要改的文件决定。
> 每条带 `file:line`（基线 `fd87cae`）、修法、必须的 pin。完整论证在各 territory 报告原文，动手前回读。
> 标 **[并入 X]** 的条目是跨报告重复命中，修 X 时一并关闭，不单独处理。

原始 227 条（Critical 6 / Major 91 / Minor 130）；合并 20 条跨报告重复后 **207 条独立条目**（Critical 6 / Major 86 / Minor 115）。

## 任务总览

| 任务 | 波次 | 条目数（独立） | Critical | Major | Minor |
|---|---|---|---|---|---|
| W1-gate | 1 | 4 | 1 | 1 | 2 |
| W1-sign | 1 | 13 | 1 | 7 | 5 |
| W1-ledgerstore | 1 | 8 | 1 | 3 | 4 |
| W1-onchain | 1 | 18 | 2 | 9 | 7 |
| W1-templates | 1 | 2 | 1 | 1 | 0 |
| W1-facade | 1 | 14 | 0 | 9 | 5 |
| D-tamper | 2 | 17 | 0 | 10 | 7 |
| D-threat | 2 | 19 | 0 | 9 | 10 |
| D-contract | 2 | 23 | 0 | 10 | 13 |
| D-ops | 2 | 22 | 0 | 10 | 12 |
| D-lock | 2 | 6 | 0 | 1 | 5 |
| D-surface | 2 | 18 | 0 | 5 | 13 |
| D-tests | 2 | 20 | 0 | 4 | 16 |
| D-web | 2 | 22 | 0 | 7 | 15 |
| W3-lead | 3 | 1 | 0 | 0 | 1 |

---

## W1-gate（Wave 1）

### C-C1 [Critical] RequireVerifiedBalance 闸只验签名，Reserve 实际放款金额仍取自攻击者可写的 balance_checkpoints，闸内算出的可信重算值被丢弃
- **位置**：`postgres/reserver_store.go:123-133` · `postgres/reserver_store.go:201-209` · `postgres/reserver_store.go:274-295` · `postgres/verified_balance_store.go:164-167` · `postgres/sql/queries/checkpoints.sql:44-71` · `postgres/sql/migrations/001_baseline.up.sql:1498-1502` · `core/reserve.go:60-73` · `docs/INVARIANTS.md:2148-2218` · `examples/tamper-evident/main.go:194-235`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **同源提示**：financial-correctness 可能独立命中「余额唯一来源是 checkpoint+delta」；threat-model 会命中 balance_checkpoints 按设计无 append-only 守卫
- **修法**：让 requireVerifiedAvailableBalance 不再丢弃 VerifiedBalance 的返回值：改签名为返回 (map[classUID]decimal.Decimal, error)，把每个 balance_role=available 分类的 entries-only 重算值收集起来求和；Reserve 在 RequireVerifiedBalance=true 时把这个和作为新参数 verifiedAvailableBase *decimal.Decimal 传给 reserveWithQueries，后者在该参数非 nil 时用它替代 sumBalancesByRoleWithQueries 的 checkpoint+delta 结果计算 availableBase（activeReserved 的扣减保持不变）；nil 时行为完全不变。闸在事务外跑而 reserveWithQueries 在 balance advisory lock 内，两者之间新落的 entry 只会让重算值偏保守（更少可用），把这条论证写进注释而不是重新查一遍。同步给 examples/tamper-evident 增加一步：篡改 balance_checkpoints 后演示带闸的 Reserve 被拒。
- **规则/文档要改**：I-32 必须改：Pinned by 段里 TestReserve_RequireVerifiedBalance_AllowsWhenEverythingSigned 的注解「it is an authorization check, not a stricter amount check」要删；Rule 段补一句「RequireVerifiedBalance=true 时 Reserve 的可用额基数取自 entries-only 重算，不读 balance_checkpoints」。core/reserve.go:63-65 的「on top of, not instead of, the normal available-balance-covers-Amount check」措辞同步改。design doc 2026-08-21 §0 决策表「余额真相」一行由未兑现变为已兑现。
- **pin**：新增 postgres.TestReserve_RequireVerifiedBalance_RejectsInflatedCheckpoint：全部 journal 合法签名，直接 SQL 把 balance_checkpoints.balance 加 1000000，断言 Reserve(RequireVerifiedBalance:true) 返回 ErrInsufficientBalance，同时同条件下不带该 flag 的 Reserve 成功（对照组，证明篡改确实生效且旧行为会放行）。

### B-M2 [Major] Service.VerifiedBalanceReader() 在 RunInTx clone 上无 tx 守卫 → 在调用方已开事务里发起（设计上允许远程的）AuthVerifier.Verify，且每笔贡献 journal 一次
- **位置**：`postgres/verified_balance_store.go:82` · `postgres/verified_balance_store.go:159` · `ledger.go:559` · `ledger.go:606` · `postgres/reserver_store.go:123` · `postgres/ledger_store.go:315` · `core/interfaces.go:647`
- **主题**：sibling · **runtime**：go · **effort**：S
- **同源提示**：C(tamper-evident)：提现闸的正确用法边界与防篡改覆盖面清单——同一处代码，C 从签名/覆盖率角度看，我从事务边界角度看
- **修法**：在 VerifiedBalanceStore.VerifiedBalance 开头加与 Reserve/Authorize 同款的 fail-closed 守卫：if s.pool == nil { return decimal.Zero, fmt.Errorf("postgres: verified balance: called on a transaction-bound store; the gate may call a remote AuthVerifier and financial.md forbids that inside an open transaction -- call VerifiedBalance on the top-level Service BEFORE RunInTx opens, then compose the withdrawal journal inside the callback: %w", core.ErrInvalidInput) }。同时在 ledger.go:559 的 VerifiedBalanceReader() accessor doc 里写明这条限制，并给出正确编排范式（先在池上过闸、再 RunInTx 里 PostAuthorized，与 service/onchain.go:957 postDepositConfirmedJournal 同形）。不要试图把 verifier 重绑到 tx——远程调用本身就不该在事务里发生。
- **规则/文档要改**：改 ledger.go RunInTx 的 doc comment（当前 ledger.go:459-482 逐条列了在 clone 上被拒的方法，VerifiedBalanceReader 必须加进这张列表）；docs/frontend.md / README 若示范过提现闸编排需同步。
- **pin**：新增测试：注入一个会 panic 或计数的 AuthVerifier，在 svc.RunInTx 回调里调 tx.VerifiedBalanceReader().VerifiedBalance(...)，断言返回 core.ErrInvalidInput 且 verifier 的调用计数为 0。删掉守卫该测试必须红。

### A-N6 [Minor] Settle / FinalizeSettlement 之后 breakdown 直接回到 avail=100 locked=0 —— settle 掉的金额完全不可见，调用方漏了在同一 tx 里 post journal 就静默变回可花额度
- **位置**：`core/interfaces.go:155` · `core/interfaces.go:187` · `postgres/reserver_store.go:383` · `docs/COOKBOOK.md:1`
- **主题**：silent-default · **runtime**：docs · **effort**：S
- **同源提示**：属 E territory 的消费方表面，E 很可能独立命中；我从上轮 C5 复核里带出来，避免它在 territory 缝里掉掉
- **修法**：C5 的处置是『修 example』，API 形状本身未动。最小修法是文档：core/interfaces.go:155-163（Settle）与 :180-187（FinalizeSettlement）的接口注释必须显式写『本方法不产生任何记账；被 settle 的金额只有在调用方于同一事务内 PostJournal 之后才离开余额』。更彻底的修法（需拍板）：给 SettleInput/FinalizeSettlementInput 加一个必填的 JournalInput 或 AuthorizedJournal，让『settle 而不记账』在类型上无法表达。实测证据：finalize 后 raw main_wallet balance 仍是 100、breakdown avail 回到 100。
- **规则/文档要改**：core/interfaces.go 的 Reserver 接口文档（上述两处）；docs/COOKBOOK.md 的 reserve→settle 配方需检查是否每处都示范了同事务记账
- **pin**：新增用例：Reserve 30 → Settle 10（不 post journal）→ 断言（选定方案下）要么被拒，要么 breakdown 明确暴露『10 已 settle 但未记账』而不是显示 avail=100。

### C-m7 [Minor] C3 主钉子的函数头部注释仍描述被推翻的旧行为，与函数体断言相反
- **位置**：`postgres/attested_auth_pin_test.go:25-36` · `postgres/attested_auth_pin_test.go:74-93`
- **主题**：other · **runtime**：docs · **effort**：S
- **同源提示**：test-credibility：测试名 / 注释 / 断言三者是否同一命题
- **修法**：重写 TestVerifiedBalance_CachedAuthorizedVerdictDoesNotSkipTheLiveCheck 的头部注释，使其描述当前断言（缓存的 Authorized 不能豁免 live check，VerifiedBalance 必须返回 ErrUnauthorizedJournal）；函数体里那段「This assertion is the inverse of what it used to be, and the reason is worth stating where the test lives」写得很好，原样保留不要动。
- **pin**：本条本身就是 pin 的文档，无需新增测试。

---

## W1-sign（Wave 1）

### A-C1 [Critical] capital_injection / capital_withdraw 的 custodial 腿符号与 credit-normal 声明相反 —— 注入 1000 平台资本实测 Custodial=-500 / margin=-1000 / Solvent=false
- **位置**：`presets/capital.go:35` · `presets/capital.go:45` · `presets/capital.go:27` · `presets/templates.go:71` · `postgres/sql/queries/platform_balances.sql:99`
- **主题**：sibling · **runtime**：go · **effort**：M
- **同源提示**：与上一轮 C7（transfer_out/transfer_in/fee_charge 方向反）同源：都是照标准会计约定写而与本库 credit-normal 声明相反；C7 的修复 d5c7357 只覆盖被点名的三个模板，未做全仓符号复核
- **修法**：custodial 是 credit-normal（presets/templates.go:71），CR 才是增加（deposit_confirm 就靠这一点）。capital_injection 应为 `custodial` **Credit** / `equity` **Credit**? 不行——两腿同为 credit 不平衡。正确形状是四条腿两对（同 withdraw_fee，presets/templates.go:213-219）或把 equity 改成 debit-normal 的对手方：注资 = `CR custodial(system, +amount)` + `DR equity_contra(system)`；抽资 = `DR custodial(system, -amount)` + `CR equity_contra`。先由 Aaron 拍板 equity 的极性语义（capital.go:10 用的是标准会计 A=L+E，与本库 custodial=credit-normal 的约定冲突），再改模板。⚠️ 交付路径不能直接改 presets 源码就完事：InstallTemplatePresets（presets/templates.go:389-409）对已存在的 template 走 validateExistingTemplatePreset 并**报错**、从不更新，且 entry_template_lines 有无角色豁免的 BEFORE UPDATE 守卫（003_config_table_guards.up.sql:96-98）。所以必须走 deployment.md 的 expand：新增 capital_injection_v2 / capital_withdraw_v2 两个 template code（INSERT 不受守卫限制），把旧两个置 is_active=false（entry_templates 的 is_active 是白名单里唯一可变列），文档标注迁移窗口。
- **规则/文档要改**：docs/INVARIANTS.md 需新增一条「系统对手方账户的极性约定」不变式：custodial=credit-normal 且 CR=增加托管资产，任何新模板的 custodial 腿方向必须由此推导，不得沿用标准会计的 debit-normal 资产直觉。presets/capital.go:10 的注释 'Equity is credit-normal (A = L + E); increases on the right side' 需改写，明确它与本库 custodial 约定的关系。
- **pin**：新增 postgres/solvency_test.go 用例：装 extended presets，capital_injection 1000 之后断言 Custodial 增加 1000、margin=+1000、Solvent=true；同时**反转**（而非删除）presets/capital_test.go:73-74 与 :83-84 的 EntryType 断言，保留原证伪步骤。

### A-M1 [Major] balance_trends.sql 是第 18 处 normal_side 符号实现且方向是错的 —— 实测同一行 JSON balance=395 而 inflow=105 / outflow=500，存入 500 被报成流出
- **位置**：`postgres/sql/queries/balance_trends.sql:42` · `postgres/sql/queries/balance_trends.sql:43` · `postgres/sql/queries/balance_trends.sql:35` · `core/audit.go:18` · `server/handler_trends.go:18` · `docs/openapi.yaml:2788`
- **主题**：sibling · **runtime**：sql · **effort**：S
- **同源提示**：与 J territory 若报『ledger-react 的 trends 图方向反』为同一根因；与 I territory 若报『trends 端点无可观测』不同源
- **修法**：daily_flows CTE（balance_trends.sql:35-51）加 `INNER JOIN classifications c ON c.id = je.classification_id`，把两列改为经 I-43 权威函数计算：`SUM(GREATEST(ledger_signed_amount(c.normal_side, je.entry_type, je.amount), 0)) AS inflow` / `SUM(GREATEST(-ledger_signed_amount(...), 0)) AS outflow`。注意 classification_id=0（跨分类求和）分支下 JOIN 仍成立（每行有自己的 classification）。语义定义为「使该分类余额增加的量 = inflow」，与 holder.sql:17 的既有措辞（"entry_type equals normal_side ... Positive net = in"）对齐，全仓只留这一种方向语义。
- **规则/文档要改**：docs/openapi.yaml:2788-2789 的 inflow/outflow 目前无 description，语义在 wire 上完全未定义；需补「inflow = 使该维度余额增加的当日合计（normal_side-aware）」。core/audit.go:14 的 'net credit/debit posted for that day' 是机械描述而非方向语义，需改写。docs/INVARIANTS.md I-43 的 'Enforced by' 文件清单需加入 balance_trends.sql。
- **pin**：在既有 postgres/balance_trends_store_test.go:17-121（TestBalanceTrends_GapFill，已建 debit-normal+available 的 wallet_trend 并记 DR 500）里补一行断言 `points[4].Inflow == 500 && points[4].Outflow == 0` —— fixture 已就绪，只差这一行就能红。另把 :150-151 的空账户断言保留但不再作为唯一保护。

### A-M2 [Major] checkout_settlement_gross / _net 的 main_wallet 腿符号相反 —— 实测 gross100/net97/fee3 后 margin=-3 solvent=false 且每笔累积；wire 上 kind=deposit 却 direction=out
- **位置**：`presets/settlement.go:42` · `presets/settlement.go:51` · `presets/settlement.go:52` · `presets/settlement.go:29` · `presets/settlement.go:13` · `presets/transfer.go:43`
- **主题**：sibling · **runtime**：go · **effort**：L
- **同源提示**：与 A-C1/上一轮 C7 同源（同一个 credit-normal 符号误用）；holder_kind 与 direction 矛盾这一半可能与 J territory 的『kindLabels 渲染』条目重叠
- **修法**：先拍板语义——仓内有三种互相矛盾的读法（settlement.go:29-31「商户收款」/ settlement.go:13-16「HolderTxKindDeposit，外部资金进入用户余额」/ transfer.go:43「the holder is paying」），代码与三者都不自洽。若确认是『商户收 net、平台留 fee、托管增 gross』：3 腿表达不出来（借方只有 net，贷方要 gross+fee，差 2×fee），必须改成 4 腿两对，同 withdraw_fee（presets/templates.go:213-219）：`CR custodial(system, +gross)` + `DR main_wallet(user, +net)` + `DR fee_expense(user, +fee)` + `CR fees(system, +fee)`，借 net+fee = 贷 gross+fee 成立且三方符号全对。交付路径同 A-C1：走 expand 新增 checkout_settlement_net_v2 / _gross_v2 code，旧的置 is_active=false。同时修正 transfer.go:43 那句错误注释。
- **规则/文档要改**：presets/settlement.go:21-33 的整段 header 需重写（amount key 语义 + 每条腿的方向依据）；presets/transfer.go:43 那句 'checkout_settlement credits it because the holder is paying' 必须删除或改对。docs/INVARIANTS.md I-44 需补一条：journal_types.holder_kind 声明的方向必须与该 journal type 下模板产生的 HolderTransaction.Direction 一致。
- **pin**：新增 postgres/solvency_test.go 用例：deposit 1000 → checkout_settlement_net{gross:100,net:97,fee:3} → 断言 margin 不变（0）且 Solvent=true；新增 holder 流水断言 `kind=deposit` 的行 Direction 必须为 in。**反转** presets/settlement_test.go:71-74 与 :111-114 的 EntryType 断言。另加一条通用 pin：遍历所有已发货 template，凡 journal type 的 holder_kind 是 deposit 的，其 user 腿在 debit-normal 分类上必须是 Debit。

### A-M3 [Major] M-4 把 fee_expense 改标 memo 后 holder.sql 的三处 `balance_role <> ''` 未同步 —— 提现手续费从用户账单消失：实测余额 total=395、账单净 400、5 无从解释
- **位置**：`postgres/sql/queries/holder.sql:33` · `postgres/sql/queries/holder.sql:88` · `postgres/holder_store.go:131` · `presets/templates.go:84` · `postgres/sql/queries/holder.sql:20` · `postgres/sql/queries/platform_balances.sql:79`
- **主题**：sibling · **runtime**：sql · **effort**：S
- **同源提示**：与 F territory 若报『holder_store_test 的 fixture 测的是产品不用的配置』为同一条；与 J territory 若报『流水缺手续费行』为同一根因
- **修法**：把 postgres/sql/queries/holder.sql:33（page_journals 的 pc.balance_role）与 :88（主查询的 c.balance_role）两处 `<> ''` 改成 `NOT IN ('', 'memo')`，与 M-4 已经改过的 platform_balances.sql:79 用同一个谓词。改完 withdraw_fee 的 holder 侧只剩 locked 腿（带符号 -5），net 不再归零，流水恢复一行 5 的流出。⚠️ 关键：这两处 + :118 + reconcile.sql:247 是同一个谓词的四个拷贝，M-4 只分叉了 platform_balances 那一侧；本次必须四处一起改，并把谓词收敛（见 rule_change）。
- **规则/文档要改**：docs/INVARIANTS.md I-37 的 addendum 与 I-11 需明确写出：『什么算负债』与『什么算用户可见的钱』现在是**同一个**谓词 `balance_role NOT IN ('','memo')`，任何新增 BalanceRole 值必须同时评审这两个语义。holder.sql:20-22 那段预言了本缺陷的注释应保留并加上「本条已于 <date> 被 memo 触发过一次」的史实。
- **pin**：把 postgres/holder_store_test.go:59 的 fixture 从 SeedClassification（不带 role，落 ''）改成 SeedClassificationWithRole(..., 'memo')，即**产品实际配置**；:146 的断言从 'fee stays visible despite role-less counter-entry' 改为 'fee stays visible despite memo-tagged counter-entry'。另新增一条端到端 pin：装 default presets 走 deposit500/lock105/withdraw_fee5/withdraw_confirm100，断言 ListHolderTransactions 的净额（in 减 out）等于 GetBalanceBreakdown().Total —— 这条能一次性拦住任何『余额动了但流水没有对应行』的形态。

### A-M4 [Major] fees 收入账被两个已发货模板写入相反符号 —— 实测收了两笔 30 的手续费后 fees 读出 0（trial balance debit=30 credit=30 net=0）
- **位置**：`presets/fee.go:54` · `presets/settlement.go:53` · `presets/fee.go:26` · `presets/fee.go:39`
- **主题**：other · **runtime**：go · **effort**：M
- **修法**：fees 是 credit-normal（presets/fee.go:26），收入增加 = CR。checkout_settlement_net（settlement.go:53）的 CR 是对的，fee_charge（fee.go:54）的 DR 是错的。但把 fee_charge 的 fees 腿翻成 CR 会让该 journal 不平衡（holder 腿已是 CR），所以 fee_charge 必须像 withdraw_fee 一样改成 4 腿两对：`CR main_wallet(user, -amount)` + `DR fee_expense(user, +amount)` + `DR ???` —— 与 A-M2 的正确形状同构，两条一起设计。⚠️ 不要动 fee_charge 的 holder 腿（presets/fee.go:53 的 CR main_wallet 是 C7 修对的，实测 margin=+2.5 solvent=true）。交付路径同 A-C1（expand 新 template code）。
- **规则/文档要改**：presets/fee.go:39-46 的 '⚠️ still-open question' 段落必须收口成结论并写出数值后果（两笔同额手续费互相抵消到零，聚合收入 = checkout_fees − direct_fees，既不是收入也不是任何有意义的量）。docs/audits/2026-08-25-financial-engineering/TODO.md 从未收录这一条，需补记或直接在本轮关闭。
- **pin**：新增 presets 或 postgres 层用例：同一 currency 上各跑一次 fee_charge(30) 与 checkout_settlement_net(fee=30)，断言 GetPlatformBalances 的 fees = 60（当前实测 0）；并断言 TrialBalance 里 fees 行 Net = 60。
- **依赖**：A-M2

### A-M5 [Major] effective_at 回溯的快照自愈只装在 GetSnapshotBalances 一个读入口，GetBalanceTrends 绕过仍返回过期值；且自愈是行驱动的，回溯新开的维度完全不被覆盖
- **位置**：`postgres/rollup_adapter.go:411` · `postgres/rollup_adapter.go:442` · `postgres/sql/queries/balance_trends.sql:25` · `postgres/balance_trends_store.go:96` · `postgres/snapshot_extra_store.go:49` · `docs/INVARIANTS.md:561`
- **主题**：sibling · **runtime**：go · **effort**：M
- **同源提示**：与 I territory 若报『snapshot_integrity 只扫最近一个 snapshot_date』相邻但不同源
- **修法**：两个独立缺口，一起补：(1) 让 BalanceTrendsStore.GetBalanceTrends 不再直接消费 balance_trends.sql 的 snap_balance，改为对区间内每一天走 RollupAdapter 的自愈路径（或把 snapshotDimensionIsStale 的判定下推成 SQL：在 snapshots CTE 上 LEFT JOIN GetMaxEntryCreatedAtForDimensionBefore 的等价子查询，stale 行改用 ListBalancesAt 的实时值）。前者代码量小但 N 天 N 次查询，后者一次查询但 SQL 复杂——建议前者，趋势端点本来就是低频报表。(2) 把 GetSnapshotBalances（rollup_adapter.go:421-475）的行驱动循环改成『先算 cutoff 内的实时维度全集，再与缓存行做全外连接』：缓存有而实时无 → 0；实时有而缓存无 → 用实时值补行（当前完全丢失，实测回溯新开的 pending=70 在输出里不存在）。稀疏模式（snapshot_extra_store.go:49-89 余额未变则不写行）会让『无行』成为常态，所以第 (2) 条不是边角。
- **规则/文档要改**：docs/INVARIANTS.md:561-580（I-14 的 'As-of reads self-heal' 段）当前措辞 'only reads through GetSnapshotBalances are guaranteed correct' 字面诚实但隐瞒了『第二个读 balance_snapshots 的入口存在且是用户可见 HTTP 端点』。需改成枚举全部读入口并逐个声明其自愈状态；补一条『新增任何 balance_snapshots 读入口必须接自愈』。
- **pin**：新增 postgres/snapshot_staleness_test.go 用例：CreateDailySnapshot(d) → post EffectiveAt=d-1 且引入一个**快照时不存在**的新分类的 journal → 断言三者一致：GetSnapshotBalances(d) == GetBalanceTrends(d..d) == ListBalancesAt(d+1)（当前实测三者为 150 / 100 / 150，且前者缺 pending 行）。

### A-M6 [Major] 装了 FX preset 之后买入币种的 SolvencyCheck 永久为 false —— 实测健康 FX 头寸下 FFF 报 custodial=0 liability=90 margin=-90 solvent=false
- **位置**：`postgres/sql/queries/platform_balances.sql:99` · `postgres/sql/queries/platform_balances.sql:108` · `presets/fx.go:35` · `presets/fx.go:46` · `core/platform_balance.go:74`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **同源提示**：与上一轮 M-4 同型（虚假缺口淹没真实缺口）；与 G territory 若报『链上 settlement 头寸无对账』相邻
- **修法**：三条路径供 Aaron 选（都需要拍板，不要 agent 自决）：(a) 把 Custodial 的口径从硬编码 code='custodial' 扩成『一组可配置的托管/储备分类』（含 settlement），由消费方在 SolvencyCheck 调用时或 Service 装配时声明——同时解决 A-N3；(b) 保持口径不变，但让 SolvencyReport 增加一个 `Scope`/`Incomplete` 字段，明确声明『本币种存在未计入 Custodial 的系统头寸（settlement 非零），Solvent 不可采信』，即把无意义的 false 换成诚实的『不可判定』（working-agreements §3：恒假信号比没有信号更糟）；(c) 文档层收口：明确 SolvencyCheck 不适用于跨币种 FX 部署。推荐 (a)+(b) 组合。
- **规则/文档要改**：presets/fx.go:46-48 只说 'Reconciling settlement balances against external custody figures is the caller's responsibility'，必须补『且 SolvencyCheck 会因此在买入币种永久返回 false』。core/platform_balance.go:74-81 的 SolvencyChecker 接口文档需写明 Custodial 的口径来源与它的局限。docs/INVARIANTS.md 里 I-37 一带需补 Solvent 的适用边界。
- **pin**：新增 postgres/solvency_test.go 用例：装 FX bundle，走一次 fx_sell(100 A)/fx_buy(90 B) 对，断言按选定方案的期望结果（方案 a：B 币种 Solvent=true；方案 b：Incomplete=true 且 Solvent 不被当作可采信判定）。
- **依赖**：A-N3

### A-M7 [Major] I-43「符号权威只剩一处」没有机械门禁 —— 'Enforced by' 是硬编码文件清单，两次人工枚举（审计 10 处、15d110e 9 处）都漏掉同一个文件
- **位置**：`docs/INVARIANTS.md:3612` · `postgres/normal_side_sign_test.go:26` · `core/account_policy.go:124`
- **主题**：gate-shape · **runtime**：ci · **effort**：M
- **同源提示**：与 H territory 若报『契约门禁只比对静态文件』同型（working-agreements §5：能做成机器检查的不要写成文档清单）
- **修法**：新增一条机械 gate（放 postgres/ 的测试里，跑在 go test ./... 无 path filter）：遍历 postgres/sql/queries/*.sql，用正则找出所有同时提到 je.entry_type（或 entry_type）与 amount 的 CASE/条件表达式；凡不是调用 ledger_signed_amount / ledger_signed_delta 的，必须命中一张显式白名单（当前合法成员：integrity_balance.sql:24/36/39 与 journals.sql:143 —— 逐 journal 借贷平衡检查，debit−credit 与 normal_side 无关）。白名单成员必须逐条写理由，新增未分类表达式直接 fail。Go 侧同理加一条：grep 非测试 Go 里 `entryType`/`EntryType` 与 `NormalSide` 同时出现的比较表达式，白名单外即 fail（当前合法成员：service/rollup.go:239 的负余额异常判定、service/reconcile.go:1023 的分桶）。形状照抄 grant_coverage_test.go 的『新表不分类直接 fail』与 I-18 的『从 migration 机械派生』。
- **规则/文档要改**：docs/INVARIANTS.md:3612-3620 的 'Enforced by' 从文件清单改为指向这条 gate（清单是 gate 的输出，不是真相源）。同时在 I-43 的 'Pinned by' 里加上这条 gate。
- **pin**：这条本身就是 pin。自证方式：把 balance_trends.sql:42-43 还原成裸 CASE，gate 必须红；把 core.Sign 的某个调用点改回手写 switch，Go 侧 gate 必须红。
- **依赖**：A-M1, A-N1

### A-N1 [Minor] SettlementNettingViolations 的裸符号表达式绕过 I-43 —— settlement 是 credit-normal，运维在 Finding 里看到的 net 符号是反的
- **位置**：`postgres/sql/queries/reconcile.sql:42` · `postgres/sql/queries/reconcile.sql:48` · `service/reconcile.go:1070`
- **主题**：sibling · **runtime**：sql · **effort**：S
- **同源提示**：与 A-M1 同一条 I-43 缺口（本轮共发现 2 处 SQL 残留）；与 I territory 若报『reconcile Finding 可读性』相邻
- **修法**：把 reconcile.sql:42 的 SELECT 列与 :48 的 HAVING 都改成 `ledger_signed_amount(c.normal_side, je.entry_type, je.amount)`（查询里已经 INNER JOIN classifications c，无需加表）。HAVING 的零点判定不受影响（!= 0 对符号不敏感），修的是 net_balance 呈现给运维的方向。
- **pin**：新增用例：构造一个非零的 settlement 净额（credit-normal 分类上净 CR），断言 Finding 的 Detail 里 net 的符号与 GetBalance 对该分类的读数同号。

### A-N3 [Minor] GetSystemSideCustodialBalance 把「什么是托管资产」硬编码成 code='custodial' —— 消费方换个命名就得到 Custodial=0 与永久资不抵债，且无任何报错提示这个耦合
- **位置**：`postgres/sql/queries/platform_balances.sql:108` · `core/platform_balance.go:74` · `core/platform_balance.go:51`
- **主题**：silent-default · **runtime**：sql · **effort**：S
- **修法**：把托管分类的口径从 SQL 里的字符串字面量提到消费方可注入的配置（与 FullReconciliationConfig.SettlementClassCode 同型的做法：加 SolvencyConfig{CustodialClassCodes []string}，默认 ["custodial"]），SQL 改成 `c.code = ANY($2::text[])`。同时加 fail-loud：解析出的分类集合在该 currency 上一条记账都没有时，SolvencyReport 明确标出『Custodial 口径未匹配任何分类』而不是静默返回 0。
- **规则/文档要改**：core/platform_balance.go:74-81 的 SolvencyChecker 接口文档写的是 'Custodial is the total of system-side "custodial" classification balances'，未说明这个字符串不可配置；core/platform_balance.go:51-57 的 SolvencyReport 字段文档同样需补口径来源。
- **pin**：新增用例：把托管分类命名成 'reserve'（非 'custodial'）跑一遍存款，断言（选定方案下）Custodial 正确或至少报出口径未匹配——当前会静默返回 0 且 Solvent=false。

### A-N4 [Minor] holder.sql:118 (ListHolderCurrencies) 与 reconcile.sql:247 的 `balance_role <> ''` 同样把 memo 纳入范围 —— 只有 fee_expense 有记账的币种会被列进用户的币种列表
- **位置**：`postgres/sql/queries/holder.sql:118` · `postgres/sql/queries/reconcile.sql:247`
- **主题**：sibling · **runtime**：sql · **effort**：S
- **修法**：与 A-M3 一并把这两处 `<> ''` 改成 `NOT IN ('', 'memo')`。holder.sql:118 修的是『用户看到一个自己其实没有可支配余额的币种』；reconcile.sql:247 修的是未标 holder_kind 巡检的扫描面（memo 分类的 journal type 不会出现在用户流水里，不该因此被判为需要打标）。
- **规则/文档要改**：none（并入 A-M3 的谓词收敛）
- **pin**：并入 A-M3 的谓词收敛 pin：新增一条断言『四处 balance_role 过滤谓词的字面量完全一致』的机械检查（grep postgres/sql/queries/*.sql 的 balance_role 过滤，白名单外即 fail）——这条比逐个用例更能防复发。
- **依赖**：A-M3

### A-N5 [Minor] presets/fx.go 声称 per-currency balance validation 能抓 rate-quote bug —— 该声称不成立：逐币种借贷相等对任意 qtyB 都成立，汇率错 100 倍照样通过
- **位置**：`presets/fx.go:29` · `presets/fx.go:22`
- **主题**：other · **runtime**：docs · **effort**：S
- **同源提示**：上一轮已记同一处的 doc/code 符号矛盾（已修）；本条是同一段注释里另一句仍不成立的断言
- **修法**：纯文档修正：删除或改写 presets/fx.go:29-33 的 'Keeping each leg single-currency lets per-currency balance validation (DB trigger + Go validator) catch any rate-quote bug — neither leg can be unbalanced and silently pass'。它把『每条腿自己平衡』说成了『汇率关系被校验』。改成明确声明账本不校验两腿的汇率关系、也不保证两腿都落地，跨腿原子性与 rate 正确性完全是调用方责任（与 :18-23 已有的 ATOMICITY 段落合并）。
- **规则/文档要改**：presets/fx.go:22-33 的注释即为待改文本；docs/COOKBOOK.md 的 FX 配方需核对是否复述了这个不成立的声称
- **pin**：none（纯注释）。若要一条：新增用例传一个明显错的 qtyB（qtyA×rate 的 100 倍）跑 fx_sell/fx_buy 对，断言两者**都成功**——把『账本不校验汇率』固化成一条显式契约而非隐含假设。

### F-m10 [Minor] lifecycle_acyclic 门禁只覆盖 presets.DepositLifecycle / SweepLifecycle 这两个 Go 变量，覆盖不到「实际装进数据库的那个 lifecycle」；WithdrawalLifecycle 从未被 hasCycle 检查
- **位置**：`presets/lifecycle_acyclic_test.go:71` · `presets/lifecycle_acyclic_test.go:84` · `service/onchain.go:724` · `service/onchain.go:653` · `service/expiration.go:152` · `presets/withdrawal.go:8`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **修法**：两层：(a) 静态面：把 hasCycle 也跑到 WithdrawalLifecycle 上 —— 它是有环的（failed → reserved），今天安全只因为 expired 只能从 processing 到达且是终态，而 service/expiration.go:152 的 'expire-booking-' + b.UID 键依赖的正是「每个 booking 只到达一次 expired」。补一条断言：对每个 preset lifecycle，「有环」与「键里是否带区分成分」必须一致（deposit 无环 → 可用 status 键；sweep 有环 → 键带 tx hash/channel ref；withdrawal 有环但 expired 不可重达 → 显式断言 expired 的入边只有一条且其源不可从 expired 回到）。(b) 运行面（更根本）：service.Onchain 用的是 code='deposit' 的**数据库分类**的 lifecycle，由消费方 SetLifecycleIfEmpty 自行装。在 Onchain.Run 的启动校验里（validateAutoCreditCeilings 旁边）加一步：读出 deposit 分类的 lifecycle，若有环则拒绝启动并说明 depositTransitionKey 会碰撞 —— 把「消费方装了带环 lifecycle 就静默吞掉合法重试」变成启动期显式拒绝。
- **规则/文档要改**：建议新增或扩写一条 invariant：「按 (booking, to_status) 派生幂等键的调用点，其 lifecycle 必须无环，且这一点在启动期被校验」——今天只写在 service/onchain.go:711-723 与 service/expiration.go:139-151 的注释里
- **pin**：(a) 给 WithdrawalLifecycle 加一条能重达 expired 的边，新断言必须红；(b) 用一个带环的自定义 deposit lifecycle 调 Onchain.Run，必须 ErrorIs(core.ErrInvalidInput)

---

## W1-ledgerstore（Wave 1）

### A-C2 [Critical] PostJournal 对 ReversalOfUID 零校验 —— C8「冲销剩余全部静默少退」原型复活：实测冲销后账上剩 50，返回 nil，所有对账检查 balanced=true gap=0
- **位置**：`postgres/ledger_store.go:931` · `postgres/ledger_store.go:945` · `postgres/reversal_fraction_store.go:518` · `postgres/reversal_fraction_store.go:270` · `postgres/reversal_fraction_store.go:290` · `core/journal.go:83`
- **主题**：sibling · **runtime**：go · **effort**：M
- **同源提示**：上一轮 C8（b49cb20）修的是 reversalEntriesFor 的内部推导，本条是同一缺陷经未校验的输入门复活；可能与 E territory 的『导出字段误用面』条目同源
- **修法**：在 postJournalWithQueries 解析 input.ReversalOfUID 的分支（postgres/ledger_store.go:931-945）加入与冲销 API 对称的三条校验，全部 fail-closed：(1) 拒绝 orig.ReversalOf.Valid（不得冲销一张冲销 journal，与 ledger_store.go:762 / reversal_fraction_store.go:152 的既有护栏对齐）；(2) 校验本 journal 的 entries 是被引用 journal entries 的翻转子集——逐 entryDimKey（holder, currency, classification, 翻转后的 entry_type）比对，出现原 journal 不存在的维度即 ErrInvalidInput；(3) 复用 cumulativeReversedByDimension + 逐维度上界检查，累计超过原额即 ErrConflict。若 Aaron 认为「手写 reversal_of」本身不该开放，替代方案是把 core.JournalInput.ReversalOfUID 从消费方入口收回（仅 postgres 包内部可设，导出字段改成 unexported 或加 //lint 级别的 Authorize-only 约束），冲销一律走 ReverseJournal/ReverseJournalFraction——这条更彻底但是 breaking。
- **规则/文档要改**：docs/INVARIANTS.md I-2 目前只承诺「累计冲销不超过原额」（上界），本例累计恰为 100=原额、上界未被违反却丢了 50。需补下界措辞：「num==den 的冲销之后，原 journal 在每个维度上的净额必须为 0」，并明确 reversal_of 这条链的完整性由谁保证。core/journal.go:83 的字段文档需写明「由冲销 API 设置，消费方不得手写」。
- **pin**：新增 postgres/reversal_fraction_test.go 用例：post 100 → PostJournal{ReversalOfUID:J, 四条净零腿(CR mw 50/DR mw 50/DR cust 50/CR cust 50)} → 断言这一步就被拒（ErrInvalidInput）；再加一条断言「若该 journal 落库，ReverseJournalFraction(J,1,1) 之后 holder 余额必须为 0」——断言余额而非内部形状（沿用 b49cb20 的 pin 写法）。另加一条 PostJournal{ReversalOfUID:<一张冲销 journal>} 被拒的用例。

### B-M1 [Major] ConfirmPending/CancelPending 只预锁用户 holder，随后 PostJournal 按全局序先取系统对手方 -holder，锁序与全仓相反 → 两个普通 pending 操作互锁（实跑复现真实 40P01）
- **位置**：`postgres/pending_store.go:248` · `postgres/pending_store.go:277` · `postgres/pending_store.go:279` · `postgres/pending_store.go:315` · `postgres/ledger_store.go:968` · `core/types.go:146`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：把 checkPendingBalanceAndPost 的 run 闭包里那次预锁从「只锁 {holder, currencyID} 一个 pair」改成锁「这条 journal 会碰的完整 pair 集合、按全局序」——即复用 postJournalWithQueries 用的同一组：先 s.resolveEntries(ctx, qtx, input.Entries) 拿到 resolvedEntry，再 acquireBalanceLocks(ctx, qtx, sortedUniquePairs(balancePairsFromEntries(resolved)))。这样 -holder 会先被取到，与 PostJournal/PostAuthorized 内部那次取锁完全同序（advisory xact lock 可重入，重复取是 no-op）。GetBalance(pending) 的 TOCTOU 保护不受影响，因为用户 pair 仍在锁集合内。注意 resolveEntries 必须在 qtx 上跑，且要在 idempotency recheck 之前完成，避免为解析多开一次往返。
- **规则/文档要改**：none（I-11 / 锁序不变式的措辞不用改；建议在 postgres/ledger_store.go:69-88 sortedUniquePairs 的 doc 里补一句「任何在 postJournalWithQueries 之外自取 balance lock 的调用点都必须锁完整 pair 集合」，把口头原则写成可 grep 的约束）
- **pin**：新增 postgres 集成测试：装 pending bundle → AddPending(H) → 第三个事务先取 bal(-H,C)（postgres.AcquireBalanceLocksForTest）→ goroutine 调 ps.ConfirmPending(H) → sleep → 第三个事务再取 bal(H,C)，断言两侧都不出现 40P01；把预锁改回单 pair 时该测试必须红（已实跑验证会红）。

### B-M3 [Major] ExecuteTemplateBatch 的 batch 级全局锁序只加在 pool 模式，tx 模式 executeTemplateBatchWithQueries 仍是逐 journal 取 idem→bal，两批 holder 顺序相反即 ABBA
- **位置**：`postgres/ledger_store.go:656` · `postgres/ledger_store.go:602` · `postgres/ledger_store.go:542` · `ledger.go:589`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：把 ledger_store.go:604-631 那段（去重+排序 idempotency keys 依次取 idem 锁 → 收齐所有 journal 的 balancePair、sortedUniquePairs 一次取完 bal 锁）抽成一个私有 helper，例如 func (s *LedgerStore) preacquireBatchLocks(ctx, q *sqlcgen.Queries, inputs []core.JournalInput) error，然后 pool 模式与 executeTemplateBatchWithQueries 两个分支都在进入 per-journal 循环之前调它。tx 模式不需要 Attestor 相关改动（仍是 AuthStatusUnsignedTxMode），只补锁序。顺带更新 ExecuteTemplateBatch 的 doc comment（:520-536）——它目前只讨论 pool 模式的签名位置，没提 tx 模式锁序。
- **pin**：重写 B-M4 那条 pin 让它真正驱动 ExecuteTemplateBatch，并加一个 tx-mode 变体：两个并发 svc.RunInTx，各自 tx.JournalWriter().ExecuteTemplateBatch(requests) 且 requests 的 holder 顺序互为倒序，断言两侧都不出现 40P01；把 helper 调用从 tx 分支删掉必须红。
- **依赖**：B-M4

### B-M4 [Major] TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock 全文不出现 ExecuteTemplateBatch，在测试里自己重实现了一遍修复——删掉 ledger_store.go 的整段预锁它照样绿
- **位置**：`postgres/lock_order_test.go:172` · `postgres/lock_order_test.go:155` · `postgres/ledger_store.go:602`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：F(test-credibility)：pin 不驱动被测实现的通用门禁缺失；F 还独立命中 registration_rescan_store 手写 raw SQL 与 mergeWorkerConfig 形状
- **修法**：重写该测试：不再用 runBatch 自己调 SortedUniquePairsForTest + AcquireBalanceLocksForTest，改为真正装模板（如 dev_credit / deposit_confirm 两个不同 holder）并在两个 goroutine 里各自调 store.ExecuteTemplateBatch(requests)、requests 顺序互为倒序，断言两侧 err 均为 nil。primitive 级的那份断言保留价值有限，可留作 TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient 的对照但改名去掉 ExecuteTemplateBatch 字样，避免继续冒充这条修复的 pin。对照写法见 postgres/reserver_store_test.go:227（它真的调 store.Reserve，我实跑变异确认会红）。
- **规则/文档要改**：建议给 docs/INVARIANTS.md 的 Pinned by 门禁加一条新断言：测试的 doc comment 若声称钉住某个导出符号，其源码里必须出现该符号（core.TestInvariantsDocPinsAllExist 目前只校验测试名存在）。
- **pin**：这条本身就是 pin 的修复；验收判据 = 把 ledger_store.go:604-631 整段删掉后新测试必须红（当前版本会绿）。

### B-m6 [Minor] bookings 行锁与 balance advisory lock 的取序在两条路径上相反：Transition→PostJournal 是 rowlock→bal，而带 event_uid 的 PostJournal 是 bal→rowlock（LinkBookingJournal）
- **位置**：`service/onchain.go:976` · `postgres/booking_store.go:212` · `postgres/ledger_store.go:968` · `postgres/ledger_store.go:1123` · `server/handler_journals.go:191` · `core/journal.go:84`
- **主题**：sibling · **runtime**：go · **effort**：M
- **修法**：在 postJournalWithQueries 解析 input.EventUID 的地方（在 acquireBalanceLocks 之前），把该 event 所属的 booking 行先 FOR UPDATE 锁住——即把现在 :1113 附近的 q.GetBooking 换成 GetBookingForUpdate 并提前到 balance locks 之前执行。这样两条路径都变成 rowlock→bal，环消失。若不愿多一次往返，退路是在 core.JournalInput.EventUID 的 doc 与 server/handler_journals.go 的 openapi 描述里写明「带 event_uid 的 PostJournal 应当与该 booking 的 Transition 组合在同一个 RunInTx 里（CLAUDE.md 的 Event-Journal atomicity 范式），单独 post 会与本库自己的存款确认路径产生锁序反向」——但这是靠文档，不是靠结构。
- **规则/文档要改**：若走文档退路：改 core/journal.go:84-86 EventUID 的 doc + docs/openapi.yaml 对应字段描述。走结构修法则 none。
- **pin**：新增测试：一个事务模拟 Transition 路径先取 booking 行锁再取 bal(H,C)；另一个并发调 PostJournal(EventUID=该 booking 的 event, entries 打 H)；断言两侧都不出现 40P01。把 GetBookingForUpdate 挪回 balance locks 之后必须红。

### C-m6 [Minor] attestJournal 把幂等重放命中标成 unsigned_no_attestor，靠「这个值不会落库」的注释维持不变量
- **位置**：`postgres/ledger_store.go:299-306` · `core/auth.go:359-386` · `service/attest_verify.go:454-458`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：不要在重放分支复用有语义的 AuthStatusUnsignedNoAttestor。最小改法：给包内 journalAuth 加一个 replay bool 字段，重放命中时返回 journalAuth{replay: true}（status 留空），并在 postJournalWithQueries 的入口断言「status 为空只允许出现在 replay==true 且随后走重查直接返回的路径上」，任何走到 INSERT 的路径拿到空 status 直接返回 error。不要新增会落库的 AuthStatus 值（那要动 journals.auth_status 的 CHECK 与 I-26）。理由：unsigned_no_attestor 正是 VerifyLedger:454-458 用来判「forged, or posted before the key was wired」的那个值，将来若有人给 postJournalWithQueries 加一条「重查没命中就继续插入」的分支，一条合法 journal 会被报成疑似伪造。
- **规则/文档要改**：none（若改为新增落库的 AuthStatus 值则需同步 schema CHECK 与 I-26 —— 建议避免）。
- **pin**：新增 postgres.TestPostJournal_IdempotentReplayNeverInsertsUnsignedRow：同 key 同 payload 重放，断言不产生新行且原行 auth_status 仍为 signed；配合上述入口断言，删掉断言会让该测试变红。

### E-m13 [Minor] PendingStore.ExpirePendingOlderThan 用 %v 聚合子错误，整条路径的 errors.Is / core.IsRetryable 失效
- **位置**：`postgres/pending_store.go:466` · `postgres/pending_store.go:431` · `postgres/pending_store.go:436` · `postgres/pending_store.go:456`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：把 :466 的 fmt.Errorf("pending: expire: %d errors: %v", len(errs), errs) 改成 errors.Join(errs...) 包一层：fmt.Errorf("pending: expire: %d errors: %w", len(errs), errors.Join(errs...))。errors.Join 支持 errors.Is 穿透多个子错误，里层已经用 %w 认真包过了（:431/:436/:456），只有最外层把链断掉。这是 svc.PendingTimeoutSweeper() 唯一的返回面。顺带：:431/:436/:456 的错误信息里带了 currency_id=%d 内部数字 id，改成 uid 或去掉。
- **pin**：新增 postgres 测试：构造两笔会失败的过期 pending（其中一笔命中 core.ErrInsufficientBalance），断言返回的 error 上 errors.Is(err, core.ErrInsufficientBalance) 为真。改回 %v 必红。

### F-m6 [Minor] ConfirmPending 的余额不足路径只被 CancelPending 的测试间接 pin —— 破坏共享闸只红一条，且 14 个 pending 测试里没有 ConfirmPending_InsufficientBalance
- **位置**：`postgres/pending_store.go:270` · `postgres/pending_store.go:140` · `postgres/pending_store_test.go:392` · `docs/INVARIANTS.md:100`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：新增 TestPendingStore_ConfirmPending_InsufficientBalance，形状照 :392 的 CancelPending 版本：造一个 pending 余额小于 ConfirmPendingInput.Amount 的场景，断言 ErrorIs(core.ErrInsufficientBalance)，并断言事后余额与 pending 余额都没变（拒绝必须无副作用）。今天两条路径共用 checkPendingBalanceAndPost，所以覆盖是传递的；一旦有人给 ConfirmPending 单开分支，pin 就断了而没人知道。
- **规则/文档要改**：none（可选：I-3 的 Pinned by 补这条，与已有的 TestPendingStore_AddPending_Idempotent 并列）
- **pin**：把 postgres/pending_store.go:270 的 bal.LessThan(required) 改成 required.Neg()，新测试必须变红（当前只有 CancelPending 那条红，已实跑）

---

## W1-onchain（Wave 1）

### G-C1 [Critical] 前向扫链在 IngestDeposit 失败后仍推进游标，非 ErrConflict 的失败让真实充值永久消失
- **位置**：`service/onchain.go:1246` · `service/onchain.go:1252` · `service/onchain.go:686` · `service/onchain.go:592`
- **主题**：sibling · **runtime**：go · **effort**：M
- **合并了**：I-N14（各自报告的论证都要读）
- **同源提示**：territory A：checkAmountPrecision 对 token decimals > currency exponent 的拒绝策略（postgres/booking_store.go:124）——该策略本身归 A，但它在本路径上的后果是丢钱；territory F：fakeChainReader.FetchDeposits 恒返回 (nil,nil) 使整个 pull 摄入路径不可测
- **修法**：改 service/onchain.go 的 scanChainOnce：把 for 循环里的 IngestDeposit 失败从「只记日志」改成「累计失败并阻止游标推进」——即用一个 failed 计数/首个 error，循环结束后若 failed>0 则 return 该 error（不调 SetCursor），让下一 tick 从同一 from 重扫（IngestDeposit 本身幂等，重扫安全）。同时把 IngestDeposit 里的死信写入从只覆盖 core.ErrConflict（:686）扩大为「所有非瞬时、重扫也不会成功的失败」：ErrConflict 保持现状，core.ErrNotFound（币种未注册）与 core.ErrPrecisionExceeded（token decimals > currency exponent）也写死信并跳过该 sighting 不阻塞游标，其余（DB 瞬时错误）阻塞游标。新增 core.Metrics.DepositIngestFailed(chainID int64, reason string) 并在 NoopMetrics + observability/prometheus.go 落地 counter。语义对齐 processRegistrationRescan（:592-597，失败即不 Advance）。
- **规则/文档要改**：docs/INVARIANTS.md 新增一条：游标只在窗口内每一笔 sighting 都已入账或已落死信之后才前进（与 RegistrationRescan 的 Advance 语义同构）；docs/RUNBOOK.md §14 指标表补 deposit_ingest_failed_total 的处置。
- **pin**：新增 service 测试：让 fakeChainReader.FetchDeposits 返回一笔会被 CreateBooking 拒绝的 sighting，断言 scanChainOnce 返回 error 且 ChainCursorStore.GetCursor 未前进；再让它成功后断言游标才前进（删掉阻塞逻辑必红）。

### G-C2 [Critical] TxLogSeq 取决于查询传入的地址集合，watcher 与 registration rescan 对同一笔 tx 派生不同幂等键
- **位置**：`chains/evm/reader.go:142` · `chains/evm/reader.go:153` · `service/onchain.go:588` · `service/onchain.go:1242` · `service/onchain.go:697` · `core/onchain.go:85` · `docs/INVARIANTS.md:904`
- **主题**：sibling · **runtime**：go · **effort**：L
- **同源提示**：territory F：I-20 的三条 pin 无一经过 chains/evm.Reader.FetchDeposits，属「Pinned by 不能证伪该 invariant」的实例；territory H：I-20 的声称与实现不符属契约文档漂移
- **修法**：把 TxLogSeq 的定义从「本次查询命中集合内的序号」改成「与查询集合无关的、tx 内确定的量」。两条可选（建议前者）：(a) 直接用 types.Log.Index（块级 log_index）与 tx_hash 一起作为键的一部分——I-20 拒绝它的理由是「reorg 后 log_index 会变」，但键里已经含 tx_hash，同一 tx 在同一条链上被重挖时其内部日志相对顺序不变而块级 index 会整体位移，故应改用 (b)；(b) 在 chains/evm/reader.go 的 FetchDeposits 里对每个命中的 log 用「该 tx 内、该 log 之前有多少条同 topic0 的 Transfer 日志」计算 seq——这要求按 tx 拉全量日志而不是只拉命中集合：在 FilterQuery 里去掉 Topics[2] 的地址分片、改为按 tx_hash 二次 eth_getLogs（或对命中 tx 补一次不带 to 过滤的查询），再在本地按 to ∈ registry 过滤。同时把 :147/:151 两处 continue 移到 seq 递增之后（跳过的日志也占位），使 seq 不随解码成功与否位移。channel/onchain 的 webhook 契约文档必须写明与 (b) 完全相同的定义。属破坏性契约变更，按 deployment.md 走 expand→migrate→contract：新键与旧键并存一个 release，双查后再切。
- **规则/文档要改**：I-20 全文重写：当前措辞「among the logs in that transaction that credit one of our registered addresses」把一个会变的集合写进了定义，必须改成与注册集合无关的表述；core.DepositSighting.TxLogSeq 的 doc comment 同步；channel/onchain/doc.go 的 payload 说明补上精确定义。
- **pin**：新增 chains/evm/reader_test.go：同一份日志集合分别以 [addrA,addrB] 与 [addrB] 两个地址集合调用 FetchDeposits，断言 addrB 那条 sighting 的 TxLogSeq 两次相同（当前实现下必红）；service 侧再加一条：watcher 与 rescan 先后摄入同一笔双转账 tx，断言最终每个 holder 恰好一个 booking、无死信。

### G-M1 [Major] 浅 reorg 基于单次 TxIncluded=false 直接打终态 failed，幂等键让重挖后的同一笔永远无法再入账
- **位置**：`service/onchain.go:1321` · `service/onchain.go:1327` · `service/onchain.go:739` · `presets/deposit.go:17` · `chains/evm/reader.go:174`
- **主题**：sibling · **runtime**：go · **effort**：M
- **同源提示**：territory F：recheckOneDeposit 的 !included 分支零测试覆盖
- **修法**：三处一起改：(1) 不再由单次观测下终局——在 core.ChainConfig 增加 ShallowReorgMissesBeforeFail int32（由消费方注入，无默认值，与 AutoCreditCeiling 同样的 secure-by-default 姿态），service.Onchain 维护 per-booking 连续 miss 计数（形态照抄 reconcileFailures，service/onchain.go:874-895），达到阈值才 Transition(failed)；(2) 让 failed 可恢复——在 presets.DepositLifecycle 加 failed→confirming 的重挖边（注意：这会给 DepositLifecycle 引入环，depositTransitionKey 的「booking+to_status 足够」前提随之失效，必须同步把 depositTransitionKey 改成含区分本轮的量，并更新 presets/lifecycle_acyclic_test.go 的断言与 depositTransitionKey 的 doc comment），或退一步只在 advanceConfirmation 的终态早退里对 failed 且 TxIncluded 恢复为 true 的 booking 走人工 review 队列；(3) 新增 core.Metrics.DepositShallowReorgFailed(chainID) 并写 RUNBOOK 处置。
- **规则/文档要改**：设计文档 docs/plans/2026-07-11-crypto-deposit-sweep-design.md §6 第 84 行「阈值前 tx 消失（浅 reorg）→ Transition(failed)」需补上「消失」的判定标准（连续 N 次未命中，N 由消费方注入）；若采用 (2) 的 lifecycle 加边方案，I-3 关于 deposit 幂等键的推理段与 presets/lifecycle_acyclic_test.go 必须同步。
- **pin**：新增 service 测试：setIncluded=false 后连续跑 recheck N-1 次断言仍是 confirming、第 N 次才 failed；再补一条 setIncluded 恢复 true 后该 booking 能被重新入账（当前实现两条都红）。
- **依赖**：G-m4

### G-M10 [Major] 上轮 Major-1 修复的服务侧一半无 pin：priorTxHash 被记录但从不断言，gas-bump 分支从未被任何测试执行
- **位置**：`service/onchain_integration_test.go:161` · `service/onchain_integration_test.go:744` · `service/onchain_integration_test.go:827` · `service/onchain.go:1862` · `chains/evm/sweeper_test.go:64`
- **主题**：verdict-unused · **runtime**：go · **effort**：S
- **同源提示**：territory F：修复的回归 pin 原样复现它本该防止的盲区（与 Worker.Subscribe 同形）
- **修法**：新增一条 service 集成测试真正走完 gas-bump 分支：WithSweepStuckAfter(0) 且 WithMaxSweepBumps(2)（现有两条 revival 测试用 MaxSweepBumps(0)，让 bumps>=max 在第一次就成立而直接跳过该分支），驱动 sent 状态下 TxIncluded=false，断言 (a) BatchSweep 被第二次调用，(b) 第二次调用的 fakeBatchSweepCall.priorTxHash 等于第一次返回的 txHash（不是空串），(c) 第三次调用的 priorTxHash 等于第二次返回的 txHash（即内存跟踪优先于 ChannelRef）。再补一条重启语义：清空 o.sweepTx 后断言 priorTxHash 回落到 booking.ChannelRef。fakeBatchSweepCall 已经把 priorTxHash 记下来了，只差断言。
- **规则/文档要改**：docs/INVARIANTS.md:3244 附近 I-41 关于 priorTxHash/GasPrice 的段落需把 Pinned by 补上这条新测试——当前它引用的三条 chains/evm 测试只覆盖纯函数 quoteFee，不覆盖服务侧传参。
- **pin**：本条本身就是 pin 缺口：把 service/onchain.go:1862 的 txHash 实参改成 ""（即完整退回上一轮的 bug），当前测试套件全绿——新测试必须让这个改动变红。

### G-M2 [Major] watcher 扫到链尖（to = latest）无确认深度缓冲，reorg 换掉的块里的新充值永远看不到
- **位置**：`service/onchain.go:1213` · `service/onchain.go:1221` · `service/onchain.go:1252` · `service/onchain.go:584`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：在 scanChainOnce 里把扫描上界从 latest 改为 safeTip := latest - int64(cfg.Confirmations) + 1（不足则本 tick 直接返回，不推进游标），再套 maxBlocksPerScan 上限；ChainCursorLag 的上报改用 latest-to 保持不变（此时它自然包含确认深度，语义仍是「离链尖多远」，需在 core.Metrics.ChainCursorLag 的 doc comment 里说明基线值为 Confirmations 而非 0，否则告警阈值会误判）。processRegistrationRescan（:584-587）的 to 上限做同样处理。cfg.Confirmations 已是 per-chain 配置，无需新增字段。
- **规则/文档要改**：docs/INVARIANTS.md 新增：前向扫描游标永不越过 latest-Confirmations；core.ChainConfig.Confirmations 的 doc comment 补上「同时是扫描回退深度」这一新职责。
- **pin**：新增 service 测试：fakeChainReader.LatestBlock=1000、Confirmations=12，断言 FetchDeposits 收到的 toBlock ≤ 989 且 SetCursor 不超过 989（当前实现必红）。

### G-M3 [Major] SweepPolicy.GasCeiling 字段文档单位是 wei，实现比较的是 gwei，按文档配置则闸门被抬高 10^9 倍
- **位置**：`core/onchain.go:334` · `core/interfaces.go:584` · `chains/evm/sweeper.go:29` · `chains/evm/sweeper.go:93` · `service/onchain.go:1459`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **同源提示**：territory H：字段文档与实现的单位漂移属契约一致性
- **修法**：core/onchain.go:334 的 doc comment 把 (wei) 改成 (gwei)，并与 core/interfaces.go:584 的 GasPrice 措辞互相引用，写明「这两处必须同单位，改一处必须改另一处」。同时在 SweepPolicy.Validate()（core/onchain.go:345）加一条量级合理性检查：GasCeiling 为正且 > 1e6（gwei）时直接返回 ErrInvalidInput 并在错误文案里点名「疑似按 wei 配置」——把一个纯文档矛盾变成启动期机器拦截（working-agreements §5）。
- **规则/文档要改**：core/onchain.go:334 的 (wei) → (gwei)；docs/RUNBOOK.md:1374 提到调高 GasCeiling 处补单位。
- **pin**：新增 core 单测断言 SweepPolicy{GasCeiling: 50e9}.Validate() 返回 ErrInvalidInput；新增 chains/evm 单测用假 quoteFeeClient（baseFee/tip 已知）断言 Sweeper.GasPrice 的返回值等于 feeCapBasis 除以 1e9（把单位钉死在测试里）。

### G-M4 [Major] gas-bump 的 stuck 计时器从不重置，到点后每个 sweep tick 都 bump，重试预算按 Interval 而非 sweepStuckAfter 消耗
- **位置**：`service/onchain.go:1826` · `service/onchain.go:1862` · `service/onchain.go:1866` · `service/onchain.go:280`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：在 Onchain 里为每个 sweep booking 记一个 lastBumpAt time.Time（与 sweepTx/sweepBump 同一把 sweepMu，同一处 forgetSweepTx 清理），recheckSweepSent 的 stuck 判定改为 since := b.UpdatedAt; if t, ok := o.lastBumpAt[b.UID]; ok && t.After(since) { since = t }，再比 o.sweepStuckAfter；bumpSweep 时写入 lastBumpAt。这样每次 bump 之间真的间隔 sweepStuckAfter。另：recheckSweepSent:1849 的 GasCeiling 检查比较的是市价，拦不住 prior*1.125 那一支——同时把即将支付的费用暴露出来（core.Sweeper 增加一个返回下次 bump 报价的方法，或让 BatchSweep 在超过调用方给定上限时返回错误），使 GasCeiling 对重试路径也成立。
- **规则/文档要改**：WithSweepStuckAfter（service/onchain.go:280-283）的 doc comment 改成明确的「两次 gas-bump 之间的最小间隔，也是首次 bump 前的等待」；docs/RUNBOOK.md §15 的 Residual limitation 段补上这一条（目前只披露了「重启重置 bump 计数」）。
- **pin**：新增 service 测试：sweepStuckAfter=50ms、maxSweepBumps=3、连续跑 5 次 RunSweepOnce 且每次间隔 <50ms，断言 BatchSweep 只被追加调用 1 次（当前实现会是 4 次，必红）。
- **依赖**：G-M10

### G-M5 [Major] 广播成功但 Transition(sent) 失败则 tx hash 无处可寻，nonce 被占，该链归集停摆
- **位置**：`service/onchain.go:1769` · `service/onchain.go:1773` · `service/onchain.go:1806` · `chains/evm/sweeper.go:249`
- **主题**：other · **runtime**：go · **effort**：M
- **修法**：改成「先持久化意图，再广播」：在 advanceSweep 的 pending 分支里，BatchSweep 之前先把即将使用的 (chainID, nonce, targets 摘要) 写进 booking metadata（已有 nonce/addresses，缺的是「已进入广播窗口」的标记），广播成功后再 Transition(sent) 写 ChannelRef；若 Transition 失败，下个 tick 看到该标记时不得直接重播，而必须先向链求证该 nonce 是否已被自己的 EOA 用掉——为此在 core.Sweeper 增加 TxAtNonce(ctx, chainID, nonce) (txHash string, found bool, err error)（chains/evm 用 PendingNonceAt/NonceAt 比较 + 可选的 provider 交易列表实现；无法实现时返回 core.ErrUnsupported 并让该路径 fail-closed 停在 pending 并告警，而不是盲目重播）。至少要保证「nonce too low」这一类错误不再被当成普通失败无限重试。
- **规则/文档要改**：docs/RUNBOOK.md §15 的处置流程当前假设 booking 处于 sent 且 channel_ref 有值，需补一节「booking 停在 pending 且 channel_ref 为空」的取证与手工恢复步骤。
- **pin**：新增 service 测试：fakeBooker 让第一次 Transition(sent) 返回错误，断言第二个 tick 不再调用 BatchSweep（而是走求证/告警路径），且 BatchSweep 总调用次数为 1（当前实现是 2，必红）。

### G-M6 [Major] token config 查不到时 tc 零值静默关掉 AutoCreditCeiling 与对账两道闸
- **位置**：`service/onchain.go:764` · `service/onchain.go:834` · `service/onchain.go:428` · `core/onchain.go:250`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **同源提示**：territory D：M3 上限闸被绕过的路径清单
- **修法**：advanceConfirmation:764 改成带 ok 的查表：tc, ok := cfg.CreditTokens[booking.Metadata["token"]]；!ok 时不得继续 confirm，而应走 routeToReview 并新增 reviewReason 常量 reviewReasonTokenUnconfigured（沿用既有 review 机制，无需新状态）。理由：零值 TokenConfig 让 reviewGate:835/838 两条 IsPositive 判定同时为 false 直接放行，这正是 validateAutoCreditCeilings（:428）存在的理由所要否定的默认。同时把 reviewGate 的签名从接收 core.TokenConfig 改成接收 (tc core.TokenConfig, configured bool)，让「没配」在类型上不可能与「配成零」混同。
- **规则/文档要改**：core.TokenConfig.AutoCreditCeiling 的 doc comment 补一句：该闸只在 token 仍在 CreditTokens 内时可判定，token 下架后其在途 booking 一律进 review；I-34 或新增 invariant 记录这一点。
- **pin**：新增 service 测试：先以 AutoCreditCeiling=300 摄入一笔 10000 进 confirming，再用一个不含该 token 的 ChainSet 构造新的 Onchain 跑 RunPendingRecheckOnce，断言 booking 进 review 且 journal_uid 为空（当前实现会 confirmed 并入账 10000，必红）。

### G-M7 [Major] token decimals 只来自配置，从不与链上 decimals() 交叉校验，配错则静默按错误数量级入账
- **位置**：`chains/evm/reader.go:145` · `chains/evm/reader.go:163` · `chains/evm/scanner.go:71` · `chains/evm/logs.go:63` · `core/onchain.go:198`
- **主题**：other · **runtime**：go · **effort**：M
- **同源提示**：territory A：金额精度与 currency exponent 的关系（本条是它在链上侧的入口）
- **修法**：在 chains/evm 增加一个启动期只读校验：新增 func (cs *ClientSet) VerifyTokenDecimals(ctx) error，对每条链的 CreditTokens/SweepTokens 里每个非 native token 调一次 ERC-20 decimals()（把 decimals() 加进 erc20ABIJSON，chains/evm/multicall.go:24），与配置值比对，不一致返回错误、读不到（合约无该方法）则返回一个可区分的告警级错误由消费方决定。composition root 侧在 doc.go 与 README 里写明这是接线时必须调用的一步（本仓无 binary，无法替消费方强制）。另在 core.TokenConfig 增加 Validate()，拒绝 Decimals < 0 或 > 36，并由 service.Onchain.validateAutoCreditCeilings 同批调用（该函数已被 EnableOnchain 与 Run 双调用，是现成的启动闸挂载点）。
- **规则/文档要改**：core.TokenConfig.Decimals 的 doc comment 补上「必须与链上 decimals() 一致，用 ClientSet.VerifyTokenDecimals 在启动时求证」；docs/RUNBOOK.md 补一条「入账金额数量级异常」的排查项指向该校验。
- **pin**：新增 chains/evm 单测：假 client 让 decimals() 返回 6 而配置写 18，断言 VerifyTokenDecimals 返回错误；新增 core 单测断言 TokenConfig{Decimals:-1}.Validate() 报错。

### G-M8 [Major] manual 策略下的深 reorg 告警在 reorgRecheckWindow 之后自行静默，系统里不留任何持久痕迹
- **位置**：`service/onchain.go:1390` · `service/onchain.go:1404` · `service/onchain.go:1408` · `core/onchain.go:375`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **同源提示**：territory I：onchain 六个指标零告警零 RUNBOOK（上一轮 operability.md 同名发现）；territory F：handleReorg / recheckConfirmedDeposits 零测试
- **修法**：handleReorg 在 manual 分支 return 之前，必须把「检测到 reorg」这件事落盘而不只是 log+counter：复用既有的 DeadLetterRecorder 端口（service/onchain.go:60）或新增一个 core.ReorgRecorder（append-only 表，字段 booking_uid/chain_id/tx_hash/journal_uid/detected_at/last_seen_at），每次检测更新 last_seen_at，从不删除。recheckOneConfirmedDeposit:1390 的窗口早退保持不变（它只是成本上界），但一旦某 booking 已有 reorg 记录，就不受该窗口约束、继续复检直到人工结案。ledger-cli 增加一个只读子命令列出未结案的 reorg 记录。
- **规则/文档要改**：docs/RUNBOOK.md §12 需说明「告警会在 reorgRecheckWindow 之后停止重复，但记录不会消失，去 <新表/新命令> 取证」；core.ReorgPolicyManual 的 doc comment 补上「alert only」之外的「且落盘」。
- **pin**：新增 service 测试（当前该路径零覆盖）：confirmed booking 的 TxIncluded 变 false，跑 reorg recheck，断言落盘记录存在；再把 latest 推到窗口之外，断言记录仍在且仍被复检（当前实现连第一条都没有，必红）。

### B-m7 [Minor] 链上 watcher（scanChainOnce）是裸 runLoop 无 leader 选举，且 SetChainCursor 的注释自认「单调性是 service/ 层的 invariant」而 service/ 层没实现 → 多副本重复扫链、游标可回退
- **位置**：`service/onchain.go:1926` · `service/onchain.go:1200` · `service/onchain.go:1252` · `postgres/sql/queries/chain_cursors.sql:4` · `postgres/chain_cursor_store.go:53` · `postgres/sql/queries/checkpoints.sql:6`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **合并了**：G-m2（各自报告的论证都要读）
- **同源提示**：G(onchain-money-path)：同一函数 service/onchain.go:1246-1250 的 IngestDeposit 失败只 log 不中断、:1252 照样推进游标（fail-open，单副本下一笔充值永久跳过）——那条归 G，与本条叠加
- **修法**：两处一起改：(1) 给 SetChainCursor 加单调保护，与 UpsertBalanceCheckpoint（checkpoints.sql:6-18）已经在用的同款——ON CONFLICT (chain_id) DO UPDATE SET ... WHERE chain_cursors.last_scanned_block < EXCLUDED.last_scanned_block，并把注释里「this query does not enforce monotonicity -- that is an orchestration-layer invariant (service/)」删掉（改成 enforced here）。(2) 把 service/onchain.go:1926-1935 的 per-chain watch 循环包进 NewLockedJob(fmt.Sprintf("onchain_watch:%d", chainID), ...)，与同文件 :1952 的 newSweepLockedJob 同形，用同一个 o.pool。注意 LockedJob 取锁失败是 fail-closed（跳过该 tick），对扫链是正确语义。
- **规则/文档要改**：改 postgres/sql/queries/chain_cursors.sql 的 SetChainCursor 注释（sqlc 重生成）。若 docs/INVARIANTS.md 有链上扫描相关条目需同步声明「游标单调由 SQL 强制」。
- **pin**：两条：(a) 存储级——SetCursor(chain, 300) 之后 SetCursor(chain, 200)，断言读回仍是 300；去掉 WHERE 子句必须红。(b) 编排级——从独立连接占住 advisoryLockKey("job:onchain_watch:1")，跑一次 watch tick，断言 FetchDeposits 未被调用。

### B-m8 [Minor] chains/evm.Sweeper.lastFee 只写不删，按 (chainID, nonce) 无界增长，已确认 nonce 的条目在语义上已无用
- **位置**：`chains/evm/sweeper.go:45` · `chains/evm/sweeper.go:302` · `chains/evm/sweeper.go:286`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：在 recordFee 里顺手裁剪：记录新条目时把同链上低于某个水位的 nonce 条目删掉。水位可取「本次广播用的 signerNonce」——priorFeeFloor（:286-300）只会查当前 pending 的那个 nonce，比它更低的必然已确认或已被替换，删掉不影响任何读路径。实现上在 s.mu 保护下 for n := range s.lastFee[chainID] { if n < signerNonce { delete(...) } }。若担心 O(n) 扫描，改成记录 minTracked 并按需批量裁剪。
- **pin**：新增单元测试：连续 recordFee(chain=1, nonce=0..999)，断言 len(s.lastFee[1]) 有界（<= 一个小常数）；去掉裁剪必须红。

### F-m4 [Minor] 启动闸回归时 TestOnchain_Run_Rejects* 挂死而不是失败 —— 用了永不取消的 context.Background()，同文件的两条 Allows* 却正确预取消
- **位置**：`service/onchain_integration_test.go:1301` · `service/onchain_integration_test.go:1337` · `service/onchain_integration_test.go:1314` · `service/onchain_integration_test.go:1352` · `service/onchain.go:1974`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：两条 Rejects 测试改成先取消再调用，与同文件 :1314 / :1352 的 Allows 测试对齐：ctx, cancel := context.WithCancel(context.Background()); cancel(); err := h.svc.Run(ctx)。闸在 Run 的最开头（validateAutoCreditCeilings / validateReconcileFailureLimits，早于 runLoop），所以预取消不影响它们要断言的命题，但闸一旦回归，Run 会立刻从已取消的 ctx 返回而不是进四个 runLoop 永久阻塞 —— 失败从「整个 package 挂到 -timeout 后吐 500 行 goroutine dump 并连带干掉同 package 其它结果」变回一条干净的 require.Error。（我实跑过：把 core/onchain.go:251 改成恒 true，./service 直接挂到超时。）
- **pin**：把 core.TokenConfig.AutoCreditCeilingConfigured() 改成恒 true，这两条测试必须在秒级内以 require.Error 失败，而不是等 -timeout

### G-m1 [Minor] reader 里两处静默 continue：畸形日志与非白名单 token 的丢弃没有任何信号
- **位置**：`chains/evm/reader.go:147` · `chains/evm/reader.go:151`
- **主题**：verdict-unused · **runtime**：go · **effort**：S
- **修法**：给 Reader 注入 core.Logger（NewReader 增加可选参数或 option，默认 core.NopLogger()），两处 continue 各补一行 Warn（含 chainID/txHash/logIndex/原因）；并在 core.Metrics 增加 ChainLogSkipped(chainID int64, reason string) counter。同时把两处 continue 移到 seq 递增之后，使跳过的日志占位、seq 不随解码成败位移（与 G-C2 的修法必须一起做）。
- **pin**：chains/evm 单测：喂一条 topics 数不对的日志，断言 metrics 的 skipped counter +1 且其余日志的 TxLogSeq 不因它而位移。
- **依赖**：G-C2

- **G-m2** [Minor] chain_cursors 单调性的「由 service/ 保证」在 service/ 层并不存在，watcher 循环也没有单飞锁 — **[并入 B-m7]**

### G-m3 [Minor] probeMulticall 的探测结果永久缓存，一次瞬时假阴性会让该链整个进程生命周期退化到单查路径
- **位置**：`chains/evm/scanner.go:86` · `chains/evm/scanner.go:31`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：把 multicallKnown 的 value 从 bool 改成 {has bool; at time.Time}，只缓存 has==true 永久、has==false 缓存一个 TTL（如 10 分钟）后重探；或更简单：只在 has==true 时写缓存，false 不缓存（一次 CodeAt 的成本相对一轮 N 次 balanceOf 可忽略）。
- **pin**：chains/evm 单测：假 client 第一次 CodeAt 返回空、第二次返回非空，断言第二次 ScanBalances 走了 multicall 路径。

### G-m4 [Minor] TxIncluded 用 err == ethereum.NotFound 而非 errors.Is
- **位置**：`chains/evm/reader.go:180`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：改成 if errors.Is(err, ethereum.NotFound)。当前 ethclient 直接返回该哨兵值所以行为正确，但一旦上游包装（换 transport、加重试中间件）比较会失败，把「未找到」错分到 error 分支。就本仓语义而言 error 分支反而更安全（不会打 failed），所以这条本身是 Minor——但它与 G-M1 的判定强耦合，应一并收口。
- **pin**：chains/evm 单测：假 client 返回 fmt.Errorf("rpc: %w", ethereum.NotFound)，断言 TxIncluded 返回 (false, nil)。

### G-m6 [Minor] time.Since(b.UpdatedAt) 混用进程本地时钟与 DB 时钟
- **位置**：`service/onchain.go:1826`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：stuck 判定不要用本地 wall clock 与 DB 写入的 UpdatedAt 相减。两条路：(a) 判定下推到 SQL（BookingReader 增加一个「updated_at < now() - $interval」的过滤或返回 DB 端算好的 age），全程用 DB 时钟；(b) 若保留在 Go 侧，则在 Onchain 启动时测一次时钟偏移并在比较时扣除。建议 (a)。与 G-M4 的 lastBumpAt 一起改时，lastBumpAt 是进程内产生的量，可继续用本地时钟，但两者不能直接相减——需统一到同一个时钟域。
- **pin**：service 测试：把 fakeBooker 返回的 UpdatedAt 设成比本地时钟晚 10 分钟（模拟 DB 时钟超前），断言 stuck 判定不会立刻触发也不会永久不触发。
- **依赖**：G-M4

- **I-N14** [Minor] scanChainOnce 在 ingest 失败后照样推进游标，ChainCursorLag 回落到「健康」；非 ErrConflict 的失败连死信都不记（报告 I-14） — **[并入 G-C1]**

---

## W1-templates（Wave 1）

### D-C1 [Critical] dev_credit 模板不在 ProtectedTemplateCodes() 里，一个 write scope key 在任何 ENV 下都能铸造可支配可提现的无支撑余额
- **位置**：`presets/protected_templates.go:37` · `presets/devcredit.go:32` · `presets/devcredit.go:70` · `server/handler_journals.go:244` · `server/routes.go:112` · `ledger.go:659` · `docs/INVARIANTS.md:2792` · `README.md:886`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **合并了**：D-m9（各自报告的论证都要读）
- **同源提示**：上一轮 structure.md 的『write-scope key 就能铸造与真实充值不可区分的入账，TemplateCode 无 allowlist』Major 是本条父条目（8e503b3 的 M-2 只修了四个 deposit code）；H structure / E consumer-surface 可能独立命中『protected 集是手工名单』的契约面
- **修法**：三层同批做：① 把 presets.ProtectedTemplateCodes()（presets/protected_templates.go:37-44）从四个字面量改成派生实现——遍历本包所有 TemplatePreset，任一 TemplateLinePreset 的 ClassificationCode 指向 IsSystem=true 的 ClassificationPreset 即纳入受保护集（dev_credit 自动进来，将来新增 preset 也自动进来）；② 在 server/handler_journals.go:244 的 code 查表之后补一层结构性兜底：用 s.templates 取出 req.TemplateCode 的模板行，对每行的 classification 走 handlePostJournal 已有的同一段 is_system 判据（把 rejectSystemClassificationEntries 抽成按 classification uid 集合判断的私有函数供两处复用），这样部署方自定义的 system-side 模板也拦得住，且默认关闭由同一个 Config.AllowSystemClassificationPost 控制；③ ledger.go:659 的 InstallDevCreditPreset doc 明写「安装后 dev_credit 模板行常驻 DB，与 ENV 无关；POST /journals/template 的保护由 ProtectedTemplateCodes 承担」。
- **规则/文档要改**：I-38（docs/INVARIANTS.md:2698 标题与 :2792-2794 的结论段）措辞从 deposit-shaped accounting 扩为「任何触及 is_system 分类的模板」；README.md:886 的 cannot mint deposit-shaped accounting through either endpoint 同步；presets/protected_templates.go 包注释从「四个 deposit code」改为派生规则。
- **pin**：新增一条派生式门禁：遍历所有 preset bundle 安装出的 template，凡有 is_system 腿的 code 必须在 protected 集或显式 reviewed-exempt 名单里（落地当天应红于 dev_credit）；再加一条 httptest：write-scope key POST /journals/template template_code=dev_credit 必须 403。
- **依赖**：D-m9

### H-M3 [Major] inline（非 $ref）schema 逃逸全部三层契约门禁；POST /journals/{uid}/reverse 文档把 idempotency_key 标成 required 而 Go struct 根本不读
- **位置**：`docs/openapi.yaml:230` · `docs/openapi.yaml:236` · `server/handler_journals.go:50` · `server/openapi_contract_test.go:519` · `server/middleware_idempotency.go:64` · `core/interfaces.go:59`
- **主题**：gate-shape · **runtime**：docs · **effort**：L
- **同源提示**：E（consumer-surface）若报「reverse 的幂等键不可控」为同源；B 若从并发/重放角度独立命中
- **修法**：两步。①把 12 个 inline requestBody 全部提成具名 component 并注册进 requestBodySchemaCases：/journals/{uid}/reverse(:230)、/reverse-partial(:264)、/reservations/{uid}/settle(:474)、/settle-partial(:521)、/finalize(:563)、/release(:604)、/deposits/{uid}/review/reject(:775)、/balances/batch(:381)、/journal-types(:1028)、/currencies(:1148)、/reconcile/account(:1211)、/holder-tokens(:1457)；对应 Go struct 已全部存在（reverseJournalRequest、reverseJournalFractionRequest、settleReservationRequest、settlePartialReservationRequest、terminalReservationOpRequest、batchBalancesRequest、createJournalTypeRequest、reconcileAccountRequest、mintHolderTokenRequest 等），/balances/batch 与 /snapshots 那类函数局部类型顺手提到包级。②改 schemaRefIn 的语义：遇到无 $ref 的 inline schema 不再返回空串静默跳过，而是收进一个 unregisteredInline 列表，由 EveryRequestBodySchemaIsRegistered 直接 Fatal——inline 从「不可见」变成「显式失败」。③业务侧决定 reverse 的幂等语义：要么给 reverseJournalRequest 加 IdempotencyKey 字段并透传给一个新的 ReverseJournal 重载（与 ReverseJournalFraction 的 idempotencyKey 参数对齐），要么把 docs/openapi.yaml:236 的 required 改成 [reason] 并在 description 里写明键由服务端从 reversal:{uid}:{reason} 派生。倾向前者：core/interfaces.go:59 已经承认 AuthorizeReversal 必须由调用方复现同一个键，说明这个键本就该是消费方可见的。
- **规则/文档要改**：若选后者（不加字段），需在 api-contract.md §9 的「系统事件发起=派生确定性 key」项下补一句：库内派生键的端点必须在 spec 的 description 里写明派生公式，且不得把 idempotency_key 列入 required。
- **pin**：删掉任一新提取的 component 的注册项后 EveryRequestBodySchemaIsRegistered 必须红；再把 /journals/{uid}/reverse 的 requestBody 改回 inline，也必须红。
- **依赖**：H-M1

- **D-m9** [Minor] TestPostTemplate_DefaultProtectsDepositCodes 遍历被测实现自己的返回值，结构上不可能发现漏掉的危险模板（上一轮已在 grant_coverage 修掉的同源派生形状在 HTTP 层重开） — **[并入 D-C1]**

---

## W1-facade（Wave 1）

### B-M6 [Major] Service.Worker() 是唯一不守卫 s.tx != nil 的构造器：在 RunInTx clone 上调用会返回一个 expiration 用 tx-bound store、其余用 pool 的嵌合体，事务提交后过期回收永久静默失效
- **位置**：`ledger.go:817` · `ledger.go:839` · `ledger.go:857` · `ledger.go:349` · `ledger.go:340` · `ledger.go:390` · `ledger.go:731`
- **主题**：sibling · **runtime**：go · **effort**：M
- **同源提示**：E(consumer-surface)：RunInTx doc 的「clone 上被拒方法」清单不完整（Worker + VerifiedBalanceReader 都不在列）
- **修法**：给 Worker 加与三个兄弟同款的守卫。因为 Worker 不返回 error，两个选择：(a) 保持签名不变但改成 panic 不可取——改为在 s.tx != nil 时记一条 Error 日志并返回一个「空 Worker」不可取（正是 silent-default）；(b) **推荐**：把 Worker 的返回值改成 (*service.Worker, error)，s.tx != nil 时返回 core.ErrInvalidInput，错误文案照抄 EnableOnchain（ledger.go:731）的形状——「would be stitched from a transaction RunInTx discards when the callback returns」。这是破坏性签名变更，按 deployment.md expand→migrate→contract 走：先加 WorkerE(cfg) (*Worker, error) 并让 Worker(cfg) 转发（tx 模式下 Worker(cfg) 暂时 log.Error + 仍返回，一个 release 后收口）。另外 ledger.go:857 的 attestationServiceUnchecked 调用点也要一起守——它的 doc 声称调用方已建立 s.tx == nil 前提，而 Worker 并没有。
- **规则/文档要改**：改 ledger.go RunInTx 的 doc comment（:459-482 的「在 clone 上被拒的方法」列表补上 Worker）；README 的 API surface 表（readme_api_surface_test.go 门禁）随签名变更同步。
- **pin**：新增测试：svc.RunInTx 回调里调 tx.Worker(...)，断言拿到 core.ErrInvalidInput（或过渡期断言日志里有该 Error）；再加一条端到端：预插一条 expires_at 已过期的 reservation，用 clone 上构造的 Worker 跑 1s，断言该 reservation 仍是 active 且过程有可观测错误——这条用来钉住「静默失效」这个具体后果，删掉守卫必须红。

### E-M1 [Major] 默认 core.NopLogger() 吞掉 worker 的全部可观测信号；按 README Quick Start 接线的消费方零输出、事件永远 pending
- **位置**：`ledger.go:148` · `core/logger.go:11-18` · `service/worker.go:251-267` · `service/worker.go:201` · `service/worker.go:419` · `README.md:161-164` · `README.md:180` · `examples/fullstack/backend/main.go:87`
- **主题**：silent-default · **runtime**：go · **effort**：M
- **合并了**：I-M11, C-R3（各自报告的论证都要读）
- **修法**：三处一起改。(1) service/worker.go 的 Run 启动报告、Subscribe-after-Run 的 Error、runLoop 的 interval<=0 Warn 这三条属于『接线错误』而非『业务日志』，不能只走注入的 core.Logger —— 把它们改成同时经 s.logger 与一个不可关闭的兜底通道（包级 log/slog default，或 core 新增 Logger 的 fallback 包装 core.LoggerOrDefault(l)），使未注入 logger 的消费方仍能看到。(2) 更强的做法（推荐、与 working-agreements §3 一致）：把『Subscribe 在 Run 之后』从 Error 日志升级为 Worker.Run 的启动期 error 返回，把『配了 Attestor 但 anchor 为 nil』做成 Worker.Run 的一次性 Warn + 一个 core.Metrics 计数器（metrics 也默认 Nop，所以仍需 (1)）。(3) README:161-164 的 Quick Start 代码块里直接带上 ledger.WithLogger(...)，并在 README:180 把『observability 是 opt-in』改成『不注入 logger 你将看不到 worker 报告哪些 job 没开』；examples/fullstack/backend/main.go:87 的 ledger.New(pool) 补上 WithLogger(slog 适配器)。
- **规则/文档要改**：docs/INVARIANTS.md I-40 增补一条子命题，或新开一条：『一个被跳过的可选 job 与一个在跑的 job，在默认配置（未注入 Logger/Metrics）下必须可区分』。README:180 措辞需改。
- **pin**：新增 root 包测试：ledger.New(pool)（不传 WithLogger）+ svc.Worker(DefaultWorkerConfig()) + Run，捕获进程 stderr，断言出现一行含 event_delivery_webhook=false / full_reconcile=false 的启动报告；删掉兜底通道该测试必红。

### E-M3 [Major] tx.RegisterChannel 在 RunInTx clone 上返回 nil（报成功）后被静默丢弃 —— 正是 EnableOnchain 的 clone 闸要防的形状
- **位置**：`ledger.go:677-693` · `ledger.go:569` · `ledger.go:607` · `ledger.go:730-733`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：在 (*Service).RegisterChannel 开头加与 EnableOnchain 同形的守卫：if s.tx != nil { return fmt.Errorf("ledger: RegisterChannel: called on a transaction-bound store; the registration would be written to a clone RunInTx discards when the callback returns, leaving the top-level Service without it despite this call reporting success -- call RegisterChannel on the top-level Service: %w", core.ErrInvalidInput) }。顺带把 withTx 里 channels 的 maps.Clone 注释补上『clone 上的 channels 只读』。
- **规则/文档要改**：docs/INVARIANTS.md I-40 第 4 条的方法枚举需加入 RegisterChannel（见 E-M5 的机械化建议）。
- **pin**：新增 TestService_RegisterChannel_RefusedOnTxBoundClone：先在顶层 Service 上证明同一次注册会成功，再证明从 RunInTx 回调里调用返回 error 且 svc.Channels() 不含它。删掉守卫必红。
- **依赖**：E-M5

### E-M4 [Major] tx.Onchain() 在 clone 上静默返回 nil，即使顶层 Service 已 EnableOnchain（withTx 未携带 onchain 字段）
- **位置**：`ledger.go:767` · `ledger.go:565-608` · `ledger.go:576-585`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：在 withTx 的返回 struct 里补 onchain: s.onchain，与已经这么做的 attestor/authVerifier 保持一致（ledger.go:576-585 已经为后者写了完整理由，同一理由逐字适用）。注意这不会让 clone 变得能改 onchain —— EnableOnchain 的 s.tx 守卫仍然拒绝写；这里只修复『读』。若决定不携带，则必须反过来让 Onchain() 在 s.tx != nil 时 panic 或返回一个明确报错的空实现，不能返回裸 nil（消费方 tx.Onchain().IngestDeposit(...) 会 nil panic）。
- **规则/文档要改**：I-40 第 4 条需要从『哪些方法被拒绝』扩成『clone 上每个 accessor 的行为都被声明』。
- **pin**：新增 TestService_Onchain_VisibleOnTxBoundClone：svc.EnableOnchain(...) 后在 RunInTx 回调里断言 tx.Onchain() != nil 且与 svc.Onchain() 同一实例。从 withTx 删掉该字段必红。
- **依赖**：E-M5

### E-M5 [Major] tx.Worker(cfg) 无 clone 闸，且内部自动构造出 tx.AttestationService() 刚刚拒绝掉的同一个 pool-bound 对象
- **位置**：`ledger.go:817-859` · `ledger.go:855-857` · `ledger.go:354-358` · `ledger.go:340-342` · `ledger.go:924-935`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **修法**：(1) 给 (*Service).Worker 加 s.tx 守卫。因为 Worker 的签名是 *service.Worker 无 error，两个选择：改签名为 (*service.Worker, error)（破坏性，但与 AttestationService 一致），或直接 panic 并在 godoc 声明。倾向前者 + 在 CHANGELOG 记一条 Breaking。(2) 更根本的修法（推荐，一次解决 E-M3/E-M4/E-M5/E-m14）：把 I-40 第 4 条从人工枚举改成机械派生 —— 新增一个 root 包测试，用 go/ast 扫 ledger.go 里所有导出的 *Service 方法，凡函数体内出现 s.pool 或写 s.<field> 的，必须要么命中 s.tx != nil 守卫，要么在 godoc 里出现约定标记（如 // clone-safe: 理由）。当前会命中 Worker / Ping / Pool / RegisterChannel。
- **规则/文档要改**：docs/INVARIANTS.md I-40 第 4 条改写：从『AttestationService/VerifyLedger/EnableOnchain 三者被拒绝』改成全称命题『任何在 s.tx != nil 时仍触碰 s.pool 或写 Service 字段的导出方法，要么被拒绝，要么在 godoc 显式声明其 clone 行为』。
- **pin**：(a) TestService_Worker_RefusedOnTxBoundClone；(b) 上面那条 go/ast 机械门禁 TestCloneEscapeSurfaceIsDeclaredOrGuarded —— 后者是真正的护栏，删掉任一守卫都会红，且新增一个碰 s.pool 的导出方法时会自动红。

### E-M8 [Major] 库模式重试契约只接了一半：core.IsRetryable 有了，但『重试必须复用同一个 idempotency key』只写在 docs/api.md（HTTP 侧），18 处示范都在调用点内联生成 key
- **位置**：`core/errors.go:65-118` · `idempotency.go:9-25` · `docs/api.md:135` · `README.md:110` · `README.md:136` · `README.md:146` · `README.md:153` · `docs/COOKBOOK.md:168` · `docs/COOKBOOK.md:238` · `docs/COOKBOOK.md:252` · `docs/COOKBOOK.md:258`
- **主题**：sibling · **runtime**：docs · **effort**：M
- **修法**：(1) idempotency.go:9-16 的 NewIdempotencyKey godoc 补一段红线：『生成一次，在重试循环之外。在重试路径内重新生成等于关闭幂等 —— 见 api-contract.md §9』，并把 godoc 的 Convention 示例改成显式的两步（key := ...; 循环里复用 key）。(2) README 新增一小节『Retrying a failed write』：core.IsRetryable(err) + 复用同一个 key，给一段完整的重试循环示例；放在 API Surface 的 Infrastructure helpers 附近。(3) 把 README:110/136/146/153 与 COOKBOOK:168/238/252/258 八处内联生成改成先赋值给变量再传入（credits-topup:179-180 与 billing:142-143 已经是正确形态，照抄它们）。examples 里剩余 10 处同理。
- **规则/文档要改**：docs/INVARIANTS.md I-3 增补一句：幂等键的生命周期属于契约的一部分 —— 『由发起方生成一次、跨重试复用』，与 ~/.claude/rules/api-contract.md §9 对齐；该规则对库模式与 HTTP 模式同等适用。
- **pin**：新增 docs 门禁：grep README.md + docs/COOKBOOK.md + examples/**/main.go，断言不存在 `IdempotencyKey: ledger.NewIdempotencyKey(` 这种内联形态（必须先赋值给变量）。这条门禁可证伪、且下次有人内联写会立刻红。

### F-M1 [Major] ledger.Service.Worker() 的 SetPartitionService / SetPool 两行接线没有任何 pin，删掉全套测试仍全绿；SetPool 缺失是静默 fail-open
- **位置**：`ledger.go:847` · `ledger.go:848` · `service/locked_job.go:82` · `service/worker.go:169` · `service/worker.go:299` · `service/worker.go:312` · `service/worker.go:334` · `service/worker.go:348` · `service/worker.go:362` · `service/worker.go:379`
- **主题**：sibling · **runtime**：go · **effort**：S
- **合并了**：F-P13（各自报告的论证都要读）
- **同源提示**：consumer-surface [Major] 按 README Quick Start 逐字接线的消费方看不到 worker 任何输出（同一 facade 接线面，不同后果）；concurrency [Major] Service.Worker() 是唯一不守卫 s.tx != nil 的构造器（同一函数，不同缺陷）；operability [Major] I-10 三个后台 job 家族无任何指标（同一批 job，观测面）
- **修法**：仿照 ledger_subscribe_wiring_test.go / ledger_test.go:212 已有的 facade-path 形状，在根包新增两条只走 ledger.New(pool)+svc.Worker(cfg) 的 pin：(1) 分区：把 journal_entries 的月分区地平线人为缩到过去（或直接删掉未来月的分区），起 worker、require.Eventually 断言 pg_class 里出现了新的 journal_entries_yYYYYmMM 分区；(2) 锁：断言 svc.Worker(cfg) 返回的 Worker 上 LockedJob 确实带 locker —— 由于 Worker.pool 未导出，最省的做法是给 service.Worker 加一个测试可见的 HasPool() bool（或在 service 包内加 export_test.go 暴露），然后在根包断言 svc.Worker(cfg) 的它为 true；更强的做法是并发起两个 svc.Worker(cfg).Run(ctx)，断言某个 LockedJob 每 tick 只执行一次。顺带把 service/worker.go:169 SetPool 的 doc comment 从「the reconcile and system_rollup jobs」改成实际的六处 LockedJob。
- **规则/文档要改**：建议给 I-39（advisory-lock coordination is structurally safe）补一句：facade 装配出的 Worker 必须持有 pool，并把上述两条 pin 加进 I-39 的 Pinned by；I-13 的 Pinned by 加分区那条
- **pin**：删掉 ledger.go:847 或 :848 任意一行，新 pin 必须变红（当前两行都可删而 go test ./... 全绿，已实跑）

### F-M2 [Major] 提现闸的 AuthVerifier facade 接线没有 pin —— I-32/I-33 的六条 pin 全部由测试自己把 verifier 塞进 store
- **位置**：`ledger.go:166` · `ledger.go:167` · `postgres/verified_balance_store.go:146` · `postgres/verified_balance_pin_test.go:292` · `postgres/verified_balance_pin_test.go:336` · `postgres/reserve_verified_balance_guard_test.go:42`
- **主题**：sibling · **runtime**：go · **effort**：S
- **合并了**：F-P32（各自报告的论证都要读）
- **同源提示**：operability [Major] I-11 库默认配置把诊断关掉 / ledger-cli 走 ledger.New(pool) 导致 s.authVerifier==nil（同一「facade 没接 verifier」家族，命中的是 CLI 出口）；tamper-evident [Critical] C-1 提现闸只验签名、放款金额取自可写的 balance_checkpoints（同一个闸，正交缺陷 —— 修 C-1 时应连带做这条 pin）
- **修法**：在根包新增一条 facade-path pin：svc, _ := ledger.New(pool, ledger.WithAttestor(attestor, verifier)) → 用 svc.JournalWriter().PostJournal 发一笔已签名 journal → svc.Reserver().Reserve(core.ReserveInput{RequireVerifiedBalance: true, ...}) 必须成功；再用同一个 pool 造一条 forged/未签名 journal，同一调用必须 ErrorIs(core.ErrUnauthorizedJournal)。关键是全程不出现 postgres.NewVerifiedBalanceStore —— 断言对象是 ledger.go:166 那一行，不是 store 的逻辑。同时补一条 svc.VerifiedBalanceReader().VerifiedBalance(...) 在 WithAttestor 下不返回 ErrUnauthorizedJournal 的正向断言。
- **规则/文档要改**：I-32 的 Pinned by 增列这条 facade pin，并在正文点明「闸的 verifier 由 ledger.WithAttestor 经 ledger.go:166 注入」这一步本身在保护范围内
- **pin**：把 ledger.go:166 的 s.authVerifier 换成 nil，新 pin 必须变红（当前全套绿，已实跑）

- **I-M11** [Major] 库默认全黑（NopLogger+NopMetrics）且 SetFullReconciler 不自动接线；关掉的正是那个本该告诉你其余配置也关着的机制（报告 I-11） — **[并入 E-M1]**

### I-R1 [Major] 上轮复核：EventStore.SetLogger 为修上轮 Minor 而加，却零生产调用点，行为一分未变（报告 R-1）
- **位置**：`postgres/event_store.go:79` · `postgres/event_store.go:91` · `ledger.go:169` · `ledger.go:835`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **合并了**：B-m1（各自报告的论证都要读）
- **同源提示**：上轮 TODO §6 Minor『event_store 的 claim lost 用包级 slog，消费方注入的 logger 看不到』——修复加了 setter 但从未接线
- **修法**：在 ledger.New（ledger.go:169 建 s.eventStore 处）与 Service.Worker（ledger.go:835 建 eventPoller 处，紧挨着已有的 SetClaimLease）各加一行 .SetLogger(s.logger)。更彻底的修法是消除「有 setter 没人叫」这个形态本身：把 logger 并进 NewEventStore 的参数或改成 options 变参，让漏接线在编译期就不可能。这是 C1「机制存在、示例存在、doc 存在，唯独没接到真实路径」在一次修复内部的复现。
- **规则/文档要改**：postgres/event_store.go:73-78 的 doc 里「Until this is called, warn falls back to slog.Default() -- the historical behavior, unchanged, so not calling this is not a regression」在接上之后必须删掉——这句话现在是准确的，而这恰恰是问题。
- **pin**：新增测试：用注入的 recording logger 构造 Service→Worker，用过期 claim token 调 MarkDelivered 触发一次 claim-lost，断言那行 Warn 落在注入的 logger 上而不是 slog.Default()。删掉任一处 SetLogger 接线则红。

- **B-m1** [Minor] EventStore.SetLogger 全仓零调用点：claim-lost 三条警告仍落 slog 默认 handler，配了 ledger.WithLogger 的消费方看不到 — **[并入 I-R1]**

- **B-m11** [Minor] mergeWorkerConfig 是「每字段一个 if」的形状，加第十七个字段照样会漏（上轮 AttestInterval/AttestBatchSize 就是这么漏的），零值 = 该 job 静默不跑 — **[并入 E-m17]**

- **C-R3** [Minor] svc.Worker 自动接线的 attestation 默认无锚，唯一提示是一条会落进默认 NopLogger 的 Warn — **[并入 E-M1]**

### E-m1 [Minor] ledger.go 的 InstallExtendedPresets godoc 列了 7 个 bundle，实际装 8 个（漏 FX）
- **位置**：`ledger.go:638-641` · `presets/templates.go:344-353`
- **主题**：sibling · **runtime**：docs · **effort**：S
- **修法**：ledger.go:638-639 的 godoc 在 settlement 与 spread 之后补上 FX，改成八个名字；顺带把数字 8 写进去，与 README:129/265/284/661 对齐。
- **pin**：新增 presets 包测试：从 InstallExtendedPresets 的 bundles slice 反射出 bundle 数与名字，断言与 README『All N preset bundles』的 N 及 facade godoc 一致；三处任一漂移即红。

### E-m14 [Minor] 被保留到回调之外的 RunInTx clone 上，Ping() 与 Pool() 仍走 s.pool 并报健康，而数据面已 tx is closed
- **位置**：`ledger.go:924-935` · `ledger.go:191` · `ledger.go:445-447`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：最小修法：在 RunInTx 的 caveat 列表（ledger.go:445-447 与 :477-481 附近）补一条 —— 『Ping 与 Pool 在 clone 上仍走连接池：一个被误留的 clone 会继续报连接健康，而它的每一次读写都会返回 tx is closed』。更彻底的修法（与 E-M5 的机械门禁配套）：给 Ping 加 s.tx != nil 时改用 s.tx 探活，或让它在 clone 上返回一个明确说明这是 clone 的 error。
- **规则/文档要改**：I-40 第 4 条随 E-M5 一起改成全称命题后，Ping/Pool 属于『已声明的 clone-safe 例外』。
- **pin**：随 E-M5 的 TestCloneEscapeSurfaceIsDeclaredOrGuarded 覆盖：Ping/Pool 必须带 // clone-safe: 注释才放行。
- **依赖**：E-M5

### E-m15 [Minor] RegisterChannel 的 godoc 说『Call before starting the HTTP server』，真实约束更早（必须在 svc.Channels() 被求值之前）
- **位置**：`ledger.go:675` · `ledger.go:697-705` · `examples/fullstack/backend/main.go:231`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：godoc 改成『Call before you pass svc.Channels() into the HTTP layer — server takes a snapshot of the map (see Channels), so a registration made after that point is invisible to it, even before ListenAndServe.』同时 Channels() 的 godoc 里把『返回的是 copy』这件事的后果点明。
- **pin**：server 包测试：NewFromDeps 之后再 svc.RegisterChannel(newAdapter)，断言 POST /api/v1/webhooks/<newName> 返回 404 —— 把当前行为钉成契约，防止有人以为它会生效。
- **依赖**：E-M3

### E-m17 [Minor] mergeWorkerConfig 仍是手写的 16 字段 if 列表，加新 WorkerConfig 字段无任何机制强制同步（上一轮 Attest 两项漏项的根因未变）
- **位置**：`ledger.go:865-916` · `service/worker.go:16-49` · `service/worker.go:52-71`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **合并了**：B-m11（各自报告的论证都要读）
- **修法**：(1) 用 reflect 重写 mergeWorkerConfig：遍历 service.WorkerConfig 的字段，凡是零值就从 DefaultWorkerConfig() 的同名字段取（对 time.Duration / int / int32 都成立，当前 16 个字段全是这三类）。这样加字段自动生效，if 列表消失。(2) 若不想引 reflect，退而求其次：新增测试（见 pin）。
- **pin**：新增 root 包测试 TestMergeWorkerConfig_FillsEveryField：反射遍历 service.WorkerConfig 的每个字段，对 service.WorkerConfig{} 跑 mergeWorkerConfig，断言每个字段都非零且等于 DefaultWorkerConfig() 的对应值。加一个新字段而忘了 merge 时自动红 —— 这正是上一轮 AttestInterval 漏掉时缺的护栏。

### E-m21 [Minor] Worker.Subscribe 对 w.localDeliverer 是无同步写，Run 对同一字段是读；running 是 atomic 但被保护的字段不是
- **位置**：`service/worker.go:198-206` · `service/worker.go:399` · `service/worker.go:100-105`
- **主题**：other · **runtime**：go · **effort**：S
- **同源提示**：B: goroutine 与共享字段的同步；worker claim/lease
- **修法**：给 Worker 加一个 sync.Mutex（或把 localDeliverer 换成 atomic.Pointer），Subscribe 与 Run 读写该字段时都持锁。文档已经要求『Subscribe 必须在 Run 之前』，但违反时的后果应该是一条确定的 error（见 E-M1 的 (2)），而不是一个数据竞争。
- **pin**：go test -race：新增测试并发调用 Subscribe 与 Run，当前应报 race；加锁后绿。
- **依赖**：E-M1

- **F-P13** [Minor] 附录 pin 表判定【部分】I-13：三条 pin 全在 store 层，service 层 EnsureUpcoming 与 facade 接线整条可删而全绿 — **[并入 F-M1]**

- **F-P32** [Minor] 附录 pin 表判定【部分】I-32 / I-33：store 层是真证伪，但 facade 的 verifier 接线可删而全绿 — **[并入 F-M2]**

---

## D-tamper（Wave 2）

### C-M1 [Major] VerifyLedger 第 4 步采样的是最旧的 N 条 journal（id>0 ORDER BY id ASC），不是文档四处声称的最新
- **位置**：`service/attest_verify.go:399-400` · `service/attest_verify.go:73-75` · `service/attest_verify.go:454-458` · `postgres/query_provider.go:88-108` · `postgres/sql/queries/journals.sql:96-100` · `postgres/convert.go:535-542` · `cmd/ledger-cli/main.go:318`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **同源提示**：test-credibility 会独立命中「测试用新库、样本量等于全量，使顺序错误结构上不可见」
- **修法**：在 postgres/sql/queries/journals.sql 新增 ListRecentJournals（SELECT * FROM journals ORDER BY id DESC LIMIT sqlc.arg(page_limit)::int），在 core.QueryProvider 上以新增方法（不是改 ListJournals 签名，按 deployment.md expand）暴露 ListRecentJournals(ctx, limit)，QueryStore 实现之，VerifyLedger step 4 改调它；同步修正 service/attest_verify.go:73、:399、:455 与 cmd/ledger-cli/main.go:318 四处 most recent 文案使其与实现一致。若拍板保留升序采样，则必须反过来把四处文案改成 oldest，并在 VerifyReport 里新增字段说明该步只覆盖了历史前缀（不能让读者以为它看的是活跃数据）。
- **规则/文档要改**：给 core.QueryProvider 加方法属于 port 扩展，走 expand（加方法不改既有签名）。I-26 / I-32 中描述 VerifyLedger step 4 采样范围的措辞需与实际方向一致。
- **pin**：新增 service.TestVerifyLedger_SamplesTheNewestJournalsNotTheOldest：先正常 post 25 条合法签名 journal，再直接 SQL 插一条无签名伪造 journal（id 最大），不跑 attest，断言 report.Status != VERIFIED 且 reasons 含 carry no signature。当前实现下该测试报 VERIFIED 即证伪成功。

### C-M2 [Major] VerifyLedger 缺少设计 §8.4 step 3 明写的「LEFT JOIN 找未覆盖 entry」检查，未被 attestation 覆盖的伪造 entry 报 VERIFIED
- **位置**：`service/attest_verify.go:130-397` · `service/attestation.go:37-41` · `service/attestation.go:137` · `postgres/sql/queries/integrity_attestations.sql:54-71` · `docs/INVARIANTS.md:1749-1758`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **修法**：在 VerifyLedger 链走完之后加 step 3b：VerifyConfig 新增 UncoveredProbeLimit（默认 1000），调 store.UncoveredEntries(ctx, limit)；返回非空时按其 journal 的 auth_status 分类 —— 存在未覆盖且 auth_status != signed 的 entry 则 tampered()（这正是直接 SQL 插入的伪造行的形状）；未覆盖但全部已签名则记入 VerifyReport 新增字段 UncoveredEntries int64 并在数量达到 limit 时降级为 DRIFT 并给出 reason（说明可能被截断）。AttestationStore 接口无需新方法，复用已有的 UncoveredEntries。
- **规则/文档要改**：I-27 的 Enforced by 段需补上这条真正回答「every entry covered」的检查 —— 当前列出的三项（RunAttestBatch 写侧、entry_attestations 的 PRIMARY KEY 防重复覆盖、ListUncoveredEntries 这条 SQL 存在）没有一项回答它，而 VerifyLedger 被列为执行者但只执行了前半句。
- **pin**：新增 service.TestVerifyLedger_FlagsUncoveredUnsignedEntry：先 RunAttestBatch 覆盖全部历史，再直接 SQL 插伪造 journal 且不再 attest，断言 VerifyLedger 不返回 VERIFIED。

### C-M3 [Major] 锚为空或回退被判为 DRIFT（文档定义为良性）且 CLI 退出码 0，抹掉锚即可静默关闭全部外部校验
- **位置**：`service/attest_verify.go:395-397` · `service/attest_verify.go:465-467` · `service/attest_verify.go:382-384` · `service/attest_verify.go:25-29` · `cmd/ledger-cli/main.go:349-351` · `anchordev/local_file.go:94-98` · `anchors/r2/r2.go:190-200`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **修法**：四处一起改：一、VerifyLedger 在 anchorSeq==0 && maxSeqSeen>0 时直接返回 VerifyStatusNotRun，reason 写明「anchor reports empty while the DB chain has N attestations」，不再落入 DRIFT 分支（同 §8.4 的 NOT_RUN != VERIFIED 红线）；二、DRIFT 只保留 anchorSeq>0 且落后有限个 seq 这一种情形；三、把上次观测到的 anchorSeq 持久化到一张 append-only 表（形状参考 checkpoint_rebuilds），VerifyLedger 发现本次 anchorSeq 低于上次记录值时报 TAMPERED 而不是 DRIFT；四、cmd/ledger-cli/main.go:349 把 DRIFT 也纳入非零退出，或加 --fail-on-drift 并在 RUNBOOK 里要求定时任务必须带该 flag。
- **规则/文档要改**：I-28 需补「锚为空 != 锚落后」这条区分；service/attest_verify.go:25-29 对 VerifyStatusDrift 的 doc 当前把 DRIFT 定义为 benign, expected inconsistency，必须改。
- **pin**：新增 service.TestVerifyLedger_EmptyAnchorWithNonEmptyChainIsNotRun：建好 N 条 attestation 链后删掉 anchor 文件，断言 Status==NOT_RUN 而不是 DRIFT；再加一条 cmd 层断言 verify 对该状态退出码非 0。
- **依赖**：C-m2

### C-M4 [Major] R2 载体的可信 head 是单个可变 key，不可变性只存在于没有任何代码会读的对象版本里，ledger 侧 PutObject token 足以改写它
- **位置**：`anchors/r2/r2.go:79-90` · `anchors/r2/r2.go:176-184` · `anchors/r2/r2.go:190-217` · `docs/RUNBOOK.md:978-989`
- **主题**：verdict-unused · **runtime**：go · **effort**：L
- **合并了**：F-m11（各自报告的论证都要读）
- **同源提示**：threat-model 会命中 ledger 侧 R2 token 与 DATABASE_URL 的失效域关系
- **修法**：改为 one-object-per-seq：Publish 写 <Key>/seq-<N>.json，PutObjectInput 带 IfNoneMatch 星号做条件写（已存在则 GetObject 读回比对字节，一致则幂等成功、不一致则报错）；Head 用 ListObjectsV2 以 <Key>/ 为前缀取 seq 最大的对象，而不是读单 key 的当前版本。若拍板维持单 key，则 Head 必须改走 ListObjectVersions 并取「seq 最大的那个版本」而不是最新版本。同时改 docs/RUNBOOK.md:982-989 —— 当前那段把「versioning 保留了审计轨迹」写成了性质二的兑现，而没有任何验证路径读版本历史，措辞要降级为「事后人工取证可用」。
- **规则/文档要改**：core.Anchor 的 Head doc 契约需加一条 MUST：返回历史上发布过的最高 seq，而不是最后一次写入的值。
- **pin**：anchortest 新增 phase HeadNeverRegressesAfterAnOutOfBandOlderWrite —— 对被测实现用其原生客户端（r2 用裸 s3.PutObject）绕过 Publish 写入一个更旧的 seq，断言 Head 仍返回更高的那个。
- **依赖**：C-m2

### C-M5 [Major] 密钥轮换在库自带 verifier 下结构上不可能（只持一把 key、无注册接口），轮换会让提现闸对全体存量持有人永久 fail-closed
- **位置**：`authdev/ed25519.go:86-106` · `authdev/ed25519.go:96-103` · `core/auth.go:471-477` · `postgres/verified_balance_store.go:193-195` · `cmd/ledger-cli/main.go:316-317` · `cmd/ledger-cli/main.go:337` · `docs/INVARIANTS.md:3821-3831`
- **主题**：other · **runtime**：go · **effort**：M
- **修法**：三件：一、authdev 新增 NewLocalVerifierSet(keys map[string]ed25519.PublicKey) (*LocalVerifier, error)，或给 LocalVerifier 加 Register(keyID string, pub ed25519.PublicKey) 并在 doc 里限定只能在启动阶段调用（Verify 侧读 map 需与之配套加锁或要求构造后不可变）；NewLocalVerifier 保留为单 key 便捷构造。二、cmd/ledger-cli 的 --pubkey-hex/--key-id 改成可重复 flag（flag.Var 收集 keyID:hex 对），构造多 key verifier。三、docs/RUNBOOK.md 新增「P5 签名密钥轮换」小节：旧公钥必须永久保持注册、注册前 RequireVerifiedBalance 的提现会对全体存量用户拒绝、以及轮换不缩小已泄漏私钥的攻击面（authdev 无 NotAfter，该实现自己已声明 a production verifier should）。
- **规则/文档要改**：I-45 给操作员的处置建议「register the key to restore verification coverage」在实现该 API 之后才真正可执行，Enforced by 段应指向新的多 key 构造；design doc §13 写的「轮换走 runbook」需要 runbook 真的存在。
- **pin**：新增 authdev.TestLocalVerifier_MultiKeyVerifiesBothGenerations 与 postgres.TestVerifiedBalance_SurvivesKeyRotationWhenOldKeyStillRegistered（同一维度上混有旧 key 与新 key 签的 journal，断言 VerifiedBalance 成功；只注册新 key 时断言 UNDEFINED）。

### C-M6 [Major] ledger-cli verify 只能接 anchordev.LocalFileAnchor，而 RUNBOOK 指定的生产载体是 R2，按 RUNBOOK 部署后没有随库交付的工具能做验证
- **位置**：`cmd/ledger-cli/main.go:45` · `cmd/ledger-cli/main.go:313-352` · `docs/RUNBOOK.md:894-905` · `docs/RUNBOOK.md:962-968` · `anchordev/local_file.go:1-12`
- **主题**：other · **runtime**：go · **effort**：M
- **合并了**：I-N20（各自报告的论证都要读）
- **同源提示**：structure-and-contract 会命中 module 拆分的连带后果；operability 会命中 cmd/ledger-cli 的取证能力缺口
- **修法**：二选一需拍板：(a) 把 verify 子命令拆到一个可以依赖 anchors/r2 的位置（新建 anchors/r2/cmd/ledger-verify，或一个独立的 tools module），保留 cmd/ledger-cli 的其余只读取证命令不动；(b) CLI 结构不动，但改 docs/RUNBOOK.md:962-968 删掉把 ledger-cli verify 列为 R2 只读 token 消费者的说法，改为在 examples/ 下提供一个可运行的 verify-with-r2 示例（消费方 composition root 里调 svc.VerifyLedger(ctx, r2Anchor, cfg)），并在 cmd/ledger-cli verify 的 --anchor-file 帮助文本里写明「本命令只支持 local-file anchor，不是生产验证入口」。倾向 (b)+示例，因为 (a) 会把 AWS SDK 引入发布物。
- **规则/文档要改**：docs/RUNBOOK.md「Choosing an Anchor carrier」性质三里「ledger-cli verify 必须能取到可信 head」这句在当前结构下对 R2 不成立，措辞必须改。
- **pin**：新增一条静态断言测试：cmd/ledger-cli verify 的帮助文本必须包含 local-file 限定语；若走 (b) 则再加一条 examples 编译门禁（examples/verify-r2 能 go build）。

### C-M7 [Major] journals 可插入 total_debit=total_credit=NaN（实测通过全部 CHECK），mustNumericToDecimal panic 掉验证读侧与整个 worker 进程
- **位置**：`postgres/sql/migrations/001_baseline.up.sql:316-317` · `postgres/sql/migrations/001_baseline.up.sql:332` · `postgres/convert.go:36-58` · `postgres/convert.go:195-196` · `postgres/query_provider.go:88-108` · `service/attest_verify.go:400` · `service/reconcile.go:1445` · `service/worker.go:249-400`
- **主题**：other · **runtime**：sql · **effort**：L
- **同源提示**：financial-correctness：NUMERIC(30,18) CHECK(x>0) 接受 NaN，影响 reservations/bookings/account_policies；operability：service/worker.go 全程无 recover()
- **修法**：三层一起补。一、新 migration 给每个金额列加拒绝 NaN 的 CHECK —— 注意 NaN=NaN 在 numeric 上为真、NaN>0 也为真（本轮实测），所以只能写成 CHECK (amount::text <> 'NaN')，覆盖 journals.total_debit/total_credit、journal_entries.amount（含既有分区）、reservations.reserved_amount/settled_amount、bookings.amount/settled_amount、account_policies.min_balance；migration 需可重入并先扫描存量违规行。二、把读路径上的 mustNumericToDecimal 换成返回 error 的 numericToDecimal —— 至少 journalFromRow(convert.go:195-196)、reservationFromRow(:328)、bookingFromRow(:369-370,:414-415) 三处改为返回 error 并由调用方向上传播（errors as data，discipline.md §6）。三、service/worker.go 的每个 runLoop 与 NewLockedJob 回调外层包一层 recover，把 panic 转成 Error 日志 + 该 tick 失败，绝不带走进程。
- **规则/文档要改**：I-6（Decimal precision is NUMERIC(30,18)）需补一句「NaN 不是合法金额，DB 层拒绝」；I-7 的 NOT NULL 纪律段可顺带提及 CHECK(>0) 不构成 NaN 拦截。
- **pin**：新增 postgres.TestJournals_RejectsNaNTotals 与 postgres.TestJournalEntries_RejectsNaNAmount（直接 SQL 插 NaN，断言被 CHECK 拒绝）；新增 service.TestWorker_JobPanicDoesNotKillTheWorker（注入一个必 panic 的 job，断言 Run 不返回且其余 job 继续跑）。

### C-M8 [Major] unauthorized_journals 在一条已签名 journal 都没扫到时仍报 Passed=true 且 Complete=true，指标发绿
- **位置**：`service/reconcile.go:1460-1470` · `service/reconcile.go:1500-1508` · `service/reconcile.go:539-541`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：在 runCheckUnauthorizedJournals 的收尾分支里加一条：checked == 0 && len(journalList) > 0 时设 result.Complete = false，并写 finding「scanned N journal(s), none of which carry a signature — this check verified nothing」。这样 ReconcileCheckResult 报的 Passed && Complete 变为 false，「整个账本无法验证」与「整个账本已验证通过」在机器可读信号上分开。注意保持 Passed 不变（没扫到违规确实不是违规），只动 Complete —— 与 check #2 的 partial-coverage 语义一致。
- **规则/文档要改**：I-32 的 Enforced by 段里描述 unauthorized_journals「skips journals that were never signed at all — that is a coverage gap, not tamper evidence」需补上「全部被跳过时整条检查记为 Complete=false」。
- **pin**：新增 service.TestFullReconciliation_UnauthorizedJournals_ZeroSignedIsIncomplete：配好 AuthVerifier 但所有 journal 都无签名（全部走 RunInTx 或不配 Attestor），断言该 CheckResult 的 Complete==false 且报告的 FullCoverage==false。

### C-M9 [Major] 锚发布持续失败对提现闸没有任何影响且全无指标，唯一信号是一行可能打进 NopLogger 的 ERROR 日志
- **位置**：`service/attestation.go:227-235` · `service/attestation.go:313-343` · `core/metrics.go` · `observability/prometheus.go` · `ledger.go:145`
- **主题**：silent-default · **runtime**：go · **effort**：M
- **合并了**：I-M8（各自报告的论证都要读）
- **同源提示**：operability：全库 metric 的可告警性盘点
- **修法**：一、core.Metrics 新增三个观测点：AttestationBatchResult(ok bool)、AnchorPublishResult(ok bool)、AnchorLagSeqs(lag int64)，NopMetrics 与 observability/prometheus.go 同步实现（Metrics 是 port，加方法属于破坏性接口变更 —— 按 deployment.md 走，或用一个可选的扩展接口做类型断言，避免打断已实现 core.Metrics 的消费方）。二、AttestationService 持有 core.Metrics，RunAttestBatch 与 catchUpAnchor 在成功与失败路径上都发 AnchorLagSeqs(latest.Seq - anchorSeq) 与对应的 Result。三、docs/RUNBOOK.md 增加告警建议（anchor lag 超过 K 个 seq、或 publish 连续失败超过 M 次）。四、是否让提现闸看 anchor 滞后属于设计决策需 Aaron 拍板：若采纳，由消费方注入一个 staleness 阈值，超阈值时 RequireVerifiedBalance=true 的 Reserve 返回 ErrUnauthorizedJournal。
- **规则/文档要改**：design doc 2026-08-21 §8.3「连续失败超阈值只影响 verify 判定与提现门」当前只兑现了前半句 —— 要么实现后半句，要么把它改写成「只影响 verify 判定」并显式记为有意收窄。
- **pin**：新增 service.TestAttestation_AnchorPublishFailureEmitsMetric（注入恒失败的 fake Anchor，断言 AnchorPublishResult(false) 与 AnchorLagSeqs 被调用）。

### C-R1 [Major] 经 RunInTx 落的 journal 永久 unsigned_tx_mode 且无任何补救 API，一次踩中即让该维度的 VerifiedBalance 永久 UNDEFINED
- **位置**：`ledger.go:463-476` · `postgres/ledger_store.go:428-441` · `postgres/verified_balance_store.go:193-195` · `core/auth.go:371-385` · `docs/INVARIANTS.md:1528-1546`
- **主题**：other · **runtime**：go · **effort**：L
- **同源提示**：consumer-surface：RunInTx 的误用面与零值可用性
- **修法**：上一轮把它从静默改成了「披露 + 提供 Authorize/PostAuthorized 出口」，但陷阱本身不可逆（journals append-only，无法补签）。三条路需 Aaron 拍板：(a) 新增 svc.ReauthorizeJournal(ctx, journalUID) 对已存在的 unsigned_tx_mode 行重算 canonical digest 并签名回写 auth_* 四列 —— 需放宽 ledger_journals_block_arbitrary_update 的 mutable 白名单到 auth_* 且只允许 unsigned_tx_mode 到 signed 的一次性跃迁，代价是给 journals 守卫开口子（设计 §8.2 明确反对为标记列开口，A4 就是开了口子的守卫腐化的先例）。(b) 不动守卫，改用旁表 journal_reauthorizations(journal_id PK, digest, signature, key_id, created_at)，本身 append-only + trigger 保护，VerifyJournalAuth / VerifiedBalance 在主行为 unsigned_tx_mode 时回退查该表。(c) 不做补救，只把陷阱变成硬错误：ledger.Service 新增 WithStrictSigning() 选项，开启后 RunInTx 内的 PostJournal / ExecuteTemplate / ExecuteTemplateBatch 直接返回 error 而不是静默落 unsigned_tx_mode。建议 (b)+(c)：(c) 防新增，(b) 救存量。
- **规则/文档要改**：选定方案后 I-26 的 scope note 与 ledger.go:463-476 的「permanently UNDEFINED, with no remediation API」两处都要改；若选 (a) 则 I-2（append-only journals）必须显式列出 auth_* 的一次性跃迁例外。
- **pin**：新增 ledger.TestRunInTx_StrictSigningRefusesUnsignedPost；若选 (b) 再加 postgres.TestReauthorize_UnsignedTxModeJournalBecomesVerifiable（补签后同维度的 VerifiedBalance 由 UNDEFINED 变为有值）。
- **依赖**：C-C1

- **I-M8** [Major] anchor 发布失败被无限静默吞掉，core.Metrics 对整条 P5/P6 防篡改链零覆盖（报告 I-8） — **[并入 C-M9]**

### A-N7 [Minor] core/auth.go 的 authAmountEncodedLen 注释把 NUMERIC(30,18) 的量级推导写反了
- **位置**：`core/auth.go:51`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：注释写 '16 bytes ... comfortably covers NUMERIC(30,18)'s maximum magnitude (10^30 scaled by 10^18 needs ~100 bits)'。NUMERIC(30,18) 的整数部分只有 12 位（30−18），最大定标值约 10^30 —— 结论（~100 bit、16 字节够用）是对的，推导（把 10^30 再乘 10^18）是错的。改成『NUMERIC(30,18) 的最大值约 10^12，定标到 18 位后约 10^30，需约 100 bit』。
- **pin**：none（纯注释）

### C-m1 [Minor] anchordev.LocalFileAnchor 只有散文门禁没有机器门禁，与 dev_credit 的双闸处置不一致
- **位置**：`anchordev/local_file.go:1-16` · `anchordev/local_file.go:45-50` · `server/handler_devcredit.go` · `service/worker.go:253-266`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **同源提示**：threat-model：dev-only 特性的门禁一致性
- **修法**：给 LocalFileAnchor 一个显式的 dev 确认闸，形状对齐 dev_credit：NewLocalFileAnchor(path string, opts ...Option) 增加一个必填的 anchordev.AcknowledgeNotForProduction() option（不传就返回 error），或把构造函数改名为 NewLocalFileAnchorForDevelopment 并在首次 Publish 时 logger.Warn 一次。另外把 service/worker.go:253 的 attestation_anchor 布尔字段改为报告 anchor 的具体类型名（用 %T 格式化），让「配了 dev 锚」与「配了生产锚」在启动日志里可区分 —— 当前两者都是 true。
- **pin**：新增 anchordev.TestNewLocalFileAnchor_RequiresExplicitDevAcknowledgement，以及 service.TestWorker_StartupLogNamesTheAnchorType。

### C-m2 [Minor] anchortest 不断言 head 单调不回退，而这恰好是 VerifyLedger 区分 DRIFT 与 TAMPERED 所依赖的性质
- **位置**：`anchortest/conformance.go:131-260` · `anchortest/conformance.go:40-63` · `core/interfaces.go:737-745`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：给 core.Anchor 的 Head 文档加一条硬契约：Head 必须单调不回退，一旦返回过 seq N，此后永远不得返回小于 N 的值；对应在 anchortest 的 phases 里新增 HeadNeverRegresses（publish 1、2 之后尝试 publish 一个更旧的 seq，断言返回 error 且 Head 仍为 2 —— 注意这与现有 MismatchedReplayErrorsAndDoesNotCorrupt 不同，那条只覆盖同 seq 不同字节）。同时把 conformance.go:45-56 里「本套件对非精确重放的 seq 排序不表态、接受乱序 seq 的实现不因此不合规」这段立场改掉：库自己依赖这条性质。
- **规则/文档要改**：core.Anchor 接口 doc 契约变更（新增一条 MUST），anchordev 与 anchors/r2 两个实现都要通过。
- **pin**：上述 anchortest phase 本身即 pin；anchordev/conformance_test.go 与 anchors/r2/r2_test.go 都会跑到。

### C-m3 [Minor] I-26 的 scope note 声称 ExecuteTemplateBatch / ReverseJournal 从不签名，与 I-31 及现码矛盾
- **位置**：`docs/INVARIANTS.md:1528-1531` · `docs/INVARIANTS.md:2029` · `postgres/ledger_store.go:548-566` · `postgres/ledger_store.go:709-716` · `core/auth.go:378-384`
- **主题**：other · **runtime**：docs · **effort**：S
- **同源提示**：structure-and-contract / test-credibility 可能命中 INVARIANTS 与代码漂移
- **修法**：改 I-26 的 scope note：把「PostJournal's tx-mode branch, ExecuteTemplateBatch, and ReverseJournal/ReverseJournalFraction still never sign」改成「这三者在 pool 模式下会签名（board #15，见 I-31）；只有 tx 模式（经 WithDB 绑定，即 RunInTx 内组合）仍不签名」，并加一条指向 I-31 的显式交叉引用。core/auth.go:378-384 的 AuthStatusUnsignedTxMode 注释口径是对的，不要动。
- **规则/文档要改**：I-26 措辞修正 —— 属于文档对齐代码，不是规则变更；但设计文档 §2 已把「过期的 invariant 声明会误导下一次分析」定为已知失效模式（I-1 的前科），值得在 PR body 里点名。
- **pin**：纯文档修正可接受无新 pin；若要门禁，在 I-26 里加对 I-31 的显式引用后由 core.TestInvariantsDocPinsAllExist 一族覆盖，或加一条 docs 测试断言 I-26 与 I-31 不出现互斥关键词。

### C-m4 [Minor] 未配 WithAttestor 的默认消费方 FullCoverage 永远为假、unauthorized_journals 指标永远红
- **位置**：`ledger.go:429-434` · `service/reconcile.go:1436-1443` · `service/reconcile.go:526-534` · `service/reconcile.go:539-541`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **同源提示**：operability：full_coverage 的可告警性
- **修法**：verifier 为 nil 时不要把 unauthorized_journals append 进 checks 列表（在 RunFull 里条件 append），而不是 append 一个 Complete=false 的占位 —— 与本仓刚刚删掉 check #8 的处置逻辑一致：一个在本部署下结构上不可能运行的检查不该参与投票。若要保留可见性，在 core.ReconcileReport 上新增 SkippedChecks []string 字段（expand-safe）记录被跳过的检查名。
- **规则/文档要改**：I-32 的 Enforced by 段需说明「未配 AuthVerifier 时该检查不参与 FullCoverage 计票，改记入 SkippedChecks」。
- **pin**：新增 service.TestFullReconciliation_WithoutAuthVerifier_StillReportsFullCoverage（断言 FullCoverage==true 且 SkippedChecks 含 unauthorized_journals），与已有的 TestFullReconciliation_UnauthorizedJournals_SkippedWithoutSetAuthCheck 一起改。
- **依赖**：C-M8

### C-m5 [Minor] CanonicalJournalDigest 关于 Metadata 的威胁陈述与实际 DB 守卫不符，会让维护者重复评估一个已完成的决定
- **位置**：`core/auth.go:215-231` · `postgres/sql/migrations/001_baseline.up.sql:1286-1288`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：改 core/auth.go:222-231 的注释：把「a party who can write to the journals table directly could in principle alter metadata on an existing row without invalidating auth_signature」限定为「持 ledger_owner/superuser 的一方」（ledger_app 已被 ledger_journals_block_arbitrary_update 挡住，其 mutable 白名单只有 event_id）；删掉结尾「whether journals.metadata itself should be append-only-guarded ... is a threat-model question orthogonal to this digest」并改为指向该已存在的守卫。签名域未覆盖的字段逐个核过只剩 EventUID（provenance，I-26 已披露）与 Metadata，两者都不影响记账结果或提现资格，这一结论可以直接写进注释。
- **规则/文档要改**：none（纯注释修正）
- **pin**：none 必需；可选：若 threat-model 的 guard 覆盖测试尚未断言 journals.metadata 的 UPDATE 被拒，补一条。

- **F-m11** [Minor] anchors/r2 的 Object Lock（WORM）语义在测试里不生效 —— 桶开了 ObjectLockEnabledForBucket 但注释明说不配 default retention，conformance 跑的是普通 S3 语义 — **[并入 C-M4]**

- **I-N20** [Minor] ledger-cli verify 硬编码 dev 版 anchor 与 verifier，用 anchors/r2 的生产部署没有任何取证路径（报告 I-20） — **[并入 C-M6]**

### I-N23 [Minor] jsonToStringMetadata 解析失败静默返回 nil，而该 metadata 参与幂等三态比对（报告 I-23）
- **位置**：`postgres/convert.go:465` · `postgres/convert.go:468` · `postgres/idempotency_match.go:124`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **同源提示**：territory B concurrency：该 metadata 参与 postgres/idempotency_match.go 的幂等三态判定，严重性定级归 B
- **修法**：jsonToStringMetadata 的签名改成 (map[string]string, error)，解析失败返回 error 而不是 nil，调用点向上传播（fail-closed）。若改动面太大，退而求其次：返回一个哨兵键（如 __unparseable__），保证幂等比对必定判为不匹配走 ErrConflict，而不是「看起来这行本来就没有 metadata」。日志从包级 slog 换成 store 持有的 logger（与 I-N15 同批做）。
- **pin**：写一行 metadata 为非法 JSONB 的 booking，再用相同输入走幂等路径，断言得到 error 或 ErrConflict，而不是静默判为匹配成功。

---

## D-threat（Wave 2）

### D-M1 [Major] 007 的两个 SECURITY DEFINER 分区函数不属于 ledger_owner 而属于 bootstrap 凭证；001 的 ownership 清扫是一次性循环，002-015 建的 4 表 4 序列 9 函数全未被扫到，且无任何门禁
- **位置**：`postgres/sql/migrations/001_baseline.up.sql:1578` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:114` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:179` · `docs/INVARIANTS.md:2425` · `postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:206`
- **主题**：gate-shape · **runtime**：sql · **effort**：M
- **修法**：新增 migration 016_ownership_resweep：把 001_baseline.up.sql:1553-1583 的 catalogue sweep 四个循环（表/序列/视图/routine）抽成一个幂等的 ledger_resweep_ownership() 函数并立即调用一次，使 002-015 已建对象全部归 ledger_owner；此后每个新增对象的 migration 末尾调用同一个函数（或由 D-m8 的新门禁强制）。注意 ALTER ... OWNER TO 需要现 owner 权限，016 必须由 bootstrap 凭证跑，与 D-M2 同批设计；另需处理 ledger_rebalance_default_partition 运行时创建的分区（owner 也是函数 owner），可在 007 的函数体末尾对新建分区补 ALTER TABLE ... OWNER TO ledger_owner，或接受其 owner 就是 ledger_owner（016 落地后即成立）。
- **规则/文档要改**：I-35（docs/INVARIANTS.md:2425-2427 的 Both functions are owned by ledger_owner）在 016 落地前为假，需要么先改措辞要么与 016 同批合入；I-22 的 Note on grant coverage 段补一条 ownership 规则；docs/RUNBOOK.md 的 Database roles 段补「每个新增对象必须归 ledger_owner，由门禁强制」。
- **pin**：新增 TestObjectOwnership_EverythingOwnedByLedgerOwner：枚举 public 下 relkind in (r,p,S,v,m) 的全部关系与 pg_proc 的全部函数，断言 pg_get_userbyid(owner)=ledger_owner（当前会红于 8 个表/序列 + 9 个函数）；另在 postgres/partition_function_hardening_test.go 加 assert proowner=ledger_owner。
- **依赖**：D-M2

### D-M2 [Major] migration 007 的 ALTER ROLE ... NOSUPERUSER 只有 SUPERUSER 能执行，RUNBOOK 认可的 CREATEROLE bootstrap 会卡死在 007，008-015 全部不生效
- **位置**：`postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:23` · `docs/RUNBOOK.md:648` · `docs/RUNBOOK.md:650` · `postgres/migrate.go:47` · `postgres/roles_test.go:801`
- **主题**：gate-shape · **runtime**：sql · **effort**：M
- **修法**：改 007:23-25 三条语句（已落地 migration 不改，新开一个 migration 无法回溯修 007 的失败点，因此这条必须以修改 007 文件本身或引入一个前置 000 migration 的形式处理——鉴于本库无外部用户且 CHANGELOG 已记载破坏性变更惯例，建议直接改 007 并在 header 注明原因）：把 NOSUPERUSER 从 ALTER ROLE 里去掉（Postgres 只允许 superuser 触碰该属性，即使值不变也报 42501），保留 NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS（实测 CREATEROLE 角色可执行）；对 SUPERUSER 改成安装期断言而非设置：DO 块检查 pg_roles 里三个 role 若有 rolsuper 则 RAISE EXCEPTION，让共享集群上的既存 superuser role 变成硬失败（fail-closed）而不是静默继承——这仍然满足 007 原本要关的那个 Minor。
- **规则/文档要改**：docs/RUNBOOK.md:648 保留「superuser 或 CREATEROLE」并补一句「若集群上已存在同名 role 且带 SUPERUSER，安装会以异常终止并要求人工处置」；docs/RUNBOOK.md:650 的 Every migration after 001 runs as ledger_owner 删除或改成事实（postgres.Migrate 全程只吃一个 URL，没有切换连接的机制）。
- **pin**：反转 postgres/roles_test.go:801 的 TestRoleAttributeHardeningResetsPreExistingPrivileges：不再用超级用户 pool 直接 Exec 那条语句，改为新建一个 CREATEROLE-only 角色并用它跑一次完整 postgres.Migrate，断言 001-015 全部 applied 且 schema_migrations 未 dirty（当前会红在 007）。
- **订正（2026-09-03，d-threat 落地后实测；本条的「修法」原文有一处事实错误，不要照抄）**：
  - ❌ 原文「保留 NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS（**实测 CREATEROLE 角色可执行**）」**不成立**。Postgres 按「发起 ALTER 的角色是否持有同名属性」判权，且判的是**子句有没有写**、不是有没有改变什么。postgres:17.10 实测（目标角色五个属性本来就全为 false，仍然报错）：

    | 发起角色 | NOSUPERUSER | NOCREATEDB | NOCREATEROLE | NOREPLICATION | NOBYPASSRLS |
    |---|---|---|---|---|---|
    | `CREATEROLE`（无 CREATEDB） | 42501 | **42501** | ok | 42501 | 42501 |
    | `CREATEROLE CREATEDB` | 42501 | ok | ok | 42501 | 42501 |

    即 `REPLICATION` / `BYPASSRLS` 与 `SUPERUSER` 同形（都要求发起者自己持有该属性），而 RUNBOOK 认可的最窄 bootstrap（CREATEROLE-only）连 `NOCREATEDB` 都发不出——五个子句里能发的只有 `NOCREATEROLE` 一个。所以「去掉 NOSUPERUSER、保留其余四个」这条修法本身仍会 42501 卡死在 007。落地采用的是逐属性条件执行（只对真正持有的属性发 ALTER，发不出去则 RAISE），五种 bootstrap 形态下都可安装。
  - ➕ **安装前提补充**（`postgres.Migrate` 的新机制要求，已同步写进 `postgres/migrate.go` 的 godoc 与本行）：迁移凭证必须是**超级用户**、**`ledger_owner` 本人**、或**对 `ledger_owner` 持有 ADMIN OPTION** 三者之一。装过 001 的凭证永久满足第三条（Postgres 给角色创建者永久 ADMIN OPTION）；不满足的第三方角色现在在跑任何 migration 之前就被拒，错误信息列出这三条出路。⚠️ `docs/RUNBOOK.md:648-651` 归 **D-ops** 独占，d-threat 没动：那里还写着「Every migration after 001 runs as `ledger_owner` and needs no elevated privilege」（从来不存在的机制，本条与 D-M7 都要求改），并且缺 ADMIN OPTION 这一条前提——请 D-ops / W3 按 `postgres/migrate.go` 的 godoc（唯一真相源）同步这三条。
  - ➕ **2026-09-03 新发现（跨分支交互，已在 d-threat 修掉）**：migration 018（d-tamper 本波新增）自己用了 001「Keepsake 2 of 2」的临时提权惯用法（文件头 `GRANT ledger_owner TO <runner> WITH INHERIT TRUE`、文件尾 `REVOKE`），它的 REVOKE 会把 `Migrate` 自己的提权窗口一并撤掉（两条 grant 的 grantor 都只能是 runner 自己，Postgres 只有一行可撤）。实测：CREATEROLE 非超级用户 bootstrap 装到 020 的 `CREATE TRIGGER ... ON public.account_policies` 报 `permission denied`、库被标 dirty 在 20、021 从未执行——即 D-M2 同形失败往后挪了 18 个 migration。因为其余测试全部以超级用户安装（走提权的 no-op 分支），全仓无一处能发现。修法：`Migrate` 改为**逐个 migration 各开一个提权窗口**（`applyRemainingMigrations`），使「migration 对自己 membership 做的事」不会外溢到后续 migration，同时把窗口从「整次安装」收窄到「一个 migration」。⚠️ 遗留给 W3：019 文件尾那段「未来 migration 需要 owner 权限时照抄这套 GRANT/REVOKE」的指引已按此更新（说明不要再用），但 018 里那段现在是**冗余且有害**的写法，建议 W3 清掉并加一条静态门禁（001 之外的 migration 不得出现 `REVOKE ledger_owner FROM`）。

### D-M3 [Major] account_policies 的守卫白名单恰好放行 status/min_balance/enforce_min_balance 三个风控开关，且这张表被排除在审计触发器之外，攻击成功且零痕迹
- **位置**：`postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:42` · `postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:250` · `postgres/account_policy_enforce.go:85` · `postgres/account_policy_enforce.go:110` · `postgres/roles_test.go:491`
- **主题**：silent-default · **runtime**：sql · **effort**：S
- **同源提示**：A financial-correctness 若从 min_balance 一侧命中『风控闸可被改写』，与本条同源
- **修法**：守卫的白名单不能收（UpsertAccountPolicy 确实要写这三列），所以修的是第二层：新增 migration，给 account_policies 挂一个与 006:287-307 同形的 AFTER UPDATE ... WHEN (to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW)) EXECUTE FUNCTION ledger_log_config_table_change() 触发器（该函数用 TG_TABLE_NAME，函数体无需改动），使每一次 status/min_balance/enforce_min_balance 变更——无论来自 UpsertAccountPolicy 还是裸 SQL——都落进 config_table_changes；应用层写 account_policy_changes 的路径保持不变（它承担业务 actor 归因）。可选加固：把负 min_balance 拆成独立的 overdraft_limit 列并置于 003/006 白名单之外，使「开透支」需要一次被守卫拒绝的写。
- **规则/文档要改**：I-25 补一句「account_policies 的三个 enforcement 列由白名单放行、由审计触发器留痕，而不是由守卫拒绝」；006:250-256 关于「account_policies 已有 app 层审计所以不需要 DB 审计」的推理需在新 migration 的 header 里更正（该推理漏了裸 SQL 攻击者不写那张表）。
- **pin**：新增 TestAccountPolicyChangesAudited：以 ledger_app 执行 status frozen→active 且 min_balance→-1000000，断言 config_table_changes 多一行且 old_row/new_row 覆盖这三列（当前会红）；postgres/roles_test.go:491 的 legitimate SetPolicy-shaped update 子测试保留（合法路径仍须成功）但补断言「这次写产生了审计行」，否则它只是在认证攻击可行。

### D-M4 [Major] 006 新建的两张审计表对 ledger_app 开放 INSERT，changed_by 与 changed_at 可任意伪造，取证归因不可采信
- **位置**：`postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:229` · `postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:273` · `postgres/sql/migrations/010_reconcile_scan_lap_coverage.up.sql:41` · `postgres/grant_coverage_test.go:118` · `postgres/roles_test.go:672`
- **主题**：other · **runtime**：sql · **effort**：S
- **修法**：新增 migration：把 ledger_log_config_table_change() 与 ledger_log_reconcile_scan_cursor_change() 改成 SECURITY DEFINER SET search_path = public, pg_temp（owner 必须是 ledger_owner，见 D-M1，否则等于把审计写入提到 bootstrap 权限），然后 REVOKE INSERT ON public.config_table_changes, public.reconcile_scan_cursor_changes FROM ledger_app 并 REVOKE 两个 _id_seq 的 USAGE, SELECT；两张表已带 ledger_block_mutation 守卫，会被 grant_coverage_test 的 appendOnly 分类接住，需把期望 ACL 从 {SELECT,INSERT} 调成 {SELECT}（即在 appInsertColumnScoped 之外再加一个 insertRevoked 分类，或直接把它们放进现有的 updateRevoked 语义并同步注释）。
- **规则/文档要改**：I-45（docs/INVARIANTS.md:2494-2540）补一句「审计行只能由 SECURITY DEFINER 触发器写入，ledger_app 无 INSERT」；006 的 §9 段落关于 current_user 的说明需补上「攻击者可显式赋值绕过 DEFAULT current_user」这一点（在新 migration header 里更正）。
- **pin**：新增：以 ledger_app 执行 INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by, changed_at) VALUES (..., ledger_owner, now()-interval 30 days) 必须 42501（当前会成功）；对 reconcile_scan_cursor_changes 同形；同时保留 postgres/roles_test.go:672/718 的「合法 UPDATE 仍产生审计行」断言，防止改成 SECURITY DEFINER 后把写入路径整个搞坏而无人发现。
- **依赖**：D-M1

### D-M5 [Major] §9 的取证层没有任何读者：三张审计表无查询、无 store 方法、无 ledger-cli 子命令、无 RUNBOOK 条目，只有测试读
- **位置**：`cmd/ledger-cli/main.go:119` · `docs/RUNBOOK.md:621` · `docs/INVARIANTS.md:2494` · `postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:180`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **合并了**：I-N13（各自报告的论证都要读）
- **同源提示**：I operability 的『三张审计表零 metric、零 RUNBOOK 条目』与本条同源；本条含读取面实现，I 侧若只报可观测缺口应合并
- **修法**：三件事：① postgres/sql/queries/audit.sql 新增三条 keyset 分页查询——ListConfigTableChanges（可按 table_name + 时间窗过滤）、ListReconcileScanCursorChanges（按 check_name）、ListAccountPolicyChanges（按 account_holder），make sqlc 生成；② core 侧在既有 AuditQuerier port 上加这三个方法（或新开一个 1-3 方法的 ConfigChangeReader port，接口定在消费方），postgres 适配器实现，facade ledger.go 暴露访问器并进 README API surface 表（否则 readme_api_surface_test.go 会红——顺手证明它在工作）；③ cmd/ledger-cli 新增 config-history 子命令（如 ledger-cli config-history --table currencies --since 30d）并写进 docs/RUNBOOK.md 的事故取证流程一节；可选给 config_table_changes 的行数增量加一个 core.Metrics 计数器供告警。
- **规则/文档要改**：docs/RUNBOOK.md 新增一节「配置篡改取证：谁改了决定钱去哪的规则」，列出三张表与新 CLI 子命令；I-45 的 Pinned by 段补读取面的 pin。
- **pin**：新增 TestConfigHistoryQuery_ReturnsTamperTrace：篡改一张配置表后，经新 query/CLI 路径读回该次变更（而不是测试自己直接 SELECT 那张表）；把 CLI 子命令或 store 方法拆掉即红。

### D-M6 [Major] ledger_app 保有 webhook_subscribers.secret 的 SELECT，泄露的 DB 凭证可向每个下游订阅方伪造签名有效的事件投递
- **位置**：`postgres/sql/migrations/014_webhook_subscriber_app_grant.up.sql:24` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:47` · `service/delivery/webhook.go:1` · `postgres/grant_coverage_test.go:239` · `docs/plans/2026-08-21-tamper-evident-ledger-design.md:63`
- **主题**：sibling · **runtime**：go · **effort**：L
- **同源提示**：C tamper-evident 若从『密钥失效域』一侧命中，与本条同源；014 header 自己把它记为 NOT closed here
- **修法**：这是 007 对 ledger_ro 那条修复的同形残留，但外发签名确实要读 secret，所以需要结构改动而非 grant 微调：新增一个 1 方法的 core.WebhookSigner port（Sign(ctx, subscriberName string, payload []byte) ([]byte, error)），由消费方在 composition root 注入，遵循 core.Attestor 的「密钥永不入库」原则；service/delivery/webhook.go 改为调 port（nil 时按现有 secret 为空的语义降级为不签名，但要打 slog.Warn，不许静默）；随后走 deployment.md 三步：expand 阶段两条路径并存并以 port 优先，migrate 阶段消费方迁移密钥，contract 阶段 REVOKE SELECT ON webhook_subscribers FROM ledger_app + GRANT SELECT (非 secret 列) + DROP COLUMN secret。若本轮不做结构改动，最低要求是把这条写进威胁模型表并在 INVARIANTS 里诚实声明（见 rule_change）。
- **规则/文档要改**：docs/plans/2026-08-21-tamper-evident-ledger-design.md §1 威胁模型表新增一行「app DB 凭证 → 可读出站 HMAC 密钥，向任意下游伪造签名有效的事件投递」；non-goal 1（保密性）明确排除「读到密钥后可伪造」这一类，与 007:47-56 的推理保持一致。
- **pin**：新增 TestLedgerAppCannotReadWebhookSecret（与 postgres/roles_test.go:762 的 ledger_ro 版同形），contract 阶段后由红转绿；过渡期先加一条「外发签名不经由 DB 读取的密钥」的行为 pin（注入 mock WebhookSigner，断言它被调用且 store 未被查 secret）。

### D-M7 [Major] RUNBOOK 仍指挥运维给 worker 一个 ledger_owner 连接做分区维护，照做会重新装回 007 刚拆掉的 TRUNCATE 旁路
- **位置**：`docs/RUNBOOK.md:669` · `docs/RUNBOOK.md:650` · `ledger.go:847` · `postgres/partition_store.go:92` · `docs/INVARIANTS.md:2419`
- **主题**：other · **runtime**：docs · **effort**：S
- **同源提示**：I operability 的 RUNBOOK 与代码一致性条目
- **修法**：改写 docs/RUNBOOK.md:669-672 整条为：「分区维护由 ledger_app 通过两个 SECURITY DEFINER 函数完成（I-35），serving pool 不得持有 ledger_owner 凭证——给它 owner 凭证会让 TRUNCATE 绕过 journal_entries 的 no-DELETE 触发器（TRUNCATE 不触发行级触发器），即重新打开 007 关掉的那个旁路」，并附 I-35 的 file 引用；同批删/改 :650（见 D-M2）；顺带 grep README.md 与 docs/DR.md 是否有同类残留表述（如「migration job 用 ledger_owner」「PartitionService 需要 owner pool」）。
- **规则/文档要改**：本条即文档措辞变更（docs/RUNBOOK.md:650 与 :669-672）；I-35 本身无需改。
- **pin**：新增一条廉价文档门禁（与 readme_api_surface_test.go 同形）：断言 docs/RUNBOOK.md 中不再出现 ledger_owner-backed connection / not the app pool 这组字符串；更结构化的做法是断言 RUNBOOK 的 role→用途表里每个 role 声明的权限与 migration 实际授予的一致（从 information_schema 派生）。
- **依赖**：D-M2

### D-M8 [Major] 入站 webhook 重放缓存全仓零接线、零文档、nil 时完全静默；README 把 ±5min 时间窗说成全部的重放防护
- **位置**：`server/handler_webhooks.go:20` · `server/handler_webhooks.go:28` · `server/handler_webhooks.go:53` · `server/server.go:62` · `README.md:884` · `channel/onchain/evm.go:21`
- **主题**：silent-default · **runtime**：go · **effort**：M
- **同源提示**：E consumer-surface 若从『接线陷阱』一侧命中（同 Worker.Subscribe / SetLocalPoller 家族），与本条同源
- **修法**：三层：① server/handler_webhooks.go 里给 nil 分支加一次性告警——Server 上挂 sync.Once，首次遇到 s.webhookNonces == nil 时打 slog.Warn（inbound webhook replay cache is not configured, in-window replays are accepted），与 server/server.go:492-503 那批启动 warn 同一姿态（放在 handler 而非 newServer 是因为 recorder 是 late-bound 的）；② 把接线做成默认路径：ledger.go 的 facade 暴露 svc.WebhookNonceRecorder()（返回 postgres.WebhookSubscriberStore，进 README API surface 表），并在 README 的 server 装配段与至少一个 example 里显式调用 srv.SetWebhookNonceRecorder(svc.WebhookNonceRecorder())；③ 修 README.md:884，把 nonce 缓存写进 Webhook replay 条目并明说不接线时窗口内重放会通过。
- **规则/文档要改**：README.md:884 措辞；docs/api.md:328 的 webhook 状态码段补「409 replayed 只在配置了 nonce recorder 时可能出现」；INVARIANTS 若要新增一条，措辞应是「入站 webhook 的窗口内重放由 nonce 缓存拒绝，缺失时以 warn 暴露而非静默放行」。
- **pin**：两条：TestWebhookCallback_WarnsWhenNonceRecorderMissing（注入可捕获的 slog handler，断言 nil 时确有 warn，非空洞）；以及一条真实路径 pin，按 README/example 的装配方式构造 server 后断言 s.webhookNonces != nil——与 ledger_subscribe_wiring_test.go 同形，专防『能力在库里、缺席于消费方路径』这一族。

### F-M7 [Major] TestRoleAttributeHardeningResetsPreExistingPrivileges 把 ledger_app 提成集群级 SUPERUSER，而 role 不随 database 隔离、测试服务器全仓共享 —— 跨 package 并行下会假红
- **位置**：`postgres/roles_test.go:801` · `postgres/roles_test.go:805` · `postgres/roles_test.go:816` · `postgres/roles_test.go:439` · `internal/postgrestest/postgrestest.go:37` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:23`
- **主题**：other · **runtime**：go · **effort**：M
- **同源提示**：threat-model [Major] migration 007 只有 SUPERUSER 能执行（相邻不同源：同样围绕 ALTER ROLE ... NOSUPERUSER，但那条讲 bootstrap 凭证能力不足，不是测试污染）
- **修法**：postgrestest 的隔离粒度是 database，对 pg_roles 无效，所以不能靠过滤（这与 c115818 的 pg_locks 修法不同 —— advisory lock 有 database 维度，role 没有）。三个可选修法，建议 (b)：(a) 该测试改用自己的一次性角色名（ALTER ROLE ledger_app_rolehardening_test ...），把 007 的语句参数化后对它执行 —— 但会偏离「验证的是真实那条语句」；(b) 让该测试复用 postgres.Migrate 已有的集群级互斥：暴露一个测试可见的 acquireClusterLock（postgres/migrate.go:133）包裹 805→816 这段窗口，这样它与任何并发 Migrate 互斥（Migrate 本身已持同一把锁），窗口内不会有人重置角色，也不会有人观察到 SUPERUSER 状态；(c) 给 roles_test.go 整个文件专用一个独立 Postgres 容器（不共享 server）。顺带把 postgres/roles_test.go:439 那条注释修正：它论证的是同 package 内不 t.Parallel，没有覆盖 go test ./... 的跨 package 并行 —— 而 migration 里的 ALTER ROLE 每次 SetupDB 都会执行。
- **规则/文档要改**：none（测试基建问题，不涉及 invariant 措辞）
- **pin**：修完加一条断言：该测试运行期间持有集群锁（或使用独立角色名）。回归形状是「把互斥去掉 → 与一个并发 Migrate 循环同跑 → 必现假红」

### A-N2 [Minor] I-42 的 journal_entries.id 保护只绑 ledger_app 角色名，库从不断言自己连的是哪个角色 —— 余额等式的单调性承重在一条只写在 README/RUNBOOK 的部署纪律上
- **位置**：`postgres/sql/migrations/008_journal_entries_id_sequence_only.up.sql:1` · `postgres/migrate.go:47` · `ledger.go:141` · `README.md:75`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **同源提示**：属 D territory 的 ACL 面，D 很可能独立命中；我只记录它对余额等式的承重作用
- **修法**：在 ledger.New（ledger.go:141）之后加一次性的启动自检（或提供一个显式的 svc.AssertRuntimeRole(ctx) 供消费方在 composition root 调用）：`SELECT current_user` 不是 ledger_app 时按配置 warn 或 error。默认取哪个由 Aaron 拍板——error 更符合 fail-closed，但会打断所有用 owner 连库的开发与既有部署；建议默认 warn + 提供 WithRequireAppRole() Option 让生产显式收紧。⚠️ 不要在 Migrate 里做（Migrate 本来就需要更高权限的角色）。
- **规则/文档要改**：docs/INVARIANTS.md I-42 需在 'Load-bearing prerequisite' 里补一句：本不变式的强制手段是 ACL，只约束 ledger_app；以其他角色连接的运行时不受保护。README.md:75 与 docs/RUNBOOK.md 需把『运行时必须以 ledger_app 连』从建议提到前置条件。
- **pin**：新增用例：以 owner 角色连库、显式指定 id INSERT 一行 journal_entries，断言（在选定方案下）自检会报出来——而不是断言 INSERT 被拒（以 owner 连它不会被拒，这正是本条的内容）。

### D-m1 [Minor] ledger_rebalance_default_partition 的日期区间不受任何约束，ledger_app 可一次性制造永久性分区膨胀并反复取 ACCESS EXCLUSIVE 锁
- **位置**：`postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:125` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:179` · `postgres/sql/migrations/013_partition_function_hardening.up.sql:60` · `postgres/partition_store.go:145`
- **主题**：sibling · **runtime**：sql · **effort**：S
- **修法**：新增 migration，CREATE OR REPLACE FUNCTION ledger_rebalance_default_partition(date,date) 在函数体开头补参数校验：p_first 与 p_last 必须月初对齐（date_trunc(month,x)::date = x）、p_last >= p_first、且区间长度不超过一个明确上限（如 120 个月），违反即 RAISE EXCEPTION USING ERRCODE = invalid_parameter_value。与 013 给兄弟函数 ledger_create_monthly_partition 补 name↔range 校验的理由（EXECUTE 本身是 ledger_app 可达的能力，不能信任调用方）与形状完全一致，013 只是漏了这一个。
- **规则/文档要改**：I-35 的 Enforced by 段补一句「rebalance 的日期区间同样受函数内校验约束」，与已有的 name 正则并列。
- **pin**：新增 TestPartitionRebalanceRejectsUnboundedRange：以 ledger_app 调 ledger_rebalance_default_partition(1900-01-01, 2200-12-01) 必须报 invalid_parameter_value（当前实测 2 年区间创建 24 张分区表 = 120 个关系，无任何上限）。

### D-m10 [Minor] 审计触发器的覆盖是手点的 4 张配置表，而带部分（白名单）守卫的表有 9 张：bookings / events / reservations / journals / account_policies 的合法变更全部无痕
- **位置**：`postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:287` · `postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:242` · `postgres/sql/migrations/003_config_table_guards.up.sql:66`
- **主题**：gate-shape · **runtime**：sql · **effort**：M
- **同源提示**：D-M3 是本条在 account_policies 上的特例（风控开关那张最要紧的表），本条是其一般化
- **修法**：新增 migration，用一个从目录派生的 DO 循环给「携带部分守卫的每张表」挂上 AFTER UPDATE ... WHEN (to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW)) EXECUTE FUNCTION ledger_log_config_table_change() —— 判据是「该表有 BEFORE UPDATE 行级触发器，但其函数不是 ledger_block_mutation()」，与 001 §14 派生 ACL 的谓词同源、方向相反。config_table_changes 的 old_row/new_row 是 JSONB 且函数用 TG_TABLE_NAME，无需按表定制；注意 bookings/events 的 UPDATE 是高频路径（delivery_status 每次投递都写），需评估审计表写放大，必要时用 WHEN 子句只在非投递列变化时记录。
- **规则/文档要改**：I-45 从「四张配置表」扩为「每张带部分守卫的表」；I-25 的 Enforced by 段同步。
- **pin**：新增 TestPartialGuardTablesAreAudited：从 pg_trigger 派生出「带部分守卫」的表集合，断言每张都挂了审计触发器（当前会红于 account_policies / bookings / events / reservations / journals 五张）。
- **依赖**：D-M3, D-M4

### D-m2 [Minor] I-22 的『ledger_app 不能创建任何对象、不能 TRUNCATE』在 007 之后为假，pin 只试裸 DDL 语句
- **位置**：`docs/INVARIANTS.md:1030` · `docs/INVARIANTS.md:1034` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:114` · `postgres/roles_test.go:59`
- **主题**：other · **runtime**：docs · **effort**：S
- **同源提示**：F test-credibility 若从『pin 断言的命题与标题声称的命题不同』一侧命中，与本条同源
- **修法**：改 I-22 措辞：把 cannot ... create any object, anywhere in the schema 与 cannot TRUNCATE 改成「不能直接发起任何 DDL；唯一例外是 I-35 的两个受控 SECURITY DEFINER 函数（EXECUTE 授权），它们只能按固定形状创建 journal_entries 的月分区、且只在同一语句内把行搬进永久分区后 TRUNCATE 默认分区」，并把这两个 EXECUTE 授权写进 Enforced by 段，使 I-22 与 I-35 不再互相矛盾。
- **规则/文档要改**：docs/INVARIANTS.md I-22（:1030-1038 的规则段与其 Enforced by 段）。
- **pin**：扩 postgres/roles_test.go:59 的 TestLedgerAppIsLeastPrivilege：除现有裸语句子测试外，新增两个子测试——断言 ledger_app 对这两个函数有 EXECUTE，且对 pg_proc 中 public 下其他任何函数都没有 EXECUTE（把「例外恰好是这两个」变成机器可检验，与 D-m8 的函数 ACL 门禁可合并实现）。

### D-m3 [Minor] grant_coverage_test 把 ledger_ro 的期望硬编码成全表 SELECT，结构上要求每张新表把全表读权给 BI 角色，复制出 007 刚修掉的密钥暴露
- **位置**：`postgres/grant_coverage_test.go:266` · `postgres/grant_coverage_test.go:135` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:47`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：把 postgres/grant_coverage_test.go:266 的 assertGrants(ledger_ro, table, {SELECT}) 改成两分支：先按列名正则（secret|password|passwd|token|private_key|seed|hmac|credential）扫该表的 information_schema.columns，未命中则维持 {SELECT}；命中则要求该表落在 roColumnScoped 集合里，并断言那一列对 ledger_ro 无 SELECT（复用已有的 assertColumnPrivilegeAbsent）。这样新增一张带密钥列的表，默认方向从「敞开」变成「必须显式列级收窄」。
- **规则/文档要改**：I-22 的 Note on grant coverage 段补一句「携带密钥类列的表必须对 ledger_ro 列级授权，全表 SELECT 不被门禁接受」。
- **pin**：本条本身即门禁改造；验证方式：webhook_subscribers 作为已知正例（应绿），临时造一张带 secret 列且只有表级 GRANT 的表作反例（应红），两个方向都跑一次。

### D-m4 [Minor] TryRecordNonce 对 42501 的容错降级完全无痕，缺 DELETE 授权的库会静默让重放缓存无界增长
- **位置**：`postgres/webhook_subscriber_store.go:85` · `postgres/webhook_subscriber_store.go:72`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **修法**：在 postgres/webhook_subscriber_store.go:85 被容忍的那条分支里加一次性告警：WebhookSubscriberStore 上挂 sync.Once + 该层已注入的 logger（postgres 层的 slog adapter 约定），打 slog.Warn（webhook nonce prune refused for lack of privilege, replay cache will grow without bound, apply migration 002）；可选再给 core.Metrics 加一个计数器。doc comment 已经诚实写了这个代价，缺的只是运行时痕迹（working-agreements §3 的『降级必须落痕』）。
- **pin**：新增：REVOKE 掉 ledger_app 的 webhook_nonces DELETE 后调 TryRecordNonce，断言调用仍成功（既有行为不回退）且产生了一条 warn（用可注入的 slog handler 捕获）——当前会红在 warn 那一半。

### D-m6 [Minor] 没有任何门禁断言每条 chi 路由都带鉴权/授权中间件；/api/v1/holder/ 的豁免是一次裸字符串前缀匹配
- **位置**：`server/middleware_auth.go:158` · `server/middleware_auth.go:165` · `server/routes.go:84` · `server/openapi_contract_test.go:763`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **修法**：复用已有的 chi.Walk（server/openapi_contract_test.go:763 已证明可行，其回调第 4 个可变参数就是该路由的中间件链）新增一个测试：枚举每条路由，① 落在 unauthenticatedPaths / webhookPathPrefix / holderPathPrefix 三个豁免集合内的路由必须在一个硬编码白名单里（新增一条即 Fatalf，与 grant_coverage_test 的三分类同形）；② 落在 holderPathPrefix 下的路由，其中间件链必须含 holderTokenAuth；③ 其余每条路由的中间件链必须含 requireScope 或 requireCapability 产出的中间件（用包级 sentinel 包装函数或对返回的中间件做类型标记来识别）。当前路由表实际是干净的，缺的是门禁而非现存缺陷。
- **规则/文档要改**：none（新增门禁）；可选在 I-38 或新 invariant 里声明「每条路由的鉴权归属由门禁机械派生」。
- **pin**：本条即 pin；验证方式是临时把 routes.go 里某条 write 路由挪出 ScopeWrite 组、或在 /api/v1/holder/ 下注册一条不在 holder 组里的路由，确认新门禁两种情况都变红。

### D-m7 [Minor] deposits / withdrawals 是完整的死表（sqlc 生成方法零调用方）却保有 ledger_app 的 SELECT/INSERT/UPDATE 且无任何守卫
- **位置**：`postgres/sql/queries/withdrawals.sql:1` · `postgres/grant_coverage_test.go:207` · `postgres/grant_coverage_test.go:220`
- **主题**：other · **runtime**：sql · **effort**：S
- **同源提示**：H structure 的 schema 清理条目
- **修法**：走 deployment.md 的 contract 阶段：再确认一次 0 生产调用方（postgres/sqlcgen/withdrawals.sql.go 的 10 个方法与 deposits 同类全部只被生成代码自身引用）后，删 postgres/sql/queries/withdrawals.sql 与 deposits.sql、make sqlc、新增 migration DROP TABLE withdrawals, deposits（附 down 脚本或标 breaking-rollback-fence + 说明）；若决定保留作历史台账，则至少 REVOKE INSERT, UPDATE ON public.deposits, public.withdrawals FROM ledger_app 并把两者从 grant_coverage_test 的 reviewed 移到 updateRevoked。
- **规则/文档要改**：postgres/grant_coverage_test.go:186-222 的 reviewed 注释需去掉这两项及其 history, nothing reads or writes them 的说明。
- **pin**：删表后由 make sqlc-diff + grant_coverage_test 自动覆盖（表消失即无期望）；若保留则新增断言「ledger_app 对 deposits/withdrawals 无 INSERT/UPDATE」。

### D-m8 [Minor] grant_coverage_test 完全不覆盖函数的 EXECUTE ACL，也把 journal_entries 的分区整体排除在外
- **位置**：`postgres/grant_coverage_test.go:63` · `postgres/grant_coverage_test.go:71` · `postgres/sql/migrations/008_journal_entries_id_sequence_only.up.sql:88` · `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:114`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：F test-credibility 若从『grant_coverage 三分类是否穷尽』一侧命中，与本条同源
- **修法**：给 postgres/grant_coverage_test.go 加两个测试：① 枚举 pg_proc 中 public 下所有函数，断言 ledger_app / ledger_ro 持有的 EXECUTE 恰好等于一个显式白名单（今天只有 ledger_create_monthly_partition 与 ledger_rebalance_default_partition 对 ledger_app），任何未列出的函数被授予 EXECUTE 即红——这正是本轮 D-M1 / D-m1 / D-m2 三条能长期潜伏的原因；② 枚举 pg_partition_tree(journal_entries) 的每个分区，断言其 ACL 与 008 建立的形状一致（ledger_app 只有表级 SELECT + 不含 id 的列级 INSERT），当前主查询用 NOT c.relispartition 把它们全排除了，某个分区被单独错授时无人可见。
- **规则/文档要改**：I-22 的 Note on grant coverage 段从「每张表/序列」扩为「每张表/序列/分区/函数」。
- **pin**：本条即 pin；验证方式是临时给某个 guard 函数（如 ledger_block_mutation）加一条 GRANT EXECUTE ... TO ledger_app、并给某个既有分区单独 GRANT UPDATE，确认两个新门禁分别变红。

### G-m5 [Minor] legacy ParseCallback webhook 路径可把 deposit booking 推到 confirmed 而不出 journal
- **位置**：`server/handler_webhooks.go:79` · `server/handler_webhooks.go:104` · `server/handler_webhooks.go:118`
- **主题**：sibling · **runtime**：go · **effort**：S
- **同源提示**：territory D：webhook 入站的绕过路径；territory E：消费方自带 channel adapter 的误用面
- **修法**：在 handleWebhookCallback 的 legacy 分支里，对 deposit-classification 的 booking 限制可达状态：已经有 classification confinement（:135-146 只允许 deposit booking），再加一条「legacy 回调不得把 deposit booking 推到 confirmed」——confirmed 只能由 postDepositConfirmedJournal 这唯一路径产生（I-21 已经声称它是唯一路径，但只在 service 层成立，HTTP 层没有对应约束）。实现上：payload.Status == "confirmed" 且目标是 deposit classification 时返回 httpx.ErrForbidden，指引改用 ParseSighting 形状。本仓适配器实现了 ParseSighting 所以该路径不可达，但消费方自带 adapter 会到达。
- **规则/文档要改**：I-21 的 Enforced by 段落补上 HTTP 层这一条（当前只列了 service 层的四条，HTTP 边界是个缺口）。
- **pin**：server 测试：用一个只实现 channel.Adapter（不实现 sightingParser）的假适配器，对一个 confirming 的 deposit booking 发 status=confirmed 回调，断言 403 且 booking 仍是 confirming（当前实现会 200 且推到 confirmed，必红）。

---

## D-contract（Wave 2）

### E-M11 [Major] srv.SetReady(true) 是必做接线步骤但零文档；docs/api.md:884 与 README:885 都声称 /system/ready 会在 migration+worker 就绪后自动变绿
- **位置**：`server/server.go:484` · `server/server.go:542-546` · `server/handler_system.go:43-52` · `docs/api.md:882-884` · `README.md:885` · `examples/fullstack/backend/main.go:243`
- **主题**：other · **runtime**：go · **effort**：S
- **合并了**：I-N19（各自报告的论证都要读）
- **同源提示**：H: docs/api.md 探针失败响应体与 next_cursor 示例的 wire 漂移
- **修法**：(1) 改文档口径为真话：docs/api.md:884 与 README:885 改成『返回 200 仅当宿主进程调用过 (*server.Server).SetReady(true)；库不观察 migration 或 worker 状态，就绪判定由宿主的composition root 负责』。handler_system.go:43 的注释同改。(2) 把 SetReady 加进 README 的 API Surface（Integrity and operations 表附近新增一个 『HTTP server 生命周期』小节，含 SetReady / SetHolderSurface / StartRateLimiterGC 三个同样只出现在 example 里的接线方法）。(3) 可选但推荐：给 server.Deps 加一个 Ready func() bool 或让 NewFromDeps 接受一个 ReadyProbe，使『谁决定 ready』成为构造期必答题而不是一个可遗忘的 setter。(4) docs/api.md:884 顺带修 503 body：现在是统一信封 {"code":18101,...} 而不是 {"status":"starting"}（见 E-m19，与 territory H 同源）。
- **规则/文档要改**：docs/api.md 与 README 的 readiness 语义措辞必须改（当前是错的，不只是缺）。
- **pin**：新增 server 包测试：NewFromDeps 之后不调 SetReady，GET /api/v1/system/ready 断言 503 + code 18101；调用 SetReady(true) 后断言 200。同时把 openapi_contract_test 里 ready 的失败响应 schema 对齐统一信封。

### E-M9 [Major] core.ErrUnknownAuthKey 在 core.IsRetryable(false) 与 HTTP 映射(19999->Retryable=true) 之间口径相反；防止这件事的 pin 是一张漏了它的手抄 slice
- **位置**：`core/errors.go:33-45` · `core/errors.go:113` · `pkg/httpx/response.go:203-238` · `pkg/bizcode/errors.go:206-221` · `pkg/httpx/response_test.go:127-158`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **修法**：(1) pkg/httpx/response.go 的 resolveError switch 在 ErrUnauthorizedJournal 分支之前（或之后，顺序无所谓因为两者互不 errors.Is）新增一支：case errors.Is(err, core.ErrUnknownAuthKey): return bizcode.Wrap(14011, "unknown authorization key", err)。14011 落在 14000-14999 区间，bizcode.Retryable 自动返回 false，与 core.IsRetryable 一致。同时在 pkg/bizcode/errors.go 的 displayMessages 加 14011 的脱敏文案（参照 14010 的措辞，不得泄露 key id / 部署信息），并在 docs/api.md:119 附近的错误码表补一行。(2) 把 pkg/httpx/response_test.go:128-145 的手抄 slice 改成机械派生：用 go/ast 解析 core/errors.go 顶层 var 块里所有 Err* 标识符，逐个跑 resolveError + bizcode.Retryable 对 core.IsRetryable —— 新增哨兵未处理时自动红。
- **规则/文档要改**：docs/INVARIANTS.md 里承载错误分类的那条（I-45 相邻）需增补：『core/errors.go 的每一个哨兵都必须在 pkg/httpx.resolveError 有显式映射，且 bizcode.Retryable 与 core.IsRetryable 对其结论一致』—— 全称命题，机械校验。
- **pin**：TestResolveError_AgreesWithCoreIsRetryable 改为从 core/errors.go 机械派生哨兵清单（见 fix (2)）。改完在打补丁前先跑一次：它必须因 ErrUnknownAuthKey 而红，红过才算这条 pin 有效。

### H-M1 [Major] openapi 的 query/path 参数完全在契约门禁之外，已有三处确诊漂移，其中 GET /snapshots 照 spec 调用必然 400
- **位置**：`docs/openapi.yaml:296` · `docs/openapi.yaml:312` · `docs/openapi.yaml:1256` · `docs/openapi.yaml:1260` · `server/handler_journals.go:439` · `server/handler_balances.go:31` · `server/handler_system.go:191` · `server/handler_system.go:197` · `server/openapi_contract_test.go:259`
- **主题**：gate-shape · **runtime**：docs · **effort**：M
- **合并了**：J-6（各自报告的论证都要读）
- **同源提示**：J 若报「@azex/ledger-react 生成类型里的 query 参数名与后端对不上」为同源
- **修法**：两步。①改 docs/openapi.yaml 让 parameters 对齐实现（以 docs/api.md:521/531/846 的散文为准，它是对的）：/entries 的 name:currency 改 currency_uid；/balances/{holder} 补一个 in:query name:currency_uid required:true 并把 summary 的 across currencies 改掉；/snapshots 的 currency 改 currency_uid、from 改 start、to 改 end，四个全部 required:true 且 start/end 的 format 改 date。②在 server/openapi_contract_test.go 新增 TestOpenAPIContract_QueryParamsMatchGoHandlers：从 loadOpenAPIPaths 抽每个 operation 的 parameters 里 in:query/in:path 的 name 集合，与用 go/ast 扫 server/handler_*.go 得到的 r.URL.Query().Get(字面量) 与 chi.URLParam(r,字面量) 字面量集合按 route 做双向比对，两个方向缺失都报错；路由到 handler 的映射从 setupRoutes 的 chi.Walk 拿（enumerateRoutes 已有），handler 名到文件的映射用方法名反查。
- **规则/文档要改**：none。api-contract.md 与 docs/api.md 都无需改；坏的是 docs/openapi.yaml 单方面。
- **pin**：把 /snapshots 的 currency_uid 改回 currency（或删掉 start/end）后，新增的 QueryParamsMatchGoHandlers 必须红。

### H-M2 [Major] 契约门禁只比字段名不比类型与 format；Booking.expires_at 在默认路径上吐空串而 spec 承诺 date-time
- **位置**：`server/handler_bookings.go:55` · `server/handler_bookings.go:95` · `docs/openapi.yaml:2286` · `docs/openapi.yaml:2303` · `server/openapi_contract_test.go:178` · `server/openapi_contract_test.go:235`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：J 若报「前端拿到 expires_at 空串导致日期渲染异常」为同源
- **修法**：两步。①修 wire 形状：把 bookingResponse.ExpiresAt 从 string 改成 *string（json:"expires_at"，不加 omitempty），bookingToResponse 在 op.ExpiresAt.IsZero() 时留 nil，非零时指向 UTC RFC3339 串；同步把 docs/openapi.yaml:2303 的 expires_at 从 $ref Timestamp 改成 oneOf/[type:[string,null],format:date-time] 并保留在 required 里——空串这种第三态必须消失，不能靠改文档承认它。②扩门禁：给 openapi_contract_test.go 的 goJSONFieldNames 改成返回 map[string]reflect.Type，新增 assertTypesMatch，把 spec 的 type/format（string+date-time / string(Decimal) / integer / boolean / object / array）映射到 Go 类型做校验，至少覆盖三类高危：date-time 必须对应 time.Time 或 *string、Decimal 必须对应 string、integer 必须对应 int64/int32。
- **pin**：新增 TestBookingResponse_NoExpiryEmitsNull：建一个不带 expires_at 的 booking，断言响应 JSON 里 data.expires_at 是字面 null 而不是空串；另在类型门禁里把 Booking.expires_at 的 spec 类型改成裸 string(date-time) 时必须红。

### H-M4 [Major] 出站 webhook 用 encoding/json 直接序列化 core.Event，绕过 pkg/httpx 的 UTC 强制，且该 payload 无任何机器可校验 schema
- **位置**：`service/delivery/webhook.go:9` · `service/delivery/webhook.go:235` · `core/event.go:26` · `pkg/httpx/response.go:41` · `pkg/httpx/response.go:48`
- **主题**：sibling · **runtime**：go · **effort**：M
- **同源提示**：I（operability）若从可观测/投递角度命中同一函数；D 若从 webhook 签名覆盖范围角度命中
- **修法**：三步。①把 pkg/httpx 里的 utcTimeExtension + 那个私有 jsoniter API 抽成一个独立的可复用包（如 pkg/wirejson，导出一个 Marshal(v any) ([]byte, error)），httpx 与 delivery 都消费它；service/delivery/webhook.go:235 的 json.Marshal 改调它，删掉 encoding/json 的 import。抽包而不是在 delivery 里再写一遍——再写一遍就是第三个会漂的副本。②给出站事件定 schema：在 docs/openapi.yaml 的 components.schemas 新增 OutboundEvent（与现有 Event 分开，因为它是 core.Event 的形状而非 eventResponse 的），并在 docs/api.md 的 webhook 段落引用它；同时在 core/event.go 的 Event 上加 doc comment 指明「这个类型的 json tag 即出站契约」。③顺带把 X-Ledger-Signature/X-Ledger-Timestamp/X-Ledger-Event-UID 三个 header 也写进该文档段落。
- **规则/文档要改**：api-contract.md §1 已豁免 SSE/WS 的包络但要求字段规则同样适用；建议把「出站 webhook」显式加进那句豁免的枚举里，措辞：SSE / WS / 出站 webhook 推送。
- **pin**：新增 TestWebhookPayload_TimesAreUTC：在 TZ=Asia/Singapore 下构造一个 OccurredAt 带 +08:00 的 core.Event，跑 sendHTTP（httptest 收 body），断言 occurred_at 以 Z 结尾；把 wirejson 换回 encoding/json 必须红。

### H-M5 [Major] required: 只补了 8 个 schema，7 个 2xx 响应 schema 至今全字段 optional，含 solvent / overall_passed / full_coverage 三个安全信号
- **位置**：`docs/openapi.yaml:2209` · `docs/openapi.yaml:2681` · `docs/openapi.yaml:2732` · `docs/openapi.yaml:2749` · `docs/openapi.yaml:2768` · `docs/openapi.yaml:2783` · `docs/openapi.yaml:2805`
- **主题**：gate-shape · **runtime**：docs · **effort**：M
- **同源提示**：J 若报「schema.ts 里 solvent/full_coverage 是 boolean|undefined」为同源
- **修法**：两步。①给这 7 个补 required（逐个对照 Go struct 判定哪些字段真的可缺席）：AccountPolicy 全必填；DepositTolerancePlanResult 全必填（depositToleranceResponse 无 omitempty）；BookingTrace 的 booking/events/journals 全必填；PlatformBalance 三字段全必填；SolvencyReport 五字段全必填；BalanceTrendPoint 四字段全必填；ReconcileReport 的 overall_passed/full_coverage/run_at/checks 全必填，其嵌套 checks[] 的 name/passed/complete/checked_at/findings 全必填，findings[] 的 description 必填、detail 可选（core/reconcile_extra.go:37 是 omitempty）。②在 server/openapi_contract_test.go 新增 TestOpenAPIContract_EveryResponseObjectDeclaresRequired：从 everySuccessResponseSchemaRef 出发递归下钻每个 type:object 节点（含 data、list.items、以及任意层 inline 嵌套），凡有 properties 而无 required 的一律 Fatal，错误信息里带 schema 路径。这是把 EveryRequestBodySchemaIsRegistered 那套「从产物派生完备性」的模板套到 required 上，不是新发明。
- **pin**：删掉任一 schema 的 required 行，新增的 EveryResponseObjectDeclaresRequired 必须红；特别验 ReconcileReport 的嵌套 checks[]（旧门禁不下钻，删它不会红）。
- **依赖**：H-m7

### H-M6 [Major] 分区键 created_at 与余额读路径不匹配：balance 读无分区裁剪谓词，且 populated CTE 对 holder 全历史做 DISTINCT，抵消 checkpoint+delta
- **位置**：`postgres/sql/migrations/001_baseline.up.sql:337` · `postgres/sql/migrations/001_baseline.up.sql:370` · `postgres/sql/queries/checkpoints.sql:44` · `postgres/sql/queries/checkpoints.sql:65` · `postgres/sql/queries/checkpoints.sql:170`
- **主题**：other · **runtime**：sql · **effort**：L
- **同源提示**：B 若从「Reserve 持 advisory lock 期间的余额计算时长随分区数增长」角度命中；C 若从 entry id 单调性角度命中第③步的前提
- **修法**：先量化再动结构，别直接改分区键。①先加 EXPLAIN 证据：在 postgres/benchmarks_test.go 旁新增一个建 24 个月分区的 fixture，对 ListComputedBalancesForHolders 跑 EXPLAIN ANALYZE，把 Append 下的分区数与耗时记进 docs/CAPACITY.md——目前 CAPACITY 的基线是在 4~5 个分区下测的，本条的量级完全没数据。②修 populated CTE：它只是要「这个 holder 在这个币种下碰过哪些 classification」，这份集合恰好就是 balance_checkpoints 的行集加上 watermark 之后的新维度；改成 UNION：从 balance_checkpoints 取 (account_holder, classification_id)，再 UNION 从 journal_entries 取 id > COALESCE(cp.last_entry_id,0) 的部分，把全历史 DISTINCT 降成「checkpoint 行 + 增量行」。这一步不动 schema，收益最大。③分区裁剪需要一个 created_at 下界：balance_checkpoints 已有 last_entry_at（001_baseline.up.sql:402），可以在 delta 子句里加 AND je.created_at >= cp.last_entry_at，语义上安全（id > last_entry_id 的行其 created_at 必然 >= last_entry_at，因为 id 与 created_at 同序递增），但这个前提依赖「id 全局单调且与 created_at 同序」——这一条正是 migration 008 承认只靠 ACL+单序列保证的性质，所以第③步必须等 C territory 对 entry id 唯一性/单调性的结论出来后再做，不要先落。
- **规则/文档要改**：docs/CAPACITY.md 需补一节：余额读的成本随分区数增长，基线必须在 N>=24 个分区下重测；docs/INVARIANTS.md I-5 建议补一句说明 delta 查询当前不做分区裁剪。
- **pin**：新增 TestBalanceRead_PrunesPartitions（或 bench）：建 24 个月分区后对 ListComputedBalancesForHolders 取 EXPLAIN 输出，断言 Append 子节点数远小于分区总数；改完 populated CTE 后，把它改回全历史 DISTINCT 必须让该 bench 的读行数断言红。

### H-M7 [Major] Go 导出面没有任何破坏性变更检测；core.Metrics 已 32 方法，每加一个信号都是对非嵌套实现方的静默 API 破坏
- **位置**：`readme_api_surface_test.go:80` · `core/metrics.go:24` · `core/metrics.go:93` · `core/interfaces.go:12` · `.github/workflows/ci.yml:35`
- **主题**：gate-shape · **runtime**：ci · **effort**：L
- **同源提示**：E（consumer-surface）若报「消费方实现 core 接口后被静默打断」为同源
- **修法**：三步。①在 ci.yml 加一个 apidiff job：go install golang.org/x/exp/cmd/apidiff@latest，checkout 最近一个 v* tag 到临时目录，对根模块与 chains/evm、anchors/r2 三个模块分别导出 API 快照并 diff，出现 incompatible changes 时不直接 fail 而是要求 PR 里存在 docs/BREAKING.md 的对应条目（用 grep 校验），否则红。②把破坏性变更清单从 docs/audits/2026-08-25-financial-engineering/TODO.md §10 迁到仓库根的 docs/BREAKING.md，成为发版流程的一部分而不是审计残务。③给 core.Metrics 加一个编译期防线：在 core 里放一个 var _ Metrics = NoopMetrics{} 已有，另加一句 doc 强调「消费方必须嵌入 NoopMetrics」，并在 observability/prometheus.go 的 PrometheusMetrics 上也嵌入 core.NoopMetrics（当前是全量手写实现），让库自己的实现示范正确姿势。
- **规则/文档要改**：deployment.md 的「API/事件契约」段落建议补一条：Go 库的导出面变更等同于 API 破坏性变更，必须走 expand→migrate→contract，并由 apidiff 门禁+BREAKING.md 双签。
- **pin**：apidiff job 本身即 pin：在 core.Reserver 上加一个方法且不写 BREAKING.md 条目，CI 必须红。

- **H-M8** [Major] chains/evm 与 anchors/r2 作为独立 module 外部不可消费；且 go-release.yml 不跑这两个模块的测试（ci.yml 跑） — **[并入 F-M6]**

### H-M9 [Major] core.Metrics 把内部 currencies.id 递给消费方实现并作为 Prometheus label currency_id 发布，违反 I-18 的 core 接口条款
- **位置**：`core/metrics.go:75` · `core/metrics.go:89` · `core/metrics.go:90` · `core/metrics.go:91` · `observability/prometheus.go:233` · `observability/prometheus.go:243` · `observability/prometheus.go:403` · `docs/INVARIANTS.md:790`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：I（operability）若从「Prometheus label 焊在内部主键上、重建库后 series 断裂」角度命中
- **修法**：把四个方法的 currencyID int64 改成 currencyUID string（或 currencyCode string——label 用 code 更可读且同样稳定，但 code 可变、uid 不可变，倾向 uid），observability/prometheus.go 的四处 label 名从 currency_id 改成 currency_uid 并删掉 int64Label 在这几处的调用。调用点在 service/rollup.go 与 service/reconcile*.go，那里目前手里只有 id，需要经 service.ClassificationDim/dims 反查 uid——若反查代价不可接受（在 rollup 热路径上），退而求其次：保留内部 id 不外传，把 label 改成已有的 classCode 加币种 code，由 adapter 层查一次并缓存。无论哪条，都不能让 core 接口的方法签名出现 currencies.id。注意这是 core.Metrics 的签名变更，属 H-M7 意义上的破坏性变更，需同批走 BREAKING.md。
- **规则/文档要改**：docs/INVARIANTS.md I-18 的 Enforced by / Pinned by 需补一条覆盖「core 接口的方法参数」，并把当前只覆盖 json tag 的事实写清楚（与 H-m1 合并改一次）。
- **pin**：新增 TestNoInternalIDsInCoreInterfaceSignatures：用 go/ast 解析 core/*.go 的 interface 方法参数名，凡参数名匹配 idschema.BannedKeys 派生出的 camelCase 形态（currencyID/classificationID/journalID/entryID/policyID/lastEntryID）即报错，AttestedEntry 这类有意例外走显式 allowlist 常量而不是靠看不见。
- **依赖**：H-m1, H-M7

- **J-6** [Major] docs/openapi.yaml 的 /snapshots query 参数名（currency/from/to）与 Go handler 及 TS client（currency_uid/start/end）三方不一致，且端点参数层无任何机器校验 — **[并入 H-M1]**

### J-8 [Major] M1 的 errorText 无法交付其声称的 per-instance 错误详情：httpx.Error 只发静态码表文案；message.fields 服务端无写入点、前端无映射点，全链路是死的
- **位置**：`web/packages/ledger-react/src/lib/error-message.ts:11` · `pkg/httpx/response.go:169` · `pkg/httpx/response.go:174` · `pkg/httpx/response.go:147` · `pkg/bizcode/errors.go:148` · `postgres/reserver_store.go:362` · `web/packages/ledger-react/src/client/types.ts:13` · `web/packages/ledger-react/src/client/client.ts:129`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **同源提示**：H territory：api-contract.md 逐端点核查；D territory：用户面错误文案脱敏
- **修法**：服务端：给 bizcode.AppError 增加一个「已脱敏、由 handler 显式填」的 Detail/Fields 载荷（绝不自动取 err.Error()），httpx.Error 在保持 Text=DisplayMessage(code) 的同时把它透传到 ErrorMessage.Fields；第一步先把 handler 里已经写好的用户可读文案（如 httpx.ErrBadRequest 的 reason is required）接上去。前端：src/lib/error-message.ts 不用改即可受益；另新增一个把 apiError.fields 映射到表单字段的 helper，接到 JournalsPage / TemplatesPage / CurrenciesPage 的表单校验上。若决定不实现 fields，则必须反向收口——删掉两端的死字段。
- **规则/文档要改**：api-contract.md §1 的 message.fields 目前两端都无实现：要么补实现，要么在 docs/api.md 明确标注本服务不产出 fields，不能继续留一个只有形状的契约
- **pin**：server 侧：一条 handler 测试断言 400 响应的 message.fields 含具体字段名；web 侧：一条测试断言表单字段收到 setError 并渲染错误

- **D-m5** [Minor] Config.AllowSystemClassificationPost 的 doc 声明了一个 LoadConfig 从不读的环境变量 ALLOW_SYSTEM_CLASSIFICATION_POST — **[并入 H-m5]**

- **E-m6** [Minor] server.Config.AllowSystemClassificationPost 的 godoc 声明了一个 LoadConfig 从不读的环境变量 ALLOW_SYSTEM_CLASSIFICATION_POST — **[并入 H-m5]**

### H-m1 [Minor] I-18 core 侧门禁实际只覆盖带 json tag 的字段，而 INVARIANTS 声称扫描每个导出类型；in-tree 已有无 tag 的内部 id 类型且门禁绿
- **位置**：`internal/idschema/idschema.go:141` · `internal/idschema/idschema.go:145` · `core/attestation.go:132` · `core/attestation.go:446` · `core/no_internal_ids_test.go:83` · `docs/INVARIANTS.md:834`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：F（test-credibility）若报「回归 pin 的 fixture 复现了它本该防的盲区」为同源
- **修法**：两步。①把 ScanGoFilesForBannedKeys 从正则扫 json tag 换成 go/ast 解析：遍历每个导出 struct 的每个导出字段，字段的检查键 = json tag 名（有 tag 时）或 idschema.snakeCase(字段名)（无 tag 时），后者正是 pkg/httpx 的 snakeCaseExtension 对无 tag 字段的实际行为，所以这个派生与 wire 真相一致而不是新发明。②给有意例外建显式 allowlist：在 idschema 里加一个 AllowedInternalIDTypes = map[string]bool{"core.AttestedEntry":true, "core.AttestedLeaf":true}（附理由注释指向 core/attestation.go:129-131），扫描时跳过并要求 allowlist 里的每个条目在源码里真实存在，删一个类型而 allowlist 没跟着删就红。③修 docs/INVARIANTS.md:834-838 的措辞，把 scans every exported type declared in core/*.go directly 改成与新机制一致的描述，并在 I-18 正文里把 AttestedEntry/AttestedLeaf 的 digest 例外写进规则本身。
- **规则/文档要改**：docs/INVARIANTS.md I-18：正文补 digest 输入类型的显式例外；Pinned by 段落改写覆盖范围描述（与 H-M9 合并改一次）。
- **pin**：反转 TestNoInternalIDFieldsInCoreTypes_CatchesPlantedViolation：把 fixture 的 json tag 去掉（只留 ClassificationID int64 裸字段），新扫描器必须仍然抓到——旧实现在这个 fixture 下是绿的，这就是它的证伪步骤。

### H-m10 [Minor] idempotencyHeaderAliasMiddleware 的 params 嵌套分支在当前 HTTP 面没有任何对应 request struct，是一颗按字段名撞车触发的雷
- **位置**：`server/middleware_idempotency.go:57` · `server/handler_journals.go:38` · `core/template.go:55`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **修法**：删掉 :59-62 的 target 切换分支，恒定注入到顶层（当前唯一的模板执行端点 postTemplateRequest 的 idempotency_key 就是平铺的）。若将来真要上 batch 端点，届时按端点白名单显式处理，而不是靠 body 里碰巧有个叫 params 的对象。同时删掉 :57-58 那条已经不成立的注释。
- **pin**：新增 TestIdempotencyAlias_IgnoresParamsKey：POST /journals/template 带 Idempotency-Key header、body 里额外放一个无关的 params 对象，断言键被注入顶层且请求成功——当前实现会注进 params 里并以 idempotency_key is required 失败。

### H-m11 [Minor] 响应方向门禁单向（spec ⊆ Go），Go 新增响应字段永远不会被任何机制发现
- **位置**：`server/openapi_contract_test.go:20` · `server/openapi_contract_test.go:235`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：J 若报「后端已有字段在生成类型里不存在」为同源
- **修法**：把 assertSpecSubsetOfGo 升级成带 allowlist 的双向检查：新增一个 sharedOmitemptyFields 显式表（key 为 envelope 名，value 为该 envelope 允许在 spec 里缺席的 Go 字段名集合，当前只有 journalResponse.entries 一类因多 envelope 复用而必须豁免的），除 allowlist 外「Go 有 spec 没有」一律报错。allowlist 里的每个条目必须在 Go struct 上真实存在且带 omitempty，否则也红——把文件头注释里说明的那个取舍从「整条方向关掉」收窄成「逐字段登记」。
- **pin**：给任一 *Response struct 加一个新字段而不改 openapi.yaml，双向门禁必须红；把它登记进 allowlist 后转绿，再从 Go struct 上删掉该字段则 allowlist 校验必须红。
- **依赖**：H-m7

### H-m2 [Minor] docs/openapi.yaml 的 info.version 是 0.4.0，CHANGELOG 已到 0.6.0；同文件写了 bump both together 的规则却无门禁
- **位置**：`docs/openapi.yaml:9` · `docs/openapi.yaml:16` · `CHANGELOG.md:17`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：把 docs/openapi.yaml:16 改成 0.6.0；在 server/openapi_contract_test.go 新增 TestOpenAPIContract_VersionMatchesChangelog：读 CHANGELOG.md 第一个形如 ## [X.Y.Z] 的标题，与 loadOpenAPI 的 info.version 比对，不等即红。
- **规则/文档要改**：none（规则已写在 docs/openapi.yaml:9-10，只是没有机器执行）。
- **pin**：该测试本身即 pin：把 info.version 改回 0.4.0 必须红。

### H-m3 [Minor] GET /journals 的 spec 说 descending id，实现是 ascending，且与 /holder/transactions 的 cursor 方向相反
- **位置**：`docs/openapi.yaml:114` · `postgres/sql/queries/journals.sql:96` · `postgres/query_provider.go:88` · `postgres/sql/queries/holder.sql:34`
- **主题**：other · **runtime**：sql · **effort**：M
- **同源提示**：J 若报「前端 journals 列表顺序反了」为同源
- **修法**：拍板方向后二选一。倾向改实现对齐文档与同类端点：ListJournalsCursor 改成 WHERE (cursor_id = 0 OR id < cursor_id) ORDER BY id DESC（与 holder.sql:34-36 的 page_journals 同形），nextAuditCursor 的取值随之改成最后一行的 id；这会改变 GET /journals、GET /entries、GET /audit/journals 三处的返回顺序，属 wire 行为破坏性变更，需进 BREAKING.md。若不愿动行为，则把 docs/openapi.yaml:114 的 descending id 改成 ascending id 并在 description 里写明与 /holder/transactions 方向相反。
- **规则/文档要改**：api-contract.md §6 只定了 cursor 分页的形状没定方向，建议补一句：同一 API 内所有 cursor 分页方向必须一致，默认 newest-first。
- **pin**：新增 TestListJournals_CursorOrder：插三笔 journal，断言首页第一条是最新的那笔，且带 cursor 的第二页严格更旧。

### H-m4 [Minor] 同一 API 内并存两种不分页 list 形状，holder 的两条路由用 map[string]any 直接出，绕过全部 wire struct 反射
- **位置**：`server/handler_holder.go:228` · `server/handler_holder.go:291` · `server/response.go:18` · `docs/openapi.yaml:1751` · `docs/openapi.yaml:1802` · `server/openapi_contract_test.go:370`
- **主题**：sibling · **runtime**：go · **effort**：S
- **同源提示**：J 若报「前端通用翻页 helper 在 holder 路由上拿到 undefined」为同源
- **修法**：把 handler_holder.go:228 与 :291 的 map[string]any{"list": out} 改成 PagedResponse[holderBalanceResponse]{List: out} / PagedResponse[holderHoldResponse]{List: out}（NextCursor 留 nil，序列化为 null，与其余 11 个不分页 list 端点一致）；docs/openapi.yaml 的 HolderBalanceListEnvelope(:1744) 与 HolderHoldListEnvelope(:1795) 的 data 补 next_cursor: type:[string,null] 并加入 required。这样这两条路由的 envelope 也重新落进 PagedResponse 的反射覆盖里。
- **pin**：新增 TestHolderListEnvelopes_CarryNullNextCursor：断言 /holder/balances 与 /holder/holds 的响应 data 里 next_cursor 这个键存在且为 null；同时确认 TestOpenAPIContract_NextCursorIsNullable 在补完 spec 后覆盖到这两个 envelope。

### H-m5 [Minor] Config.AllowSystemClassificationPost 的注释写了环境变量名，LoadConfig 从不读它
- **位置**：`server/server.go:185` · `server/server.go:251` · `server/server.go:294`
- **主题**：silent-default · **runtime**：go · **effort**：S
- **合并了**：E-m6, D-m5（各自报告的论证都要读）
- **同源提示**：D（threat-model）若从「文档承诺的闸门实际不存在」角度命中
- **修法**：在 LoadConfig 里补一行 allowSystemClassPost := os.Getenv("ALLOW_SYSTEM_CLASSIFICATION_POST") == "true"，并写进 :294 的 Config 字面量；同时考虑给 Validate 加一条与 DevCreditEnabled 同形的护栏（若拍板认为它只该在评审后开启，可要求 Env==dev 或至少在 newServer 里 slog.Warn——:495-497 已有 Warn，说明设计意图是可在生产开启，故只补读取即可）。
- **pin**：新增 TestLoadConfig_ReadsAllowSystemClassificationPost：t.Setenv 该变量为 true 后 LoadConfig 返回的 Config 对应字段为 true；同时补一条「Config 每个导出字段都被 LoadConfig 读或显式标注仅程序化设置」的 go/ast 门禁，防止下一个字段重蹈覆辙。

### H-m6 [Minor] settled_amount 用了 OpenAPI 3.0 的 nullable 关键字，在声明为 3.1.0 的 spec 里无意义
- **位置**：`docs/openapi.yaml:1` · `docs/openapi.yaml:2166` · `server/handler_reservations.go:58`
- **主题**：other · **runtime**：docs · **effort**：S
- **同源提示**：J 若报「生成类型里 settled_amount 的可空性与实际不符」为同源
- **修法**：Go 侧 reservationResponse.SettledAmount 是 *string + omitempty，wire 上是缺席而不是 null，所以正确修法是把 docs/openapi.yaml:2166 的 nullable:true 删掉、保持 settled_amount 不在 required 里（当前 :2160 的 required 已经正确地不含它），description 改成 absent from the wire until a settlement exists；不要改成 type:[string,null]，那会承诺一个实现不产出的值。顺带在 CI 加一个 spec lint（npx @redocly/cli lint 或 spectral），拦住下一个 3.0 遗留关键字。
- **pin**：spec lint 本身即 pin：把 nullable:true 加回去，lint 必须报 3.1 不识别的关键字。

### H-m7 [Minor] core.ReconcileReport 是唯一直接当 HTTP wire 类型使用的 core 类型，且其嵌套 checks[]/findings[] 完全不在契约门禁覆盖内
- **位置**：`server/handler_system.go:172` · `core/reconcile_extra.go:10` · `core/reconcile_extra.go:41` · `server/openapi_contract_test.go:316` · `server/openapi_contract_test.go:131` · `docs/openapi.yaml:2821`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：F（test-credibility）若报「full_coverage/complete 这两个未运行≠通过的信号本身没有契约保护」为同源
- **修法**：两步。①在 server 里加本地 wire 类型 reconcileReportResponse + reconcileCheckResponse + reconcileFindingResponse（字段与 core 一一对应），handleReconcileFull 改为经转换函数输出，responseEnvelopeCases 的 {"ReconcileReportEnvelope", core.ReconcileReport{}} 换成本地类型——领域类型不再兼任 wire 契约，与其余 67 个端点的做法一致。②让门禁下钻：把 envelopeDataObjectNames 改成递归——遇到 properties 下的 type:object 或 array.items 且是 inline 时继续下钻，把嵌套路径（如 data.checks[].complete）也纳入与 Go 嵌套 struct 字段的比对。这一步同时给 H-M5 的 required 完备性检查铺路。
- **pin**：改名 core.CheckResult.Complete 的 json tag（complete → completed）后，扩展后的门禁必须红——当前是绿的。

### H-m8 [Minor] balance_checkpoints / rollup_queue / balance_snapshots 的 currency_id 与 classification_id 无外键，而 system_rollups 有
- **位置**：`postgres/sql/migrations/001_baseline.up.sql:396` · `postgres/sql/migrations/001_baseline.up.sql:408` · `postgres/sql/migrations/001_baseline.up.sql:427` · `postgres/sql/migrations/001_baseline.up.sql:439`
- **主题**：other · **runtime**：sql · **effort**：M
- **同源提示**：A（financial-correctness）若从「孤儿行被 AggregateCheckpointsByClassification 汇总进 system_rollups」角度命中
- **修法**：新建 migration 016_checkpoint_dimension_fks：给三张表的 currency_id 与 classification_id 各加 REFERENCES currencies(id) / classifications(id)（NOT VALID 先加、再 VALIDATE CONSTRAINT，避免长锁），加之前先跑一次孤儿检测把存量清掉。若拍板认为不加（例如为了让 rollup 写入不受 classification 软删影响），则必须在 001_baseline.up.sql 的第 5 节注释里写明为什么这三张表与 system_rollups 采用不同策略——当前只有 account_policies 的通配符例外有解释（:676-681），这三张没有。
- **规则/文档要改**：none（若选不加，则是在 001 注释里补理由，不改 invariant）。
- **pin**：新增 postgres.TestCheckpointDimensions_RejectOrphanFK：往 balance_checkpoints 插一条 currency_id 指向不存在行的记录，必须被 FK 拒绝（加 FK 后）；若拍板不加，则改成一条断言 reconcile 的 orphan 检查能抓到它的测试。

### H-m9 [Minor] holder 读面三条路径都对该 holder 全历史做扫描或 DISTINCT，其中两条无分页上限
- **位置**：`postgres/sql/queries/holder.sql:27` · `postgres/sql/queries/holder.sql:92` · `postgres/sql/queries/holder.sql:110` · `postgres/sql/migrations/001_baseline.up.sql:370` · `server/handler_holder.go:275`
- **主题**：other · **runtime**：sql · **effort**：L
- **同源提示**：J 若报「holder 钱包页在数据量大时慢/无分页」为同源；I 若从容量角度命中
- **修法**：三步。①加索引：新建 migration 给 journal_entries 加 (account_holder, journal_id)（建在分区父表上，随分区继承），让 page_journals 的过滤+去重不必回表全扫。②ListHolderCurrencies 改成从 balance_checkpoints 取该 holder 的 distinct currency_id（那张表本就是每维度一行），只在 checkpoint 缺失时回落到 journal_entries，避免全历史 DISTINCT。③给 ListHolderHolds 与 ListHolderBalances 加 LIMIT + cursor：holds 用 r.id DESC 做 cursor，balances 按币种数天然有界可只加一个硬上限（如 200）并在超限时返回明确错误而不是静默截断。②③需要同步改 docs/openapi.yaml 的两个 envelope（与 H-m4 同一处改动）。
- **规则/文档要改**：api-contract.md §6 建议补一句：任何返回集合的端点必须有显式上限，无上限即视为契约缺陷。
- **pin**：新增 postgres.TestListHolderTransactions_PageCostDoesNotGrowWithHistory：为一个 holder 造 N 与 10N 两档历史，断言取第一页的扫描行数不随 N 线性增长（用 EXPLAIN ANALYZE 的 rows 或 pg_stat 统计）；拆掉新索引必须红。
- **依赖**：H-m4

### I-N15 [Minor] HTTP 访问日志与 API 错误日志绕开注入的 logger；每个 4xx 都打 Error 级；holder ID 进日志与自身策略矛盾（报告 I-15）
- **位置**：`server/middleware_logger.go:56` · `server/handler_holder.go:198` · `pkg/httpx/response.go:171` · `server/handler_deposit_reviews.go:64`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：① pkg/httpx/response.go:171 按 ae.HTTPStatus() 分级：>=500 用 Error，4xx 用 Info/Debug。当前每个 404 / 参数校验失败 / 幂等冲突都打 Error，一个扫描器打一批坏 uid 就能把 Error 级刷满，真 5xx 被埋。② 给 server.Config 加 Logger core.Logger（默认 slogadapter.New(nil)），把 middleware_logger.go:56、handler_holder.go:198、handler_deposit_reviews.go:64 三处包级 slog 换成它。⚠️ server.go:492-503 的四条危险配置警告必须保持包级 slog（故意不可静音，见 I-M11），不要一并改掉。③ handler_holder.go:198 删掉日志里的 holder 字段——它与 middleware_logger.go:32-33 自己声明的「Query strings are dropped because they may contain holder IDs」直接矛盾。
- **规则/文档要改**：middleware_logger.go:32-35 的注释需要说明哪些字段是刻意保留的（api_key name/scope 是有意的，holder 不是）。
- **pin**：新增 server 测试：用 slog test handler 捕获，打一个必然 404 的请求断言不产生 Error 级记录；再打一个必然 500 的断言产生。

- **I-N19** [Minor] server.SetReady 无人自动调用，消费方的 readiness 探针可能永远 503 且无任何提示；README 措辞暗示它是自动的（报告 I-19） — **[并入 E-M11]**

### J-14 [Minor] metadata 的 8 个 create/deactivate 发送服务端完全不读的 Idempotency-Key，而 hook 注释声称符合 api-contract §9
- **位置**：`web/packages/ledger-react/src/hooks/use-metadata.ts:7` · `web/packages/ledger-react/src/hooks/use-metadata.ts:36` · `web/packages/ledger-react/src/hooks/use-metadata.ts:50` · `web/packages/ledger-react/src/hooks/use-metadata.ts:73` · `web/packages/ledger-react/src/hooks/use-metadata.ts:110` · `web/packages/ledger-react/src/hooks/use-metadata.ts:164` · `server/handler_metadata.go:372`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **同源提示**：H territory：api-contract.md §9 逐端点核查
- **修法**：建议走后端：server/handler_metadata.go 的 4 个 create + 4 个 deactivate 真正消费注入到 body 的 idempotency_key，走与 booking/reservation 一致的 receipt 机制（financial.md 要求每个写操作有幂等键；现在靠 code 唯一约束兜底不是幂等，超时重试会真的产生第二次写尝试）。若判定这批元数据写操作不值得上幂等，则反向收口：删掉 use-metadata.ts 里这 8 处 key 与那段声称 api-contract §9 合规的注释，改注明这些端点靠 code 唯一约束去重。
- **规则/文档要改**：docs/openapi.yaml 对应端点标注是否支持 Idempotency-Key；use-metadata.ts:7-24 的注释必须与最终选择一致
- **pin**：server 测试：同 key 重放 create 返回原结果而非第二条记录；同 key 异 payload 返 ErrConflict

---

## D-ops（Wave 2）

### I-M1 [Major] core.Metrics 32 个方法里 12 个零生产调用点：整个 postgres/ 写入层没有 metrics 依赖（报告 I-1）
- **位置**：`core/metrics.go:24` · `observability/prometheus.go:106` · `core/engine.go:20` · `ledger.go:284` · `postgres/ledger_store.go:429` · `postgres/reserver_store.go:83` · `postgres/booking_store.go:163` · `postgres/idempotency_match.go:43`
- **主题**：verdict-unused · **runtime**：go · **effort**：L
- **修法**：给 postgres 层补上缺失的依赖边。① 给 postgres.LedgerStore / ReserverStore / BookingStore 各加一个 metrics core.Metrics 字段（默认 core.NopMetrics()）+ WithMetrics(core.Metrics) 链式 setter；在 ledger.New 建 store 的那一段（ledger.go:157-180）统一注入 s.metrics。② 发射点：LedgerStore.PostJournal 成功/失败出口发 JournalPosted / JournalFailed / JournalLatency / JournalEntryCount；ExecuteTemplate 失败出口发 TemplateFailed；idempotency_match.go 判出 ErrConflict 处发 IdempotencyCollision；ReserverStore.Reserve/Settle/Release 三个成功出口发 ReserveCreated / ReserveSettled / ReserveReleased；BookingStore.Transition 成功出口发 BookingTransitioned。③ ActiveReservations / ReservedAmount 是 fleet 级 gauge，不要在单次请求路径写——改由 system_rollup job 每轮 Set（复用 postgres/query_provider.go:309 已有的 CountActiveReservations / SumActiveReservations）。④ JournalFailed 的 reason 必须是代码里定义的受限常量集合，不能是自由字符串（Prometheus 基数 + RUNBOOK §7 词表要反过来引用它）。
- **规则/文档要改**：RUNBOOK §7 的 reason 词表要从「文档先写、代码没有」倒过来：代码定义常量、文档引用常量。README:559 的「~20 methods」改成 32。
- **pin**：新增 observability/emission_coverage_test.go：反射 core.Metrics 的每个方法名，机械扫描 service/ + postgres/ 的非测试源码，断言每个方法名至少有一个调用点。这条 pin 在修复前跑必须报出全部 12 个（JournalPosted / JournalFailed / JournalLatency / JournalEntryCount / IdempotencyCollision / TemplateFailed / BookingTransitioned / ReserveCreated / ReserveSettled / ReserveReleased / ActiveReservations / ReservedAmount）。

### I-M10 [Major] 三个 job 家族无任何指标；RollupItemFailed 恰好漏在最该告警的分支；ExpirationService 持有 metrics 却一次没用（报告 I-10）
- **位置**：`service/worker.go:118` · `service/worker.go:435` · `service/locked_job.go:107` · `service/locked_job.go:129` · `service/rollup.go:110` · `service/rollup.go:133` · `service/expiration.go:44` · `service/expiration.go:63`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **修法**：① 给 core.Metrics 加三个通用 job 方法：JobTickCompleted(job string)、JobTickFailed(job string)、JobTickSkippedLocked(job string)（job 取自 runLoop / NewLockedJob 已有的 name，天然是受限集合）。② NewWorker（worker.go:118 现在只取 engine.Logger()）保存 engine.Metrics()；runLoop 在 fn(ctx) 前后打点；LockedJob.Run 的未获锁分支（locked_job.go:107-111，当前只有 Info 级日志）发 JobTickSkippedLocked、fn 失败分支（:129-132）发 JobTickFailed，需要把 name 传进去。③ 修 service/rollup.go:104-112 与 :125-135：RollupItemFailed() 现在写在 ReleaseRollupClaim 成功的 else 分支里，释放 claim 也失败时（DB 抖动，恰恰是最该告警的时刻）计数器不增——移出来无条件发。④ ExpirationService 已有的 metrics 字段（expiration.go:44/63，从头到尾零使用）接上 ReserveReleased()，「过期抽走了 N 笔冻结」当前完全无指标。
- **规则/文档要改**：RUNBOOK 新增一节「某个后台 job 停止推进」的通用处置：用 increase(ledger_job_tick_completed_total{job=...}[window]) == 0 作告警，替代现在只能靠 log 的状态。一个 replica 卡死、其余每 tick 打 Info 级 skip，整个 fleet 停摆而 Prometheus 一片安静。
- **pin**：新增 service/worker_test.go：用 recordingMetrics 跑一轮，断言每个启用的 job 各产生一次 JobTickCompleted；让一个 job 失败断言 JobTickFailed{job}；用恒不给锁的 fake locker 断言 JobTickSkippedLocked。另加一条独立 pin：让 ReleaseRollupClaim 失败，断言 RollupItemFailed 仍被调用（当前会红，这条是 ③ 的证伪）。
- **依赖**：I-M1

### I-M2 [Major] README 广告的 6 个指标名 4 个不存在、2 个恒 0；方法数写 ~20 实际 32（报告 I-2）
- **位置**：`README.md:552` · `README.md:553` · `README.md:554` · `README.md:559` · `observability/prometheus.go:106`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：把 README:552-555 的六个名字换成从 observability/prometheus.go 实际注册名抄的：ledger_journal_latency_seconds → ledger_journal_post_seconds、ledger_pending_rollups → ledger_rollups_pending、ledger_balance_drift → ledger_balance_drift_units、ledger_reconcile_gap → ledger_reconcile_gap_units（另两个名字本来就对，但恒 0，依赖 I-M1）。README:559 的「~20 methods」改 32。更稳的做法是不在 README 里列名字，改成一句「完整列表见 observability.NewPrometheusMetrics」，把清单的唯一真相源留在代码里。
- **pin**：新增 observability/doc_metric_names_test.go：抽出 README.md 里所有反引号包裹的 ledger_[a-z_]+ token，逐个断言存在于 NewPrometheusMetrics() 的注册集合（用 registry.Gather() 取名字）；并断言 README 声明的方法数与反射 core.Metrics 得到的方法数一致。仓库已经为 README 的方法表建了 readme_api_surface_test.go，这条是把同一形状的门禁扩到指标名——被机器盯着的那部分是对的，没盯的 6 个错了 4 个。
- **依赖**：I-M1

### I-M3 [Major] RUNBOOK §6/§7 是为永远不会触发的告警写的 on-call 流程；§1-§7 普遍拿 Go 方法名冒充指标名（报告 I-3）
- **位置**：`docs/RUNBOOK.md:37` · `docs/RUNBOOK.md:253` · `docs/RUNBOOK.md:294` · `docs/RUNBOOK.md:398` · `docs/RUNBOOK.md:428` · `docs/RUNBOOK.md:1239`
- **主题**：other · **runtime**：docs · **effort**：M
- **修法**：两步。(a) 把 §1-§7 的 Alert source 换成真实导出名与真实标签名：ReconcileCompleted{success=false} → ledger_reconciliations_completed_total{success}；PendingRollups gauge → ledger_rollups_pending；CheckpointAge{class_code} histogram → ledger_checkpoint_age_seconds{class}（并改正它是 Gauge 不是 histogram）；IdempotencyCollision{journal_type_code} → ledger_idempotency_collisions_total{journal_type}；JournalFailed{journal_type_code, reason} → ledger_journals_failed_total{journal_type, reason}。(b) §6 / §7 / §14 的 ledger_template_failed_total 这三处，在 I-M1 落地前必须各加一行显式声明「该指标当前无发射点，本节暂不可用」——不允许留成看起来能用的样子；I-M1 落地后删除该声明。
- **规则/文档要改**：RUNBOOK §7 的 5 行 reason 表（unbalanced / unauthorised_classification / insufficient_balance / currency_mismatch / db_error）当前是一套从未被任何代码产生过的词表，且 §14 还让 on-call 跨节引用它——必须改成引用 I-M1 里新定义的 reason 常量。
- **pin**：把 I-M2 的门禁扩到 docs/RUNBOOK.md + docs/CAPACITY.md + docs/DR.md：所有文档里出现的 ledger_* token 必须在注册集合里，且文档写出的标签名必须在该 collector 的 label 集合里。
- **依赖**：I-M1, I-M2

### I-M4 [Major] P0 偿付能力那一节（RUNBOOK §2）的三条确认命令全部执行不了（报告 I-4）
- **位置**：`docs/RUNBOOK.md:211` · `docs/RUNBOOK.md:218` · `docs/RUNBOOK.md:220` · `server/routes.go:76` · `cmd/ledger-cli/main.go:259` · `server/handler_system.go:60` · `server/response.go:18`
- **主题**：other · **runtime**：docs · **effort**：M
- **修法**：逐条改。① :211 的 POST /api/v1/system/solvency → GET /api/v1/platform/solvency（server/routes.go:76、docs/openapi.yaml:1390），并删掉「(when wired)」——它是常驻路由。② :218 的 ledger-cli solvency --currency 1 → --currency <currency-uid>：cmd/ledger-cli/main.go:259 的 flag 收的是 uid（I-18 uid-only 迁移后没同步的残留）；顺带给 CLI 补一个 currencies / classifications 列表子命令，否则运维手上根本没有 uid。③ :220 的 jq 表达式 .rollups[] | select(.classification_code==...) 改成 .data.list[] | select(.classification_uid==...)：响应是 code/message/data 信封、列表键是 list（server/response.go:18-21）、字段是 classification_uid（server/handler_system.go:60-65），三处都不匹配。④ 该节标注 P0 却没有任何指标源（core.Metrics 全 32 个方法无 solvency 相关），补一行「本库不发 solvency 指标，消费方需自建定时 SolvencyCheck 并自埋指标」。
- **规则/文档要改**：none（若决定给 core.Metrics 加 solvency 指标，另立条目，不要塞进本条）
- **pin**：新增 docs 门禁：抽出 RUNBOOK/DR 里所有 GET|POST|PUT|DELETE /api/v1/... 片段对照 docs/openapi.yaml 的 paths+method；抽出所有 ledger-cli <cmd> --<flag> 片段对照 cmd/ledger-cli 各 FlagSet 实际注册的 flag 名。任一不存在就红——路径这一半（/system/solvency）修复前会被机器抓住；--currency 传 id 还是 uid 属于类型误用，机器抓不到，需人工核。

### I-M5 [Major] RUNBOOK §3 的确认 SQL 查一张不存在的表 checkpoint_rollup_queue（实际 rollup_queue）（报告 I-5）
- **位置**：`docs/RUNBOOK.md:262` · `docs/RUNBOOK.md:263` · `postgres/sql/migrations/001_baseline.up.sql:1`
- **主题**：sibling · **runtime**：docs · **effort**：S
- **同源提示**：上轮 operability / threat-model 的『RUNBOOK 假定了从未被 migration 创建的 ledger_app』——同一形态、同一根因：RUNBOOK 里的 SQL 从未被执行过
- **修法**：两行里的 checkpoint_rollup_queue 改成 rollup_queue（列名 created_at 本身是对的）。同时把该节 :253 的 Alert source「PendingRollups gauge」换成 ledger_rollups_pending（与 I-M3 合并做）。§3 是 rollup 积压，是最常被打开的一节。
- **pin**：新增 docs SQL 门禁（放 postgres 包，可复用 internal/idschema 的 migration 解析）：正则抽出 RUNBOOK/DR 里所有 SQL 片段的 FROM|JOIN|UPDATE|INSERT INTO <ident> 表名，逐个对照 postgres/sql/migrations/*.up.sql 的 CREATE TABLE 集合 + pg_ 系统目录白名单，缺一个就红。修复前跑必须报出 checkpoint_rollup_queue。

### I-M6 [Major] ledger-cli 自称 read-only，reconcile --full 会 UPSERT reconcile_scan_cursors；DR 拿这个「只读」当取证依据（报告 I-6）
- **位置**：`cmd/ledger-cli/main.go:1` · `cmd/ledger-cli/main.go:12` · `cmd/ledger-cli/main.go:249` · `service/reconcile.go:808` · `postgres/reconcile_queries.go:323` · `postgres/sql/queries/reconcile.sql:210` · `docs/DR.md:114`
- **主题**：other · **runtime**：go · **effort**：M
- **修法**：两选一，倾向 (a)。(a) 让 CLI 真的只读：给 service.FullReconciliationConfig 加 ReadOnly bool，为 true 时 runCheck2GlobalBalance 跳过全部 SetScanCursor 调用（reconcile.go:808 / :846 / :878 / :895），并在该 check 的 Finding 里声明「read-only 模式：未持久化 resume 游标，本次覆盖不计入 lap」；cmd/ledger-cli/main.go:248 传 ReadOnly:true，另加 --allow-writes 给确实要推进 lap 的场景。(b) 若保留写：删掉 main.go:1 / :12 与 DR.md:114-115 的 read-only 措辞，改成「reconcile --full 会写 reconcile_scan_cursors；对事故库取证请先克隆」，并在 DR §5 前加一步「对 evidence 库只跑 solvency / journals，不跑 reconcile --full」。注意 reconcile_scan_cursors 正是上轮 threat-model §4-3 认定的、攻击者会篡改的检测层游标——取证动作污染取证对象是本条最要紧的后果；其次是与调度中的 full_reconcile job 抢 lap 记账。
- **规则/文档要改**：docs/DR.md:114-115「this is the reason cmd/ledger-cli is read-only — point it anywhere」必须改写；cmd/ledger-cli/main.go:12「never posts journals or mutates state」同。
- **pin**：新增测试：用记录写调用的 fake ReconcileQuerier，以 ReadOnly 配置跑 RunFullReconciliation 断言 SetScanCursor 调用次数为 0；再以默认配置跑一次断言 > 0。cmd/ledger-cli 当前全包只有 24 行测试、唯一断言是 flag 文案，read-only 这个论断从未被验证过。

### I-M7 [Major] ledger-cli 的 30s 全局超时低于 reconcile 单个 check 的 2 分钟预算，大库上产出一份十几项虚假失败的报告（报告 I-7）
- **位置**：`cmd/ledger-cli/main.go:100` · `service/reconcile.go:338` · `service/reconcile.go:658` · `service/reconcile.go:808`
- **主题**：other · **runtime**：go · **effort**：M
- **修法**：① cmd/ledger-cli/main.go:100 的 30s 硬编码改成 per-command 默认 + 全局 --timeout flag：balance / journals / health 这类保持 30s，reconcile / verify / trial-balance / solvency 默认 10m 或不设 deadline。② 修 reconcile.go 的 partialReason 文案（:704 / :720 / :730）：现在无条件写「scan timed out after <Check2Timeout>」，而实际生效的是父 ctx 的 deadline——应报告真正到期的那一个，否则日志继续撒谎。③ 更根本的：reconcile.go:808 的 SetScanCursor 故意用父 ctx 而非 scanCtx（:668-671 有注释解释，该推理对长命 worker 成立、对 CLI 恰好反了）——改成 cleanupContext(ctx) 那种 detached + 有界的 context，让游标持久化不受父 ctx 取消影响。④ 父 ctx 取消导致的后续 check 失败应记成 Complete=false + 明确 Finding，而不是 Passed=false。
- **pin**：新增 service/reconcile_test.go 用例：父 ctx 给 50ms、Check2Timeout 给 2m，断言 (a) partialReason 里出现的是父 ctx 的到期原因而不是 2m，(b) 游标仍成功持久化（detached ctx），(c) 后续 check 报 Complete=false 而不是 Passed=false。当前所有 reconcile 测试都直接传 context.Background()，CLI 这一层的 ctx 从未进入任何测试。
- **依赖**：I-M6

### I-M9 [Major] worker 全部后台 goroutine 无 panic 兜底：消费方自己的 Subscribe handler panic 会打死整个进程（报告 I-9）
- **位置**：`service/worker.go:271` · `service/worker.go:435` · `service/delivery/local.go:67` · `postgres/convert.go:51` · `server/server.go:519`
- **主题**：other · **runtime**：go · **effort**：M
- **修法**：① 在 Worker.runLoop 的 fn(ctx) 调用点（service/worker.go:435）外包一层 recover：defer 里 w.logger.Error 打 job 名 + panic 值 + debug.Stack()，并发 w.metrics.JobPanicked(name)，只吞掉这一个 tick，下个 tick 继续。② delivery.LocalDispatcher.ProcessBatch（local.go:67）对每个 handler 调用单独再包一层 recover，把 panic 转成 handler error 走既有的 MarkRetry 路径——消费方的 bug 不该让整批投递停摆，也不该杀他自己的进程。③ 注意不对称的证据：同一个 mustNumericToDecimal（postgres/convert.go:51，故意 panic，方向正确）在 HTTP 路径上是 500（server.go:519 装了 middleware.Recoverer），在 worker 路径上（attestation_store.go:64/87、ledger_store.go:1212）是进程死亡。全仓生产代码 recover() 命中数当前为 0。
- **规则/文档要改**：service/worker.go:176-197 Subscribe 的 doc 补一段「handler panic 会被捕获并按 handler error 处理（计入 attempts）」；Run 的 doc（:221-250，现有 30 行完全没提 job 崩了会怎样）补「单个 job 的 panic 不终止 Worker」。
- **pin**：两条：(a) 注册一个必 panic 的 Subscribe handler，跑一轮 ProcessBatch，断言不 panic 出来且该 event 被 MarkRetry；(b) 让某个 job 的 fn panic 一次，断言 Run 不返回、下一 tick 仍执行且 JobPanicked 被计数。两条在修复前都会让测试进程直接崩掉，是很干净的证伪。
- **依赖**：I-M10

### I-R2 [Major] 上轮复核：full_coverage 库路径已修，但唯一的运维入口 ledger-cli reconcile --full 恒为 false；DR 演练误归因为「seed 脚本的问题」（报告 R-2）
- **位置**：`cmd/ledger-cli/main.go:248` · `ledger.go:434` · `service/reconcile.go:1438` · `docs/DR.md:192` · `service/reconcile_full_test.go:261` · `service/reconcile_unauthorized_journals_test.go:77`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **同源提示**：上轮 operability Major『full_coverage 永远为假』——库路径已修，运维入口未修
- **修法**：① 给 ledger-cli reconcile 加 --pubkey-hex / --key-id（与 verify 子命令同形），有值时构造 authdev.NewLocalVerifier 并经 ledger.WithAttestor(nil, verifier) 传进 ledger.New，让 unauthorized_journals 真正能运行；无值时在报告顶部显式打印一行「full_coverage=false 的原因是未提供 verifier，不是账本问题」，不要逼运维自己去 Findings 里翻。② 改 docs/DR.md:192-194：删掉「a seed-script gap, not a restore defect」的归因，改成「ledger-cli 需要 --pubkey-hex / --key-id 才能覆盖 unauthorized_journals」——这不是一次性的环境瑕疵，是通过 CLI 无论如何都提供不了 verifier。
- **规则/文档要改**：docs/DR.md:192-194 的归因必须改写（当前把产品缺陷记成一次性环境瑕疵，下一个读它的人会跳过）。
- **pin**：新增 CLI 层（或 service 层模拟 CLI 接线）测试：以 ledger.New(pool)（不带 WithAttestor）构造 FullReconciler 跑一轮，断言 FullCoverage==false 且报告里含一条明确指出 verifier 缺失的顶层说明；再以带 verifier 的接线跑一轮断言 FullCoverage==true。现有两条 pin（reconcile_full_test.go:266、reconcile_unauthorized_journals_test.go:77）都只覆盖后者，且都做了 CLI 做不到的接线——这正是契约 §1.4「测试做了消费方不会做的准备」。
- **依赖**：I-M6

### B-m10 [Minor] failed_attempts >= 10 的 rollup_queue 项永久卡住：没有任何 API 能 drain 或复位，而 CountPendingRollups 不带该过滤 → PendingRollups 指标永久 > 0，等于一个永远在响的告警
- **位置**：`postgres/sql/queries/checkpoints.sql:129` · `postgres/sql/queries/checkpoints.sql:86` · `postgres/sql/queries/integrity_checkpoint.sql:57` · `service/rollup.go:151`
- **主题**：silent-default · **runtime**：sql · **effort**：M
- **同源提示**：I(operability)：可告警性——这条我从「上轮 RebuildCheckpoint 修复的遗留」角度发现，落地归 I
- **修法**：两件事：(1) 把 CountPendingRollups 拆成两个指标——CountPendingRollups 加 AND failed_attempts < 10（可告警的「真的在排队」），新增 CountStuckRollups（failed_attempts >= 10，单独一个 core.Metrics gauge，如 StuckRollups）。这样「排队积压」与「需要人介入」两个信号不再互相淹没（与 integrity_checkpoint.sql:57 已经采用的同款口径一致）。(2) 加一个运维入口 ResetRollupClaim(ctx, id)（把 failed_attempts 归零、claimed_until=NULL），经 cmd/ledger-cli 暴露为只读之外的唯一写操作，或至少在 RUNBOOK 里给出手工 SQL——目前一个卡住的维度既不会被 dequeue、也没有任何 API 能救它。
- **规则/文档要改**：docs/RUNBOOK.md 补一节「卡住的 rollup 项怎么诊断与复位」；core/metrics.go 加 StuckRollups 的 doc。
- **pin**：新增测试：把一条 rollup_queue 项的 failed_attempts 置为 10，断言 CountPendingRollups 返回 0 且 CountStuckRollups 返回 1；去掉过滤必须红。

### B-m12 [Minor] LockedJob.Run 在 fn 执行期间从不复核自己是否还持有锁；连接被服务端切断即静默失去单飞保证，而 sweep 是唯一没有 DB 层兜底的 job（nonce 序列竞争只靠这把锁挡）
- **位置**：`service/locked_job.go:96` · `service/locked_job.go:33` · `service/onchain.go:1997` · `chains/evm/sweeper.go:70`
- **主题**：gate-shape · **runtime**：go · **effort**：L
- **同源提示**：G(onchain-money-path)：同 nonce 双广播的资金后果（一笔静默覆盖另一笔、卡住的一侧 gas-bump 成 ErrConflict）归 G 评估
- **修法**：给需要强单飞的 job 加 DB 层兜底，不要只依赖 advisory lock 的存活。sweep 这条的正确兜底是让 nonce 分配本身可仲裁：在库里加一张 sweep_nonce_claims(chain_id, signer_address, nonce) 带 UNIQUE(chain_id, signer_address, nonce)，sweepTick 在广播之前先 INSERT 该 nonce，23505 即说明另一个副本已占用该 nonce → 本 tick 放弃。这样即使 advisory lock 静默丢失，两个副本也不会在同一 nonce 上各广播一笔（对照 ledger_attestations.seq 的 UNIQUE 与 entry_attestations 的 PK——attestation job 正是靠这个在锁丢失时降级为报错而非污染）。其余 job 已有兜底（expiration 靠幂等 receipt、snapshot/system_rollup 是 upsert、partition 是 DDL），无需改动。另可选：给 LockedJob 加一个可选的心跳复核（每 N 秒在持锁连接上 SELECT 1 + 查 pg_locks 确认仍持有），失败则 cancel fn 的 ctx。
- **规则/文档要改**：docs/INVARIANTS.md 若有 sweep 单飞相关条目，需把「advisory lock 是唯一防线」改成「advisory lock + nonce claim 唯一索引双保险」；service/onchain.go:1984-1996 newSweepLockedJob 的长注释同步。
- **pin**：新增测试：不取任何 advisory lock，直接并发跑两次 sweepTick（同链同 policy），断言恰好一侧成功、另一侧因 nonce claim 冲突放弃且不广播；去掉 UNIQUE 索引必须红（表现为两笔同 nonce 广播）。

### E-m16 [Minor] 16/26 个 postgres store 不调 normalizeStoreError，core.IsRetryable 的『单一事实源』在这些路径上靠 default:true 兜底（PLAUSIBLE：未逐个坐实哪个永久性错误被误判）
- **位置**：`postgres/errors.go:19-70` · `postgres/pending_store.go` · `postgres/reversal_fraction_store.go` · `postgres/checkpoint_integrity_store.go` · `postgres/verified_balance_store.go` · `postgres/holder_store.go` · `core/errors.go:115-117`
- **主题**：gate-shape · **runtime**：go · **effort**：L
- **修法**：(1) 先做机械门禁再改代码：新增 postgres 包测试，用 go/ast 扫所有 *_store.go 里返回 error 的导出方法，断言每条 return 路径上的 error 要么经 wrapStoreError/normalizeStoreError，要么 %w 了一个 core 哨兵。当前会列出 16 个文件 —— 这张清单就是工作量。(2) 按清单把 pending_store / reversal_fraction_store / checkpoint_integrity_store / verified_balance_store / holder_store（money-path 优先）改成走 wrapStoreError。(3) 顺带复核 core.IsRetryable 的 default: true 是否该保留 —— 它对未归一路径是 fail-open：永久性约束违反会被判成可重试，消费方的退避循环会一直撞。归一覆盖到 100% 之后可以考虑把 default 翻成 false，但那是另一次拍板。
- **规则/文档要改**：docs/INVARIANTS.md 错误分类那条（配合 E-M9）增补：『postgres adapter 返回的每个 error 都必须落在 core 哨兵空间内 —— 未归一的裸 driver error 不是合法返回值』。
- **pin**：上述 go/ast 门禁本身；改完之前它会红，改完之后新增一个不归一的 store 方法会立刻红。

### F-m12 [Minor] docs/CAPACITY.md 的基准数字未编造但已漂移；表里也漏了一个真实存在的 benchmark
- **位置**：`docs/CAPACITY.md:9` · `docs/CAPACITY.md:14` · `postgres/benchmarks_test.go:29` · `postgres/benchmarks_test.go:82` · `postgres/benchmarks_test.go:126`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：四行表格全部对应真实存在的 benchmark 函数，没有编造的函数名 —— 这条只是维护性修正，不是失实。实测（本机，非文档声明的 M3 Max）：PostJournal_SingleAccount 3.08 ms/op / 17389 B / 259 allocs（文档 ~2.5 ms / ~13 KB / 288 allocs）；GetBalance_ColdCheckpoint 0.80 ms/op / 3857 B / 71 allocs（文档 ~0.7 ms / ~4 KB / 86 allocs）。量级一致、alloc 数已漂移。修法：在参考主机上重跑 go test ./postgres/ -bench=. -benchtime=3s -run='^$' 刷新四行，并把 BenchmarkListComputedBalancesForHolders 补进表；同时在表头标注采集时的 commit（数字没有 commit 锚点就无法判断漂移是代码变了还是机器变了）。
- **规则/文档要改**：CAPACITY.md §1 表头加「measured at <commit>」；文档已自述「use these for relative comparison」，口径本身不需要改
- **pin**：none（性能数字不适合做门禁；加 commit 锚点即可让下一个人自己判断新鲜度）

### I-N12 [Minor] 库模式（Worker.Subscribe）的事件投递零指标，而 CAPACITY 拿这些指标定 SLO；且全仓无事件队列深度 gauge（报告 I-12）
- **位置**：`service/delivery/local.go:55` · `service/delivery/webhook.go:78` · `docs/CAPACITY.md:68` · `core/queries.go:61` · `postgres/query_provider.go:298`
- **主题**：verdict-unused · **runtime**：go · **effort**：S
- **修法**：① NewLocalDispatcher 加 metrics core.Metrics 参数（Worker.Subscribe 在 worker.go:203 构造它时传 w.metrics），在 ProcessBatch 的三个出口发 EventDelivered / EventDeliveryFailed / EventDead，与 webhook.go:174/203/211/213 完全对齐。② 给 core.Metrics 加 PendingEvents(count int64)，由 event_delivery 与 event_callback 两个 job 每轮 Set；同时给 core.HealthMetrics 加 EventQueueDepth 字段并在 postgres/query_provider.go:298 的 GetHealthMetrics 里补一条 count，让 RUNBOOK §5 不必只能手写 SQL。
- **规则/文档要改**：CAPACITY.md:68 的 Event delivery SLO 补一句「库模式（Worker.Subscribe）同样发射这两个计数器」——只有修完才成立。
- **pin**：用 recordingMetrics 跑一轮 LocalDispatcher，断言成功 / 失败 / dead 三条路径各发一次；再加一条对等性断言：webhook 与 local 两条投递路径对同一批事件发出的指标集合必须相同（skin-parity 式），任一路径漏发就红。
- **依赖**：I-M1

- **I-N13** [Minor] 死信表与两张取证表写得进、读不出、文档不提：ingest_dead_letters / config_table_changes / reconcile_scan_cursor_changes 零读取路径（报告 I-13） — **[并入 D-M5]**

### I-N16 [Minor] OTEL span 携带金额与 holder 外发到 APM 供应商，无脱敏层、无文档、无开关（报告 I-16）
- **位置**：`postgres/ledger_store.go:430` · `postgres/reserver_store.go:83` · `pkg/otel/tracing.go:29` · `README.md:562`
- **主题**：other · **runtime**：go · **effort**：M
- **同源提示**：territory D threat-model：OTEL 外泄面可能被 D 从威胁模型角度独立命中
- **修法**：给 pkg/otel 加属性策略：otel.SetAttributePolicy(p)，默认 PolicyMinimal——在 ledgerotel.StartSpan 内部按策略过滤，丢掉 amount / actual_amount / account_holder / actor_id，保留 uid 类与受限枚举；PolicyFull 由消费方显式打开。在 StartSpan 里过滤而不是逐个改 16 个调用点。现状统计供参考：account_holder×7、idempotency_key×6、amount 与 actual_amount×4、actor_id×2。README:562-566 的 tracing 段补一句「默认不上报金额与 holder；需要时显式 PolicyFull，并确认你的 APM 供应商的信任层级」——当前那段写的是 automatic / No injection needed，等于消费方为任何目的配了全局 tracer 的那一刻，逐笔金额就开始外流。
- **规则/文档要改**：README tracing 段措辞；另可在 docs 里补一条说明「trace 也是一个外发表面」（user-facing-surfaces.md 的同族视角）。
- **pin**：新增 pkg/otel 测试：用 in-memory span exporter 跑一次 PostJournal 与 Reserve，断言默认策略下导出的 span 属性里不含 amount / account_holder；显式设 PolicyFull 后断言含。

### I-N17 [Minor] CAPACITY 头号 SLO 与头号扩容信号挂在永不写入的 histogram 上；三个被引用的告警规则名全仓不存在（报告 I-17）
- **位置**：`docs/CAPACITY.md:5` · `docs/CAPACITY.md:29` · `docs/CAPACITY.md:64` · `docs/CAPACITY.md:73` · `docs/RUNBOOK.md:370`
- **主题**：sibling · **runtime**：docs · **effort**：S
- **修法**：① CAPACITY.md:5 删掉对 Helm metrics.prometheusRules.thresholds 的引用（chart 已随 C1 30bd872 删除，ls deploy 不存在）。② :29 的 LedgerRollupBacklog / LedgerCheckpointAgeHigh 与 RUNBOOK.md:370 的 LedgerEventDeliveryDead 三个规则名全仓找不到定义——改成「消费方自建的告警（本库不发货告警规则，见 RUNBOOK §14）」并直接给出触发表达式，而不是给一个搜不到的规则名。③ :64 / :73 的 ledger_journal_post_seconds 在 I-M1 落地前加「当前无发射点」标注，落地后即生效。
- **pin**：并入 I-M3 的文档指标门禁；另加一条：文档里出现的 Ledger[A-Z]\w+ 形态的告警规则名，必须能在仓库里找到定义，否则红（当前会报出这三个）。
- **依赖**：I-M1, I-M3

### I-N18 [Minor] DR §5 的验收指令与 CLI 实际输出不符（must print PASS），且 check 数在演练次日就漂了（13 vs 现在 15）（报告 I-18）
- **位置**：`docs/DR.md:121` · `docs/DR.md:190` · `service/reconcile.go:470` · `cmd/ledger-cli/main_test.go:18`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：① DR.md:121 的「must print PASS on every check」改成可执行判据：用 jq 断言 .data.checks[] 里没有 passed 不为 true 的项，且 .data.overall_passed 为 true——CLI 输出的是 jsonOut(report)，从不出现 PASS 字样，演练记录自己（:190-191）写的也是 passed: true。② DR.md:190 的「all 13 runnable checks」改成不含数字的措辞（「每一个 runnable check」），理由与 reconcileFullFlagUsage 常量完全相同：演练在 bb5a583（08-26），untagged_holder_kind 在 2ec4f16（08-27）加入，次日就漂了。
- **pin**：把 cmd/ledger-cli/main_test.go:18 的 staleCheckCountPattern 抽成共享的 docs 门禁，扫 docs/*.md 里「<数字> checks」这一形态的出现。现有 pin 只盯 reconcileFullFlagUsage 这一个字符串常量，同样的硬编码数字立刻在 DR.md 里重现——这是典型的门禁形状太窄。

### I-N21 [Minor] 假的 drift 守卫：注释声称能检测漂移，实际只是让未使用的 import 编译过（报告 I-21）
- **位置**：`cmd/ledger-cli/main.go:425` · `cmd/ledger-cli/main.go:426` · `cmd/ledger-cli/main.go:427` · `service/onchain.go:1255`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：删掉 cmd/ledger-cli/main.go:425-427 三行并同时删掉 strings 与不再需要的 import——注释里「keep imported types referenced so the file fails fast if any drift」是虚构的，它不检测任何东西。service/onchain.go:1255 的 `_ = cfg // cfg reserved for future per-chain scan tuning` 同理：要么在 scanChainOnce 里真的用上 cfg，要么把 cfg := o.chains[chainID] 一起删掉（golang.md：不允许「先造再丢」）。
- **pin**：不需要新增回归测试，go vet + 编译本身就是判据；若要门禁，给 delivery-gate 加一条静态断言：禁止 `_ = <ident>` 后跟注释这一形态出现在非测试 Go 文件里。

### I-N22 [Minor] CLAUDE.md 的 File Layout 指向已删除的 deploy/helm/ledger/，make docker 的说明与 Makefile 不符（报告 I-22）
- **位置**：`CLAUDE.md:54` · `CLAUDE.md:149` · `Makefile:26`
- **主题**：sibling · **runtime**：docs · **effort**：S
- **同源提示**：与 I-N17 同源：C1 删掉 deploy/ 之后，多份文档里的引用没有一起清
- **修法**：CLAUDE.md:149 删掉 deploy/helm/ledger/ 那一行（目录已随 C1 30bd872 删除）；CLAUDE.md:54 的 `make docker # docker compose up --build` 改成 `make docker # docker compose up -d postgres`，与 Makefile:26 一致。CLAUDE.md 是 agent 与新人的第一张地图，指向不存在的目录会让人以为自己 checkout 不完整。
- **pin**：加一条 docs 门禁：CLAUDE.md 的 File Layout 表里每个路径必须在文件系统上存在（目录或文件），缺一个就红。

### I-R4 [Minor] 上轮复核：RUNBOOK §14 表里混进一个永不发射的指标，两个确实会发射的链上故障指标反而缺席（报告 R-4）
- **位置**：`docs/RUNBOOK.md:1213` · `docs/RUNBOOK.md:1239` · `service/onchain.go:915` · `service/onchain.go:1514`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：§14 的两张表：① 删掉或明确标注 ledger_template_failed_total（在 I-M1 落地前它永不发射）。② 补进两个确实会发射却缺席的链上指标——ledger_sweep_address_unreadable_total{chain_id}（service/onchain.go:1514，「本轮有地址余额读不到，被排除出可归集集合」）与 ledger_deposit_review_required_total{chain_id, reason}（service/onchain.go:915，M3 补偿控制的核心信号；§13 有整节处置流程却没把它列为告警源，两处要交叉链接）。
- **pin**：并入 I-M3 的文档门禁，并加一条反向断言：observability/prometheus.go 注册的每一个故障类指标（*_failed_total / *_detected_total / *_required_total / *_unattributed_total / *_lag_*）都必须在 docs/RUNBOOK.md 里至少出现一次，缺一个就红——这样新增指标不会再默认失去处置说明（上轮链上六指标零 RUNBOOK 就是这么来的）。
- **依赖**：I-M3

### I-R5 [Minor] 上轮复核：DR.md 仍挂着一条已被否决的开放建议（改 migration 008 注释），与 INVARIANTS 记录的决定矛盾（报告 R-5）
- **位置**：`docs/DR.md:280` · `docs/DR.md:286` · `docs/INVARIANTS.md:3474` · `postgres/sql/migrations/008_journal_entries_id_sequence_only.up.sql:27`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：把 docs/DR.md:280-286 的「Recommendation: migration 008's comment should be corrected ... Left to Team Lead per this task's instructions to report before touching code」改写成已决状态：「已拍板：migration 008 的注释不改（deployment.md：已落地的 migration 永不编辑），更正记录在 docs/INVARIANTS.md I-42 的 Correction (2026-08-26) 段」。⚠️ 只改 DR.md，不要动 migration 008 与 INVARIANTS——后两者已经是正确的最终状态（我核过：I-42 的撤回声明有正式 Correction 段落、给了实测数字、并说明注释故意保留；CHANGELOG 无残留）。只读 DR.md 的人会去改 migration 008，这是唯一的风险面。
- **规则/文档要改**：docs/DR.md:280-286 的措辞。
- **pin**：无需回归测试。若要门禁：docs 里出现「Left to Team Lead」「Recommendation:」这类未收口标记时提醒复核。

---

## D-lock（Wave 2）

### B-M5 [Major] ClosePeriod 不取任何锁也不设屏障、关账闸是 READ COMMITTED 下的一次普通读 → I-15「没有任何 journal 的 effective_at 早于活跃关账线」在并发下不成立，且无任何对账检查能事后发现
- **位置**：`postgres/period_close_store.go:50` · `postgres/ledger_store.go:951` · `postgres/ledger_store.go:968` · `postgres/sql/queries/periods.sql:8` · `docs/INVARIANTS.md:609`
- **主题**：gate-shape · **runtime**：go · **effort**：L
- **同源提示**：A(financial-correctness)：历史报表可否被事后改动、越线 journal 的资金层面影响
- **修法**：给关账线一个真正的互斥屏障：定义一个固定的 period-close advisory key（如 hashtextextended('period:close', 0)，与 bal:/idem: 前缀空间天然不相交），在 postJournalWithQueries 读 GetActivePeriodClose 之前取 pg_advisory_xact_lock_shared(periodKey)，在 PeriodCloseStore.ClosePeriod 的 INSERT 之前取排他 pg_advisory_xact_lock(periodKey)。共享锁不影响记账并发度（多个 journal 可同时持有），排他锁让 ClosePeriod 天然等到所有 in-flight journal 落地或回滚。新增两条 sqlc 查询 AcquirePeriodReadBarrier / AcquirePeriodCloseBarrier 放 postgres/sql/queries/periods.sql，注释里写清为什么是 shared/exclusive 而不是行锁（journals 侧不写 period_closes，行锁没有排他效果）。**独立地**再补一条对账检查（service/reconcile.go 的 full suite 里加 period_close_violations）：统计 journals 中 effective_at < 当前活跃 close_before 且 created_at > 该 close 行 created_at 的行数，非零即报——屏障再严也需要一个能证伪它的观测量。
- **规则/文档要改**：改 docs/INVARIANTS.md I-15：Enforced by 目前只写「reads the active close line inside the same transaction as every write path」，这句不足以支撑全称命题（ClosePeriod 不参与任何锁，同事务读没有排他效果）。加上 advisory shared/exclusive 屏障与新对账检查，并把 Pinned by 里现有 6 条全为单线程顺序断言这件事明确掉。
- **pin**：新增并发测试：T1 开事务走完 PostJournal(effective_at=过去) 到写 entries 但不 commit；T2 ClosePeriod(close_before=未来) 并 commit；T1 commit；断言 T1 必须失败（ErrPeriodClosed）或 T2 必须阻塞到 T1 结束。另加一条 reconcile 检查的 pin：手工插一条越线 journal，断言 period_close_violations 报非零。拆掉屏障两条都必须红。

### B-m2 [Minor] event delivery 的 MarkDelivered/MarkRetry 用可能已取消的 ctx，未走 cleanupContext —— 与 cleanup_context.go 自己写的「every release-on-the-way-out call site」不一致
- **位置**：`service/delivery/local.go:74` · `service/delivery/local.go:82` · `service/delivery/webhook.go:172` · `service/cleanup_context.go:18`
- **主题**：sibling · **runtime**：go · **effort**：S
- **同源提示**：I(operability)：关停窗口内 webhook 集中重投，值得进 RUNBOOK
- **修法**：cleanupContext 目前是 package service 的私有函数，delivery 是子包，拿不到。两个做法：(a) 把它提到一个共享内部包（如 internal/ctxutil）由两边 import；(b) **推荐**：在 service/delivery 里加一个同款的私有 cleanupContext（context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)），并在 service/cleanup_context.go 的 doc 里交叉引用说明为什么有两份（包边界，不是漂移）。然后把 local.go:74/:82 与 webhook.go:172 以及 deliverEvent 里其余 Mark*/RecordDeliveryStatus 调用点全部换成 cleanup ctx。注意 sendHTTP 仍必须用原 ctx（关停时该被取消），只有「记录已发生的结果」这一步用 detached ctx。
- **规则/文档要改**：改 service/cleanup_context.go 的 doc comment：明确「every call site」现在包含 service/delivery 的 Mark* 三处，并说明包边界导致有两份实现。
- **pin**：新增测试：用一个已取消的 ctx 调 LocalDispatcher.ProcessBatch（poller 用 fake，Deliver 立即成功），断言 MarkDelivered 仍被调用且成功；换回原 ctx 必须红。

### B-m3 [Minor] journals.sql 的残余风险注释断言「其他 single-key 调用方都用非阻塞 pg_try_advisory_lock，绝不可能死锁」——migrate.go:147 是阻塞式 session 级 pg_advisory_lock，是反例；真正的隔离理由（按 database 隔离）注释一个字没提
- **位置**：`postgres/sql/queries/journals.sql:183` · `postgres/migrate.go:147` · `postgres/migrate.go:24` · `postgres/migrate.go:133`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：改 AcquireBalanceLock 的残余风险段（journals.sql:183-199）：把「every one of those other callers uses the non-blocking pg_try_advisory_lock」换成正确理由——PostgreSQL 的 advisory lock 按 database 隔离（I-47 的整个论证就建立在这条性质上），而 acquireClusterLock 连的是集群的 postgres 维护库，因此它与应用库里的 bal:/idem:/job: 三套 key 物理上不在同一锁空间；并把 migrate.go:147 显式列为「全仓唯一的阻塞式 session 级 advisory lock」这个已知例外，附一句边界：若把账本装进 postgres 维护库本身，两组 key 就会共享同一 64-bit 空间，那时「不可能成环」不再成立。改完 make sqlc 重生成（注释会同步进 sqlcgen）。
- **规则/文档要改**：改 postgres/sql/queries/journals.sql 的 AcquireBalanceLock doc 注释（sqlcgen 同步）；docs/INVARIANTS.md I-47 可加一句指回这条。
- **pin**：无法用测试钉住注释正确性。替代：加一条静态断言测试，grep 全仓 pg_advisory_lock（非 _try_、非 _xact_）的出现次数必须恰好为 1 且在 postgres/migrate.go——新增第二个阻塞式 session 锁就红，迫使作者回来重读这段推理。

### B-m4 [Minor] acquireClusterLock 用 context.Background() + 阻塞式 pg_advisory_lock，无超时、无 ctx、无日志：一个卡住的 Migrate 让同集群所有 Migrate 永久静默挂起
- **位置**：`postgres/migrate.go:136` · `postgres/migrate.go:147` · `postgres/migrate.go:50` · `postgres/migrate.go:47`
- **主题**：other · **runtime**：go · **effort**：M
- **同源提示**：I(operability)：启动期无诊断 + postgres 维护库 CONNECT 前置条件
- **修法**：在取锁的连接上先 conn.Exec(ctx, "SET lock_timeout = '10s'")，然后把 pg_advisory_lock 换成有界重试循环：每次 attempt 失败（55P03 lock_not_available）就打一条 Info「waiting for cluster migration lock, attempt N」并重试，累计超过一个总预算（如 5 分钟）后返回错误，错误文案指明「另一个 Migrate 正在同集群运行，或上一次 Migrate 的连接未被回收」。或直接改用 pg_try_advisory_lock 轮询（同款可观测性）。同时给 Migrate 加一个带 ctx 的兄弟入口 MigrateContext(ctx, databaseURL) 并让 Migrate 转发 context.Background()，这样调用方能取消——按 deployment.md expand 走，不改 Migrate 现有签名。
- **规则/文档要改**：docs/RUNBOOK.md 的「Database roles」段补上：Migrate 现在硬依赖对集群 postgres 维护库的 CONNECT，且等锁有超时上限与日志；卡住时怎么查（pg_locks 里 locktype='advisory' 且 objid 对应 2573143714）。
- **pin**：新增测试：从一个独立连接先取 pg_advisory_lock(2573143714)（连 postgres 库）并保持，然后调 Migrate，断言它在有界时间内返回明确错误而不是永久阻塞（用 -timeout 短值或 assert.Eventually）。去掉超时必须红（表现为测试超时）。

### B-m5 [Minor] dimCache 按 pool 全局共享且只在 miss 时 refresh；tx 模式下 refresh 走调用方事务，会把未提交的配置行写进共享缓存，事务回滚后污染永不自愈（hit 不复核）
- **位置**：`postgres/dims.go:53` · `postgres/dims.go:112` · `postgres/dims.go:70` · `postgres/dims.go:6` · `ledger.go:594`
- **主题**：other · **runtime**：go · **effort**：M
- **修法**：两个做法，二选一：(a) **推荐**：给 dimCache 记住它的 pool，tx 模式下 refresh 走 pool 上另开的只读连接而不是调用方的 *sqlcgen.Queries——这样缓存里只会出现已提交行；实现上把 dimCacheFor 返回的结构加一个 pool 字段，refresh 时若传入的 q 是 tx-bound 则改用 pool 建一个临时 Queries。(b) tx-bound clone 用自己的一次性 dimCache（WithDB 里 dims: &dimCache{} 而不是 s.dims），代价是每个 RunInTx 至少一次全表 refresh。无论选哪个，都要改 postgres/dims.go:6-9 的包注释——它目前只论证了「缓存的字段不可变」，完全没论证「缓存条目的存在性」，而后者才是这个 bug 的所在。
- **规则/文档要改**：改 postgres/dims.go 的包注释 Cache safety 段：补「未提交行的可见性」这一维度。
- **pin**：新增测试：svc.RunInTx 回调里 CreateCurrency 然后按 uid 解析它（触发 refresh），回调返回 error 让事务回滚；随后在池上用同一 uid 调 GetBalance/PostJournal，断言返回 core.ErrNotFound 而不是外键错误。改回让 refresh 走调用方事务必须红。

### B-m9 [Minor] I-3「Every state-changing operation requires an idempotency_key」在配置写路径上仍是空头承诺：SetPolicy / ClosePeriod / Create* / Deactivate* 全无幂等键
- **位置**：`docs/INVARIANTS.md:100` · `core/account_policy.go:55` · `core/period_close.go:22` · `core/interfaces.go:282` · `core/interfaces.go:356` · `core/interfaces.go:391` · `core/interfaces.go:414`
- **主题**：gate-shape · **runtime**：docs · **effort**：M
- **修法**：不建议给配置写加幂等键（它们是 upsert 或 append-only，语义上自幂等，加键是纯成本）。建议改 I-3 的措辞，把范围说准：把「Every state-changing operation」改成「Every operation that moves money or advances a money-bearing state machine（journals / reservations / bookings / pending / 三个终态操作 / Transition）」，并新增一段明确列出**不要求**幂等键的类别（配置写：分类/币种/模板/账目类型的创建与停用、账户策略 upsert、期末关账 append）以及为什么（upsert / append-only 自幂等）。同时给 server/middleware_idempotency.go 的 doc 补一句：它是 header→body 别名，对没有 idempotency_key 字段的端点（如 /periods/close）注入是无害但也无效的，不要误读成「这些端点也幂等了」。
- **规则/文档要改**：改 docs/INVARIANTS.md I-3 的第一句与 Enforced by 段；改 server/middleware_idempotency.go 的 doc comment。
- **pin**：加一条静态断言测试：反射枚举 core 包里所有 *Input 类型，要求「带 IdempotencyKey 字段」的集合与 I-3 新措辞里列出的白名单完全一致——新增一个 money-path Input 忘了加键就红，新增一个配置 Input 也不会误报。

---

## D-surface（Wave 2）

### E-M10 [Major] server.NewFromDeps（为修『23 个位置参数 + panic』而加）全仓零使用；README / RUNBOOK / examples/fullstack 仍全部教 NewWithConfig
- **位置**：`server/server.go:441-446` · `server/server.go:400-433` · `README.md:188` · `README.md:192` · `docs/RUNBOOK.md:780` · `examples/fullstack/backend/main.go:12` · `examples/fullstack/backend/main.go:220-246`
- **主题**：verdict-unused · **runtime**：go · **effort**：S
- **合并了**：H-R1（各自报告的论证都要读）
- **修法**：(1) examples/fullstack/backend/main.go:220-246 改用 server.NewFromDeps(cfg, server.Deps{...})，并把 :244-246 的 panic(err) 改成向上 return error（run() 已经是 error 返回）；文件头 :12 的示范清单同步改名。(2) README:188-193 换成 NewFromDeps 的示例并说明为什么（21 个同形接口参数位置错位编译不报错）。(3) docs/RUNBOOK.md:780 同步。(4) 给 server.New / server.NewWithConfig 的 godoc 加 // Deprecated: prefer NewFromDeps —— README:856 的弃用政策说 post-v1.0 才需要保留一个 minor，现在是 v0.x，但加 Deprecated 标记本身零成本且能让 IDE 提示。
- **规则/文档要改**：README:856 的弃用政策措辞不变；若决定直接删 NewWithConfig 则属破坏性变更，需进 CHANGELOG（见 E-M6）。
- **pin**：新增静态断言（可放进 server 包或 delivery-gate）：examples/ 与 README.md 中不得出现 server.NewWithConfig( —— 唯一允许的装配入口是 NewFromDeps。

### E-M12 [Major] 没有任何 example 演示 Authorize + PostAuthorized —— RunInTx 里保住可验证性的唯一写法零示范，而两个规范示例仍在教会毒化 VerifiedBalance 的写法
- **位置**：`examples/credits-topup/main.go:181-198` · `docs/COOKBOOK.md:249-262` · `ledger.go:463-476` · `README.md:481-492` · `postgres/ledger_store.go:363-370`
- **主题**：verdict-unused · **runtime**：go · **effort**：M
- **修法**：(1) 给 examples/tamper-evident 增加一步（它已经有 WithAttestor 与 anchor）：演示svc.AuthorizeTemplate(ctx, code, params) 在 RunInTx 之前，回调里 tx.JournalWriter().PostAuthorized(ctx, authorized)，跑完断言该 journal 的 auth_status=signed 且该维度 VerifiedBalance 仍可解 —— 与它现有的『unsigned_tx_mode 会让 VerifiedBalance UNDEFINED』形成对照。参考实现已存在：service/onchain.go:964-990。(2) examples/credits-topup:181-198 与 docs/COOKBOOK.md Recipe 4 各加一段注释，明说这里用的是 ExecuteTemplate-in-tx、该 journal 会是 unsigned_tx_mode、如果你启用了 WithAttestor 并打算用 RequireVerifiedBalance 提款闸，就要改成 Authorize + PostAuthorized，并指向 (1) 的 example。(3) README:481-492 已经讲对了，补一句『可运行示例见 examples/tamper-evident』。
- **规则/文档要改**：docs/INVARIANTS.md I-32 / I-40 内容不变，但『如何在 RunInTx 下保住可验证性』需要从 godoc 提升到有可运行示范。
- **pin**：新增 root 包或 examples 层测试：WithAttestor 下走 Authorize + PostAuthorized 落一笔 journal，断言 auth_status=signed 且 VerifiedBalanceReader 对该维度返回具体金额而非 ErrUnauthorizedJournal；同一测试里用 ExecuteTemplate-in-tx 落另一笔，断言该维度转为 UNDEFINED。两个方向都钉住。

### E-M2 [Major] README:419-431『Add a custom lifecycle』示例编译得过但运行必失败；必填字段 balance_role 在 README 与 COOKBOOK 里 0 命中
- **位置**：`README.md:419-431` · `README.md:243` · `README.md:378-385` · `core/interfaces.go:328-330` · `core/types.go:169-205` · `docs/COOKBOOK.md:0`
- **主题**：gate-shape · **runtime**：docs · **effort**：M
- **修法**：(1) README:419-431 的 ClassificationInput 补 BalanceRole: core.BalanceRoleMemo（kyc_review 是流程记录不是负债桶），并在代码块下加一句说明为什么是 memo。(2) README:243 的 Core Concepts 表给 Classification 补上 BalanceRole 列/描述（当前只写了 NormalSide + 可选 Lifecycle）。(3) 在 README『Extending the Ledger → Add a custom classification』段新增一小节讲四个取值的选择标准：available=Reserve 唯一会花的桶、pending=待确认入款、locked=已锁定、memo=不计入用户负债的备查/成本账户；并链到 docs/INVARIANTS.md I-25 / I-37。(4) docs/COOKBOOK.md Recipe 2b/3 里凡是建自定义分类的地方同样补 balance_role。
- **规则/文档要改**：README:243 Core Concepts 表措辞必须改（当前描述遗漏了一个必填字段）。docs/INVARIANTS.md I-25/I-37 内容不变，但需要从 README 可达。
- **pin**：把 readme_api_surface_test.go 扩成（或新开）一个 README 代码块可执行门禁：抽取 README 里标注 ```go runnable 的代码块，在 testcontainers 起的空库上真跑一遍。至少覆盖 Quick Start Tier1/Tier2 与 Extending 三段。当前 README:419-431 会红。

### E-M6 [Major] CHANGELOG 的 [0.6.0] 破坏性清单漏掉全部 Go API 层破坏性变更，且 61177f9 之后已无指针指向完整的 23 条清单
- **位置**：`CHANGELOG.md:17-313` · `CHANGELOG.md:636` · `docs/plans/2026-08-27-release-readiness.md:32-64` · `core/interfaces.go:168` · `core/interfaces.go:187` · `core/reserve.go:119-123` · `core/booking.go:81-84`
- **主题**：verdict-unused · **runtime**：docs · **effort**：M
- **修法**：(1) 在 CHANGELOG [0.6.0] 的 Removed/Changed 段补回六条（每条一行 + 消费方要做什么）：core.Reserver.Release 签名 (ctx,string)->(ctx,ReleaseInput)；core.Reserver.FinalizeSettlement 同形；core.SettleInput.IdempotencyKey 新增且 Validate 为空即拒；core.TransitionInput.IdempotencyKey 同上；core.RollupQueueItem 移出 core（->service）；core.BalanceCheckpoint 字段从内部 id 改为 uid 且去掉 LastEntryID。注意：Aaron 在 61177f9 拍板删掉的是『要改数据』那两条的升级须知，与此不冲突 —— 这里补的是 API 层，不要把被删掉的那段搬回来。(2) 恢复一行指针：『完整的 23 条按消费方动作分类见 docs/plans/2026-08-27-release-readiness.md §3』。(3) CHANGELOG:636 的 [Unreleased] 链接 compare 基准从 v0.2.0 改到 v0.6.0，并新建一个真正的 [Unreleased] 段落收纳 v0.6.0 之后的 18 个提交（含 anchors/r2，见 E-m18）。
- **规则/文档要改**：none（文档补齐，不改 invariant）
- **pin**：新增 CI 门禁 changelog_breaking_test.go 或一个 make 目标：对 git 上一个 vX.Y.Z 与 HEAD 之间的 ledger.go + core/**.go 做导出符号 diff（函数签名 + 类型字段），任何删除或签名变更若在 CHANGELOG 当前版本段里 grep 不到该符号名就失败。这正是 readme_api_surface_test.go 已验证有效的机械派生手法。

### E-M7 [Major] examples/event-subscribe:95-97 仍教 at-most-once（『返回 error 也会标记 delivered』），而 godoc 已改成 at-least-once
- **位置**：`examples/event-subscribe/main.go:95-97` · `service/worker.go:176-186` · `service/delivery/local.go:19-24` · `README.md:30` · `README.md:794` · `docs/RUNBOOK.md:348`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：(1) 把 examples/event-subscribe/main.go:95-97 的注释换成与 service/worker.go:181-185 一致的at-least-once 表述，并明写『handler 必须按 event UID 幂等；返回 error 不代表没有副作用』。(2) 让这个 example 真的演示幂等：handler 里维护一个 seen map[string]bool 按 evt.UID 去重，并加一次故意返回 error 的分支，跑出『同一个 event 被投递两次、只生效一次』。(3) README:30 的 feature bullet 与 README:794 的 example 说明各补一句交付语义（at-least-once），对齐 docs/RUNBOOK.md:348 对 webhook 投递的既有措辞。
- **规则/文档要改**：docs/INVARIANTS.md 若无覆盖『事件投递语义』的条目，建议新增一条：『webhook 与 in-process 两条投递路径的交付语义相同（at-least-once），且该命题在 godoc / example / README 三处措辞一致』。
- **pin**：新增一个跨文件的措辞一致性测试（cheap）：grep examples/ 与 README.md，断言不存在 'still marked delivered' / 'at-most-once' 字样；外加 service/delivery 层已有的 MarkRetry 行为 pin 保持不变。

### E-m10 [Minor] 八个 example 对同一个 USDT 声明的精度不一致（18 vs 6），现在会互相硬失败，而 README 的 example 清单没说要各用各的库
- **位置**：`examples/embed/main.go:193` · `examples/billing/main.go:193` · `examples/event-subscribe/main.go:0` · `examples/tx-compose/main.go:193` · `examples/credits-topup/main.go:238` · `examples/fullstack/backend/main.go:291` · `README.md:787-800`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：(1) 统一所有 example 的 USDT Exponent 为 6（USDT 链上真实精度就是 6；当前 5 个 example 用 18，只有 fullstack 用 6 —— 用 6 更诚实，也顺带让 examples 之间可共库）。(2) README:787-800 的 example 清单加一句总纲：『每个 example 建议指向自己的空数据库；它们会创建同名的 currency/classification，精度或配置不一致时会主动报错而不是静默接受』。(3) 各 example 头注释里的 DATABASE_URL 示例库名统一成 ledger_example_<name>。
- **pin**：delivery-gate 静态断言：examples/**/main.go 里所有 core.CurrencyInput{Code: "USDT"} 的 Exponent 字面量必须相同。

### E-m11 [Minor] examples/tx-compose 的示范表存账本内部 currency_id（硬编码 1），与 I-18 和 README:106 自己的 uid-only 主张相反
- **位置**：`examples/tx-compose/main.go:82-89` · `examples/tx-compose/main.go:130-133` · `README.md:106`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：把 demo_orders 的 currency_id BIGINT 改成 currency_uid TEXT NOT NULL，插入时传 currencyUID （:71 已经拿到了）而不是字面量 1。这个 example 的全部意义是示范『怎么把自己的表和账本写进同一个事务』，所以它的表结构就是示范。（同文件上一轮的『用 svc.DBTX() 跑 DDL』已修，:77-80 改用 pool.Exec 并写了理由 —— 这一处没扫到。）
- **pin**：扩展 internal/idschema 那套机械派生门禁的适用范围：新增断言 examples/**/main.go 的 CREATE TABLE 语句里不得出现 `_id` 结尾的、指向账本维度的列名（currency_id / classification_id / journal_id）。

### E-m12 [Minor] readme_api_surface_test.go 的覆盖面比它 doc comment 宣称的窄：只匹配整份 README 任意位置，且完全不管包级导出面（含 WithAttestor）
- **位置**：`readme_api_surface_test.go:100` · `readme_api_surface_test.go:14-26` · `ledger.go:100-134` · `ledger.go:141` · `ledger.go:620` · `idempotency.go:17`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：(1) 把匹配范围收窄到 README 的 §API Surface 段（从 `## API Surface` 到下一个 `## `），而不是整份文档 —— 当前 45/45 都恰好在段内是巧合，把某个方法挪到 Quick Start 散文里测试照样绿。(2) 扩展到包级导出面：扫 root 包里所有导出的顶层 func 与 Option 构造器（New / Migrate / NewIdempotencyKey / WithLogger / WithMetrics / WithAttestor），同样要求在 README 里可 grep。WithAttestor 是整个 P5 签名体系的唯一入口，从 README 消失不会有任何东西红。
- **规则/文档要改**：readme_api_surface_test.go 的 doc comment 需同步改（它现在描述的覆盖面大于实际）。
- **pin**：就是这条测试自身的改造；改完先人为把一个包级函数从 README 删掉验证它会红。

### E-m18 [Minor] CHANGELOG [0.6.0] 的『已知敞开项』说外部锚定没有生产实现，但 anchors/r2 已在 29b0c00 落地且 README:815 把它列为生产载体；无 [Unreleased] 段，compare 基准还停在 v0.2.0
- **位置**：`CHANGELOG.md:19-21` · `CHANGELOG.md:636` · `README.md:802-844` · `README.md:815` · `docs/plans/2026-08-27-release-readiness.md:70-72` · `docs/RUNBOOK.md:920-929`
- **主题**：verdict-unused · **runtime**：docs · **effort**：S
- **同源提示**：C: anchors/r2 的生产就绪度与 svc.Worker 自动接线永远传 nil anchor 的降级形态
- **修法**：(1) 新建 CHANGELOG `## [Unreleased]` 段，收纳 v0.6.0 之后的 18 个提交，重点两条：anchors/r2（Cloudflare R2 + Object Lock 的 core.Anchor 实现，独立 Go module）与 anchortest.RunConformance（I-48）。(2) [0.6.0] 的『已知敞开项』那句保留原样（它对 0.6.0 是真的），但在 [Unreleased] 段里明确写『该敞开项已由 anchors/r2 部分关闭』。(3) 同步 README:815 与 docs/RUNBOOK.md:920-929 的 MJ-7 口径 —— README 目前把 anchors/r2 无保留地列为『What ships』，而 RUNBOOK 诚实地写了它还不能 go get；README 必须带上这个限定。(4) CHANGELOG:636 的 [Unreleased] 链接 compare 基准 v0.2.0 改成 v0.6.0。
- **pin**：随 E-M6 的 CHANGELOG 门禁一起：断言 HEAD 与最新 tag 之间若有新增的顶层目录或新 Go module，CHANGELOG 必须有 [Unreleased] 段提及。
- **依赖**：E-M6

### E-m19 [Minor] docs/api.md 与代码的 wire 漂移：/system/ready 的 503 body 写成 {"status":"starting"}，两处 next_cursor 示例写成空串
- **位置**：`docs/api.md:884` · `docs/api.md:900` · `server/handler_system.go:48` · `server/response.go`
- **主题**：other · **runtime**：docs · **effort**：S
- **同源提示**：H: openapi/api.md 与 Go handler 的三方一致性；next_cursor 与探针信封
- **修法**：docs/api.md:884 的 503 示例改成统一信封 {"code":18101,"message":{"text":"..."},"data":null}；两处 "next_cursor": "" 改成 "next_cursor": null。两条都是 D-contract 已经改过、且 CHANGELOG 专门提醒消费方改监控/分页判断的变更，长文档没跟上。
- **pin**：扩展 server/openapi_contract_test.go 的思路到 docs/api.md：至少把 api.md 里的 JSON 示例块解析出来，断言 next_cursor 若出现则为 string|null 而非空串，且探针失败示例含 code 字段。

### E-m2 [Minor] 三处数量型文档断言过期：invariants 34→48、core.Metrics ~20→32、openapi 56/59→59/97
- **位置**：`README.md:778` · `CLAUDE.md:150` · `README.md:559` · `core/metrics.go:20` · `README.md:782`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：README:778 与 CLAUDE.md:150 的『34 invariants』改 48（或改成不写数字：『docs/INVARIANTS.md 列出的全部不变式』—— 推荐，从源头消灭这类漂移）。README:559 的『~20 methods』改 32 并去掉『intentionally narrow』（32 个方法不 narrow，core/metrics.go:13-23 自己已经改口说 intentionally wide + 建议 embed NoopMetrics），同时把 NoopMetrics 可嵌入这件事写进 README。core/metrics.go:20 的『30 empty method bodies』改 32。README:782 的 56 paths/59 schemas 改 59/97。
- **pin**：新增 docs 门禁：机械计数 docs/INVARIANTS.md 的 ^## I-N 标题数、core.Metrics 接口方法数、docs/openapi.yaml 的 paths/schemas 数，逐一比对 README 与 CLAUDE.md 里写的数字。上一轮 preset bundle 数就是同一形状，值得一次做成通用断言。

### E-m20 [Minor] RequireVerifiedBalance 提款闸在 docs/api.md 与 docs/COOKBOOK.md 里 0 命中，只读长文档的消费方不知道它存在
- **位置**：`docs/api.md:0` · `docs/COOKBOOK.md:0` · `server/handler_reservations.go:26` · `docs/openapi.yaml:2143` · `core/reserve.go:61-73`
- **主题**：verdict-unused · **runtime**：docs · **effort**：S
- **修法**：(1) docs/api.md 的 POST /reservations 段补 require_verified_balance 字段说明 + 一个被拒绝时返回 14010 的示例。(2) docs/COOKBOOK.md 新增一小段（或并进 Recipe 5 的 cash-out）：提款路径应当同时 (a) 用 svc.CheckpointIntegrity().RecomputeBalance 而不是 BalanceReader.GetBalance，(b) 传 RequireVerifiedBalance: true，并指向 examples/tamper-evident。（HTTP 表面与 openapi、TS 类型上一轮已补齐，只剩这两份长文档。）
- **pin**：docs 门禁：断言 core.ReserveInput 上每个带 json tag 的字段名都在 docs/openapi.yaml 与 docs/api.md 里可 grep 到。

### E-m3 [Minor] README 与 CLAUDE.md 都还列着 30bd872 已删除的 deploy/helm/ledger/
- **位置**：`README.md:734` · `CLAUDE.md:149`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：删掉两处条目。（我对两份文档里的全部路径做过机械存在性检查，只有这一条不存在。）
- **pin**：新增 docs 门禁：解析 README Architecture 目录树与 CLAUDE.md File Layout 表里的所有 `path` 反引号项，逐个 os.Stat，不存在即红。

### E-m4 [Minor] README『Local Development with go.work』照抄会直接失败：go 1.26.1 被拒、go.work 只列 2/5 个 module、且声称 go.work 是 git-ignored（实际被 track）
- **位置**：`README.md:50-51` · `README.md:62` · `README.md:65` · `README.md:66-67` · `.gitignore` · `go.mod:3` · `go.work`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：(1) README:51 与 :65 的 go 1.26.1 改成 1.26.6，与 go.mod:3 一致（实测 1.26.1 报 `requires go >= 1.26.6`，第一个代码块就跑不动）；更好的做法是写『与 ledger/go.mod 的 go 指令同版本』并让门禁校验。(2) README:53-57 的外层 go.work 补齐 ./ledger/chains/evm、./ledger/anchors/r2、./ledger/anchors/r2/internal/miniotest 三个 module（仓库自带的 go.work 有五个）。(3) 删掉 README:66『The workspace file is git-ignored by convention』—— go.work 与 go.work.sum 都在 git 里、.gitignore 没有对应条目；同时把 :62 的 `rm ledger/go.work` 改成『外层 go.work 会覆盖内层，无需删除内层（它是被 track 的）』。(4) 顺带补一句为什么不能用 replace：根 go.mod:28 require 了 internal/postgrestest 的伪版本、其 replace 是相对路径不传递，消费方 go mod tidy 会报 `invalid version: unknown revision 000000000000`。（go get 已发布 tag 不受影响，我实测过。）
- **pin**：docs 门禁：断言 README 里出现的 `go 1.x.y` 版本号与 go.mod 的 go 指令一致；断言 README 的 go.work 示例 use 块条目数与仓库 go.work 一致。

### E-m5 [Minor] README『## Configuration』在描述一个已不存在的服务入口：4 个变量库里无人读，4 个 LoadConfig 真读的变量不在表里
- **位置**：`README.md:860-877` · `README.md:184-186` · `server/server.go:249-311`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：把表头『The service entry point reads:』改成『server.LoadConfig() reads:』，然后按 server/server.go:252-292 逐字校对表格内容：删除 DATABASE_URL / HTTP_PORT / MIGRATE_MODE / EVM_WEBHOOK_SECRET 四行（前两个只有 example 自己读，MIGRATE_MODE 全仓 0 命中，EVM_WEBHOOK_SECRET 只在注释里）；新增 HOLDER_TOKEN_SECRET（>=32 字节，holder token 签名密钥）、DEV_CREDIT_ENABLED（凭空造余额，Validate 要求 ENV=dev）、PROTECTED_TEMPLATE_CODES、ALLOW_GENERIC_TEMPLATE_POST（后两个决定 I-38 那道闸开在哪）。DATABASE_URL/HTTP_PORT 可以另起一小段说明『这两个由你的 composition root 自己读，见 examples/fullstack』。
- **规则/文档要改**：README:860 表头措辞必须改（当前描述的主体不存在）。
- **pin**：新增 server 包测试：用 go/ast 扫 server/server.go 里 LoadConfig 内所有 os.Getenv 字面量，断言集合与 README Configuration 表里的变量名集合完全相等（双向，多写少写都红）。

### E-m7 [Minor] docs/COOKBOOK.md:270 的 Release(rsv.UID) 用的是 W1-A 已改掉的签名
- **位置**：`docs/COOKBOOK.md:270` · `core/interfaces.go:168`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：改成 `Release(ctx, core.ReleaseInput{ReservationUID: rsv.UID, IdempotencyKey: releaseKey})`。（COOKBOOK 的 21 个 Go 代码块签名我逐个核过，其余全对；只有这一处，且因为它写在散文里，任何『抠代码块编译』的门禁都抓不到 —— 所以顺带建议把它改写进一个真正的 ```go 代码块。）
- **pin**：随 E-M2 的『README/COOKBOOK 代码块可执行门禁』一起覆盖（前提是把这行改成代码块）。
- **依赖**：E-M2

### E-m8 [Minor] InstallPendingBundle 在 README 与 docs 里 0 命中，而 pending 两阶段充值是 README Features 的头牌
- **位置**：`README.md:27` · `README.md:602-603` · `ledger.go:295-301` · `presets/templates.go:344-353` · `presets/pending.go:87-99`
- **主题**：verdict-unused · **runtime**：docs · **effort**：S
- **修法**：(1) README:602-603 的两行 API 表描述各补一句『需先 presets.InstallPendingBundle(ctx, ...)；InstallDefaultPresets 与 InstallExtendedPresets 都不含它』。(2) README『Built-in Presets』段的两个便捷安装器下面补一行 pending 的单独安装示例。(3) README:27 的 feature bullet 加同样的括注。（facade godoc ledger.go:295-301 与 postgres/pending_store.go:33-37 已经说对了，只有消费方文档没跟。失败是响的 ErrNotFound，故 Minor。）
- **pin**：扩展 E-m1 那条 preset 一致性测试：断言仓库里定义的每个 *Bundle() 要么在某个 Install*Presets 里，要么在 README 里有显式的单独安装说明 —— 当前 PendingBundle 与 DevCreditBundle 都不在任何便捷安装器里，DevCredit 有 README:611，Pending 没有。

### E-m9 [Minor] examples/credits-topup 里两个模板 helper 仍把任意错误当 not found，而同文件第三个已修成 errors.Is 形态
- **位置**：`examples/credits-topup/main.go:258` · `examples/credits-topup/main.go:287` · `examples/credits-topup/main.go:311-318`
- **主题**：sibling · **runtime**：go · **effort**：S
- **修法**：把 :258 与 :287 改成与同文件 :311-318 一致的三分支形态：if _, err := svc.Templates().GetTemplate(ctx, code); err == nil { return nil } else if !errors.Is(err, core.ErrNotFound) { return fmt.Errorf("get template %s: %w", code, err) }。（其余 example 的 ensureCurrency / ensureJournalType / ensureClassification 我逐个核过，全部已修；只剩这两个。同一文件里两种写法并存是最容易被照抄错的残留。）
- **pin**：delivery-gate 静态断言：examples/**/main.go 里不得出现 `; err == nil {` 之后紧跟 return 且无 errors.Is 分支的形态（或更简单：examples 里出现 err == nil 时必须在 5 行内出现 errors.Is）。

- **H-R1** [Minor] 上轮修复复核：NewFromDeps 已落地但唯一的完整装配示例仍用 22 参数的 NewWithConfig，且旧构造器未标 Deprecated — **[并入 E-M10]**

---

## D-tests（Wave 2）

### F-M3 [Major] 四个导出的资金相邻方法整体换成 no-op，go test ./... 仍全绿（ExpireStaleBookings / EnsureUpcoming / CheckAndBackfillOnStartup / MergeWithLive）
- **位置**：`service/expiration.go:129` · `service/partition.go:40` · `service/snapshot_backfill.go:115` · `postgres/snapshot_extra_store.go:125` · `postgres/partition_store.go:117` · `core/snapshot_extra.go:41`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **同源提示**：concurrency [Minor] EventStore.SetLogger 全仓零调用点（同形：加了机制没接线 / 无覆盖）
- **修法**：四条各补一个断言具体结果的测试，不要只断 NoError：(1) ExpireStaleBookings —— 仿 service/expiration_test.go:90 已有的 ExpireStaleReservations 四例，造一个 expires_at 已过且当前状态有 expired 出边的 booking，断言返回 count==1 且 booking.Status=='expired'，再断言第二次调用返回 0（幂等键复用）；(2) EnsureUpcoming —— DB-backed：删掉未来月分区后调用，断言 pg_class 里补齐；再往 default 分区塞一行（复用 postgres/partition_store_test.go:98-129 的 clone 手法）后调用，断言 DefaultPartitionHasRows 变 false（这条同时覆盖 F-m7 的 RebalanceDefault）；(3) CheckAndBackfillOnStartup —— 断言零快照时会真的 backfill（SnapshotsCreated>0 且逐日 balance 值正确），有快照时不动；(4) MergeWithLive —— DB-backed：造 N 天历史快照 + 当天未 rollup 的 journal，断言返回 N+1 条且最后一条等于实时余额（当前返回 nil,nil 无人发现）。
- **规则/文档要改**：这四条今天没有任何 invariant 覆盖。建议至少给 MergeWithLive 与 EnsureUpcoming 各挂一条（前者是消费方可见的余额历史读面，后者是 I-13 的 service 层实现）
- **pin**：四个函数首行插 return <zero>，新测试必须逐个变红（当前四条同时 no-op 而全套绿，已实跑）

### F-M4 [Major] I-6 声称「Postgres 用 NUMERIC(30,18)」，但没有任何测试读过 information_schema 的 numeric_precision/scale；金额列改成 NUMERIC(20,8) 或 DOUBLE PRECISION 不会有任何门禁变红
- **位置**：`docs/INVARIANTS.md:306` · `docs/INVARIANTS.md:322` · `core/journal_invariants_test.go:110` · `postgres/schema_migrations_test.go:79` · `postgres/schema_migrations_test.go:147`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **同源提示**：financial-correctness 的「NUMERIC(30,18) 与 currencies.exponent：声明精度 vs 存储精度」一节（相邻：它核的是签名 scale 与存储 scale 一致，没有报「schema 无门禁」）
- **修法**：仿 internal/idschema 给 I-18 做的机械派生，新增 postgres/precision_schema_test.go：(a) 查 information_schema.columns，断言每一个金额列（判定规则从 postgres/sql/migrations/*.up.sql 机械派生，而不是硬编码表名单：匹配 data_type='numeric' 的列）的 numeric_precision==30 且 numeric_scale==18；(b) 断言全库不存在 data_type IN ('double precision','real','float') 的列（financial.md 红线，当前零门禁）；(c) 新表/新列若无法判定归属，fail-closed（照 grant_coverage_test.go:227 的 t.Fatalf 形状），不要默认放过。现状核实：24 处 NUMERIC(30,18)、零处非 30,18 的金额列、零处浮点列 —— 今天是对的，缺的只是门禁。
- **规则/文档要改**：I-6 的 Pinned by 增列该新测试；正文里「Enforced by: Schema: every amount column is NUMERIC(30,18) NOT NULL」这句从「靠人记得」升级为机器可校验
- **pin**：新增一条 migration 把 journal_entries.amount 改成 NUMERIC(20,8)（或加一个 DOUBLE PRECISION 列），新测试必须变红

### F-M5 [Major] chains/evm 的 e2e 带 //go:build e2e，全仓无任何地方传 -tags e2e —— CI 不执行、go build/go vet 也不编译，唯一覆盖 sweep 签名广播全链的测试处于编译期盲区
- **位置**：`chains/evm/e2e_test.go:1` · `chains/evm/e2e_artifacts.go:1` · `chains/evm/e2e_test.go:202` · `.github/workflows/ci.yml:68` · `.github/workflows/ci.yml:12` · `.github/workflows/ci.yml:104` · `Makefile:7`
- **主题**：gate-shape · **runtime**：ci · **effort**：M
- **合并了**：G-M9（各自报告的论证都要读）
- **同源提示**：onchain-money-path [Major] chains/evm 的真链语义只由一个 CI 从不执行的 e2e 测试覆盖（同源；我补的增量证据：go build/go vet 也不编译，且我实跑过它 PASS —— G 明说没跑）
- **修法**：两件事分开做，编译门禁优先（成本近零、收益最大）：(1) 编译期：给 ci.yml 的 lint 与 build job 的 chains/evm 步骤加 -tags e2e（go vet -tags e2e ./... / go build -tags e2e ./...）—— 我已实跑 go vet -tags e2e ./... exit 0，立刻就能挂上，保证这两个文件不会静默腐烂；(2) 执行期：在 ci.yml 的 test job 里加 foundry-rs/foundry-toolchain 安装 anvil，再加一步 go test -tags e2e -race -timeout 5m -count=1 ./...（working-directory: chains/evm）。我已实跑 go test -tags e2e -run TestE2E -v：--- PASS: TestE2E_WatchThenSweep (2.43s)，代码今天还是好的。同时把 -tags e2e 写进 Makefile（新 target test-e2e）与 CLAUDE.md 的 Key Commands，否则本地也没人会跑。
- **规则/文档要改**：ci.yml:63-67 那段「submodule 对根模块 ./... 不可见所以要单独跑」的注释应扩写一句 build tag 的同形陷阱；CLAUDE.md 的 Gotchas 段补 -tags e2e
- **pin**：e2e 一旦进 CI，它自己就是 pin。另建议加一条极便宜的元门禁：扫全仓 //go:build 标签，任何非 ignore 的自定义 tag 若在 .github/workflows 与 Makefile 里都零命中就 fail —— 防止下一个 tag 重蹈

### F-M6 [Major] go-release.yml 的 verify 只跑根模块 vet/build/test，缺 lint、sqlc diff、govulncheck 与两个子模块；且 anchors/r2 / chains/exm 的 tag 不匹配 v* filter，子模块发版零 gate
- **位置**：`.github/workflows/go-release.yml:17` · `.github/workflows/go-release.yml:44` · `.github/workflows/go-release.yml:9` · `.github/workflows/ci.yml:87` · `.github/workflows/ci.yml:63`
- **主题**：sibling · **runtime**：ci · **effort**：M
- **合并了**：H-M8（各自报告的论证都要读）
- **同源提示**：structure-and-contract [Major] chains/evm 与 anchors/r2 作为独立 module 不可被外部消费；且发版流水线不跑它们的测试（同源，H 侧还含 replace 指令导致消费方版本解析失败这一半）；structure-and-contract「ledger-react-publish.yml 补 codegen:check 修复成立，但同形缺陷在 go-release.yml 上原样存在」（同源，且已指出这是上轮修复未横向扫描）
- **修法**：根治做法：把 ci.yml 的五个 job 抽成一个 reusable workflow（workflow_call），ci.yml 与 go-release.yml 都 uses: 它 —— 这样两个出口在结构上不可能再分叉，而不是把 step 复制第二遍（复制就是这条发现的成因）。若嫌重构大，最小修法是把 lint / sqlc-diff / vulncheck / 两个子模块的 test+build step 补进 go-release.yml 的 verify job。另外给子模块加 tag-driven 发版：on.push.tags 增加 'anchors/r2/v*' 与 'chains/evm/v*'，各自在自己目录跑全套 verify。
- **规则/文档要改**：go-release.yml 头部注释「re-runs the full verify gate so a tag can never point at code that doesn't pass CI」当前为假，必须随修复一起改（或在修好后才成立）
- **pin**：加一条元门禁测试：解析 .github/workflows/ci.yml 与 go-release.yml，断言 release 的检查集 ⊇ CI 的检查集（按 job 名或 run 命令集合比对）。去掉任一 step 就红 —— 这是唯一能防止第三次分叉的形状

- **G-M9** [Major] chains/evm 的真链语义只由一个 CI 从不执行的 e2e 测试覆盖 — **[并入 F-M5]**

### C-R2 [Minor] I-46（微秒截断）的钉子依赖运行平台的时钟分辨率，在 macOS 上可能恒绿
- **位置**：`core/auth.go:125-162` · `docs/INVARIANTS.md:3896-3947`
- **主题**：other · **runtime**：go · **effort**：S
- **同源提示**：test-credibility：平台依赖的绿灯
- **修法**：给 I-46 补一条不依赖 time.Now() 的确定性钉子：构造一个显式带纳秒余数的 effectiveAt（如 time.Unix(0, 1700000000123456789).UTC()），断言 CanonicalJournalDigest(input, t) 与 CanonicalJournalDigest(input, t.Truncate(time.Microsecond)) 字节相同；再加一条 DB 往返断言（用该 effectiveAt 签名并落库，读回后重算 digest 必须与存储 digest 相等）。这两条在任何平台上都能证伪，去掉 canonicalTimestamp 调用即红。
- **规则/文档要改**：none；I-46 的 Pinned by 里注明新钉子不依赖平台时钟分辨率。
- **pin**：如上：core.TestCanonicalJournalDigest_IgnoresSubMicrosecondDigits 与 postgres.TestPostJournal_SignedAtSubMicrosecondEffectiveAtStillVerifies。

### F-P10 [Minor] 附录 pin 表判定【部分】I-10：把 journals.event_id 恒置 nil，I-10 自己的两条 pin 都不红，抓到的是 I-25 的 TestJournalsGuard_EventIDSetOnce
- **位置**：`docs/INVARIANTS.md:391` · `postgres/ledger_store.go:1002` · `postgres/audit_store_test.go:1` · `postgres/integration_test.go:1`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：I-10 声称「events 与 journals 共享一个事务」，是双向链接。现有两条 pin（TestAudit_TraceBooking / TestIntegration_FullLedgerFlow）覆盖了 events.journal_id 那一半，journals.event_id 那一半只被 I-25 的守卫测试偶然抓到。修法：在 TestIntegration_FullLedgerFlow（或新加一条）里补断言 —— 一次带 EventID 的 PostJournal 之后，SELECT event_id FROM journals WHERE uid=... 必须非 NULL 且解析回同一个 event uid（与 service/onchain_integration_test.go:1405-1418 已有的反向断言对称）。或者更省：把 TestJournalsGuard_EventIDSetOnce 一并列进 I-10 的 Pinned by，并在正文说明两个方向各由谁 pin。
- **规则/文档要改**：I-10 的 Pinned by 增列 journals.event_id 方向的 pin（或显式说明该方向由 I-25 覆盖）
- **pin**：把 postgres/ledger_store.go:1002 的 EventID 恒置 nil，I-10 名下至少一条 pin 必须红（当前为零，已实跑）

### F-P11 [Minor] 附录 pin 表判定【部分】I-11：与 I-4 共用同一条空洞并发 pin
- **位置**：`docs/INVARIANTS.md:415`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：同 F-m1：从 I-11 的 Pinned by 删掉 postgres.TestReserverStore_Reserve_Concurrent。I-11 其余 6 条（含 TestReserve_AvailableBasisExcludesPendingLockedAndRoleless）是真断言，不动。
- **规则/文档要改**：I-11 Pinned by 删一项
- **pin**：无需新测试
- **依赖**：F-m1

### F-P12 [Minor] 附录 pin 表判定【部分】I-12：三条 pin 全是 happy-path「平衡」断言，没有「检测到真实全局失衡」的正面证伪；并订正报告统计行的 off-by-one
- **位置**：`docs/INVARIANTS.md:492` · `service/reconcile.go:478`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：I-12（全系统钱守恒）的三条 pin（TestMoneyConservation_Network / TestCheck4AccountingEquation_Balanced / TestReconciliationService_BalancedSystem）都断言「系统是平的」。负向由 I-24 的 TestFullReconciliation_JournalBalance_DetectsPerJournalDrift 间接补，但 check1（global_dr_cr_equality）在 FullReconciliationService.RunFullReconciliation 这一层没有对应的 ..._DetectsGlobalImbalance 集成测试 —— check2/3/4/5/6/7/9/10 / systemRollupIntegrity / snapshotIntegrity / unauthorizedJournals 全部有「注入 drift 后断言 Passed=false」的测试，唯独 check1 没有。修法：新增 TestFullReconciliation_DetectsGlobalImbalance，用裸 SQL 注入一条单腿 entry（绕过应用层），断言完整报告里 check1 的 Passed=false 且 Findings 含该维度。⚠️ 同时订正：test-credibility.md 附录末尾的统计行写「部分 5（I-4 / I-10 / I-11 / I-13 / I-32+33）」，漏计了 I-12 —— 实际是 6 组。表格行本身（I-12 标 ⬜ 部分）是对的，统计行 off-by-one。
- **规则/文档要改**：I-12 的 Pinned by 增列该负向测试；test-credibility.md 附录统计行「部分 5」改「部分 6」
- **pin**：让 runCheck1JournalBalance 的 r.Balanced → result.Passed 映射恒真，新测试必须红

### F-P20 [Minor] 附录 pin 表判定【部分】I-20：booking metadata 幂等排除列表 5 个键，只有 2 个有专属测试
- **位置**：`docs/INVARIANTS.md:904` · `postgres/idempotency_match.go:124`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：postgres/idempotency_match.go 的排除列表含 block_number / review_reason / reject_reason / approved_by / rejected_by。上一轮只有 block_number 有测试，本轮已补到 2 个（TestDepositBooking_IdempotencyKey_StableAcrossReviewAuditMetadata），剩下的键没有逐个证明「排除不会误伤真实冲突」。修法：把现有两条测试改成 table-driven，对**列表里每一个键**跑两组：① 只有该键不同 → 必须判为同一次操作（幂等命中）；② 该键之外任一业务字段不同 → 必须 ErrConflict。关键是表的数据源要从排除列表本身派生（导出一个测试可见的 excludedMetadataKeys），这样以后往列表里加第六个键会自动进表 —— 否则同样的漏会再犯一次。
- **规则/文档要改**：I-20 正文仍写「this one metadata key」（单数），与代码里的 5 个不符，需同步
- **pin**：往排除列表加一个键而不加测试数据 → table-driven 派生会自动覆盖它；删掉某个键的排除 → 对应行必须红

### F-P34 [Minor] 附录 pin 表判定【无 pin】I-34：只有 Enforced by；它的测试真实存在但挂在 I-38 名下，删空 I-34 不会有任何东西变红
- **位置**：`docs/INVARIANTS.md:2364` · `docs/INVARIANTS.md:2698`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：I-34 的两个性质（deposit review 需要 Scope 之外的 capability / 二次源持续不可达要升级到 review 而非永久重试）实际由 server.TestCapabilityIndependentOfScope、server.TestDepositReview_SelfMintSelfApprove_MI2、service.TestOnchain_IngestDeposit_ReconcileError_EscalatesToReviewAfterFailureLimit、service.TestOnchain_IngestDeposit_ReconcileError_FailsClosedStaysConfirming 覆盖，但这四条列在 I-38 的 Pinned by 里。修法：给 I-34 加自己的 **Pinned by** 段，把这四条列进去（一条测试可以同时 pin 两条 invariant，重复引用不是问题；门禁只校验存在性，不校验唯一性）。顺带核一下 I-38 的 Pinned by 是否该保留它们 —— 若两条 invariant 语义确实重叠，考虑合并而不是各列一份。
- **规则/文档要改**：I-34 新增 **Pinned by** 段（F-m2 的门禁会报出这条）
- **pin**：无需新测试，是 pin 归属的登记修正
- **依赖**：F-m2

### F-P4 [Minor] 附录 pin 表判定【部分】I-4：新 pin RejectsOverCommit 有效，但旧的空洞 Reserve_Concurrent 仍在 Pinned by 列表里
- **位置**：`docs/INVARIANTS.md:211`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：同 F-m1：从 I-4 的 Pinned by 删掉 postgres.TestReserverStore_Reserve_Concurrent。
- **规则/文档要改**：I-4 Pinned by 删一项
- **pin**：无需新测试
- **依赖**：F-m1

### F-P6 [Minor] 附录 pin 表判定【空洞】I-6：两条 pin 一条测 core 的 decimal 往返、一条测 HTTP 字符串解码，都不碰数据库
- **位置**：`docs/INVARIANTS.md:322`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：同 F-M4：I-6 的 Pinned by 增列新的 information_schema precision/scale 门禁测试。本条只是 pin 表侧的登记，实修在 F-M4。
- **规则/文档要改**：I-6 Pinned by 增列
- **pin**：见 F-M4
- **依赖**：F-M4

### F-P7 [Minor] 附录 pin 表判定【无 pin】I-7（NOT NULL by default）：只有 Enforced by（三条 migration），没有任何 Pinned by
- **位置**：`docs/INVARIANTS.md:326` · `docs/INVARIANTS.md:351` · `postgres/schema_migrations_test.go:121`
- **主题**：gate-shape · **runtime**：go · **effort**：M
- **修法**：I-7 声称「每一列都 NOT NULL，例外只有六个 FK 目标列」。补一条机械门禁（与 F-M4 同一个文件即可）：查 information_schema.columns，断言 is_nullable='YES' 的列集合**恰好等于** I-7 正文列出的六个例外（journals.reversal_of / bookings.journal_id / bookings.reservation_id / events.journal_id / reservations.journal_id / journals.event_id）；多一个少一个都 fail。例外清单从 INVARIANTS.md 解析出来最好（文档即事实源），退一步在测试里硬编码也比没有强，但要在 fail 信息里指回 I-7。现有 postgres/schema_migrations_test.go:121 只对三张表的个别列点查 is_nullable，不是全库覆盖。
- **规则/文档要改**：I-7 新增 **Pinned by** 段（这也是 F-m2 那条门禁会立刻报出来的两条之一）
- **pin**：把任意一列改成 nullable（或从例外清单里删一项），新门禁必须红
- **依赖**：F-m2

### F-m1 [Minor] 空洞的 TestReserverStore_Reserve_Concurrent 仍被 INVARIANTS 列为 I-4 / I-11 的 pin，且其注释断言了一个假的因果（50+30<100，有锁无锁同样为真）
- **位置**：`postgres/reserver_store_test.go:153` · `postgres/reserver_store_test.go:162` · `docs/INVARIANTS.md:211` · `docs/INVARIANTS.md:415`
- **主题**：gate-shape · **runtime**：docs · **effort**：S
- **修法**：测试本身可以留（它测的是「并发不崩」，有价值），但要做两件事：(1) 从 docs/INVARIANTS.md 的 I-4 与 I-11 的 Pinned by 里删掉它 —— 对这两条 invariant 的核心命题（超额并发必须被拒）它为空，留在列表里就是虚报覆盖；(2) 把 :162 的注释 '// Both should succeed (advisory lock serializes)' 改成实话：「50+30=80 < 100，两者本就都该成功，与加不加锁无关；真正的超额并发命题见 TestReserverStore_Reserve_Concurrent_RejectsOverCommit」，并考虑改名为 ..._Concurrent_BothFitWithinBalance 以免下一个人再当它是锁的 pin。
- **规则/文档要改**：I-4 / I-11 的 Pinned by 删除 postgres.TestReserverStore_Reserve_Concurrent
- **pin**：无需新测试（RejectsOverCommit 已是真 pin，作者自己做过 mutation 并把三次试验结果写在 reserver_store_test.go:214-225）

### F-m2 [Minor] I-7 与 I-34 没有 Pinned by 段，而 TestInvariantsDocPinsAllExist 只校验「被引用的名字存在」，从不校验「每条 invariant 至少有一条 pin」
- **位置**：`core/invariants_pins_test.go:53` · `docs/INVARIANTS.md:326` · `docs/INVARIANTS.md:2364` · `docs/INVARIANTS.md:2685`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：在 core/invariants_pins_test.go 里加第四段检查：按 '\n## I-' 切分 INVARIANTS.md，对每个 section 断言其正文含 '**Pinned by**'（允许把 addendum/correction 形式也算上），缺失就 t.Errorf 报出该 I-n。当前会立刻报出 I-7 与 I-34 两条 —— 那正是它该报的。注意别顺手把门禁写成「只要文档里出现过 TestXxx 就算」：bareReference 那条正则扫的是全文，不限于 Pinned by 块，新检查必须按 section 定位。
- **规则/文档要改**：docs/INVARIANTS.md「How to add a new invariant」第 4 条（Add at least one test under Pinned by）从散文规则升级为机器检查；这条规则今天写着但无人执行
- **pin**：从任意一条 invariant 删掉整个 **Pinned by** 段，新检查必须变红

### F-m3 [Minor] 三个 Fuzz target 被 INVARIANTS 当作 I-1 / I-8 / I-16 的 pin 引用，但 CI 从不 fuzz —— go test 只跑 f.Add 的种子，且两个 target 连语料目录都没有
- **位置**：`core/journal_invariants_test.go:158` · `core/lifecycle_stress_test.go:167` · `core/money_test.go:207` · `docs/INVARIANTS.md:53` · `docs/INVARIANTS.md:373` · `docs/INVARIANTS.md:717` · `core/testdata/fuzz/FuzzLifecycleValidate/72dc44896ff458f1`
- **主题**：gate-shape · **runtime**：ci · **effort**：S
- **修法**：二选一，别留现状：(a) 落实文档自己写的规则 —— 在 ci.yml 加一个 job（或 test job 加一步）跑 go test ./core/ -run='^$' -fuzz=FuzzJournalValidate -fuzztime=30s，三个 target 各一次；新发现的语料由 CI 产出 artifact，人工 commit 进 core/testdata/fuzz/（这一步不要自动 commit）；(b) 如果决定不在 CI fuzz（fuzztime 会拖慢流水线，是合理取舍），就把 INVARIANTS「How to add a new invariant」第 5 条改掉，并在 I-1 / I-8 / I-16 的 Pinned by 里给这三条加注「seed-corpus only, not fuzzed in CI」——不要让读者按「它被 fuzz 过」的强度理解。
- **规则/文档要改**：docs/INVARIANTS.md 第 5 条「If the test is a fuzz target, run it for a few seconds in CI and commit any corpus seeds it discovers」当前完全未实现，必须实现或删除
- **pin**：若选 (a)：CI 的 fuzz step 本身就是。若选 (b)：无需 pin，是文档诚实性修正

### F-m5 [Minor] make test 没有 -count=1，会返回缓存的绿；实测 ./postgres/ 第一次 65.7s、第二次 (cached) —— 与 CI 不是同一个 gate
- **位置**：`Makefile:7` · `Makefile:10` · `.github/workflows/ci.yml:57`
- **主题**：silent-default · **runtime**：ci · **effort**：S
- **修法**：Makefile 的 test / test-short target 加 -count=1，与 ci.yml:57 完全对齐（go test -race -timeout 5m -count=1 ./...）。同时补一个 test-submodules（或让 test 依赖它）跑 chains/evm 与 anchors/r2，因为 make test 的 ./... 同样不跨 module —— CLAUDE.md 把 make test 写成「needs PostgreSQL — testcontainers, no mocks」的权威门禁，它现在既可能一个容器都不碰，也从不碰两个子模块。
- **规则/文档要改**：CLAUDE.md 的 Key Commands 段：make test 的说明需注明它与 CI 的差集（修好后可删）
- **pin**：none（配置对齐，靠 F-M6 的 CI/release 一致性元门禁顺带覆盖 Makefile 更好）

### F-m7 [Minor] PartitionStore.RebalanceDefault 全仓测试零引用；I-13 引用的 TestPartitions_RebalanceStrandedDefaultRows 实际调的是 EnsureMonthlyPartitions
- **位置**：`postgres/partition_store.go:117` · `postgres/partition_store_test.go:79` · `postgres/partition_store_test.go:137` · `service/partition.go:57` · `docs/INVARIANTS.md:510`
- **主题**：gate-shape · **runtime**：go · **effort**：S
- **修法**：给 RebalanceDefault 补一条直接调用的测试：复用 partition_store_test.go:98-129 已有的「克隆 journal 到未来月使其落进 default 分区」手法造出滞留行，然后直接 store.RebalanceDefault(ctx, stranded, 1)，断言返回的分区名列表非空、DefaultPartitionHasRows 变 false、且滞留的两条 entry 出现在目标月分区里。这条自愈路径今天完全无覆盖：它的唯一调用方 PartitionService.EnsureUpcoming 也没测（见 F-M3），facade 接线也没测（见 F-M1）—— 整条链三层全空。
- **规则/文档要改**：I-13 的 Pinned by 增列这条（现有那条名字像是在测 RebalanceDefault，实际不是，属于虚报）
- **pin**：把 RebalanceDefault 的函数体换成 return nil, nil，新测试必须变红
- **依赖**：F-M3

### F-m8 [Minor] internal/postgrestest 与 anchors/r2/internal/miniotest 两个 workspace module 完全不在 CI 里 —— 无 go vet、无 golangci-lint、无 go build
- **位置**：`go.work:4` · `.github/workflows/ci.yml:12` · `.github/workflows/ci.yml:24` · `.github/workflows/ci.yml:104`
- **主题**：gate-shape · **runtime**：ci · **effort**：S
- **修法**：在 ci.yml 的 lint job 加两个 go vet ./... 步骤（working-directory: internal/postgrestest 与 anchors/r2/internal/miniotest），build job 同理加 go build ./...。两者没有 _test.go，所以没有测试损失；但它们是全仓每一个 DB / MinIO 测试的 fixture 层 —— 这层坏了，表现是「所有集成测试莫名失败或跳过」，排查方向完全错。成本近零。顺带：go.work 列了五个 module，CI 覆盖三个；建议加一条元门禁从 go.work 机械派生 module 列表并断言每个都在 CI 里出现（照 F-M6 的思路，防止加第六个 module 时再漏）。
- **pin**：元门禁版本：在 go.work 里加一个新 module 而不更新 CI，门禁必须变红

### F-m9 [Minor] TestService_Ping_Integration 是一个永久跳过的空测试（t.Skip 之后只有 _ = context.Background()）
- **位置**：`ledger_test.go:91`
- **主题**：other · **runtime**：go · **effort**：S
- **修法**：删掉。它在测试计数里占一格、永远不执行任何断言，而它声称覆盖的 live Ping 路径确实已由 postgres 集成测试覆盖 —— 所以正确处置是删除，不是实装。若想保留一条 facade 级 Ping 冒烟，就用 postgrestest.SetupDB 真跑一次 svc.Ping(ctx) 并断言 NoError，别留 skip。
- **pin**：none

---

## D-web（Wave 2）

### J-1 [Major] BalancesPage 的 Balance Trend 图表在两个 skin 上恒发缺必填 currency_uid 的请求（服务端恒 400），且 isError 被丢弃，整张卡片静默消失
- **位置**：`web/packages/ledger-react/src/components/pages/BalancesPage.tsx:38` · `web/packages/ledger-react/src/components/pages/BalancesPage.tsx:42` · `web/packages/ledger-react/src/components/pages/BalancesPage.tsx:118` · `web/packages/ledger-react/src/heroui/pages/BalancesPage.tsx:52` · `web/packages/ledger-react/src/heroui/pages/BalancesPage.tsx:56` · `web/packages/ledger-react/src/heroui/pages/BalancesPage.tsx:140` · `server/handler_system.go:191`
- **主题**：silent-default · **runtime**：web · **effort**：M
- **修法**：两个 skin 的 BalancesPage 把 snapParams 补上 currency_uid —— 图表本来就是按单一币种画趋势，取用户选中的（或余额表返回的第一个）currency_uid 作为维度；同时消费 useSnapshots 的 isLoading/isError，失败渲染 ErrorState+Retry，而不是靠 chartArray.length > 0 把整张卡片隐藏掉。若产品上要多币种叠加趋势，则改为按币种各发一次请求（或推动后端放宽必填，属 H territory）。
- **规则/文档要改**：none（若选择放宽后端必填，需同步 docs/openapi.yaml 与 server/handler_system.go）
- **pin**：页面级测试：输入 holder 后断言请求 URL 含 currency_uid；再让 /snapshots 返 400，断言渲染 ErrorState 而不是隐藏卡片——前提是 MSW handler 会校验必填 query（J-17）
- **依赖**：J-17

### J-2 [Major] SnapshotsPage 在不填 holder 时把「请求根本没发」渲染成「No snapshots found」
- **位置**：`web/packages/ledger-react/src/components/pages/SnapshotsPage.tsx:43` · `web/packages/ledger-react/src/components/pages/SnapshotsPage.tsx:80` · `web/packages/ledger-react/src/heroui/pages/SnapshotsPage.tsx:35` · `web/packages/ledger-react/src/heroui/pages/SnapshotsPage.tsx:92` · `web/packages/ledger-react/src/hooks/use-system.ts:59`
- **主题**：silent-default · **runtime**：web · **effort**：M
- **修法**：三处一起改：(1) SnapshotsPage 两个 skin 把 holder 设为必填输入，handleSearch 前校验缺失即 toast 并 return，不写入 query；(2) hasSearched 判据从 Object.keys(query).length 换成一个显式的 useState 布尔 searched（现在 handleSearch 恒写 4 个 key，该判据在首次搜索后永远为真）；(3) useSnapshots 缺参时不要只靠 enabled 静默 no-op，见 J-18。另外把只填 holder 时服务端 400 的场景接上 errorText，让运维知道 currency+日期也是必填。
- **pin**：只填 currency + 日期区间点 Search，断言页面不出现「No snapshots found」（应出现必填校验提示），且 Network 层零请求这一事实被测试显式断言
- **依赖**：J-18, J-17

### J-3 [Major] 钱包余额卡在 holds 查询 loading/失败时显示「Nothing is on hold right now.」，与同卡片上非零的 locked 金额自相矛盾（终端用户面）
- **位置**：`web/packages/ledger-react/src/wallet/components/balance-card.tsx:34` · `web/packages/ledger-react/src/wallet/components/balance-card.tsx:139` · `web/packages/ledger-react/src/wallet/components/balance-card.tsx:154` · `web/packages/ledger-react/src/wallet/components/balance-card.tsx:192` · `web/packages/ledger-react/src/wallet/heroui/balance-card.tsx:33` · `web/packages/ledger-react/src/wallet/heroui/balance-card.tsx:142` · `web/packages/ledger-react/src/wallet/heroui/balance-card.tsx:180`
- **主题**：silent-default · **runtime**：web · **effort**：S
- **修法**：两个 skin：WalletBalanceCard / WalletBalances 不再传 holds.data ?? []，改为把整个 holds 查询结果（或 {data,isLoading,isError,refetch}）透传给 BalanceCardView 再传给 HoldsDetail；HoldsDetail 按四态分支——loading 渲染小骨架、error 渲染「暂时无法加载锁定明细，请重试」+Retry、只有 isSuccess 且 length===0 才说「Nothing is on hold right now.」。文案继续遵守 user-facing-surfaces.md（不出现 reservation/ledger 等内部词）。
- **pin**：MSW 让 /api/v1/holder/holds 返 500 而 /holder/balances 正常且 locked>0，断言展开 On hold 后不出现「Nothing is on hold right now.」

### J-4 [Major] dist/styles.css 把包自己的 --font-sans / --font-mono 写进宿主全局 :root，C2a 门禁的 allowlist 恰好放行它
- **位置**：`web/packages/ledger-react/src/styles/index.css:107` · `web/packages/ledger-react/src/styles/index.css:109` · `web/packages/ledger-react/test/styles.test.ts:39` · `web/packages/ledger-react/test/styles.test.ts:134` · `web/packages/ledger-react/README.md:208`
- **主题**：gate-shape · **runtime**：web · **effort**：M
- **修法**：src/styles/index.css 的 @theme inline 里不要再定义 --font-sans / --font-mono（theme.css 已在 .ledger-root 作用域内定义同名变量，scoped preflight 用的就是作用域内的值）。若确实需要 Tailwind 的 font 命名空间映射来生成 font-sans/font-mono 工具类，改为映射到包私有名（--font-ledger-sans / --font-ledger-mono）并在 .ledger-root 内桥接，使全局 :root 只剩 Tailwind 原版值（对照 dist/heroui.css 的同位置就是原版）。
- **规则/文档要改**：README.md 的 Theming 段修正措辞：删除或限定「They carry no host-affecting resets」，并说明字体 token 已不再进全局
- **pin**：test/styles.test.ts 收紧：断言全局 :root/:host 里不出现 --font-sans / --font-mono（或其值必须与 Tailwind 默认字体栈逐字相同）

### J-5 [Major] 四个图表把金额 parseFloat 后交给没有 formatter 的 recharts Tooltip/YAxis，运维读到丢精度且未走 financial.md 分档的裸浮点
- **位置**：`web/packages/ledger-react/src/components/dashboard/balance-trend.tsx:24` · `web/packages/ledger-react/src/components/dashboard/balance-trend.tsx:65` · `web/packages/ledger-react/src/components/pages/BalancesPage.tsx:47` · `web/packages/ledger-react/src/components/pages/BalancesPage.tsx:129` · `web/packages/ledger-react/src/heroui/pages/DashboardPage.tsx:154` · `web/packages/ledger-react/src/heroui/pages/BalancesPage.tsx:63`
- **主题**：other · **runtime**：web · **effort**：S
- **修法**：四处 <Tooltip> 与 <YAxis> 都传 formatter/tickFormatter，统一走 formatAmount(String(v))；更稳的做法是在 chartData 里同时保留原始字符串（如 balanceRaw: b.total_balance），parseFloat 出的 number 只用于几何定位，tooltip 的 formatter 从 payload 里取原始字符串再 formatAmount，这样连 double 的精度损失都不进展示。
- **pin**：渲染测试：喂一个 18 位小数的 total_balance，断言 tooltip / 轴刻度文本等于 formatAmount 的输出，而不是 JS 默认数字串

### J-7 [Major] types-conform.ts 只钉住 21 个 wire type 里的 11 个，其「spec 里没有对应 schema」的注释已过期 6 项
- **位置**：`web/packages/ledger-react/test/client/types-conform.ts:53` · `web/packages/ledger-react/test/client/types-conform.ts:83` · `docs/openapi.yaml:2428` · `docs/openapi.yaml:2467` · `docs/openapi.yaml:2527` · `docs/openapi.yaml:2561` · `docs/openapi.yaml:1976` · `docs/openapi.yaml:2631`
- **主题**：gate-shape · **runtime**：web · **effort**：M
- **同源提示**：H territory：openapi ↔ Go ↔ TS 三方一致
- **修法**：补齐 Classification / JournalType / Template(对应 types.ts 的 EntryTemplate) / Currency / HealthStatus / TemplatePreviewResult(对应 PreviewResult) 六条赋值断言；删掉文件末尾已过期的清单，只保留仍然为真的 Snapshot / HolderBalances / BalanceByCurrency / JournalWithEntries，并在注释里写清这四个仍缺 spec schema。同时在注释里说明现有断言是单向的（schema→hand），它能抓「手写类型比 spec 严」，抓不到「手写类型比 spec 松」与 spec 新增字段。
- **pin**：本文件即 pin；补完后手工验证一次：把 types.ts 里某个字段类型改错，npm run typecheck 必须红

### J-9 [Major] ./headless 面出金额而不出金额格式化器；所有 display/decimal/address 工具都到不了任何 package entry，且 decimal.ts 自述的 import 路径编译不过
- **位置**：`web/packages/ledger-react/src/headless.ts:11` · `web/packages/ledger-react/src/index.ts:1` · `web/packages/ledger-react/src/lib/utils/decimal.ts:5` · `web/packages/ledger-react/src/lib/utils/index.ts:1`
- **主题**：other · **runtime**：web · **effort**：S
- **同源提示**：E territory：facade 导出面的误用与缺口
- **修法**：src/headless.ts 增加 export { formatAmount, formatSignedAmount, validateAmount, formatUTC, formatDateUTC, shortenAddress, shortenHash, parseUnits, formatUnits, addAmounts, subAmounts, gtAmount, gteAmount, isZeroAmount } from './lib/utils'（root barrel 已 export * from './headless'，自动继承；heroui / wallet 入口按需同办）。同时修正 src/lib/utils/decimal.ts:5-7 的 import 示例使其真的可用。
- **规则/文档要改**：docs/frontend.md 与 README 的 Headless 段补上金额格式化 API 参考，说明 headless 消费方不应自行实现分档
- **pin**：CI 的 Assert build artifacts 步骤增加：grep dist/headless.d.ts 与 dist/index.d.ts 必须含 formatAmount

### J-10 [Minor] formatCompact 是零调用点、零测试、docstring 举例错误（'999' 实际返回 '999.0000'）的金额格式化器
- **位置**：`web/packages/ledger-react/src/lib/utils/display.ts:124` · `web/packages/ledger-react/src/lib/utils/display.ts:130` · `web/packages/ledger-react/src/lib/utils/display.ts:144` · `web/packages/ledger-react/src/lib/utils/index.ts:2`
- **主题**：other · **runtime**：web · **effort**：S
- **修法**：二选一。① 保留：随 J-9 一起导出，补 test/lib/display.test.ts 用例（含 K/M/B 三档边界与负数），把 docstring 的 formatCompact('999') 改成 '999.0000'，并把「callers must clamp first」改成函数内自己对超过 Number 安全区的输入做处理（现在没有任何 caller 去 clamp）。② 不需要：直接删掉 formatCompact 与 lib/utils/index.ts 的导出。建议 ①（compact 展示对 dashboard 有用）。
- **pin**：test/lib/display.test.ts 补 formatCompact 分档表用例；若选 ② 则由删除本身收口
- **依赖**：J-9

### J-11 [Minor] formatAmount('') 返回 '0.00'——金额缺失与金额为零在展示上不可区分
- **位置**：`web/packages/ledger-react/src/lib/utils/display.ts:69` · `web/packages/ledger-react/src/lib/utils/display.ts:77`
- **主题**：silent-default · **runtime**：web · **effort**：S
- **修法**：formatAmount 在 parseUnits 之前先判空：value.trim() === '' 时返回一个明确的缺失占位（与页面里既有的 '-' 惯例一致），不要落进 raw === 0n 的分支返回 '0.00'。formatSignedAmount 同办。
- **pin**：test/lib/display.test.ts 增加空串、纯空白两条用例

### J-12 [Minor] M3 声称的 shadcn↔heroui a11y/overflow parity 实际落成的是一条把 21:63 差距冻结下来的各自 ratchet；shadcn 15 个 table 全无可访问名
- **位置**：`web/packages/ledger-react/test/skin-parity.test.ts:154` · `web/packages/ledger-react/test/skin-parity.test.ts:171` · `web/packages/ledger-react/src/components/pagination-bar.tsx:22` · `web/packages/ledger-react/src/components/pages/TemplatesPage.tsx:1` · `web/packages/ledger-react/src/components/pages/DepositsPage.tsx:1`
- **主题**：gate-shape · **runtime**：web · **effort**：L
- **修法**：把 shadcn skin 补到与 heroui 同级：15 个 <table> 各加 aria-label（或 sr-only <caption>）；src/components/pagination-bar.tsx 的容器加 role=navigation + aria-label；TemplatesPage 的行内无标签输入补 aria-label；所有用 truncate 的 flex/grid 子项补 min-w-0（当前 shadcn 全包只有 1 处 min-w-0，heroui 有 7 处，truncate 在缺 min-w-0 的 flex 子项上是静默失效的）。改完把 CENSUS_BASELINE 抬到实际值。
- **规则/文档要改**：若最终确认无法完全对齐，需在 CLAUDE.md 的「page logic must stay mirrored」处写明 a11y 不在镜像范围内；否则该句与代码现状不符
- **pin**：test/skin-parity.test.ts 的 hardening gate 从「各自绝对下限」改成跨 skin 相对断言（shadcn 计数不得低于 heroui 计数的某个比例），使单侧退化必红

### J-13 [Minor] useLedgerMutation 的幂等 key 作用域是 hook 实例而非实体/payload，今天仅因所有金额对话框恰好是 per-row 组件、且 approve/reject 端点恰好不读 key 才安全
- **位置**：`web/packages/ledger-react/src/hooks/use-ledger-mutation.ts:52` · `web/packages/ledger-react/src/hooks/use-ledger-mutation.ts:72` · `web/packages/ledger-react/src/hooks/use-deposit-reviews.ts:84` · `web/packages/ledger-react/src/hooks/use-deposit-reviews.ts:107` · `postgres/booking_store.go:356` · `postgres/reserver_store.go:350`
- **主题**：sibling · **runtime**：web · **effort**：M
- **修法**：useLedgerMutation 接一个可选 keyOf(variables) => string，内部用 Map<payloadKey, uuid> 缓存，成功时只删掉该 payload 对应的条目（而不是清空整个 ref）；默认 keyOf 为 JSON.stringify(variables)，使「A 失败后点 B」必然拿到不同 key。use-deposit-reviews.ts 的两个手写 useRef 同样改造（它们目前 page-scoped，是唯一真正跨实体共用的实例）。这条把 M4/M7 的 key 粒度从「hook 实例」纠正到规则要求的「一次逻辑提交」。
- **规则/文档要改**：none（api-contract.md §9 本来就是「一次逻辑提交一个 key、跨重试复用」，这是实现对齐规则）
- **pin**：test/hooks/use-ledger-mutation.test.tsx 增加：对 variables A 失败后再对 variables B 发起 mutate，断言两次 key 不同；A 的重试断言 key 相同（现有测试全程 mutate(undefined)，跨实体维度零覆盖）

### J-15 [Minor] useReverseJournal 丢弃 useLedgerMutation 铸的 key，落回 client.ts 的 per-attempt randomUUID——M4 的原始形态在冲销路径上仍在
- **位置**：`web/packages/ledger-react/src/hooks/use-journals.ts:48` · `web/packages/ledger-react/src/client/client.ts:181` · `web/packages/ledger-react/src/client/client.ts:60` · `web/packages/ledger-react/src/client/client.ts:105` · `server/handler_journals.go:337`
- **主题**：sibling · **runtime**：web · **effort**：S
- **修法**：二选一，取决于后端。若 handleReverseJournal 将来要消费幂等键：client.ts 的 reverseJournal 增加 idempotencyKey 参数并放进 Idempotency-Key header，useReverseJournal 用 useLedgerMutation 提供的第二参数（现在它声明了却不用）。若确定不消费（当前靠 journals.reversal_of 的部分唯一索引去重）：把 useReverseJournal 从 useLedgerMutation 换成普通 useMutation + 手工 invalidate，并在注释写明该端点的去重机制，免得下一个人以为幂等键在起作用。
- **pin**：client 层测试：断言 reverseJournal 的两次 attempt 携带同一个 Idempotency-Key（若选方案一）；或断言它根本不发该 header（若选方案二）
- **依赖**：J-13

### J-16 [Minor] heroui 的 ReconciliationPage 用 sonner 的 success toast 播报「账不平」
- **位置**：`web/packages/ledger-react/src/heroui/pages/ReconciliationPage.tsx:21` · `web/packages/ledger-react/src/heroui/pages/ReconciliationPage.tsx:34`
- **主题**：other · **runtime**：web · **effort**：S
- **修法**：两处 toast.promise 的 success 回调不能承担「结果为负面」的表达：改为 mutateAsync().then(result => result.balanced ? toast.success(...) : toast.danger(...)).catch(...)，或保留 toast.promise 但在 balanced===false 时额外弹一条 danger。对齐 shadcn 侧用 StatusBadge status=failed 的表达。
- **pin**：渲染测试：mock 返回 balanced:false，断言不出现 success 样式/图标的 toast

### J-17 [Minor] 覆盖 Balances/Snapshots 的唯二测试只断言「搜索之前」，且 MSW 的 getOk 完全忽略 query 参数——缺必填参数的请求在测试里照样返 200
- **位置**：`web/packages/ledger-react/test/pages/static-pages.test.tsx:9` · `web/packages/ledger-react/test/pages/static-pages.test.tsx:19` · `web/packages/ledger-react/test/pages/render-page.tsx:15`
- **主题**：gate-shape · **runtime**：web · **effort**：M
- **同源提示**：F territory：测试做了真实服务端/消费方不会做的准备
- **修法**：render-page.tsx 的 getOk 增加可选 requiredQuery: string[]，缺参时返回 400 业务包络而不是 200（这样 MSW 的行为才与真实 handler 一致）；static-pages.test.tsx 为 BalancesPage / SnapshotsPage 各补三条「搜索之后」用例：正常返回、缺必填参数、服务端 500。这是 J-1 / J-2 回归 pin 的前置——不改 MSW 的话，补的测试依然是绿的空洞断言。
- **pin**：本条即 pin

### J-18 [Minor] useSnapshots / useEntries / useJournal 的 enabled 门在 headless 面是静默 no-op 陷阱：缺必填参数时返回全 false 的对象，签名上看不出参数必填
- **位置**：`web/packages/ledger-react/src/hooks/use-system.ts:59` · `web/packages/ledger-react/src/hooks/use-journals.ts:70` · `web/packages/ledger-react/src/hooks/use-journals.ts:26`
- **主题**：silent-default · **runtime**：web · **effort**：M
- **同源提示**：E territory：必须先满足前置条件才能用，但签名不体现的接线陷阱
- **修法**：三个 hook 的返回值增加一个显式的 isDisabled（或 isMissingParams）布尔，并在 JSDoc 里写明哪些参数是必填、缺失时查询不会发出；headless 消费方据此渲染「请先选择 X」而不是把 data===undefined 当成空结果。同时把必填参数从可选参数提升为必填类型（例如 useSnapshots 的 holder 从 number|undefined 改成 number）会更彻底，但属破坏性变更，需按 no-compat 约定评估。
- **规则/文档要改**：docs/frontend.md 的 hook 参考补上每个 hook 的必填参数与 disabled 语义
- **pin**：hook 层测试：不传 holder 调用 useSnapshots，断言 isDisabled===true 且不发请求

### J-19 [Minor] ReconciliationPage 两个 skin 的错误分支不走 errorText，且文案「Check the API logs.」把内部排障手段写进操作面
- **位置**：`web/packages/ledger-react/src/components/pages/ReconciliationPage.tsx:45` · `web/packages/ledger-react/src/components/pages/ReconciliationPage.tsx:101` · `web/packages/ledger-react/src/heroui/pages/ReconciliationPage.tsx:25` · `web/packages/ledger-react/src/heroui/pages/ReconciliationPage.tsx:40`
- **主题**：sibling · **runtime**：web · **effort**：S
- **同源提示**：I territory：可运维性与操作面文案
- **修法**：四处错误文案改用 errorText(mutation.error, fallback)（M1 声称覆盖 every mutation，这两页因为走 inline isError / toast.promise 的 error 分支而被漏掉）；fallback 文案从「Reconciliation failed. Check the API logs.」改成面向操作者的下一步，例如「对账未能完成，请稍后重试；持续失败请联系值班」。
- **pin**：渲染测试：mock 返回业务错误码，断言页面显示服务端 message.text 而不是硬编码串
- **依赖**：J-8

### J-20 [Minor] C1 的 mutation-feedback 门禁扫描面窄于它守护的不变式：不扫 wallet/ 与 dashboard/，不匹配 mutateAsync，且「既无 onSuccess 也无 onError」被无条件豁免
- **位置**：`web/packages/ledger-react/test/mutation-feedback.test.ts:13` · `web/packages/ledger-react/test/mutation-feedback.test.ts:63` · `web/packages/ledger-react/test/mutation-feedback.test.ts:110`
- **主题**：gate-shape · **runtime**：web · **effort**：S
- **修法**：扫描目录从两个 pages 目录扩展到 src 下全部 tsx（至少加上 src/wallet/**、src/components/dashboard/**、src/heroui/**）；匹配正则同时覆盖 mutate( 与 mutateAsync(（mutateAsync 走 toast.promise 时应要求存在 error 分支）；把「既无 onSuccess 也无 onError」的豁免收窄为一份带理由的显式 allowlist，而不是按形状默认放行。
- **pin**：本条即 pin

### J-21 [Minor] 钱包流水行自行从 direction 推导正负号并硬拼 +/-，与 formatSignedAmount 的契约相悖；TS 侧没有任何东西钉住「服务端发绝对值」
- **位置**：`web/packages/ledger-react/src/wallet/components/transaction-list.tsx:80` · `web/packages/ledger-react/src/wallet/heroui/transaction-list.tsx:1` · `web/packages/ledger-react/src/lib/utils/display.ts:96` · `postgres/holder_store.go:144`
- **主题**：sibling · **runtime**：web · **effort**：S
- **修法**：两个 skin 的 DefaultRow 改成：把 direction 折成带号字符串（out 时 '-' + amount）后交给 formatSignedAmount，由格式化器统一产出符号与配色判据，不再在调用点硬拼前缀（display.ts:96-99 的契约明写 callers must never re-derive the sign）。同时在 wallet client 的 WalletTransaction 类型上注明 amount 是绝对值、由 postgres/holder_store.go 的 net.Abs() 保证。
- **pin**：测试：喂一条 direction=out 且 amount 带负号的响应，断言渲染结果不出现双负号

### J-22 [Minor] DepositAddressCard 的 assets: string[] 接受空数组，产出「Only send  on Ethereum」——这句正是防不可恢复资损的警告
- **位置**：`web/packages/ledger-react/src/wallet/components/deposit-address-card.tsx:76` · `web/packages/ledger-react/src/wallet/components/deposit-address-card.tsx:154` · `web/packages/ledger-react/src/wallet/heroui/deposit-address-card.tsx:1`
- **主题**：other · **runtime**：web · **effort**：S
- **修法**：两个 skin 把 DepositAddressCardProps.assets 的类型收紧为非空元组 [string, ...string[]]，使传 [] 成为编译错误（与 M5 把 network/assets 设为必填的同一意图：缺失必须是编译错误而不是静默省略）；再加一道运行时守卫，assets 为空时不渲染这句警告而渲染「请联系支持确认可接收的资产」。
- **规则/文档要改**：docs/frontend.md 的 DepositAddressCard 段说明 assets 必须非空
- **pin**：类型层即 pin（传 [] 必须 tsc 报错）；再加一条渲染测试断言警告文案不含连续空格

### J-23 [Minor] dev 依赖存在 1 个 high（browserslist）+ 2 个 low 通告；生产依赖 npm audit 为 0
- **位置**：`web/package-lock.json:1` · `web/packages/ledger-react/package.json:1`
- **主题**：other · **runtime**：ci · **effort**：S
- **修法**：跑 npm audit fix 升级 browserslist / esbuild / postcss-selector-parser（全部落在 devDependencies，不进 dist），并在 .github/workflows/ledger-react.yml 增加一步 npm audit --omit=dev --audit-level=high，把门禁明确定位在生产依赖上（dev 通告不阻塞 CI，但要有人看得见）。
- **pin**：CI 新增 npm audit --omit=dev --audit-level=high 步骤

### J-24 [Minor] skin-parity 门禁按构造无法发现「两个 skin 对称地错」——本轮三条 Major 全是这个形态
- **位置**：`web/packages/ledger-react/test/skin-parity.test.ts:91` · `web/packages/ledger-react/test/skin-parity.test.ts:123`
- **主题**：gate-shape · **runtime**：web · **effort**：M
- **修法**：在 skin-parity 之外补一条与 skin 无关的「查询消费完整性」静态 gate：扫描 src 下所有 tsx，对每个从 useXxx() 解构出 data 的调用点，断言同一解构里也取到了 isError（或该调用点在一份带理由的 allowlist 里）。这条 gate 不做跨 skin 比对，因此能抓到两侧同时缺失的情况——J-1（BalancesPage 丢弃 snapshots 的 isError）与 J-3（wallet 不读 holds 状态）在这条 gate 下应当立刻变红。
- **pin**：该 gate 本身；落地时必须先验证它对 J-1 / J-3 的当前代码是红的，修完才转绿
- **依赖**：J-1, J-3

### J-25 [Minor] nav-routes 门禁只查 nav→route 单向，dogfood app 里有路由而 nav 无入口的情况不会被发现
- **位置**：`web/packages/ledger-react/test/nav-routes.test.ts:46` · `web/packages/ledger-react/test/nav-routes.test.ts:52`
- **主题**：gate-shape · **runtime**：web · **effort**：S
- **修法**：test/nav-routes.test.ts 增加反向断言：web/src/app 下每个 page.tsx 的路由，要么出现在 LEDGER_NAV_ITEMS 里，要么在一份显式的 EXPECTED_UNLISTED allowlist 里（如 /、/login、/journals/[id] 这类详情或非导航页）。
- **pin**：本条即 pin

---

## W3-lead（Wave 3）

### H-R2 [Minor] 上轮修复复核：审计 README 对 openapi 契约门禁覆盖面的描述宽于机制实际覆盖，读者会高估它
- **位置**：`docs/audits/2026-08-25-financial-engineering/README.md:41` · `server/openapi_contract_test.go:20`
- **主题**：other · **runtime**：docs · **effort**：S
- **修法**：把 README.md:41-43 那句「反射 Go wire struct 对照 docs/openapi.yaml，双向报『文档有代码没有』与『代码有文档没有』」改成与机制一致的措辞：requestBody 方向是双向精确比对，response 方向是单向（spec ⊆ Go），且覆盖范围限于 $ref 引用的具名 schema 的顶层字段名——query 参数、字段类型/format、inline schema、嵌套对象均不在内。该目录的 README 是全仓唯一维护到最终状态的一份，所以它的措辞就是下一个人对门禁能力的认知基线。若 H-M1/M2/M3/m11 全部落地后覆盖面确实扩到了那句话描述的程度，则改成描述新状态并注明日期。
- **规则/文档要改**：docs/audits/2026-08-25-financial-engineering/README.md 的措辞订正（该文件自陈是唯一维护到最终状态的一份，允许改）。
- **pin**：无（纯文档）。建议改完后在 server/openapi_contract_test.go 文件头注释里加一句反向指针，指向该 README 段落，让两处措辞下次一起改。
- **依赖**：H-M1, H-M2, H-M3, H-m11

---

## 破坏性变更清单（修复过程中追加；每条写清消费方需要做什么）

| 条目 | 变更 | 消费方需要做什么 |
|---|---|---|
| （本波的行都追加在下方「Lead 追加」表尾） | W1-templates 起形成的事实约定：破坏性变更行与 lead 追加项同表。此处保留标题，避免两处清单分叉 | W3-lead 收口时并表 |
| D-M2 (D-threat) | **`postgres.Migrate` 的安装机制变了**：现在先单独跑 001，再把 `ledger_owner` 的权限临时授予迁移凭证（`GRANT ledger_owner TO <runner> WITH INHERIT TRUE`），跑完在每条退出路径 `REVOKE`。以 superuser 或以 `ledger_owner` 本人连库时整段跳过（`pg_has_role(...,'USAGE')` 已为真） | 绝大多数部署无需动作。**以一个对 `ledger_owner` 无 ADMIN OPTION 的第三方角色跑 Migrate 现在会直接报错**（此前是跑到 002 才 42501 并把库标 dirty）。这不是新增权限面：001 自己写明 bootstrap 对 `ledger_owner` 保有永久 ADMIN OPTION、随时可自行重做同一套 GRANT/REVOKE；本变更只是把这个窗口显式化并限定在一次 Migrate 内。**收益：docs/RUNBOOK.md 认可的 CREATEROLE 非 superuser bootstrap 现在真的装得上**——此前它死在 002 的 `GRANT DELETE ON webhook_nonces`，008/014 等收窄全部静默缺席 |
| D-M2 (D-threat) | **migration 007 文件本身被修改**（deployment.md 通常禁止）。三条无条件 `ALTER ROLE ... NOSUPERUSER ...` 改为逐属性条件执行：只在该属性当前为真时才发 ALTER，发不出去则 `RAISE EXCEPTION` | 无动作。golang-migrate 不校验 migration 文件哈希，已装过 007 的库不会重跑。理由与实测（`NOSUPERUSER`/`NOREPLICATION`/`NOBYPASSRLS` 只有持该属性的角色能写，即使是 no-op）写在 007 文件头 |
| D-M4 / D-m7 (D-threat) | `ledger_app` 失去若干权限：`config_table_changes` / `reconcile_scan_cursor_changes` 的 `INSERT` 与两个 `_id_seq` 的 `USAGE, SELECT`；`deposits` / `withdrawals` 的 `INSERT, UPDATE` | 无动作 —— 这四张表没有任何生产写入方（审计行由 `SECURITY DEFINER` 触发器写；两张死表零调用方）。自建组合根若直接写过这两张审计表，改为让触发器写 |
| D-m8 (D-threat) | `public` 下**所有函数**对 `PUBLIC` 的默认 `EXECUTE` 被撤销，按名单只授回 5 个（两个分区函数给 `ledger_app`；三个 sign helper 给 `ledger_app` + `ledger_ro`） | 无动作。若自建查询直接调用过 `ledger_signed_amount` 之外的库内函数，需在自己的 migration 里显式 GRANT |
| D-m1 (D-threat) | `ledger_rebalance_default_partition(p_first, p_last)` 现在拒收：非月初对齐、`p_last < p_first`、跨度 > 120 个月（`invalid_parameter_value`） | 合法调用方（`PartitionStore.rebalanceDefault`）传的一直是月初对齐、跨 `PartitionConfig.MonthsAhead` 的区间，不受影响。手工调用该函数的运维脚本需按月初对齐 |
| D-M3 / D-m10 (D-threat) | 审计触发器从 4 张表扩到 **11 张**（新增 `account_policies` `bookings` `events` `reservations` `journals` `deposit_addresses` `entry_template_lines`）。`bookings` / `reservations` 是**业务速率**写入，`config_table_changes` 每次审计到的 UPDATE 多写约两份行副本 | 无 API 变更。**容量变更**：`config_table_changes` 的增长量级从「配置表偶发改动」变为「每次 booking transition / 每次 settle-release」，需在 `docs/CAPACITY.md` 定容并考虑保留期。`events` 的投递记账列（`delivery_status` / `attempts` / `next_attempt_at` / `delivered_at`）已在 `WHEN` 子句里减去，不计入 |
| D-M5 / A-N2 / D-M8 (D-threat) | facade 新增三个访问器：`svc.ConfigHistory()`、`svc.AssertRuntimeRole(ctx)`、`svc.WebhookNonceRecorder()`；`core` 新增 `ConfigChangeReader` / `WebhookSigner` 两个 port 与四个类型 | 纯新增，无破坏。**但 `svc.WebhookNonceRecorder()` 建议立刻接线**：`srv.SetWebhookNonceRecorder(svc.WebhookNonceRecorder())`，否则入站 webhook 在签名 ±5min 窗口内的重放会通过（此前无人接线且完全静默，现在首次回调会打一条 warn） |
| G-m5 (D-threat) | `POST /api/v1/webhooks/{channel}` 的 **legacy `ParseCallback` 路径**现在拒绝 `status="confirmed"`（403） | 本仓自带的 `channel/onchain` 适配器实现了 `SightingParser`，走另一条路径，**不受影响**。自带 `channel.Adapter` 且用状态回调推进充值的消费方：确认必须改走 sighting 形状（实现 `ParseSighting`），因为 confirmed 要出 journal，而这条路径不出 |
| D-M6 (D-threat) | `delivery.WebhookDeliverer` 新增 `SetSigner(core.WebhookSigner)`；未接线时仍读 `webhook_subscribers.secret`，但**首次投递会 warn 一次** | 无强制动作（expand 阶段，两条路径并存）。要关闭「泄漏的 DB 凭证可向任意下游伪造签名有效事件」这条：实现 `core.WebhookSigner` 并 `SetSigner`，把密钥移出 DB。contract 阶段（`REVOKE SELECT (secret)` / `DROP COLUMN secret`）留给已迁移密钥的部署 |
| D-m4 (D-threat) | `postgres.WebhookSubscriberStore` 新增 `SetLogger(core.Logger)`；nonce 清理因缺权限被容忍时 warn 一次 | 无动作。经 `ledger.New` 的消费方自动接线到注入的 logger |

## Lead 追加（整改期间发现，归属见括号）

| 来源 | 条目 | 归属 |
|---|---|---|
| w1-gate 复核 | `examples/tamper-evident` 只演示伪造 journal、不演示篡改 checkpoint，且把闸演示成完整提现闸；随 I-49 更新演示步骤 | D-surface |
| w1-gate 兄弟扫描 | `postgres/account_policy_enforce.go:114` 的 `min_balance` 放行读 checkpoint+delta。本波不修：篡改 checkpoint 只能放宽 floor，不构成出金路径。留作后续评估项 | 后续 |
| lead 复核 f48ae31 | 闸开启路径的 `availableBase` 从 lock 内读改为 lock 外算，I-4 在该路径被削弱；已要求 w1-gate 补 lock 内 entries-only 重算取 min | W1-gate（进行中） |
| w1-templates 复核 | `POST /journals/deposit-tolerance` 同形铸币路径（契约 §7.11） | W1-templates（进行中） |
| w1-templates | H-M3 后半：reverse 的幂等键改为客户端可控（跨 openapi / handler / web 三面） | D-contract（新增条目，编号 H-M3b） |
| w1-templates | `docs/api.md:390` 示例仍用被拒的 `deposit_confirm`；`middleware_auth.go:168` 与 `middleware_idempotency.go:35` 是同一 webhook 前缀字面量的两份拷贝 | D-contract / D-threat |
| D-C1（W1-templates） | `POST /journals/template` 现在**结构派生**地拒绝任何一条腿落在 `is_system` 分类上的模板（403），不再只拒四个 deposit code。实测受影响的已发货 preset 模板：`dev_credit` `capital_injection` `capital_withdraw` `fee_charge` `checkout_settlement_gross` `checkout_settlement_net` `fx_buy` `fx_sell` `transfer_in` `transfer_out` `withdraw_confirm` `withdraw_fee` `deposit_pending` `deposit_resolve_overage` `deposit_release_overage`（前四个由本轮新增拦截，其余同批）。仅 `lock_funds` / `unlock_funds` 仍可经该端点执行 | 这些账务改走各自的专用编排 / 服务端流程（库自己的流程本就不走这个端点）；确有评审过的理由要保留某个 code 时，逐个列入 `ALLOW_GENERIC_TEMPLATE_POST` / `Config.AllowGenericTemplatePost`——它现在同时是名单层与结构层的唯一放行口 |
| D-C1（W1-templates） | `presets.ProtectedTemplateCodes()` 从 4 个 code 变为 5 个（新增 `dev_credit`） | 硬编码该集合长度或元素的代码需更新；把它当「额外硬编码名单」使用的部署无需改动 |
| §7.11（W1-templates，兄弟扫描新发现） | `POST /journals/deposit-tolerance`：① 计划里每个将执行的模板都过 `/journals/template` 同一道闸（结构派生 ∪ 名单），默认配置下四个 deposit 码全被拒 → **该端点默认 403**；② 路由从 `write` scope 组移到 **admin** scope 组（与 `/dev/credits` 同组）。此前一个 write key 用 `expected_amount=actual_amount=1000000` 即可拿到全额 `deposit_confirm_pending` 入账，绕开刚修好的两层闸 | 确有理由经 HTTP 解决充值容差的部署：把 `deposit_confirm_pending` `deposit_confirm` `deposit_release_pending` `deposit_record_overage` 列入 `ALLOW_GENERIC_TEMPLATE_POST`，并改用 **admin** key 调用；否则改由库内 Go 编排（`presets.BuildDepositTolerancePlan` + `ExecuteDepositTolerancePlan`）调用，不经 HTTP。`docs/openapi.yaml:184` 与 `docs/api.md:411` 的 scope/403 说明归 D-contract |
| H-M3 Go 侧（W1-templates） | `POST /journals/{uid}/reverse` 现在拒绝请求体里的 `idempotency_key`（400），`Idempotency-Key` header 别名同样被拒——此前该字段被静默丢弃，而 openapi 把它标为 required | 全额冲销不要再带 key（键由服务端从 journal uid + reason 派生）；需要自选幂等键的调用改用 `POST /journals/{uid}/reverse-partial` 并传 `num=den=1`（等价全额冲销，且该端点本就接受 `idempotency_key`）。`docs/openapi.yaml` 的 `required` 与 description 由 D-contract 同步 |
| w1-ledgerstore 兄弟扫描 | `PostJournal.EventUID` 只查存在性：任意 journal 可挂到陌生 event 上并消费掉该 booking 的 set-once `journal_id`，让其后续真正记账 transition 永久 `ErrConflict`；`event_uid` 是 wire 字段，触达面大于 `reversal_of` | W1-ledgerstore（进行中，并入 I-51） |
| w1-ledgerstore 兄弟扫描 | `DeactivateJournalType` / `DeactivateClassification` / `DeactivateCurrency` 是静默 no-op：`is_active=false` 在 Post/Render/Resolve 路径零校验（对照 `entry_templates.is_active` 在 `Render` 真拦） | D-lock（新增条目 B-X1；独占面追加 `postgres/classification_store.go` `currency_store.go` `journal_type_store.go` `dims.go` 的 resolve 路径） |
| w1-ledgerstore 兄弟扫描 | `JournalInput.Metadata` / `Source` 无长度上界 | D-contract（Minor，请求体大小限制之外的字段级上界） |
| A-C1（W1-sign） | `equity` 分类由 credit-normal 改为 **debit-normal**（migration 016 以 owner 身份临时停 `classifications_mutation_guard` 后改，并删除 equity 的 `balance_checkpoints` / `balance_snapshots` 缓存行强制全量重算） | 跑 016。**016 之前用 `capital_injection` / `capital_withdraw` 记过账的部署**：历史 `journal_entries` 不会被改写（append-only），旧分录在新极性下读数符号相反 —— 必须对这些 journal 逐笔 reversal 后按新模板重记。没记过资本类账的部署无需动作 |
| A-C1（W1-sign） | `capital_injection` / `capital_withdraw` 两腿方向互换（注资 = CR custodial + DR equity） | 无 API 变更；amount key 不变。若消费方硬编码断言过这两个模板的 entry_type 顺序，需要跟着翻 |
| A-M2（W1-sign） | `checkout_settlement_net` 由 3 腿改 **4 腿**，且**不再接受 `gross_amount`**：现在只用 `net_amount` + `fee_amount`（gross 由两者相加隐含），新增 `fee_expense`（用户侧 memo）与 `fees` 两条腿 | 调用方继续传 `gross_amount` 是无害的（多余 key 被忽略），但**必须传 `net_amount` 与 `fee_amount`**；只传 gross 的调用会报 `missing amount key`。`SettlementBundle()` 现在还会安装 `fee_expense` 分类 |
| A-M2（W1-sign） | `checkout_settlement_gross` 两腿方向互换（商户 DR main_wallet +gross / CR custodial +gross） | 同上，amount key 不变 |
| A-M4（W1-sign） | `fee_charge` 由 2 腿改 **4 腿**（`fee_expense` DR / `custodial` DR / `main_wallet` CR / `fees` CR）；`FeeBundle()` 现在还会安装 `fee_expense` 分类 | amount key 不变（仍是 `amount`）。已用 `fee_charge` 记过账的部署：`fees` 账的历史余额符号是反的，需 reversal 重记 |
| A-M6 / A-N3（W1-sign） | `SolvencyCheck` 的托管口径由硬编码 `code='custodial'` 改为可注入的分类码集合，默认 **{custodial, settlement}**；口径在 `classifications` 里一个都匹配不到时**返回 `core.ErrInvalidInput`** 而不是报 Custodial=0 | 默认口径下 FX 部署的买入币种从「永久 solvent=false」变为正确值；托管分类不叫 `custodial` 的部署改用 `ledger.New(pool, ledger.WithCustodialClassCodes(...))`（facade 出口，W1-facade 已补；库模式无需再直接构造 store），HTTP/自建组合根仍可用 `postgres.NewPlatformBalanceStore(pool).WithCustodialClassCodes(...)` |
| A-M3 / A-N4（W1-sign） | holder 面四处 `balance_role <> ''` 收敛为 `NOT IN ('', 'memo')` | 无 API 变更。行为变化：提现手续费重新出现在 `ListHolderTransactions` 里；只有 memo 记账的币种不再出现在 `ListHolderBalances` 中 |
| A-M1（W1-sign） | `GET /holders/{h}/trends` 的 `inflow` / `outflow` 语义由「credit / debit 合计」改为 normal_side-aware 的「使余额上升 / 下降的合计」 | 对 debit-normal 分类（`main_wallet` / `locked`）两列数值互换。任何按这两列画图的消费方在升级后会看到方向反转 —— 那是修正，不是回归 |
| A-M5（W1-sign） | `RollupAdapter.GetSnapshotBalances` 在检出回溯写入时返回**实时重算的完整维度集**（可能多于缓存里的行）；`GetBalanceTrends` 对被回溯写入作废的天数同样实时重算 | 无 API 变更。消费方若假设 `GetSnapshotBalances` 的行数等于快照表行数，需要放弃该假设 |
| w1-facade 实测 | `DATABASE_URL=<共享 PG>` 模式下 `go test ./...` 假红：`internal/postgrestest.isolatedConnection` 的库名是 per-binary 计数器 `ledger_test_<n>`，多个测试二进制共享一台 PG 必撞（实测 `database "ledger_test_13" already exists`）；testcontainers 模式每二进制一台 server 所以看不出 | D-tests（新增条目 F-X1） |
| w1-facade rebase | `docs/INVARIANTS.md` 的「How to add a new invariant」标题出现两次，且 I-51 被追加在它之后；编号门禁只查序号不查位置 | W3-lead |
| G-C2 (W1-onchain) | `core.DepositSighting.TxLogSeq` 语义变更：从「本次查询命中集合内的序号」改为「该 log 在其 tx receipt 内的零基位置」。`chains/evm` 的 watcher 自动改用新定义 | 喂 `channel/onchain` webhook 的**外部扫描器必须改用同一定义**（receipt 相对位置），否则两条摄入路径对同一笔转账派生不同幂等键。无外部消费方期直接切，不做双键并存（no-compat 约定）。存量库里由旧定义写入的 `bookings.metadata.txlog_seq` 不迁移：旧键仍然唯一，只是与新键不相等——同一笔转账若在切换后被重新观测会新建一个 booking。**切换前应确认无 confirming/pending 的在途 deposit**（`SELECT count(*) FROM bookings b JOIN classifications c ON ... WHERE c.code='deposit' AND b.status IN ('pending','confirming')` 为 0） |
| G-M8 (W1-onchain) | `service.Onchain.Run` 在配置了 `ChainReader` 但未接线 `ReorgRecorder` 时返回 error（fail-closed） | 传 `service.WithReorgRecorder(postgres.NewDepositReorgStore(pool))`（或经 `ledger.EnableOnchain` 的 opts）。仅 webhook 摄入（不传 reader）的部署不受影响 |
| G-M3 (W1-onchain) | `core.SweepPolicy.GasCeiling` 的单位口径由文档的 wei **更正为 gwei**（实现一直是 gwei）；`Validate()` 现在拒收 > 1e6 gwei 的值 | 按旧文档以 wei 配置过 GasCeiling 的消费方（如 `50000000000` 表示 50 gwei）必须改成 gwei 数值（`50`），否则启动期 `SweepPolicy.Validate` 直接报错。这是把一个静默 10^9 倍失效的闸变成启动拦截 |
| G-M7 (W1-onchain) | 新增 `core.TokenConfig.Validate()`，并由 `service.Onchain.Validate*`（`Run` / `EnableOnchain` 两个入口）对 CreditTokens + SweepTokens 全量调用：`Decimals < 0` 或 `> 36` 拒启 | 修正配置。另建议在 composition root 启动时调用新增的 `(*evm.ClientSet).VerifyTokenDecimals(ctx)` 与链上 `decimals()` 交叉校验（库无 binary，无法代为强制） |
| G-M1 (W1-onchain) | 浅 reorg 不再由单次 `TxIncluded=false` 直接打终态 `failed`，默认需连续 3 次（`service.WithShallowReorgMisses`） | 无需动作；若原先依赖「立刻 failed」的时序，改配置为 1 可恢复旧行为（不建议） |
| B-m7 (W1-onchain) | `SetChainCursor` 加单调保护：向后写变成 no-op（不再报错、也不再倒退） | 无需动作。多副本部署下 watcher 现在走 per-chain advisory lock（`onchain_watch:<chainID>`），需要 `service.WithPool(pool)` 才生效 —— `ledger.EnableOnchain` 此前**从不传** pool（`service.NewLockedJob` 对 nil pool 是「跳过加锁、无条件跑」），本波已在 facade 侧补上，所以经 facade 的消费方无需动作；直接用 `service.NewOnchain` 的消费方必须自己传 `service.WithPool(pool)`，否则 watch/sweep 单飞锁静默失效 |
| B-M5 (D-lock) | `PeriodCloseStore.ClosePeriod` 现在在**排他关账屏障**下写入，并且 pool 模式自开事务。取不到屏障（默认 10s 预算内一直有 in-flight journal 写）时返回 `core.ErrTransient` 且**不追加** close 行 | 关账调用方要处理 `ErrTransient` 重试（此前 ClosePeriod 只会因输入或 DB 错误失败）。把 `ClosePeriod` 组合进 `RunInTx` 且该事务还取账本锁的消费方，可能收到 40P01 → `ErrTransient`（见 I-59 残留段）；建议关账单独走一个事务 |
| B-M5 (D-lock) | 每次 `PostJournal` / `ExecuteTemplate*` / `Reverse*` 多一条 `pg_advisory_xact_lock_shared` 往返 | 无需动作。共享锁之间不互斥，记账并发度不变 |
| B-M5 (D-lock) | 新增对账检查 `period_close_violations`；`service.ReconcileQuerier` 加方法 `PeriodCloseViolations`，`FullReconciliationConfig` 加 `PeriodCloseViolationPageLimit`（默认 200） | 自建 `ReconcileQuerier` 实现的消费方必须补这个方法（编译期可见）。用 `postgres.NewReconcileAdapter` 的无需动作 |
| B-m4 (D-lock) | `postgres.Migrate(url)` 变为 `Migrate(url string, opts ...MigrateOption)`（可变参，源码兼容），新增 `MigrateContext(ctx, url, opts...)`、`WithMigrateLogger`、`WithMigrateLockBudget`。集群迁移锁从阻塞式改为轮询 + 5 分钟预算 | 无需动作。超过 5 分钟的迁移窗口用 `WithMigrateLockBudget` 放宽；想让「正在等锁」进自己的日志用 `WithMigrateLogger` |
| B-X1 (D-lock) | `is_active=false` 的 currency / classification / journal type 现在被**新 journal 拒收**（`ErrInvalidInput`），此前是静默 no-op | 若有部署把 `Deactivate*` 当作纯展示层过滤（停用后仍继续记账），那些账务会开始报错——把维度重新激活，或改用新维度。历史与余额不受影响（软删除本意） |
| B-X1 (D-lock) | `DeactivateCurrency` / `DeactivateClassification` / `DeactivateJournalType` / `DeactivateTemplate` 对不存在的 uid 返回 `core.ErrNotFound`，此前返回 nil | 依赖「停用不存在的 uid 也算成功」的调用方（多为幂等清理脚本）需要容忍 `ErrNotFound` |
| B-m9 (D-lock) | I-3 措辞从「Every state-changing operation」收窄为「moves money or advances a money-bearing state machine」，并列出**不要求**幂等键的配置写类别 | 无代码变更。此前把 I-3 读成「所有写都幂等」的文档/客户端说明需要跟着改口径 |
| G-M5 (W1-onchain) | sweep 的 pending booking 在重播前先向链求证 nonce；若发现 nonce 已被自己的 EOA 花掉，该 tick 返回 `core.ErrConflict` 并停止该 (chain,token) 的归集 | 运维动作：按 RUNBOOK §15 手工找回丢失的 tx hash 并把 booking 推到 sent（或人工作废）后归集才会继续。旧行为是盲目重播，代价是「nonce too low」永久停摆或 underpriced 替换 |
| G-M4 (W1-onchain) | `core.Sweeper` 新增方法 `ReplacementGasPrice(ctx, chainID, signerNonce, priorTxHash) (decimal.Decimal, error)`：返回一次 gas-bump **实际会出的**报价（gwei）。sweep 的 gas-bump 分支现在用它而非 `GasPrice()` 比 `SweepPolicy.GasCeiling` | 自建 `core.Sweeper` 实现的消费方必须新增该方法（不实现则编译不过 —— 这是刻意的：静默沿用 `GasPrice` 语义等于闸继续失效）。语义要求：必须与随后同 `(signerNonce, priorTxHash)` 的 `BatchSweep` 实付一致；无 prior 时退回市价即可。`chains/evm.Sweeper` 已实现。行为变化：gas 尖峰期 bump 会被 GasCeiling 真正挡住（有 Info 日志），旧行为是照样以 prior×1.125 出价。**同形兄弟一并修**：`advanceSweep` 的 dispatch 分支（也是 revive 的出口，复用 failed booking 的 nonce）此前只被 `sweepTick` 的市价闸挡过，现在同样按本 nonce 的实际出价比 GasCeiling，超了则跳过该 tick（booking 停在 pending，下个 tick 重试）|
| F-m10 运行面 (W1-onchain) | `service.Onchain.Run` 启动时读 DB 里 `code='deposit'` 分类的 lifecycle，**有环则拒启**（`core.ErrInvalidInput`）。这是 Run 唯一一个读库的启动校验 | 消费方装的 deposit lifecycle 必须无环（`presets.DepositLifecycle` 本身无环，用它的消费方无需动作）。若确需可重入的 deposit 状态机，必须同时把 `depositTransitionKey` 改成带区分本轮的量，否则第二次到达同一状态会静默解析回第一次的幂等键（转账成功、什么都没发生）|
| 新增 migration | `017_deposit_reorgs`（带 down）| 常规 migrate；新表需纳入 D-threat 的 ownership 清扫（见迁移文件头注） |
| w1-onchain | G-M6（token 零值 fail-closed）与 G-M8（深 reorg 异常持久化）各需一条 invariant，I-52/I-53 已用完 | W3-lead（Wave 2 号段之后追加，暂定 I-62 I-63） |
| w1-onchain 兄弟扫描 | `EnableOnchain` 从不传 pool → `NewLockedJob(nil)` 静默不加锁，onchain 单飞锁对全部 facade 消费方是死的（F-M1 同形）；已在 `ledger.go` 补 `service.WithPool(s.pool)` 并带 pin | 已修（W1-onchain） |
| w1-onchain 兄弟扫描 | `advanceSweep` 的 dispatch 分支（revive 出口，复用 failed booking 的 nonce）同样绕过 GasCeiling | 已修（W1-onchain） |
| E-M5 / B-M6 (W1-facade) | `(*ledger.Service).Worker(cfg)` 的签名由 `*service.Worker` 改为 `(*service.Worker, error)` | `worker := svc.Worker(cfg)` → `worker, err := svc.Worker(cfg)` 并处理 err。返回 error 的唯一情形是在 `RunInTx` 回调里调用它（此前会静默造出一个半绑事务的 worker） |
| E-M1 (W1-facade) | `(*service.Worker).Subscribe(handler)` 的签名由无返回值改为返回 `error` | `worker.Subscribe(h)` → `if err := worker.Subscribe(h); err != nil { ... }`。返回 error 的唯一情形是在 `Run` 之后订阅（此前只打一条 Error 日志，而默认 logger 会吞掉它） |
| E-M1 / I-M11 (W1-facade) | **运行期行为变更**：`(*service.Worker).Run` 在 logger 为 `core.NopLogger`（`ledger.New` 的默认值）时**返回 error 且不启动任何 job** | 二选一：① 传 `ledger.WithLogger(...)`（推荐）；② 显式接受静默 —— `ledger.WithSilentWorker()`（facade）或 `(*service.Worker).AllowSilent()`（手搓 Worker）。**这条会让「按 README Quick Start 接线且没注入 logger」的既有部署在启动时报错**，是刻意的：此前那种部署零输出、事件永远 pending |
| I-M11 (W1-facade) | `(*ledger.Service).Worker` 现在自动 `SetFullReconciler(svc.FullReconciler(service.FullReconciliationConfig{}))` | 已经手动调 `SetFullReconciler` 的消费方无需改动（后调的覆盖自动接线）；想要非默认 `FullReconciliationConfig` 的仍在 `Worker()` 返回后自行覆盖。**副作用：此前默认不跑的 15 项对账 check 现在默认每 `FullReconcileInterval` 跑一次**，会产生新的 DB 读负载与新的告警面 |
| E-M3 (W1-facade) | `(*ledger.Service).RegisterChannel` 在 `RunInTx` clone 上返回 error（此前返回 nil 后静默丢弃） | 把 `RegisterChannel` 移到顶层 Service 上调用，且必须在 `svc.Channels()` 被求值之前 |
| E-m14 (W1-facade) | `(*ledger.Service).Ping` 改走 `DBTX()`：在 `RunInTx` clone 上探的是该事务而非连接池 | 只影响把 clone 误留到回调之外的代码 —— 那种 clone 现在 `Ping` 会报错（与它的每一次读写一致），此前报健康 |
| I-R1 / B-m1 (W1-facade) | `ledger.New` 与 `(*Service).Worker` 现在都调用 `EventStore.SetLogger`：claim-lost 的三条 Warn 从 `slog.Default()` 改走注入的 `core.Logger` | 依赖这三行落在 `slog.Default()` 的日志采集配置需要跟着改 |
| E-M4 (W1-facade) | `RunInTx` clone 上的 `Onchain()` 由恒 nil 改为返回顶层 Service 的实例 | 无需改动（此前写 `tx.Onchain().X()` 会 nil panic）。`EnableOnchain` 仍在 clone 上被拒 |
| d-tests | 32 条 invariant 的 Enforced-by 引用未导出符号或纯散文，pin 引用门禁对它们只能 advisory；需重写引用风格后收进 blocking | W3-lead |
| d-tests | I-7 Exceptions 正文未列 `deposits`/`withdrawals` 死表与 `rollup_queue`/`registration_rescans` 的 claim-lease 可空列（新 NOT NULL 门禁按既有设计分类通过） | W3-lead |
| d-web 发现 | main 上 `POST /journals/{uid}/reverse` 对任何 `Idempotency-Key` 400，ledger-react client 默认注入 → 真实调用必 400；旧 MSW 测试不模拟 header 别名中间件所以一直绿 | D-web（client 已修）/ D-contract（openapi 收口，H-M3b 取消） |
| d-threat | 修改已合入 migration 007（契约 §8 勘误）；migrate.go 安装机制变更待 W3 对抗式复审 | D-threat / W3-review |
| d-web | 根 `CLAUDE.md`「page logic must stay mirrored」补 a11y 标注写法可不同的例外注记 | W3-lead |
| d-web | J-23 的 CI 步骤 `npm audit --omit=dev --audit-level=high` 加进 `.github/workflows/ledger-react.yml` | W3-lead（D-tests 已合入） |
| d-web | d-contract 改 openapi 后 `web/packages/ledger-react` 需 `codegen` 再生成 `schema.ts` 并过 `codegen:check` | W3-lead（d-contract 合入后） |
| d-threat 接手 | migration 018 尾部照抄 001 的 `REVOKE ledger_owner FROM <runner>` 会撤掉 `Migrate` 的提权窗口（非 superuser 安装死在 020）；`Migrate` 已改逐 migration 开窗口绕过。需：清掉 018 那段 + 静态门禁「001 之外不得出现 `REVOKE ledger_owner FROM`」 | W3-lead |
| d-threat 接手 | `docs/RUNBOOK.md` 安装前提（CREATE ROLE + public CREATE；`postgres` 库 CONNECT；superuser / ledger_owner 本人 / 对 ledger_owner 的 ADMIN OPTION 三者之一）落后于 `Migrate` godoc | D-ops |

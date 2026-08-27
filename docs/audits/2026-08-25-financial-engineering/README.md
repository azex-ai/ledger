# 金融工程审计 — 2026-08-25

> 八个 territory 的只读审计 + 交叉分析。契约见
> `docs/plans/2026-08-25-financial-audit-contract.md`（territory 划分、纪律、产出格式）。
>
> **本目录是审计当时的快照。** 各 territory 报告保持提交时的原文，不随后续修复更新 ——
> 它们记录的是「审计发现了什么」，不是「现在是什么状态」。当前状态看代码与 `docs/INVARIANTS.md`。
> 本文件（README）是唯一维护到最终状态的一份。

## 处置状态（2026-08-26 收口）

**8 条 Critical 全部已修并合入。** 每条都是先复现再修，修完用同一探针复验：

| # | 发现 | 提交 | 复现证据 |
|---|---|---|---|
| C1 | 服务二进制从未接线防篡改，26 条发现依附其上 | `30bd872` | 两条产品线零引用；删掉二进制与部署面 |
| C2 | 攻击者改配置表让应用替他签 | `7ec13ff` | 以 `ledger_app` 实跑六条攻击，修复后全被拒 |
| C3 | 缓存判定跳过唯一能发现金额被改的检查 | `51afad8` | 篡改 entry 金额的新 pin |
| C4 | 角色分离与入站 webhook 互斥 | `068afb4` | 以 `ledger_app` 连库实测被拒 |
| C5 | `Settle` 释放冻结不记账，example 演示错用法 | `6eecc7c` | 实跑 100 → 84.25 |
| C6 | `''→available` 可把历史余额变成可提现 | `305d6f3` | 三条子测试覆盖三种情形 |
| C7 | 转账/收费借贷方向反了 | `d5c7357` | 实跑转账 100：付款方 +100 → 修复后 −100 |
| C8 | 「冲销剩余全部」静默少退 | `b49cb20` | 实跑：账上剩 40 → 0 |

### Major / Minor 的整改（2026-08-26，三波）

原始 44 Major + 35 Minor（合并跨 territory 重复命中后 40 + 34），经 Wave 1 / 1.5 / 2 处理。
契约与波次划分见 `docs/plans/2026-08-26-audit-remediation-contracts.md`。

| 波次 | 内容 | 结果 |
|---|---|---|
| **Wave 1** | 契约层三条：终态操作幂等键、`BalanceCheckpoint` uid 化 + I-18 门禁重建、错误分类（bizcode 14010 + 瞬时可重试） | 已合入 |
| **Wave 1.5** | Aaron 拍板恢复被弱化的两条不变式：`Transition` 幂等键改必填（I-3 恢复全称）、id-keyed checkpoint 类型移出 `core`（I-18 恢复原措辞） | 已合入 |
| **Wave 2** | 八个域并行：记账正确性 / 锁与键空间 / 防篡改接线 / DB 角色与守卫 / HTTP 与契约面 / 可观测与链上 / 文档与 examples / 测试可信度 | 已合入 |
| **Wave 3** | 符号语义收敛、`journal_entries.id` 跨分区不唯一、发版收口 | 进行中 |

**这一轮真正的产出不是「修了 N 条」，是把「靠人记得」换成了「机器会拦」**：

- I-18 门禁从硬编码词表改成从 `postgres/sql/migrations/*.up.sql` **机械派生**，并抽进
  `internal/idschema` 单一事实源（原来 `core` / `server` 各一份，会静默漂移）
- 新增 `server/openapi_contract_test.go`：反射 Go wire struct 对照 `docs/openapi.yaml`，
  **双向**报「文档有代码没有」与「代码有文档没有」，跑在 `go test ./...` 无 path filter
  —— 旧门禁只比对两个静态文件，谁都没碰过 Go handler，这是所有契约漂移的元凶
- 新增 `readme_api_surface_test.go`：任何导出的 `*Service` 方法不在 README 表里就红。
  上次审计发现「缺 11 个方法」，人工补完又漏 3 个（含 `Authorize` / `AuthorizeTemplate`
  —— RunInTx 下拿到已签名 journal 的唯一途径）。**手工补表就是会漏，两次为证**
- 新增 `presets/lifecycle_acyclic_test.go`：deposit 幂等键的安全性挂在「生命周期无环」上，
  原本只写在注释里。加一条 `failed → confirming` 重试边就会让合法重试被当成重放静默吞掉，
  而**没有一个测试会红**
- `grant_coverage_test.go` 从「镜像 schema 用的同一个谓词」改成显式三分类，**新表不分类直接 fail**

**验证方式**：每一条修复都由 Team Lead 在合并前亲手证伪 —— 拆掉实现、种回漂移、
以 `ledger_app` 实跑攻击、停掉 advisory lock。**报告说绿不算数，我自己看它红过才算。**

几个代表性证据：拆掉 advisory lock 后 10 路并发预留总额 **150 > 余额 100**（旧 pin 在同样条件下是绿的）；
还原 `expires_in` 漂移，契约门禁两个方向同时报错；拆掉存证接线，
pin 报 `the P6 batch attestation chain never advanced within 5s`；
以 `ledger_app` 伪造收据的 UPDATE 被 SQLSTATE 42501 拒绝。

### 修复期间改动的两条 invariant

不是把文档对齐到代码，是**规则本身错了**：

- **I-33** 原标题声称缓存判定「至少和 live check 一样严格」—— 它更宽松，宽松的量正好是
  attestation 到读取之间那段时间。
- **I-25** 补上 `'' → available` 的历史限制：`available` 是 `Reserve` 唯一花的桶。

### 三处「测试认证了 bug」

均**反转而非删除**，保留原有的证伪步骤，让「变了什么」可读：

- `TestFeeBundle_Template_Balance` 断言用户腿必须是 `Debit`
- `TestVerifiedBalance_TrustsCachedAuthorizedVerdictEvenIfLiveRecheckWouldFail`
- `TestReserverStore_Reserve_Concurrent`（断言的命题在有锁无锁下同样为真；**未修**，见 test-credibility）

### 修复过程中差点复制的同一个形态

`TG_ARGV` 在零参数时是 `NULL` 而非空数组 → `to_jsonb(OLD) - NULL` → `NULL` →
`NULL IS DISTINCT FROM NULL` 为假 —— 守卫装上了、触发了、**放行了一切**。
四张表拒绝了攻击而 `deposit_addresses` 没有，是**拿攻击去打守卫**才发现的；
事先验过 `jsonb - '{}'::text[]` 这个算子 —— **验了算子，没验输入**。

---

## 0. 一句话结论

> ⚠️ **本节的第一版结论是错的，保留原文与更正**——territory A 最后交付，推翻了它。
> 原文：「账本的数学是对的，接线不对。80 条发现里没有一条是『算错数』。」
> **有一条是。而且是最直接的那种。**

**账本的引擎是对的，配置的方向是反的，接线没接上。**

引擎层（借贷平衡、decimal 精度、幂等三态、Reserve 的 TOCTOU 锁、`core.Allocate` 的
big.Rat + 最大余额法分摊）确实扎实——`audit-concurrency` 的原话是「并发本身守得住」，
territory A 的原话是「舍入本身是这份代码里做得最扎实的部分」。

但**三个已发货的 preset 把借贷方向配反了**（C7），实测 P2P 转账 100 元的结果是
**付款方 +100、收款方 −100**。这不是接线问题，是记账错误。

其余 Critical 仍是同一形态：**一个机制被正确实现，然后没有被接到真实路径上，
而测试因为自己把线接好了所以一直是绿的。**

## 1. 数量与去重

| 严重度 | 原始条数 | 去重后 |
|---|---|---|
| Critical | 11 | **8** |
| Major | 44 | **40** |
| Minor | 35 | **34** |

原始条数 = 九份报告里 `[Major]` / `[Minor]` 标题的机械计数（合计 90 条）。
去重后的 Major / Minor 数以 `TODO.md` 的实际行数为准（本表初版写的 42 / 29 / ~35 / ~26
是 territory A 与 structure 补交之前的估数，已订正）。

去重后的 Critical（合并了跨 territory 的重复命中）：

| # | 发现 | 独立命中它的 territory 数 |
|---|---|---|
| C1 | 防篡改子系统在 `ledgerd` 与库门面上都未接线（`WithAttestor` / `SetAttestor` / `RequireVerifiedBalance` 三处皆缺） | **6** |
| C2 | 签名的**输入**存放在 `ledger_app` 可写的配置表里——攻击者让应用替他签 | 1 |
| C3 | `VerifiedBalance` 信任缓存判定，跳过唯一能发现 entry 被改的检查 | 1 |
| C4 | `ledger_app` 缺 `webhook_nonces` DELETE——角色分离与入站 webhook 互斥 | 1 |
| C5 | `Settle` 释放冻结不记账，而唯一的示范文档演示了错的用法并打印自相矛盾的预期值 | 2 |
| C6 | `balance_role` 的 `'' -> <role>` 合法跃迁可把非可支配余额一次性变成可提现 | 1 |
| **C7** | **三个已发货 preset 的借贷方向与 `main_wallet` 的 normal_side 相反**——转账把钱转反、收手续费给用户加钱 | 1 |
| C8 | 同维度多条 entry 时「部分冲销后再冲销剩余全部」静默少冲并返回成功 | 1 |

**C1 被六个 territory 独立命中**，是全审计信号最强的一条。它们从六个不同角度出发
（测试可信度 / 并发 / 消费方表面 / 威胁模型 / 结构 / 可运维性）收敛到同一处，
这不是回音，是同一个缺口在六个投影面上都留下了影子。

**八条全部已修复并合入**，提交与复现证据见本文开头的处置表。

## 2. 贯穿性主题：四种「绿灯的窄化」

把 90 条按**失效机理**而非 territory 重排，剩下四类：

### 主题一：机制存在，但没接到真实入口（Critical 里占 3 条）

C1（防篡改全链）、C4（webhook 权限）、以及本 session 早先修的 `Worker.Subscribe`。

共同结构：**能力在库里、示例里、测试里都工作，唯独在消费方真正会走的那条路径上缺席。**
而测试之所以绿，正是因为测试自己做了消费方不会做的接线。

`audit-tests` 对 `Worker.Subscribe` 修复的评价最精准：**「修复的回归测试原样复现了它本该防止的盲区」**
——三条回归 pin 全部手动 `SetLocalPoller`，删掉 facade 那行接线，三条照样绿。
（已补真实路径 pin，`1d65101`。）

### 主题二：检查回答了一个更窄的问题（Critical 里占 2 条）

C3：`VerifiedBalance` 的缓存判定回答「**当时**是否授权」，不回答「**至今**是否未改」。
而 canonical digest 逐 entry 覆盖 `Amount`，所以 live check 本能抓到——T4 优化跳过的
恰好是唯一有效的那道检查。

`full_coverage` 恒假（P0 为区分「没找到违规」与「验证完整」引入的信号，因一个永久跳过的
check 一票否决，从落地起不可能变绿，`"full suite passed"` 是死代码）。

I-4 的 TOCTOU pin 断言「两个并发 Reserve 都成功」——这个命题在有锁和无锁两个世界里同样为真。

### 主题三：签名/守卫覆盖了结果，没覆盖依据（C2）

守卫保护 14 张表；`ledger_app` 有 UPDATE 却零守卫的有 **22 张**，其中
`deposit_addresses` / `entry_templates` / `entry_template_lines` / `account_policies` /
`currencies` 直接决定记账结果。**实测两条攻击语句均成功。**

产出的 journal 签名有效、验签通过、链上覆盖、`verify` 报 VERIFIED、偿付能力平衡、
借贷平衡——**每一层防御都会说没问题**，因为钱确实进来了，只是记错了人。

### 主题四：fail-open 藏在「防御性代码」里

`scanner.go` 读不到余额记为零（fail-open）而并发回退路径同样情况返回 error（fail-closed），
**走哪条取决于该链碰巧有没有部署 Multicall3**；`anyToDecimal` 的 float64 分支
warn-and-continue（当前不可达，已实测）；nonce prune 失败原本让整个请求 500（已修）。

## 3. 共同盲区：八份报告都没问的问题

> 契约 §4 要求 lead 找「六个人都没提的东西」。找到一个，而且它放大 C2。

**事发之后，能不能重建出攻击者做了什么？**

账本对「谁动了钱」记录得极细——`journals` / `events` / `account_policy_changes` /
`period_closes` / `checkpoint_rebuilds` 全部带 `actor_id`。
但对**「谁改了决定钱去哪的规则」几乎不记录**：

| 表 | 审计列 | UPDATE 后可查到 |
|---|---|---|
| `entry_template_lines` | 无 | 什么都没有 |
| `currencies` | 无 | 什么都没有 |
| `deposit_addresses` | 仅 `created_at` | **连「改过」这件事都看不出来** |
| `entry_templates` | 仅 `created_at` | 同上 |

C2 里我实测成功的那条攻击（改 `deposit_addresses.account_holder`）**不留任何痕迹**：
没有 `updated_at`、没有 actor、没有历史表、没有 trigger。事后唯一的发现途径是
「注意到钱记错了人」——而那要等到受害者投诉。

这个库存在的起因是一次数据库泄露。**它把「防止篡改」做得很深，却没做「篡改之后怎么复盘」。**
`account_policy_changes` 证明这个模式是已知的（策略变更有 append-only 审计表），
只是没有推广到其余的规则表。

> **C2 修复后的现状**（`7ec13ff`）：上面那句「没有 trigger」已不成立 —— 四张配置表现在有
> append-only 守卫，那条 UPDATE 会被**拒绝**。但可见性一分没变：**拒绝本身不留痕迹**，
> 一次失败的攻击尝试与从未发生仍然没有区别。本节要的审计列/变更表仍未落地，见 `TODO.md` §9。

## 4. 流程数据点（不是代码发现，但值得记）

1. **六个 territory 有两个第一轮零产出**（`audit-financial` / `audit-structure` 未交报告即失联）。
   若只看 idle 通知而不核对文件，会误以为它们完成了。已重派范围收窄的替补，`structure` 已交。
2. **契约漏了两个 territory**（可运维性、链上资金路径），由 lead 补审，产出 7 条发现含 3 条 Major。
   定 territory 时按「代码分层」想，漏掉了「运行时」与「独立 module」两个维度。
3. **契约写了「审计员只读」，没写「lead 会不会并发写仓库」**——导致一次归因歧义，
   一个 agent 误判 lead 的提交为越权 fork 并删除了文件。流程缺口，非判断失误。
4. **交叉验证是真的**：`audit-tests` 的并行子任务独立核查、未见初稿，收敛到完全相同的两条缝隙。

## 5. 拍板结果（全部已定，一条例外）

本节初版是「待拍板」清单，现记录实际拍法：

| # | 当时的问题 | 拍法 | 落点 |
|---|---|---|---|
| C1 | `ledgerd` 是否默认启签名；密钥入口如何设计（`custody.md`：不得与 `DATABASE_URL` 同失效域） | **删掉服务二进制与整个部署面** —— 本仓不再有 composition root，密钥入口回到消费方 | `30bd872` |
| C2 | 配置表完整性：纳入签名 / 归 `ledger_owner` 只读 / 显式声明不在保护范围 | **加 append-only 守卫**（`003_config_table_guards`），`ledger_app` 不能再改记账依据 | `7ec13ff` |
| C3 | T4 缓存判定是否保留 | **保留，但缓存 Authorized 不再跳过 live check** | `51afad8` |
| C5 | 修 example 还是改 `Settle` 语义 | **修 example**（`examples/billing` 进 `RunInTx` 并断言余额） | `6eecc7c` |
| C6 | `balance_role` 跃迁是否需要额外授权 | **有历史的分类不得升为可支配**（`004_balance_role_upgrade_guard`） | `305d6f3` |
| — | §3 的审计盲区是否补（规则表加 `updated_at` + actor + 变更表） | **仍未做** —— 唯一一条拍了「建议补」却没落地的 | `TODO.md` §9 |

⚠️ C1 的拍法解决的是「谁来装配」，**没有**解决「库模式下四步接线全靠消费方自觉」——
那部分仍然开着，见 `TODO.md` §3。

## 6. 未覆盖

- ~~territory A 尚未交付~~ —— **已交付**（18:24），并推翻了本文第一版结论。
  其结论：Q1 舍入扎实（`core.Allocate` 用 big.Rat + 最大余额法，残差不丢）；
  Q2 精度校验逐入口核过**无遗漏**；Q3 四个条件里三个成立，**effective_at 回溯不成立**
  （已写入的历史快照永不失效）；Q4 找到 C7。
- `web/` 前端、性能、容量：契约 §5 明确排除。
- **DR 演练未实跑**：恢复流程只做静态阅读。
- **check #8（`pending_journal_timeout`）被删除而非重新实装** —— 它等待的 `journals.status`
  schema 字段从未添加（`TODO.md` §6「可观测与运维」原始条目所指的那个恒假信号）。移除本身是
  正确的收口：一个结构上永远返回 `Complete=false` 的检查会一票否决 `FullCoverage`，恒假的信号
  比没有信号更糟（`working-agreements.md` §3）。移除时的 commit message 称这是
  "tracked in the audit remediation backlog"，但 `docs/plans/2026-08-26-audit-remediation-contracts.md`
  与本文件都没有这一条（m-9, `.local/independent-review-2026-08-26.md`）——**这里补上**：重新实装
  check #8 需要先给 `journals.status` 加 schema 字段（`deployment.md` expand 步骤），再对着它写一个
  真正会跑的检查，取代当前永久移除的占位。未排期。

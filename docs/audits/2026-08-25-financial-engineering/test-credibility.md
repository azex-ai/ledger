# 审计报告 — test-credibility

> 本报告由多条独立核查路径合并而成（invariant 逐条核查按 I-1~16 / I-17~25 / I-26~34 三段分工，
> 另有独立的 money-path 猎杀 x2 + 消费方对等性 + 空洞断言全仓扫描）。多处发现被两条互不知情的
> 路径独立命中（I-4 的空洞并发测试、Worker.Subscribe 的 facade 旁路），这本身是交叉验证信号。
>
> 更正（归因）：本报告初稿曾误将 commit `1d65101`（`ledger_subscribe_wiring_test.go`）归因为
> 审计子任务越权提交——经 Team Lead 核实，该 commit 是 Team Lead 本人在收到本报告"Subscribe
> 回归测试绕开 facade"这条发现后，按契约 §4 逐条验证并补上的 pin，与审计子任务无关，审计过程
> 全程只读。下方 Worker.Subscribe 相关发现的证据链不受影响（基于两条独立只读核查路径各自打开
> `service/worker_subscribe_test.go` 源码得出的结论）。

## 摘要

这 30k 行测试里最硬核的部分（reversal fraction 的并发守恒、mutation guard 的直接 SQL 篡改测试、
reconcile 的 DB-backed drift 注入测试、`TestReservationStatus_AllTransitions` 这类穷举矩阵、
attestation/merkle 那一串里多处"禁用检查后确认测试变回绿/VERIFIED"的显式证伪记录）质量很高，是
仓库测试文化里最值得信任的部分。但存在几处与本次审计缘起（`SetLocalPoller` 从未真正接线、伪造
journal 被误判 VERIFIED）**同构、甚至更隐蔽的活漏洞**：

1. **（最严重）`VerifiedBalanceStore.VerifiedBalance` 的 verdict 缓存机制信任"journal 签名仍然
   有效"这个结论，却从不检查驱动余额数字的 `journal_entries.amount` 本身是否在缓存 verdict 之后
   被篡改**——这正是团队点名的历史 bug（"只比对结论是否变了，不比对结论是否为负面"）的直系兄弟，
   而且更隐蔽：它比对的甚至不是"结论"本身，而是一个跟entry内容脱钩的缓存标签。
2. I-4（TOCTOU 安全）唯一的并发测试从未构造出真正的超额并发场景，无论 advisory lock 在不在，这个
   测试都会通过——两条独立核查路径各自打开源码后得出同一结论。
3. `Worker.Subscribe` 的原 bug 修复本身是对的，但全部回归测试都绕开 `ledger.Service.Worker()`
   （真实消费者唯一能拿到 Worker 的方式），直接手动接线，原样复现了它本该防止的盲区。
4. `Worker.SetAttestor`——P6/P7 整条防篡改批量签名链的唯一调度入口——在库模式和服务模式的任何组合
   根里都从未被调用，调度循环本身也没有任何测试覆盖。
5. I-18（uid-only 外部标识）的机械扫描门禁用的是硬编码词表，遗漏了 schema 里实际存在的内部 id 列名；
   同时 `core.BalanceCheckpoint` 已经在库 API 面上暴露内部 id，违反了 I-18 文档自己的措辞，且没有
   任何 pin 覆盖这条 Go 库 API 面。
6. Extended preset bundle（FX/Capital/Settlement/Spread）、`ReserverStore.Settle` 的实际效果、
   snapshot backfill 的具体数值，都存在"测试跑通但没锁定真正要验证的东西"的缺口。

`docs/INVARIANTS.md` 自带的 pin 检查器（`TestInvariantsDocPinsAllExist`）只验证"引用的测试名字
存在且包名对"，不验证测试内容是否真的证明了该 invariant——上面第 2、5 条都是这个已知盲区的具体
实例：引用的测试真实存在、也确实在测对应的函数，但断言覆盖的场景对该 invariant 的核心命题是空的。

## 发现

### [Critical] `VerifiedBalanceStore.VerifiedBalance` 信任缓存的授权 verdict，但从不检查其对应 entry 的内容是否已被事后篡改（I-32/I-33）

- **位置**：`postgres/verified_balance_store.go:95-154`（`VerifiedBalance`，verdict 分流逻辑约在
  122-136 行，最终余额来自第 154 行 `s.recompute.RecomputeBalance(...)`）
- **判定**：CONFIRMED
- **失效场景**：`VerifiedBalance` 的契约是"任一 contributing journal 携带 cached
  `JournalAuthVerdictAuthorized`，就信任它、跳过对它的 live 签名重验"（I-33 的设计意图），但它
  **返回的余额数字**是直接对当前 `journal_entries` 求和得到的，与上面的 verdict 判断完全脱钩。
  攻击场景（本 wave 的标准威胁模型：owner-role 绕过 no-arbitrary-update trigger，I-27/I-29 的
  falsification 测试都用这个模型）：某条已经被 cache 为 `Authorized` 的 journal，其名下某个
  `journal_entries.amount` 被直接改大。`VerifiedBalance()` 会（a）把被篡改后的金额计入返回的余额，
  且（b）因为该 journal 已经落在 `authorizedByCachedVerdict` 分支里，永远不会进入
  `needsLiveCheck`，`core.VerifyJournalAuth`（本可以重新计算 digest、发现签名不匹配、抓住这次
  篡改）根本不会被调用。
- **为什么现有机制没拦住**：唯一相关的测试
  `TestVerifiedBalance_TrustsCachedAuthorizedVerdictEvenIfLiveRecheckWouldFail`
  （`postgres/attested_auth_pin_test.go:37`）**只篡改了 `journals.auth_signature`**（签名列本身），
  从未篡改 `journal_entries.amount`（驱动余额的实际内容），最后断言 `balance.Equal(250)`——因为它
  压根没动过驱动这个余额的 entry，这个断言当然为真。全仓库核对了全部 7 个 `TestVerifiedBalance_*`
  测试名，**没有任何一个**同时构造"cached Authorized + entry 内容被篡改"的场景来检验返回的余额
  数字或错误状态。I-33 文档描述的"straddle 场景"（一个 journal 的 entries 跨两个 attestation
  batch，一部分 cached Authorized、一部分 Unknown）如果被篡改的 entry 恰好落在 Unknown 部分会被
  live check 抓到，但落在 Authorized 部分则是同一漏洞的变体，同样没有专门测试覆盖。
- **最小复现**：造一个已被某次 `RunAttestBatch` 缓存为 `Authorized` 的 journal；直接 SQL
  `UPDATE journal_entries SET amount = amount * 2 WHERE journal_id = ...`（在标准威胁模型下这条
  UPDATE 本身不会被任何应用层触发器拦——`journal_entries` 的 append-only 保护是"不可 UPDATE"这件
  事本身该被 DB 权限模型挡住，但这里假设的正是"该权限已被绕过"这个 wave 明确声明的威胁模型）；
  调用 `VerifiedBalance`，预期应返回错误或至少反映真实（未篡改）金额，实际会返回一个包含被篡改
  金额的、标记为"已验证"的余额。
- **后果**：I-32/I-33 存在的唯一理由就是"withdrawal 前的 fail-closed 校验"
  （`ReserveInput.RequireVerifiedBalance`）——这里它会静默通过一笔基于被篡改金额算出的"已验证"
  余额，直到下一次周期性 `VerifyLedger`（异步、非实时）才会被发现。

### [Critical] I-4 的 TOCTOU 并发 pin 从未测试真正的超额并发场景（两条独立路径各自命中同一结论）

- **位置**：`postgres/reserver_store_test.go:127-167`（`TestReserverStore_Reserve_Concurrent`），被
  `docs/INVARIANTS.md` I-4（及 I-11）引用为 pin
- **判定**：CONFIRMED（本人直接读取了测试源码复核）
- **失效场景**：该测试给 holder 10 充值 100，然后并发发起两个 Reserve：50 和 30。50+30=80 < 100，
  两个 Reserve 无论如何都应该成功——即使 `postgres.ReserverStore.Reserve` 里的 advisory lock 被
  整个删掉，两个并发请求各自读到"余额 100，可用"然后各自插入，结果依然是两个 reservation 都成功、
  断言（`err1==nil && err2==nil && res1.UID != res2.UID`）全部通过。`docs/INVARIANTS.md` I-4 的
  "Why" 原文明确写的是"Two concurrent reserve calls can each read 'balance is enough', then both
  insert reservations, leaving the holder over-committed"——但这个测试从未逼近这个场景。全仓库
  grep `Reserve.*Concurrent` 只有这一个测试；grep `OverCommit|Overcommit|ExceedsAvailable` 唯一
  命中的 `TestReserverStore_Settle_ExceedsReservedRejected` 测的是 `Settle` 超过
  `reserved_amount`，不是并发 `Reserve` 超过可用余额。
- **最小复现**：把 `postgres/reserver_store.go` 里 `Reserve` 的 advisory lock 获取整行删掉（或
  改成 no-op），`go test ./postgres/ -run TestReserverStore_Reserve_Concurrent -race` 依然全绿。
  真正能证伪的场景（balance=100，两个并发 Reserve 各 60，预期恰好一个成功）目前在仓库里不存在。
- **为什么现有机制没拦住**：`TestInvariantsDocPinsAllExist` 只验证"被引用的测试名字在仓库里存在、
  包名对"，不验证测试内容是否真的证明了该 invariant（I-15 曾因"引用了不存在/不相关的测试名字"被
  这个检查器抓到过一次）；这次是同一类问题的变种：引用的测试**存在且确实在测这个函数**，只是断言
  覆盖的输入场景对这条 invariant 的核心命题（"超额并发会被拒绝"）是空的。

### [Critical] `Worker.Subscribe` 修复的回归测试复现了它本该防止的盲区

- **位置**：`service/worker_subscribe_test.go`（`TestWorker_Subscribe_HandlerReceivesEvent`、
  `TestWorker_Subscribe_HandlerErrorDoesNotBlockQueue`、
  `TestWorker_PollerWithoutSubscribe_DoesNotDrainQueue`、
  `TestWorker_SubscribeAfterPoller_UsesTheStoredPoller`）与 `ledger.go:712-739`
  （`Service.Worker`，facade 组合根）
- **判定**：CONFIRMED
- **失效场景**：原始事故是"`ledger.Service.Worker()` 建出来的 Worker 没有自动接好
  `SetLocalPoller`，消费者只调用 `Subscribe` 永远收不到事件"，修复把 `w.SetLocalPoller(s.eventStore)`
  加进了 `ledger.go:737`。但四个 Subscribe 相关测试全部通过 `newMinimalWorker(engine)`
  （`service` 包内部辅助函数，直接 `service.NewWorker(...)`）构造 `Worker`，然后**手动调用
  `worker.SetLocalPoller(poller)`**——这正是真实消费者不会做、只有测试会做的那一步骤，原封不动地
  留在了修复本身的回归测试里。全仓库 grep 找不到任何测试调用 `ledger.New(pool).Worker(...)` 再
  `.Subscribe(...)`：`ledger_test.go` 和 `postgres/tx_composition_test.go` 是仓库里仅有的两个
  调用 `ledger.New(` 的测试文件，两者都不出现 `Worker`/`Subscribe` 字样。
- **最小复现**：把 `ledger.go:737` 的 `w.SetLocalPoller(s.eventStore)` 删掉（或不小心挪到一个不会
  执行到的分支），`go test ./...` 全绿——包括这次修复自己新增的全部测试——因为它们从不经过这行代码。
- **为什么现有机制没拦住**：修 bug 的人显然理解问题所在（`ledger.go:737` 上方注释写得很清楚），但
  把回归测试写在了"内部 Worker 的 Subscribe 逻辑对不对"这一层，而不是"facade 的接线对不对"这一层
  ——两者是不同的断言对象。这是 `working-agreements.md` §3 失败模式的一个变种：不是"什么都没发生
  被当成通过"，而是"测试验证了相邻但不同的东西，真正需要验证的接线步骤被测试自己绕开了"。

### [Critical] `Worker.SetAttestor`——防篡改批量签名链的唯一调度入口——在任何组合根都未被调用，调度本身也从未被测试跑过

- **位置**：`service/worker.go:152`（`SetAttestor` 定义）、`service/worker.go` 约 279-284 行
  （`w.attestation != nil` 才会执行的调度分支）
- **判定**：CONFIRMED
- **失效场景**：`ledger.go:712-736`（`Service.Worker(cfg)`，库模式唯一组合根）从不调用
  `w.SetAttestor(...)`；`cmd/ledgerd/main.go`（服务模式组合根）同样不调用；`examples/tamper-evident/
  main.go` 是仓库里唯一提到 `AttestationService` 的地方，但它直接调 `attestSvc.RunAttestBatch(ctx, 500)`，
  同样绕开 `Worker.SetAttestor` + 定时循环。一个按标准流程接入这个库的消费者
  （`ledger.New(pool, ledger.WithAttestor(...))` + `svc.Worker(cfg)` + `go worker.Run(ctx)`），
  I-27~I-30 的整条 Merkle root / 外部锚点 / gapless 覆盖能力**永远不会自动运行**，除非消费者自己
  发现并手写一个没有出现在任何文档或示例里的 `worker.SetAttestor(...)` 调用。
- **最小复现**：按标准流程部署这个库，只调 `ledger.New` + `svc.Worker(cfg)` + `Run(ctx)`；一年后
  `ledger_attestations` 表是空的，`ledger-cli verify` 无事可验证，且没有任何告警。
- **为什么现有机制没拦住**：`grep -rn "SetAttestor|attestation" service/worker_test.go` 零命中——
  `Worker` 的调度循环本身完全没有测试覆盖，无论是否走组合根。所有 I-27~I-33 的测试都直接构造
  `service.NewAttestationService(...)` 手动调 `RunAttestBatch`，验证的是"这个函数逻辑对不对"，
  从未验证"这个函数会不会被真的调起来"——这正是 `SetLocalPoller` 事故的同类兄弟。

### [Major] I-18（uid-only 外部标识）的机械扫描门禁有遗漏，且 `core.BalanceCheckpoint` 已在库 API 面违反该契约

- **位置**：`server/contract_pin_test.go:17-18`（`TestContract_NoInternalIDKeysInJSON`）、
  `core/checkpoint.go:11-25`（`BalanceCheckpoint`）、`core/interfaces.go:143`
  （`CheckpointIntegrityStore.RebuildCheckpoint`）
- **判定**：CONFIRMED
- **失效场景**：`TestContract_NoInternalIDKeysInJSON` 只用固定枚举的 banned 词表
  （`id|currency_id|classification_id|journal_type_id|booking_id|reservation_id|event_id|
  journal_id|reversal_of|template_id`）扫 `server/*.go`。但 `postgres/sqlcgen/models.go` 里实际
  存在的内部 id 列名如 `policy_id`、`entry_id`、`last_entry_id`、`previous_last_entry_id`、
  `new_last_entry_id` 完全不在词表里——这些名字一旦被未来某个 handler（如 policy 变更审计端点、
  checkpoint_rebuilds 审计端点）用作 JSON 字段，这个门禁不会报警，当前只是"目前没人这么用"而非
  "机制堵死了"。更直接的是：`core.BalanceCheckpoint`（`CurrencyID`/`ClassificationID` 均为
  `int64` 内部 id）通过消费方可见的 port `CheckpointIntegrityStore.RebuildCheckpoint` 直接返回给
  库消费方——这正是 I-18 文档原文声称"library-mode Go API(`core` types and interfaces speak uids
  exclusively)"的地方，当前已经不成立。I-18 的两个 pin 测试（`TestContract_NoInternalIDKeysInJSON`
  只扫 HTTP JSON、`TestReconcileFindings_NoInternalIDPatternsInSource` 只扫 reconcile 报告文本）
  都不覆盖 Go 库 API 面，这个违例不会被现有门禁抓到。
- **判定级别**：CONFIRMED（读了 regex 词表、枚举了 schema 实际列名、追踪了 interface 暴露路径）

### [Major] Extended preset bundle（含 FX）从未在真实 Postgres 上过账测试

- **位置**：`presets/fx_test.go`（及 `capital_test.go`/`settlement_test.go`/`spread_test.go` 等
  同类文件，均为 `package presets` 内部测试，针对 `presets/presets_test.go` 定义的 fake store）；
  `ledger.go:551`（`Service.InstallExtendedPresets`，真实消费者调用的 facade 方法）
- **判定**：CONFIRMED
- **失效场景**：`presets/fx_test.go` 的全部测试只调用 `Render()`（纯函数）或针对
  `newFakeClassificationStore()`/`newFakeJournalTypeStore()`/`newFakeTemplateStore()`
  （纯 in-memory map，无精度校验、无 FK；且 fake 版 `SetBalanceRole` 允许任意方向切换，不像真实
  `postgres.ClassificationStore.SetBalanceRole` 那样只允许从空值升级一次）——**从未调用过
  `ledger.PostJournal`/`ExecuteTemplate` 把 `fx_sell`/`fx_buy` 模板真正过账到 Postgres**。
  `InstallExtendedPresets` 在全部 `*_test.go` 里只在 `presets/devcredit_test.go:115` 被调用过
  一次，且同样是对着 fake store。`examples/credits-topup/main.go` 和 `examples/billing/main.go`
  是仓库里仅有的两处真实（对接真实 Postgres）`svc.InstallExtendedPresets(ctx)` 用法，而
  `find examples -name "*_test.go"` 零命中——没有 CI 会跑这些 example。
- **最小复现**：在 `postgres.ClassificationStore.CreateClassification` 或
  `postgres.TemplateStore.CreateTemplate` 里故意引入一个只对 `settlement`/`fx_sell`/`fx_buy`
  生效的 bug（例如精度校验用错 exponent），`make test` 全绿。
- **为什么现有机制没拦住**：`presets` 包的测试策略是"用 fake store 验证 bundle 的结构/幂等/方向"，
  这对大多数逻辑是合理的分层，但对"这个 bundle 真的能在真实约束下过账"完全是空白——fake store
  的行为比真实 store 更宽松，意味着 fake 环境下"通过"的东西不能反推真实环境也会通过。

### [Major] `ReserverStore.Settle`（区别于 `SettlePartial`）的实际效果在单测层完全没有被验证

- **位置**：`postgres/reserver_store_test.go` 全文 8 处 `store.Settle(ctx, ...)` 调用（分属
  `TestReserverStore_Reserve_Settle`、`TestReserverStore_Reserve_Release`、
  `TestReserverStore_Settle_InvalidTransition`、`TestReserverStore_Settle_ZeroAmountRejected`、
  `TestReserverStore_Settle_NegativeAmountRejected`、
  `TestReserverStore_Settle_ExceedsReservedRejected`、
  `TestReserverStore_Settle_ExactReservedAmountAccepted`）
- **判定**：CONFIRMED
- **失效场景**：这 8 次调用里，凡是"应该成功"的分支，断言只有 `require.NoError(t, err)`，随后没有
  任何一处检查 `HeldAmount`、reservation 的 `Status` 是否变为 `settled`、有没有 journal 被真正
  post 出来、或账户余额是否按预期方向变化。`TestReserverStore_Settle_ExactReservedAmountAccepted`
  （名字明确宣称验证了"exact amount 被接受"）函数体在 `require.NoError` 后直接结束。如果 `Settle`
  内部逻辑被改错——比如金额判断对了但忘记真正扣减 `held_amount`，或状态没有真正落到 `settled`——
  这个文件里的全部测试仍然全绿。
- **相关测试**：`postgres/integration_test.go` 确实在一次端到端流程里验证了 `Settle` 之后
  `wallet` 和 `locked` 两个分类的余额正确变化——但那是唯一一次，且只覆盖"预留全额、结算全额"这一种
  输入组合。`TestReserverStore_HeldAmount` 覆盖了 `Release` 对 held amount 的影响，却没有覆盖
  `Settle` 对 held amount 的影响。
- **为什么现有机制没拦住**：`docs/INVARIANTS.md` 没有把"`Settle` 正确扣减/结转"单列为一条
  invariant（只在 I-11 里以 `SettlePartial` 的名义间接提到），所以没有 pin 机制去检查这个缺口。

### [Major] snapshot backfill 只验证行数/天数，从未断言具体余额数值

- **位置**：`service/snapshot_backfill.go` 约 83 行（`backfillSingleDay`）
- **判定**：CONFIRMED
- **失效场景**：唯一覆盖 `BackfillSnapshots` 的测试 `TestBackfill_FiveDays`
  （`service/snapshot_integration_test.go:150-194`）给 holder 造了 5 天递增的存款
  （100/200/300/400/500，累计余额应为 100/300/600/1000/1500），backfill 后只断言
  `result.DaysProcessed==5`、`result.SnapshotsCreated>0`、`balance_snapshots` 行数==5。**从未
  读取任何一行 snapshot 的 `balance` 列去比对期望的累计值**。若 `backfillSingleDay` 的
  `cutoff := date.AddDate(0,0,1)` 出现差一日的偏移（漏算/多算当天存款），或底层 `ListBalancesAt`
  的 SQL 边界条件写反，行数仍是 5（因为每天余额确实不同），测试照样全绿。
- **判定级别**：CONFIRMED

### [Minor] `AggregateCheckpointsByClassification`（system_rollups 聚合来源）的跨 holder 聚合从未被真实测试验证

- **位置**：`postgres/sql/queries/checkpoints.sql` 约 192-199 行（`AggregateCheckpointsByClassification`）
- **判定**：CONFIRMED（缺口本身），严重度定为 Minor 因为暂无证据表明当前 SQL 写错，只是"如果写错
  不会被发现"
- **失效场景**：SQL 语义应为跨 holder 按 `(currency_id, classification_id)` 聚合。但唯一真实
  Postgres 路径（`service/reconcile_full_integration_test.go` 的
  `TestFullReconciliation_DetectsSystemRollupDriftFromPoisonedCheckpoint`）全程只用一个 holder；
  `service/system_rollup_test.go` 的 `TestSystemRollupService_MultipleAccounts` 名字暗示"多账户"，
  但它 mock 掉了 `AggregateCheckpointsByClassification` 本身，根本没跑真实 SQL。没有任何测试验证
  "两个不同 holder、同一 (currency, classification) 的 checkpoint 会被正确加总"——如果这条 SQL
  的 `GROUP BY` 不小心多带了 `account_holder`，现有测试全部照样通过。

### [Minor] `CanonicalJournalDigest` 不覆盖 `JournalInput.Metadata`，且这个排除未被文档声明（缓解：受 append-only trigger 保护）

- **位置**：`core/auth.go` 约 201-231 行（`CanonicalJournalDigest`）、`core/journal.go:78`
  （`Metadata map[string]string`）
- **判定**：CONFIRMED
- **说明**：签名摘要枚举的字段里没有 `Metadata`，且这个排除不像 `EventUID` 那样被 I-26 正文显式
  讨论过。缓解因素：`journals.metadata` 不在 `ledger_journals_block_arbitrary_update()` 的
  `mutable` 白名单里，所以 UPDATE 会被 append-only trigger 拦（标准威胁模型下成立），真正可利用
  窗口很窄（需要复用同一 idempotency_key 的 INSERT，而它是 UNIQUE 的）。不构成可实际利用的资金
  路径漏洞，但属于"签名声称覆盖 posting 全貌，实际有一个未披露例外"，按
  `working-agreements.md` §4 的标准应当被写进文档而未写。

### [Minor] I-21 的"事件-journal 交叉链接"断言其实没有测——`core.Booking` 结构上就拿不到 EventUID

- **位置**：`service/onchain_integration_test.go:1136`（`TestOnchain_ApproveReview_PostsJournalWithEventLink`）
- **判定**：CONFIRMED
- **失效场景**：`docs/INVARIANTS.md` I-21 与测试名都声称这个测试验证"approve review 后发出的
  `deposit_confirm` journal 通过 `EventUID` 与事件交叉链接"，但测试实际只断言
  `Status=="confirmed"`、`JournalUID` 非空、`approved_by` metadata、以及重复 approve 的幂等性
  ——从未查询或断言任何 event/journal 的链接关系。`core.Booking` 这个类型本身根本没有
  `EventUID`/`EventID` 字段暴露出来，所以这个测试在结构上就没有能力验证它名字里声称的交叉链接
  命题。这个命题可能确实成立（I-10 在别处覆盖了同事务 event/journal 链接），但**这一个 pin 没有
  验证它**。

### [Minor] I-19（sweep 从不入账）的 postgres 层 pin 单独看只覆盖 `sent → confirmed`

- **位置**：`postgres/invariants_test.go:411-449`（`TestSweepBooking_NeverPostsJournal`）
- **判定**：CONFIRMED（覆盖缺口本身），但不构成实质风险——I-19 一并引用的另一个测试
  （`service.TestOnchain_Sweep_NonceReuseAndNoJournal`）确认覆盖了重试/`failed` 分支下
  `JournalUID` 始终为空。两条 pin 合起来看覆盖是完整的，仅记录在此供归档。

### [Minor] booking metadata 幂等排除列表从 1 个键涨到 4 个键，只有第一个键有专属测试

- **位置**：`postgres/idempotency_match.go` 约 124-144 行（排除列表：`block_number`、
  `review_reason`、`reject_reason`、`approved_by`、`rejected_by`）
- **判定**：PLAUSIBLE
- **失效场景**：`docs/INVARIANTS.md` I-20 的文字仍只提到"this one metadata key"（即
  `block_number`），但代码里排除列表已经涨到 5 个键，其中 4 个没有专属测试证明"排除不会误伤真实
  冲突"。全仓库只有 `TestDepositBooking_IdempotencyKey_StableAcrossBlockNumberChurn` 验证了
  `block_number` 这一个键。

### [Minor/PLAUSIBLE] check1（`global_dr_cr_equality`）缺少"检测到真实全局失衡"的集成测试

- **位置**：`service/reconcile.go` 约 478 行（`runCheck1JournalBalance`）
- **判定**：PLAUSIBLE
- **说明**：底层 `CheckAccountingEquation` 本身被 `TestReconciliationService_Imbalanced`/
  `CrossCurrencyMismatch` 扎实锁定了具体数值，但 wrapper 自身（`r.Balanced → result.Passed`、
  `r.Details → Findings` 的映射）在 `FullReconciliationService.RunFullReconciliation` 这一层
  没有对应的 `TestFullReconciliation_..._DetectsGlobalImbalance` 式集成测试——check2/3/4/5/6/7/
  9/10/systemRollupIntegrity/snapshotIntegrity/unauthorizedJournals 全部有这种"在真实报告里断言
  Passed=false"的测试，唯独 check1 没有，只能推断它"应该没问题"。

## 保护充分、值得记录的对照组（不是发现，供 Team Lead 判断本报告的基线）

- `core/journal.go`、`core/template.go`、`core/reserve.go`（FSM 转换表）、`core/money.go`
  （`Round`/`ConvertAt`/`Allocate`）：全部有锁定具体输出值的测试或 property test（
  `TestAllocateInvariant_SumAlwaysEqualsTotal` 500 次随机 trial + `FuzzAllocate`），核心计算逻辑
  被删掉会立刻见红。
- `postgres/rollup_queue_redirty_test.go` 的 claim/lease 机制（`TestRollupQueue_
  StaleWorkerCannotMarkOrReleaseReclaimedRow`）、reconcile 的 check2/3/4/5/6/7/9/10/
  systemRollupIntegrity/snapshotIntegrity/unauthorizedJournals（均有真实注入 drift 并断言
  `Passed=false` 的测试，不是只测 happy path）。
- I-26~I-33（除上面 I-32/33 那条）：篡改类测试普遍断言具体的负面结论而非泛泛的
  `!= VERIFIED`；`TestVerifiedBalance_TrustsCachedAuthorizedVerdictEvenIfLiveRecheckWouldFail`
  做了先证明活检查会失败、再证明缓存路径依然通过的真正分离验证；
  `TestNaiveIDRangeWatermark_WouldMissTheLateEntry` 把被否决的替代设计跑一遍同一 fixture 证明其
  结构性丢数据。

## I-1 ~ I-34 逐条核实情况（供 Team Lead 快速定位复核优先级）

- **深度核实且确认扎实**：I-1、I-2（含并发 reversal 互斥）、I-3（`TestIdempotency_ConcurrentSameKey`
  真用 100 goroutine + channel barrier）、I-5、I-6~I-10、I-8（穷举矩阵）、I-11（除 I-4 共用的空洞
  并发测试外，`TestReserve_AvailableBasisExcludesPendingLockedAndRoleless` 等均为真实断言）、
  I-12~I-17、I-19、I-20、I-22（`TestGrantCoverage_*` 真动态枚举，非硬编码表名单）、I-23、I-25
  （journals 部分：`TestJournalsGuard_FutureColumnsProtectedByDefault` 真的 `ALTER TABLE`
  验证"未来列默认受保护"）、I-27~I-31、I-34。
- **深度核实且发现缺口**：I-4（Critical）、I-18（Major）、I-21（Minor）、I-32/I-33（Critical）。
- **WEAK（有缓解，不构成实质风险，已记录）**：I-26。

## 移交

- `presets/fx.go` 声称"settlement 吸收净额"，但代码本身不做任何汇率/舍入计算——两腿金额完全由
  调用方传入，账本只保证"每一腿各自借贷平衡"。"是否真的守恒"取决于调用方能否正确配平两腿金额，
  属于 **territory A（financial-correctness）**的问题；但请一并知悉：这个问题目前**无法用现有
  测试回答**，因为没有 DB-backed 测试曾经把两条腿真正过账到真实 Postgres（见上方 Major 发现）。
- `examples/` 目录零自动化测试（没有 CI 跑过任何一个 `examples/*/main.go`）——这既是
  **territory C（consumer-surface，"examples 是唯一的真实用法文档"）**的问题，也是本报告多个
  发现的共同根源，提请合并去重时注意。
- `Worker.SetAttestor` 从未被组合根调用，同时是 **territory C**（消费者读文档/示例学不到要调用它）
  和 **territory D（threat-model）**（P6/P7 防篡改能力可能在真实部署里完全不生效）的问题——本报告
  只从"测试是否验证了它会被调起来"的角度报告，具体该不该在组合根默认接线，请另外两个 territory 复核。
- `core.BalanceCheckpoint` 在库 API 面暴露内部 id（见上方 I-18 发现）——这既是 **territory F
  （structure，"接口在消费方/uid-only 契约"）**的问题，也可能是 **territory D** 关心的信息泄露面，
  提请两边都过一遍。
- `presets/presets_test.go` 里的 `fakeClassificationStore.SetBalanceRole` 不像真实
  `postgres.ClassificationStore.SetBalanceRole` 那样限制"只能从空值升级一次"——这是
  **territory F（structure）**可能关心的"测试替身与真实实现语义漂移"问题，附带指出供参考。

## 我没能验证的

- **规模声明**：132 个 `*_test.go` 文件、30k 行测试，多条核查路径合计直接打开并逐行核对断言的
  文件约 45-50 个，集中在 money path 相关文件与被 `docs/INVARIANTS.md` 引用的 pin 测试。
- **34 条 invariant 中，逐条打开源码核实（非仅读文档描述）的约 28 条**；I-6 的
  `pkg/httpx.TestDecode_*` 具体断言内容、I-8 的 `FuzzLifecycleValidate` 语料库健康度、I-30/I-31
  的部分并发窗口测试（未逐行确认是否真用 goroutine 触发而非顺序调用）——只做了结构性核查，未逐行
  确认。
- **完全没有打开读过的测试文件**：`server/` 全部 handler 测试（除 I-18/I-34 相关抽查）、
  `channel/onchain/` adapter 测试、`service/partition_test.go`、`service/expiration_test.go`、
  `postgres/onchain_*` 等约 50 个文件。
- **互斥测试（两个测试断言矛盾行为）**：多条核查路径均未找到确凿的例子。抽查了 account-policy 的
  frozen/min-balance 语义（文档描述得非常细致，显然经过多轮打磨），没发现矛盾，但这只是抽样，不是
  穷举，不能排除还有未发现的契约冲突。
- **`postgres/rollup_boundary_test.go`（`TestRollup_BoundarySemanticsAcrossConsecutiveRollups`）、
  `service/reconcile_full_integration_test.go` 里除 system-rollup 之外的 Check2 分页/续跑测试**：
  只读了名字/抽查了一个，未逐一打开核对。

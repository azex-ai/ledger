# Security Review — M3 补偿控制（threshold gate + reconciliation → review queue）

> reviewer `rev-m3-sec` · scope `git diff c82526b~1..HEAD`（#10 core + #11 HTTP）· **read-only, no code changed**
> 前一轮基线 `.team/context/review-sec.md`（C1/M2 已修，M3 = 本次实现）· 设计 SoT §9
> money path: 伪造 sighting = 凭空铸钱。M3 是 §5-1「RPC-as-oracle 无上限铸币」的唯一补偿控制——审它是否真堵住。

---

## 结论（先说）

**M3 的机制实现是正确的、fail-closed 的、且真正堵住了 §5-1 的「无上限」铸币——但有一个前提：`AutoCreditCeiling` 必须被配成正值。**

- ✅ 伪造单源大额 sighting（amount > AutoCreditCeiling）**必进 review 不自动过账**：`reviewGate` 阈值门在过账前、且排在对账之前，over-ceiling 立即返回 review，与是否配置第二源无关（onchain.go:675）。这正是 M3 要堵的路径，已堵。
- ✅ 对账门 fail-closed：第二源 err → `advanceConfirmation` 返回 error，booking **停在 confirming，永不自动过账**；included=false 或金额不等 → review。攻击者控主 RPC 拿不到自动铸币。
- ⚠️ **残留 1（可接受的设计权衡）**：amount ≤ AutoCreditCeiling 且 ≤ ReconcileCeiling → 两门都跳过 → 单源自动过账。即「小额仍信任单源 RPC」，单笔铸币被**有上限**地限制在 AutoCreditCeiling。§9.2 明确接受。
- 🟠 **残留 2（必须在上线前钉死，见 MJ1）**：`AutoCreditCeiling` 缺省 = 0 = 关闭；`DepositConfirmer` 缺省 nil = 关闭。**两门全关时 = 单次 confirmed sighting 即过账 = pre-M3 的无上限铸币原样保留**，且 composition root 无任何 guard 拒绝/告警「开了 onchain 入账却没设 ceiling」。M3 把工具造好了，但**默认是开着的门**。

一句话：**机制堵住了；默认配置没堵。** 上线前把 MJ1 钉死即闭环。

---

## 🔴 CRITICAL
无。M3 核心机制无 critical。

## 🟠 MAJOR

### MJ1 — 补偿控制默认全关，且无 composition-root guard（"secure by default" 缺失）
- **位置**：`core/onchain.go:197`（`AutoCreditCeiling` 0=关）+ `:203`（`ReconcileCeiling` 0=关）；`service/onchain.go:326` `NewOnchain` 不校验；无 `ChainConfig.Validate()`；`reviewGate` onchain.go:675/678 两门均 `IsPositive()` 短路。
- **攻击场景**：消费方启用 onchain 入账（`SetDepositIngester` + webhook secret / watcher），但 `AutoCreditCeiling` 留默认 0 且未配 `DepositConfirmer`（最省事的配法，也是 copy-paste COOKBOOK 最容易漏的一项）。→ 每笔达确认阈值的 sighting **单源直接过账，无金额上限**。`EVM_WEBHOOK_SECRET` 泄漏或主 RPC 说谎 = 无上限铸币，与 M3 之前完全一致。M3 的存在给了「已加固」的错觉，反而更危险。
- **为何 Major 而非 Critical**：是配置依赖，不是代码缺陷；但这是**整个入账信任模型的落地开关**，且默认态 = 全开，属于「安全默认」反模式。对 money-path trust boundary，安全默认必须优先。
- **修法（择一，建议都做）**：
  1. `NewOnchain`（或 `Run()` 启动校验）在**有 credit token 但该 token `AutoCreditCeiling` 非正**时 fail-fast 或强制 `log.Warn` 一条显著告警（"onchain deposit ingestion enabled with UNBOUNDED auto-credit for token X — set AutoCreditCeiling"）。
  2. RUNBOOK/COOKBOOK crypto 段头部加硬性 fence：「启用入账 **必须** 为每个 credit token 设正 `AutoCreditCeiling`，否则等于无上限铸币」。当前 RUNBOOK §13 讲了 review 处理，但没讲「不设 = 门是开的」。
- **注**：这是本次评审给 backend-engineer + go-financial 拍板的点——库形态下 opt-in 是惯例，但这一项的默认值直接等于「信任边界敞开」，值得破例做成 fail-fast 或 mandatory。

### MJ2 — 人工 approve/reject 无操作者归属（audit 缺失）
- **位置**：`service/onchain.go:772 ApproveReview` → `postDepositConfirmedJournal:727`（journal `Source: onchainSource`，无 `ActorID`）；`:834`/`:793` 的 `log.Warn` 只记 booking_uid/amount/reason，**不记谁 approve 的**；HTTP handler（`handler_deposit_reviews.go:79/103`）也不从 API-key 提取 actor。
- **攻击场景/后果**：approve 是**直接触发补铸币**的最高权动作（review→confirmed + 过账）。事后审计无法回答「哪个 key / 哪个操作员批准了这笔铸币」。若某 ScopeWrite key 泄漏被用来 approve 伪造大额 review booking，journal / event / 日志三处都查不到 actor，事件溯源断链。§9.4 只提了 emit `deposit.review_approved` 供告警，没要求记 actor。
- **修法**：把 API-key 身份（middleware_auth 已有 scope，应也有 key id）透传到 `ApproveReview/RejectReview` → 落 Transition metadata 的 `approved_by` / journal 的 `ActorID` / 日志字段。至少日志必须带 actor。

## 🟡 MINOR

### mi1 — approve/reject 只校验 status，不校验 classification（defense-in-depth，当前不可利用）
- **位置**：`ApproveReview:780`/`RejectReview:816` 仅 `switch booking.Status { case "review" ... }`，**不校验 booking 的 classification == deposit**。`postDepositConfirmedJournal` 无条件用 `depositConfirmTemplate` 过账。
- **当前可利用性 = 无**：只有 deposit preset 用 `"review"` 状态（withdrawal 用的是 `"reviewing"`，presets 全表已确认）。所以今天所有 `review` 态 booking 都是 deposit，无法用 `/deposits/{uid}/review/approve` 驱动任意 booking。
- **潜在风险**：一旦将来任何 classification 引入 `"review"` 状态，ScopeWrite 调用方即可对其调 approve → 强行走 deposit_confirm 铸币到该 holder。`ListReviews` 已正确按 `classification=deposit` 过滤，approve/reject 却没有这层护栏。
- **修法**：approve/reject 在 status 检查旁加一条 `booking.ClassificationCode == presets.DepositClassificationCode`（或 GetBooking 后断言），不匹配返 ErrConflict。廉价，闭合结构。

### mi2 — 无职责分离：同一 ScopeWrite key 可自建 + 自批
- approve 落在 ScopeWrite 组（routes.go:113）。ScopeWrite 已能直接 POST /journals、/bookings/{uid}/transition，所以 approve **不构成越权**（不是新增能力，与既有写面一致）。但对「铸币复核」这类控制，理想是职责分离：能触发/摄入 deposit 的 key ≠ 能 approve review 的 key，否则复核形同虚设（自己造大额→自己批）。
- **建议**（设计层，非本期阻塞）：为 review 批准引入独立 scope 或至少与 ingester key 分离。§9.4 明确定了 ScopeWrite，故按设计执行；此条留作后续加固记录。

### mi3 — reject_reason / review_reason 原样存储并经 API 回显（stored-XSS 供给源）
- `RejectReview` 把 operator 自填 `reason` **verbatim** 写进 booking metadata（onchain.go:830），handler 注释显式说不做 sanitization（operator-authored）。`bookingToResponse` 会把 metadata 连同 `reject_reason`/`review_reason` 回显到 `GET /deposits/reviews` 与 approve/reject 响应。
- 风险低（operator 是可信面），但若消费方前端直接渲染该 metadata 而不转义 → stored XSS。ledger 侧按契约存原文，**转义责任在消费方 UI**——建议在 docs/frontend.md 或 RUNBOOK 注一句。

### mi4 — 对账 err fail-closed 路径无测试覆盖
- `fakeDepositConfirmer.ConfirmDeposit`（integration_test.go:198）永不返回 error，故 `reviewGate` 的 err 分支（第二源宕→停 confirming，不自动过账）这条**安全关键的 fail-closed 路径未被断言**。代码路径本身正确，但回归无保护。
- **修法**：加一个 confirmer 返回 error 的用例，断言 booking 停在 `confirming`、无 journal、可重试。

### mi5 — 第二源持久性故障 → 合规入账在 band 内无限期滞留（availability）
- reconcile err = fail-closed 停 confirming（安全正确）。但若第二源长期不可用，落在 (ReconcileCeiling, AutoCreditCeiling] 区间的**合法**充值将一直卡 confirming、既不 confirmed 也不进 review，用户拿不到钱。安全上无害（不铸币），可用性上是坑。
- **建议**：对账连续失败 N 次后要么进 review（交人工），要么发 metric/告警，别静默卡死。当前只在 recheck 循环里 log。

---

## ✅ 已核实无问题（逐条对 team-lead 威胁清单）

**T1 铸币防线成立**：`reviewGate` 阈值门用 `Amount.GreaterThan(AutoCreditCeiling)`（严格 >），amount == ceiling 走过账 = 配置允许的上限本意，非绕过。over-ceiling 无条件先于对账触发（onchain.go:675 在 678 之前 return），伪造大额永远进 review。✔

**T2 对账门不可绕过**：主 RPC 被控但第二源独立时——`ConfirmDeposit` err → `advanceConfirmation` 返回 error → 停 confirming，**fail-closed**（onchain.go:632-635）。`!included || !amount.Equal` 两种不一致都 → review（onchain.go:690）。included=false 与金额不等均拦。✔（唯一前提：`DepositConfirmer` 已配 + `ReconcileCeiling` 正值——见 MJ1 的默认关问题）

**T3 approve 高权 + 鉴权**：approve/reject 在 ScopeWrite 组（routes.go:113-114）；ScopeWrite 本就能 POST /journals + transition，故 approve 不是越权升级（能力等价）。审计缺口见 MJ2；自批问题见 mi2。✔（鉴权分级本身一致）

**T4 review_reason/reject_reason 排除出幂等比较——不构成双花绕过**：`bookingMetadataObservationVariantKeys`（idempotency_match.go:124）剥离 `block_number`/`review_reason`/`reject_reason` 三键。关键点：这三键**从不出现在 `CreateBookingInput.Metadata`**（review/reject 由 CreateBooking 之后的 Transition 注入），攻击者无法经 sighting/webhook 注入它们（IngestDeposit 硬编码只写 chain_id/tx_hash/txlog_seq/token/block_number，onchain.go:562-568）。而 amount/holder/currency/classification/channel/expires_at 仍**逐字段精确比较**（idempotency_match.go:93-98）→ same key + 不同 amount 仍 ErrConflict。剥离对双花中性。✔

**T5 reject 脱敏 + 非 review 态行为**：approve/reject 先 GetBooking 校验 status——`confirmed`/`failed` 幂等 no-op，其余非 review 态返 `ErrConflict`（onchain.go:780-787 / 816-823）；reject 要求非空 reason（handler:119）。**不能用它 transition 任意 booking**（classification 护栏缺失见 mi3，但今天不可利用）。reason 脱敏责任移交 caller，见 mi3。✔

**其他**：
- `review` 态在 `advanceConfirmation` switch 的早退分支（onchain.go:608），watcher/webhook 重观测已 review 的 sighting = no-op，绝不再评估门或过账——只有人工 approve/reject 能移出。✔
- 双 approve 安全：`deposit-confirm-{uid}` 幂等 key + booking.journal_id set-once + 第二次 approve 时 status 已 confirmed → no-op。无双铸。✔
- I-21（review 期账本零变动）：review 分支不进 `postDepositConfirmedJournal`，journal 只在真 confirmed 时产生；integration_test.go:892/930 已 pin。✔
- reconcile 调用在 `RunInTx` 之外（onchain.go:670-673 注释 + 结构确认），不违反「DB 事务内禁外部调用」。✔
- recheck 循环（recheckOneDeposit:1064）复用 `advanceConfirmation` → 门在 recheck 路径同样生效，无第二条绕过门的过账入口。✔

---

## 给 team-lead 的拍板项

1. **MJ1 是唯一上线阻塞项**：默认全关 = 无上限铸币原样保留。建议 backend-engineer + go-financial 拍板：`NewOnchain`/`Run` 对「有 credit token 却无正 `AutoCreditCeiling`」做 fail-fast 或强告警（打破库的 opt-in 惯例，因为这一项默认值 = 信任边界敞开）。至少 RUNBOOK 加硬 fence。
2. **MJ2** 建议本期补：approve/reject 落 actor 归属（money-path 审计）。
3. mi1(classification 护栏) / mi4(fail-closed 测试) 廉价，建议顺手补。mi2/mi3/mi5 记录为后续加固。

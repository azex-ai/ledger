# Wave 3 对抗式复审 — money-path

> 复审对象：`git diff fd87cae..HEAD`（HEAD = `676ae7e`，Wave 1 + Wave 2 全部合入后的 main）。
> 只读复审。所有实跑在仓外一次性 clone `/tmp/w3rev`（`git clone --no-hardlinks`）+ testcontainers
> 里做，主仓工作树零改动；PoC 代码全文内联在下面各条，可原样贴回一个 clone 重跑。
> 目标不是「看看有没有问题」，是**证伪**：每个攻击面回答「这个修复在什么输入 / 时序 / 权限下
> 仍会算错钱、丢状态、或让钱离开系统」。
>
> 威胁模型沿用本仓标准第一行：**攻击者持有应用的数据库凭证 `ledger_app`**，且所有签名都是真的。
> 这不是我引入的假设——I-49 的 "Why" 段自己就是按这条写的。

## 摘要

八个攻击面里 **三个被攻破、一个部分攻破、四个未攻破**。

最要紧的一条：**I-49 修好了 `min(V, E)` 这一半，但没有修同一个表达式里的另一半——
减掉的「活跃预留额」这一项。** `SumActiveReservations` 读 `reservations` 表，而
`ledger_reservations_guard`（001_baseline:1161）**故意允许** `active -> settling` 与
`settled_amount` 增大，`SumActiveReservations`（reservations.sql:73-78）对 `settling` 行只记
`reserved - settled`。于是**一条 `UPDATE` 就把 hold 归零，同时预留仍然可结算**：实跑中一个
1000 余额的 holder 拿到了两笔各 1000 的**闸内**（`RequireVerifiedBalance: true`）预留，
第一笔的 `FinalizeSettlement` 依旧返回 `nil`。I-49 的正文明写
「Holds are subtracted from either base identically: reservations are not part of what a
checkpoint or an unsigned journal can misstate」（`postgres/reserver_store.go:425`）与
「it cannot allow one that reader would have refused」（`docs/INVARIANTS.md:4902`）——
这两句话在当前代码下都不成立。这与 C-1 是**同一形态的兄弟**（「决定放多少钱的那个数来自
攻击者可写的地方」），契约 §0 要求的横向扫描只扫了余额项、没扫 hold 项。

第二组：**「checkpoint 是不可信缓存」这条结论只落到了 `Reserve` 一个消费点。**
另外两个消费点仍然逐字读 `checkpoint + delta`——`account_policies.enforce_min_balance`
（I-17 自称「the only balance floor」的那一个，也是直接 `PostJournal` / `ExecuteTemplate`
路径上唯一的透支闸），以及 `SolvencyCheck`（全库唯一的无背书发行告警）。两条都用一条
`INSERT INTO balance_checkpoints` 实跑破掉：min_balance 从「拒绝 -500」变成放行，
solvency 从 `solvent=false` 变成 `solvent=true`。

第三组：`Migrate` 的逐 migration 提权窗口（契约 §8 标为必审）**是角色级的，不是会话级的**。
`GRANT ledger_owner TO <runner> WITH INHERIT TRUE` 写 `pg_auth_members`，对**所有**以该角色
连接的会话立即生效。本仓 7 个 example 里 7 个都用同一个 `DATABASE_URL` 同时喂 `Migrate` 和
`pgxpool.New`，所以在每次迁移期间，应用连接池本身就是 owner-equivalent——append-only 触发器
可以 `DROP`。RUNBOOK:693 的原话是「There is no window ... where a connection has broader
access than the table below describes」。

被证伪（未攻破）的四个也写出来了：I-51 的冲销子集规则我没能用多 journal 分步绕过；
「把伪造 journal 标成 `unsigned_tx_mode` 来洗白」在**已被 attest 覆盖**的路径上被
auth_verdict 检查抓住（我原本认为这是最重的一条，实跑推翻了）；受保护模板的结构派生闸没找到
铸币口；扫链游标 / TxLogSeq / 重放我没能构造出重复入账或丢账。

---

## 攻击面逐条结论

| # | 攻击面 | 结论 |
|---|---|---|
| 1 | I-49 提现闸 `min(V, E)` | **攻破**（不在 V/E，在被减掉的 holds 项）— C-1 |
| 2 | 符号修复 / solvency / 存量修正 | **部分攻破**：模板组合未攻破；`SolvencyCheck` 经 checkpoint 攻破（M-2）；scope fail-loud 只防全错、不防漏（m-1/m-2）；存量修正路径**已在 CHANGELOG 交代**，未攻破 |
| 3 | I-51 冲销 / 事件链 | 冲销**未攻破**；`event_uid` 冒领**攻破**为永久锁死（M-3） |
| 4 | 受保护模板结构派生 + deposit-tolerance | **未攻破** |
| 5 | 扫链 I-52 / I-53 / receipt 位置 | **未攻破**（一条行为变更未登记，m-6） |
| 6 | `Migrate` 逐 migration 提权窗口 | **攻破**（角色级而非会话级，M-5） |
| 7 | 锚与验证（I-55 / 零签名） | 危险方向**未攻破**（观测表只能升不能降）；不可恢复的误报 TAMPERED**攻破**（m-4）；未覆盖 + `unsigned_tx_mode` 只报 DRIFT（M-4）；`unauthorized_journals` 的跳过口径**攻破**（M-7） |
| 8 | 默认关着的东西 | **攻破**：默认装机 `Warnings: []`（M-6） |

---

## 发现

### [Critical] C-1 · I-49 的 hold 项由攻击者可写的 `reservations` 决定 —— 闸内 `Reserve` 可以对同一笔余额授权两次

- **位置**：
  - `postgres/reserver_store.go:442`（`SumActiveReservations`）与 `:454`（`available := availableBase.Sub(activeReservedDecimal)`）
  - `postgres/sql/queries/reservations.sql:73-78`（`settling` 行只计 `reserved - settled`，`WHERE status IN ('active','settling')`）
  - `postgres/sql/migrations/001_baseline.up.sql:1161-1197`（`ledger_reservations_guard`：`reserved_amount` 不可改，但 `active -> settling` 允许、`settled_amount` **允许增大**）
  - `postgres/sql/migrations/001_baseline.up.sql:1498-1502`（grant 循环：`reservations` 没有 `ledger_block_mutation` 的 UPDATE 触发器 → 归入非 append-only → `GRANT SELECT, INSERT, UPDATE ... TO ledger_app`；021 只 REVOKE 了 `deposits` / `withdrawals`）
  - 被推翻的正文：`postgres/reserver_store.go:425-426` 与 `docs/INVARIANTS.md:4832-4834` / `:4902-4903`
- **判定**：**CONFIRMED（实跑）**
- **失效场景**：holder 余额 1000（entries-only，全部签名有效）。
  1. `Reserve(1000, RequireVerifiedBalance: true)` → 成功，hold = 1000。
  2. 对照：再 `Reserve(1000, RequireVerifiedBalance: true)` → `available 0 < requested 1000: insufficient balance`（闸工作正常）。
  3. 攻击者（`ledger_app`）执行**一条**语句：
     ```sql
     UPDATE reservations SET status = 'settling', settled_amount = reserved_amount WHERE uid = '<R1>';
     ```
     触发器放行（两项改动都在白名单内）。此时 `SumActiveReservations` 对 R1 记 `1000 - 1000 = 0`。
  4. 再 `Reserve(1000, RequireVerifiedBalance: true)` → **成功**。闸对同一笔 1000 授权了 2000。
  5. R1 仍可结算：`FinalizeSettlement(R1)` → `nil`。
  `min(V, E)` 全程正确（都等于 1000）——被篡改的是它后面那个减号。
- **最小复现**（实跑输出）：
  ```
  HOLDS first gated reservation 01a063e7-54e4-...  for 1000 on a 1000 balance
  HOLDS control second reservation refused: postgres: reserve: available 0 < requested 1000: insufficient balance
  HOLDS second gated reservation 01a063e7-54f4-... ALSO granted for 1000 -- gate authorized 2000 against 1000
  HOLDS FinalizeSettlement(first) -> <nil>
  ```
  PoC（放进 clone 的 `postgres/zz_w3_holds_test.go`，复用现成的 `setupVBFixture` / `newTestAttestor`）：
  ```go
  f := setupVBFixture(t, pool, ctx); const holder int64 = 9407
  attestor, verifier := newTestAttestor(t, "ed25519-w3-holds")
  ls := postgres.NewLedgerStore(pool).WithAuth(attestor)
  ls.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("d"), decimal.NewFromInt(1000)))
  reserver := postgres.NewReserverStore(pool, ls, postgres.NewVerifiedBalanceStore(pool, verifier))
  first, _ := reserver.Reserve(ctx, core.ReserveInput{AccountHolder: holder, CurrencyUID: f.CurrencyUID,
      Amount: decimal.NewFromInt(1000), IdempotencyKey: postgrestest.UniqueKey("a"), RequireVerifiedBalance: true})
  // control: second Reserve -> ErrInsufficientBalance
  pool.Exec(ctx, `UPDATE reservations SET status='settling', settled_amount=reserved_amount WHERE uid=$1`, first.UID)
  // second Reserve -> nil, and FinalizeSettlement(first) -> nil
  ```
  以 `ledger_app` 身份执行该 `UPDATE` 的权限也实跑过（见 M-1 的 PoC-H 输出：`UPDATE reservations status/settled -> <nil>`）。
- **为什么现有机制没拦住**：
  - I-49 的两条 pin（`RejectsInflatedCheckpoint` / `RechecksUnderLock`）都只压 `min(V, E)` 的两个输入项，**没有一条 pin 触碰 hold 项**；I-11 的 pin 压的是「并发 Reserve 不过量」（正常路径），不是「hold 数被改」。
  - I-11 的 Scope 段（`docs/INVARIANTS.md`「holds bind Reserve, and only Reserve」）讨论的是「hold 挡不住直接 journal」，从来没讨论「hold 数本身是否可信」。
  - 契约 §0 的兄弟扫描把 C-1 描述成「金额读了 checkpoint」，于是扫描面被框在 `balance_checkpoints` 上；真正的形态是「**决定放多少钱的算式里，有项来自攻击者可写的表**」，`reservations` 是这个形态的第二项。
- **修法建议**（供 lead 拍板，非本报告职责）：闸内路径的 hold 不应读 `reservations.status/settled_amount`
  这两个可变列。可选：(a) 从 `reservation_settlement_legs` / `reservation_operation_receipts`
  （二者都是 `ledger_block_mutation` 全封 + REVOKE UPDATE，006:135-149）重算已结算额，
  hold = `reserved_amount - Σ(receipt legs)`，`reserved_amount` 本来就不可改；
  (b) 给 `reservations` 的 `settled_amount` 增量加「必须有对应 receipt 行」的触发器约束，
  让「结算了多少」只能由 append-only 表说。任一种都要配一条「篡改 `reservations` 后闸仍拒绝」的 pin。

---

### [Major] M-1 · `enforce_min_balance` —— 直接 journal 路径上唯一的透支闸 —— 仍然读 `balance_checkpoints`

- **位置**：`postgres/account_policy_enforce.go:114`（`before, err := s.getBalanceWithQueries(...)`）→ `postgres/ledger_store.go:1449-1476`（`GetBalanceCheckpoint` + `SumEntriesSinceForClassification`，即 checkpoint + delta）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**：I-17 自己写着「the only balance floor in the system was `Reserve`'s
  available-balance check, which a direct journal post bypasses entirely」——`enforce_min_balance`
  就是补这个洞的那一半。`PostJournal` / `ExecuteTemplate` 没有 `RequireVerifiedBalance` 这种
  对应物，所以对「消费方通过直接 journal 出款」这条路径，min_balance 是唯一的闸，而它读的正是
  I-49 的 Why 段亲手定性为 untrusted cache 的那张表。
  实跑：holder 9201 余额 0，`min_balance = 0, enforce_min_balance = true`。
  ```
  MINBAL before tamper: postgres: post journal: account 9201 ... balance -500 would fall below min_balance 0 (policy 1): insufficient balance
  -- 攻击者：INSERT INTO balance_checkpoints (..., balance, last_entry_id) VALUES (9201, cur, cls, 1000000, 0)
  MINBAL after tamper: err=<nil>
  MINBAL true entries-only balance now: -500 (floor was 0)
  ```
- **最小复现**：见上；PoC 全文 = clone 的 `service/zz_w3_poc2_test.go` 的 `TestW3C_...`，
  只用 `postgres.NewAccountPolicyStore(pool).SetPolicy` + `postgres.NewLedgerStore(pool).PostJournal`，
  没有绕过任何接线。
- **为什么现有机制没拦住**：W1-gate 的独占面被切成 `postgres/reserver_store.go` +
  `verified_balance_store.go`，`account_policy_enforce.go` 不在里面；I-17 的三条 min_balance pin
  测的是「金额算得对不对」，没有一条测「金额来源可不可信」。事后有 `checkpoint_balance` 对账
  能发现漂移——但这正是 I-49 自己驳回过的兜底（"the money is gone by then"）。

---

### [Major] M-2 · `SolvencyCheck` 两侧都是 checkpoint + delta —— 全库唯一的无背书发行告警可以被一条 `INSERT` 变绿

- **位置**：`postgres/sql/queries/platform_balances.sql:81-96`（liability，`COALESCE(bc.balance,0) + COALESCE(d.delta,0)`）与 `:120-137`（custodial，同形）；消费点 `postgres/platform_balance_store.go:196-232`
- **判定**：**CONFIRMED（实跑）**
- **失效场景**：
  ```
  SOLVENCY before tamper: liability=1 custodial=-1 solvent=false
  -- 攻击者：INSERT INTO balance_checkpoints (holder=-9301, cur, cls, balance=1000000, last_entry_id=0)
  SOLVENCY after tamper:  liability=1 custodial=999999 solvent=true
  ```
  RUNBOOK 与 `presets/capital.go` 都把 `SolvencyCheck` 当作「唯一会喊无背书发行」的东西
  （`capital.go:53-56`：*"the only shipped templates that move the solvency margin"*）。
  它读的是不可信缓存，而 `custodial` 侧的维度只要有过任何 entry，checkpoint 行就能被抬高。
- **最小复现**：clone 的 `service/zz_w3_poc2_test.go` 的 `TestW3D_...`，
  `postgres.NewPlatformBalanceStore(pool).WithCustodialClassCodes(code).SolvencyCheck`。
- **为什么现有机制没拦住**：W1-sign 的验收是「每个已发货 preset 一条偿付能力 pin」——
  全部在**干净库**上跑，checkpoint 与 entries 恰好一致，所以读哪一个都一样（和 C-1 里
  `AllowsWhenEverythingSigned` 那条 pin 完全同形的失效）。没有一条 pin 在 checkpoint 与
  entries 分叉时断言 solvency 的取值。
- **附注**：与 M-1 合起来看，「checkpoint 不可信」这条结论目前只在 `Reserve` 一处落地。
  I-49 的 Why 段（`docs/INVARIANTS.md:4859-4866`）是全库范围的论断，实现是单点的。

---

### [Major] M-3 · `event_uid` 冒领仍可永久锁死一笔 booking 的结算，且没有解除路径

- **位置**：`postgres/ledger_store.go:1066-1071`（set-once 拒绝）与 `:1123-1128`（I-51 rule 4 的维度检查，只要求 booking 的 `(holder, currency)` 出现在 entries 里）；`docs/INVARIANTS.md`「Amounts and classifications are deliberately *not* constrained」
- **判定**：**CONFIRMED（实跑）**
- **失效场景**：I-51 rule 4 把「任何 journal 都能冒领任何事件」收窄成「任何**触碰同一
  (holder, currency)** 的 journal 都能冒领」。对一个 write-scope key 来说这不是障碍——
  holder 和 currency 是它本来就知道的。实跑：
  ```
  WEDGE squatter journal 01a063e4-... claimed event 01a063e4-... (0.01, unrelated classification)
  WEDGE real settling journal: postgres: post journal: event "..." is already linked to a journal: conflict
  WEDGE bookings.journal_id is now permanently 1 (the squatter's)
  ```
  0.01 的一笔无关 journal 之后，这笔 booking 的真实结算 journal **永久**无法记账：
  `events.journal_id` 与 `bookings.journal_id` 都是 set-once，`ledger_bookings_guard`（006:57-90）
  明写 journal_id set-once，本库不提供任何 unlink / 管理修复入口，且 journal 是 append-only
  所以冒领 journal 也删不掉。整条充值/结算流水线对该 booking 停摆，没有恢复手段。
- **最小复现**：clone 的 `postgres/zz_w3_event_wedge_test.go`（改自现成的
  `TestPostJournal_EventUID_RejectsUnrelatedJournal`，唯一改动是把 outsider 换成 owner、
  金额换成 0.01）。
- **为什么现有机制没拦住**：I-51 rule 4 的 pin（`RejectsUnrelatedJournal`）用的是**不同
  holder**，所以最弱的规则和最强的规则在这条 pin 下无法区分。正文承认了「the weakest rule
  that makes the claim mean something」，但没有写「因此同 holder 的永久锁死仍然可达，且不可
  恢复」——`working-agreements.md` §3 的「降级必须落痕」在文档面上缺这一句。
- **修法建议**：要么给 `events.journal_id` / `bookings.journal_id` 一条 owner-only 的解除路径
  （审计留痕），要么把 rule 4 收紧到「journal 必须至少有一条腿的金额与 booking 的 amount 相关」
  ——前者更现实，因为后者会挡掉正当的多腿结算。至少 I-51 要写明这条残留。

---

### [Major] M-4 · 未被 attest 覆盖的伪造 journal，只要自称 `unsigned_tx_mode`，就只报 DRIFT 而不是 TAMPERED

- **位置**：`service/attest_verify.go:518-522`（step 3b：`case core.AuthStatusUnsignedTxMode: uncoveredTxMode++`，counted not flagged）与 `:584-586`（step 4 同口径）；`journals.auth_status` 是 `postgres/sql/migrations/001_baseline.up.sql:314-315` 的普通 CHECK 列，没有守卫触发器，`ledger_app` 在 INSERT 时可以任意选值
- **判定**：**CONFIRMED（实跑）**
- **失效场景**：
  ```
  UNCOVERED auth_status=unsigned_tx_mode     -> DRIFT     [2 entr(ies) ... not covered by any attestation yet (1 of those journals were posted in tx mode); the next attestation run covers them]
  UNCOVERED auth_status=unsigned_no_attestor -> TAMPERED  [2 of the 2 entries no attestation covers belong to 1 journal(s) with no valid authorization ...]
  ```
  同一笔 1,000,000 的直插伪造，只因为 `auth_status` 一列不同，一个报「良性积压，下一轮就覆盖了」，
  一个报「这就是直插伪造的样子」。**攻击者选哪一个由攻击者决定。**
  窗口有多长取决于批次 attest 是否在跑：`WithAttestor` 只装签名器，批次 attest 是
  `svc.Worker` 的一个 job（I-54 property 4）。**签名装了但 Worker 没跑（或 attest job 被关）
  的部署，这个 DRIFT 是永久的**——而 `ledger-cli verify` 对 DRIFT 退出 0。
- **最小复现**：clone 的 `service/zz_w3_poc2_test.go` 的 `TestW3B_*` 两条（攻击 + 对照）。
- **未攻破的那一半（写出来，因为它推翻了我原本的假设）**：如果攻击者**让**下一轮 attest 覆盖
  这些 entry，`RunAttestBatch` 会给该 journal 算出 `auth_verdict = unauthorized`，
  `VerifyLedger` 的链上重查随即报 TAMPERED：
  ```
  auth_status="unsigned_tx_mode" -> VerifyLedger status=TAMPERED
    reasons=[seq 2: 1 attested journal(s) carry an UNAUTHORIZED authorization verdict (internal journal ids [2]) -- a journal live in this batch has no valid signature]
  ```
  所以 `unsigned_tx_mode` 不是通用的洗白标签——**只在「尚未被覆盖」这段时间里有效**。
  同样地，闸内 `VerifiedBalance` 不看 `auth_status`（`core.VerifyJournalAuth` 只看 digest/签名，
  `core/auth.go:489-507`），所以提现闸不受此影响。这是我这轮唯一一次「假设被自己的实跑推翻」，
  按 §1.3 记录在此。
- **为什么现有机制没拦住**：step 3b 的两条 pin
  （`FlagsUncoveredUnsignedEntry` / `UncoveredButLegitimateEntriesAreDriftNotVerified`）
  分别用「默认 auth_status 的伪造」和「真的走 tx-mode 路径的合法 journal」——
  没有一条用「**伪造的**、但**自称 tx-mode** 的 journal」，也就是这两条 pin 的分界线正好
  由攻击者选择的那一列决定。

---

### [Major] M-5 · `Migrate` 的提权窗口是角色级的，不是会话级的；本仓所有 example 让应用连接池在迁移期间变成 owner-equivalent

- **位置**：`postgres/migrate.go:230-261`（`applyRemainingMigrations`）、`:263-278`（`withLedgerOwner`）、`:331-357`（`elevateToLedgerOwner`：`:343` 的 `pg_has_role(..., 'USAGE')` 探测 + `:351` 的 `GRANT ledger_owner TO <runner> WITH INHERIT TRUE`）；`examples/fullstack/backend/main.go:81` + `:85`（同一个 `dbURL` 先 `ledger.Migrate` 再 `pgxpool.New`）；其余 6 个 example 同形（`examples/embed/main.go:56`、`billing:59`、`tx-compose:53`、`event-subscribe:54`、`crypto-deposit:75`、`tamper-evident:92`）；被推翻的文档：`docs/RUNBOOK.md:691-694`
- **判定**：**CONFIRMED（读码 + 机制确定性）**——机制层面无歧义：`GRANT <role> TO <role>` 写
  `pg_auth_members`（cluster 级共享目录），Postgres 的权限判定每条语句都过
  `has_privs_of_role()`，**不区分会话**。我没有单独实跑「迁移中另一条连接是否真的能 DROP
  TRIGGER」，标注在「我没能验证的」一节。
- **失效场景**：注释把这个窗口宣传为「narrows the window from 'the whole install' to 'one
  migration'」（migrate.go:207-228 的注释），但窗口的**维度选错了**：收窄的是**时间**，没有收窄
  **主体**。窗口打开期间，任何以同一 runner 角色连接的会话都继承 `ledger_owner`：
  - 迁移期间应用池里的连接可以 `DROP TRIGGER journal_entries_no_update`、
    `TRUNCATE journal_entries`、`DETACH PARTITION`——即 I-22 在这段时间内不成立；
  - `revokeLedgerOwner` 失败时（migrate.go:379-397 已正确上报），这个状态是**永久**的；
  - 021 之后每次 `Migrate` 都会重开一次窗口（每个 migration 一次 GRANT/REVOKE 往返）。
  RUNBOOK:691 的「There is no window, on a fresh install, where a connection has broader
  access than the table below describes」在有非 superuser runner 时是错的，并且它自己在
  :726-741 描述了这个 GRANT——两段互相矛盾。
- **附带的第二个副作用**：`elevateToLedgerOwner` 的探测是
  `pg_has_role(current_user, 'ledger_owner', 'USAGE')`，SET-only 成员资格返回 false。
  001 给 bootstrap 凭证的正是 SET（migrate.go:171-176 自己说 "holds SET but not INHERIT"）。
  于是第一次非 superuser `Migrate` 会把那条成员行**改成 INHERIT**，收尾的
  `REVOKE ledger_owner FROM runner` 再把整行**删掉**——`Migrate` 悄悄拆掉了 001 有意留下的
  SET 成员资格。可用 ADMIN OPTION 重建，但这是 `Migrate` 在改运维方没要求它改的权限配置。
- **最小复现**（未跑，机制清楚）：以 CREATEROLE 非 superuser 装到 001 之后，另开一条同角色
  连接 `SELECT pg_has_role(current_user,'ledger_owner','USAGE')`，在 `Migrate` 运行 002..N
  期间该值为 true；随后 `DROP TRIGGER journal_entries_no_update ON journal_entries` 应当成功。
- **为什么现有机制没拦住**：`postgres/migrate_elevation_test.go` 验的是「窗口在每条退出路径上
  都会释放」（时间维度），没有一条断言「窗口期间**别的**连接看不到这份权限」（主体维度）。
  契约 §8 把它标成「W3 对抗式复审必审项」是对的——这就是它藏的东西。
- **修法建议**：要么明确文档化「运行 `Migrate` 的凭证必须与应用凭证不同，且迁移期间不得有
  其它会话使用它」，并把 7 个 example 改成两个 URL（`MIGRATE_DATABASE_URL` + `DATABASE_URL`）；
  要么改成 `SET SESSION AUTHORIZATION` / `SET ROLE` 方案（注释 :299-303 说 golang-migrate
  自己开连接所以 SET ROLE 不适用——那就要把 owner 连接交给 migrate 驱动，而不是靠改角色图）。
  不改机制也行，但 RUNBOOK:691 那句必须删。

---

### [Major] M-6 · 默认装机把整套防篡改栈全关着，而 `StartupReport().Warnings` 是空的

- **位置**：`service/worker.go:298-323`（`StartupReport`：只在 `attestation != nil && anchor == nil`、`!LeaderElection`、dev-anchor 三种情况下追加 warning）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**：
  ```
  DEFAULT StartupReport = {FullReconcile:true EventDeliveryWebhook:false EventDeliveryLocalCallback:false
                           Attestation:false AttestationAnchor:false AttestationAnchorType: Partition:true
                           LeaderElection:true Warnings:[]}
  DEFAULT Warnings      = [] (len=0)
  DEFAULT AuthVerifier  = <nil>
  ```
  `ledger.New(pool, ledger.WithLogger(...))` + `svc.Worker(...)`：没有 attestor → 没有签名、
  没有 `VerifiedBalance`（任何带 journal 的维度直接 `ErrUnauthorizedJournal`）、
  `unauthorized_journals` 被跳过、`VerifyLedger` 恒 NOT_RUN。也就是**比「attest 但无锚」
  更严重的降级**——而后者会 warn，前者一声不响。I-54 property 2 的原话是
  「A degraded-but-permitted mode ... is never reported only by a log line」，这里连 log line
  都没有。
- **最小复现**：clone 的 `zz_w3_defaults_test.go`（根包，8 行）。
- **为什么现有机制没拦住**：I-54 的 pin `StartupReportIsReadableWithoutALogger` 用的是
  **`WithAttestor` 的 Service**，所以「没有 attestor 时报什么」没有任何 pin。
- **顺带**：同一默认下还有两件事无信号——(a) 没有任何 `account_policies` 行 = 全库无透支底线
  （I-17 的默认「unconstrained」），(b) `RequireVerifiedBalance` 逐调用 opt-in，
  消费方漏传时行为与 0.6.0 之前一致且不可观测。建议 `StartupReport` 增
  `Signing bool` + 对应 warning，把「本部署没有防篡改」变成数据。

---

### [Major] M-7 · `unauthorized_journals`：只要一页里有**一条**签名 journal，任意多条伪造的未签名 journal 都会被静默跳过并报绿

- **位置**：`service/reconcile.go:1533-1535`（`if j.AuthKeyID == "" { continue }`）与 `:1572-1580`（只有 `checked == 0 && len(journalList) > 0` 才降 `Complete`）
- **判定**：**CONFIRMED（读码）**
- **失效场景**：D-tamper 修的是 C-M8「零条已签名仍报绿」，做法是「**全部**被跳过时才不报绿」。
  跳过逻辑本身没变：`auth_key_id = ''` → `continue`，不计入 `checked`，不产生 Finding。
  于是一页 200 条里有 1 条真签名 + 199 条直插伪造 → `checked = 1` → `Passed = true,
  Complete = true` → `ReconcileCheckResult` 绿。fleet 上任何有历史签名的库都满足这个条件。
  （`VerifyLedger` 会抓到——但那是另一条链路、另一个退出码，且如 M-4 所示它也有窗口。）
- **最小复现**（未跑，读码充分）：一页里放一条 `WithAuth` 的 journal + 若干直插 journal
  （`auth_key_id = ''`），`RunFull` 的 `unauthorized_journals` 结果为 `Passed=true, Complete=true`。
- **为什么现有机制没拦住**：C-M8 的修法把「零覆盖」当成唯一的不诚实状态，
  但「1/200 覆盖」和「200/200 覆盖」在机器可读输出里仍然是同一个信号。
  `CheckResult` 里已经有 `checked` 的概念，但它没有出到 `Complete` 以外的任何字段。
- **修法建议**：`Complete` 应当是 `checked == len(journalList)`（或把 `checked/total`
  作为字段吐出来，让告警侧自己定阈值），而不是 `checked > 0`。

---

### [Minor] m-1 · custodial scope 的 fail-loud 只防「全错」，不防「漏一个」

- **位置**：`postgres/platform_balance_store.go:209-221`（`CountClassificationsWithCodes` → `:215` 的 `matched == 0` 才报错）
- **判定**：CONFIRMED（读码）
- **失效**：`WithCustodialClassCodes("custodial", "setlement")`（拼错一个）→ `matched = 1` → 通过，
  `settlement` 里的 FX 存货 / transit 全额静默缺席。§7.3 引入注入式口径的理由正是
  「a deployment naming its custody classification something else got Custodial = 0 with no
  error at all」——这条修复只覆盖了「一个都没命中」，没覆盖「命中一部分」，而多码 scope
  正是这次新引入的形态。建议逐码校验，报出未命中的那些码。

### [Minor] m-2 · 没有任何约束说「被命名为 custodial 的分类必须是资产类」

- **位置**：`ledger.go:150-172`（`WithCustodialClassCodes`）+ `postgres/platform_balance_store.go:39-61`
- **判定**：CONFIRMED（读码）
- **失效**：`DefaultCustodialClassCodes` 的注释精确解释了为什么 `equity` / `fees` / `spread` /
  `dev_credit` **不**在里面（它们不是背书 holder 债权的资产），但这个判断只存在于注释里。
  消费方 `WithCustodialClassCodes("custodial", "dev_credit")` 一行配置，就能让「唯一的无背书
  发行告警」把它本该暴露的缺口加回资产侧。既然 §7.3 已经把「什么算托管资产」从硬编码改成
  「按分类属性判定」，那这里就该真的按属性拦（`is_system` 且 `balance_role = ''` 且不在
  已知负债/备忘集合），而不是接受任意码。

### [Minor] m-3 · `ledger_app` 的 `'' -> <role>` 升级可以把非可用余额搬进闸内的可用基数

- **位置**：`postgres/sql/migrations/003_config_table_guards.up.sql:111-135`（`ledger_classifications_guard`，`:121` 只拒绝 `OLD.balance_role <> ''` 的改动，即允许 `'' -> <role>` 升级）；消费点 `postgres/reserver_store.go:253-282`（V）与 `:297-323`（E），两者都从 config 现读 `balance_role`
- **判定**：CONFIRMED（权限已实跑：`UPDATE classifications balance_role ''->available -> <nil>`，以 `ledger_app` 身份）
- **失效**：某个 role-less（`balance_role = ''`）的用户侧分类上有真实、签名有效的正余额时，
  一条 `UPDATE classifications SET balance_role='available'` 会让 V 和 E **同时**变大，
  `min` 起不到作用，闸放行本不可动用的钱。已发货 preset 不产生这种分类（要么带 role 要么是
  `memo`），所以这条依赖部署自定义分类——但 I-49 正文把「只从 config 读 `balance_role`」写成
  安全论证（`docs/INVARIANTS.md:4855-4857`），实际上这正是残留面，应写明。
  `memo -> available` 被守卫拒绝（`OLD.balance_role <> ''`），方向是对的。

### [Minor] m-4 · `anchor_observations` 对 `ledger_app` 开放 INSERT + append-only ⇒ 一行伪造观测把 `VerifyLedger` 永久钉在 TAMPERED

- **位置**：`postgres/sql/migrations/018_...up.sql:221-231`（no-update / no-delete 触发器 + `GRANT SELECT, INSERT ... TO ledger_app`）；`service/attest_verify.go:463-472`（`anchorSeq < lastObserved` → TAMPERED，`lastObserved = MAX(observed_seq)`）
- **判定**：CONFIRMED（权限已实跑：`INSERT anchor_observations -> <nil>`，以 `ledger_app` 身份）
- **危险方向未攻破**：只能 INSERT，`MAX` 只能升不能降，所以**无法让回退看起来像前进**——
  这是 I-55 的设计目标，成立。
- **残留**：反方向可达且**不可恢复**。`INSERT ... observed_seq = 999999` 之后
  `anchorSeq < lastObserved` 恒真，`VerifyLedger` 永久 TAMPERED，而表是 append-only、
  owner-only 才能删、RUNBOOK 没有对应处置流程。fail-closed 是对的，但「一次误写 / 一次
  恶意写 = 永久红灯且无 runbook」应当在 I-55 与 RUNBOOK 里写明处置路径（owner 侧的
  更正记录，而不是删行）。

### [Minor] m-5 · CHANGELOG 对 migration 016 的极性描述反了

- **位置**：`CHANGELOG.md:66-69`
- **判定**：CONFIRMED（读码）
- **原文**：「those journals are append-only and read with the **OLD** polarity after the
  migration」。016 把 `equity.normal_side` 改成 `debit` 并删掉 equity 的 checkpoint/snapshot
  （016:88-101），`ledger_signed_amount` 现读 `classifications.normal_side` 的**新**值，
  所以旧 entry 迁移后是按**新**极性解释的——含义整体翻转，这正是需要 reverse+repost 的原因。
  这句话把结论说成了相反的机制。（「reverse them and re-post」这个动作本身是对的。）

### [Minor] m-6 · I-53 的确认深度下界使 watcher 侧充值不再经过 pending/confirming 两阶段，未登记为行为变更

- **位置**：`service/onchain.go:1580`（`safeTip := latest - confirmationDepth(cfg) + 1`）与 `chains/evm/reader.go:270-272`（`confirmations = latest - blockNumber + 1`）
- **判定**：CONFIRMED（读码）
- **失效**：前向扫描的窗口上界现在恒在 `Confirmations` 深度之外，所以每个 sighting 交给
  `IngestDeposit` 时 `Confirmations` 已经达标，`advanceConfirmation` 一路推到 confirmed。
  即：watcher 路径上的 `pending` / `confirming` 状态与 `deposit_confirm_pending` 模板实际
  不再出现（webhook 路径不受影响）。这可能正是想要的，但依赖「先 pending 再 confirmed」
  做 UX 的消费方会看到行为变化，`TODO.md` 的破坏性清单里没有这条。

---

## 移交

- **`ledger-cli` 退出码 × DRIFT**（M-4 的下游）：`cmd/ledger-cli/main.go` 只对 TAMPERED /
  NOT_RUN `os.Exit(1)`，M-4 的窗口期正好落在 DRIFT 上。归 D-ops / RUNBOOK 口径。
- **`reservations` 的 `settled_amount` 与 `reservation_operation_receipts` 之间没有数据库层
  一致性约束**（C-1 修法 (b) 的前置）。归 D-lock / W1-gate 后续。
- **`web/` 侧**：`GET /balances/.../breakdown` 的 `available` 同样是 `hold` 减出来的
  （`postgres/ledger_store.go:1571`），C-1 的篡改会让前端显示的可用余额也变高。归 D-web 知情，
  无独立修法。

## 我没能验证的

1. **M-5 的实跑**：我没有真的以「非 superuser CREATEROLE runner」装一遍并在迁移中途从另一条
   同角色连接 `DROP TRIGGER`。结论基于 `pg_auth_members` 是 cluster 级共享目录 +
   `has_privs_of_role()` 不区分会话这两条机制事实，以及代码里的 GRANT 语句本身。
   若要定级为 Critical，需要这一跑。
2. **C-1 的 `settling` 分支是否还有更划算的变体**：我只试了
   `status='settling', settled_amount=reserved_amount`。`active -> released` 试过（会让预留
   不可结算，无收益）。没有穷举 `SettlePartial` / `FinalizeSettlement` 与被篡改 `settled_amount`
   组合后的账务后果（例如伪造的 `settled_amount` 是否会让后续真实结算腿的金额算错）——
   这可能比我报的 hold 归零更严重，建议 lead 在修 C-1 时一并核。
3. **I-51 rule 2/3 的多货币与同维双侧变体**：我论证了「平衡的伪冲销必然是真冲销」
   （每条腿都必须反转原 journal 某条腿，且 per-currency 必须平），并检查了 4 腿
   `checkout_settlement_net` 的按维度部分冲销——都没找到漏洞。但我没有对
   「原 journal 在同一 `(holder, currency, classification)` 上同时有 DR 与 CR 腿」这种形状
   做穷举，`originalByDim` 的 key 含 `entry_type`，理论上存在我没想到的组合。
4. **`ExecuteTemplateBatch` / `PostAuthorized` / `SettlePartial` 三条写路径**我只核了它们
   都汇入 `postJournalWithQueries`（因此吃到 I-51 的两个输入门），没有逐条走完它们各自的
   锁序与幂等分支。
5. **`unauthorized_journals` 的 M-7 我没实跑**（读码充分，但没有红/绿输出）。
6. **前端 / `web/`** 完全未看——不在本次攻击面内。
7. **`anchors/r2` 子模块**的 head 不可回退实现（I-56）未复审——D-tamper 的 `anchortest`
   一致性套件我只读了它存在，没有验证它对一个刻意回退的实现会红。

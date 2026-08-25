# 审计报告 — threat-model

## 摘要

既有威胁模型（「DB 写凭证已泄露，程序端可信」）的两层防线在**逻辑上**是自洽的，但**部署上是空的**：唯一的 composition root `cmd/ledgerd` 从不 wire `WithAttestor` / `SetAttestor`，HTTP 层没有任何入口能设 `RequireVerifiedBalance`，而 `docker-compose.yml` 与 Helm 默认都把 serving 进程指向 bootstrap 超级用户凭证 —— 也就是说 P1 role 分离（一切 DB 层强制的前提）、P5 签名、P6 attestation、Wave 2 提现闸在仓库自带的每一件交付物里都是关闭的，且**启动时没有任何告警**（`DevCreditEnabled` 有 `slog.Warn`，「账本在无签名状态运行」没有）。

模型**内部**也有洞。§14 声称权限从 trigger 派生因而 fail-closed，实际是对「意图」fail-open：只识别 `ledger_block_mutation()`，因此任何**只在注释里被声明为不可变**的表拿到了 `UPDATE`。逐表实测（一次性 postgres 容器 + 真 `001_baseline` + 真 `ledger_app` 连接）证实 `ledger_app` 可以改写 `deposit_addresses.account_holder`（把别人的充值改记到自己名下）、`entry_template_lines`（改写过账规则）、`account_policies`（解冻账户 / 开透支）、`account_policy_changes`（改写自己那次改动的审计留痕）、`bookings.journal_id`+`amount`、`events.amount`+`to_status`、`reconcile_scan_cursors`（让检测层报绿）。这里最重的一条是**签名机制的输入本身可被篡改**：`AuthorizeTemplate` 从这些可写表读出 holder 与模板，再对渲染结果签名 —— 攻击者不需要伪造签名，只需要让应用替他签一份他设计的 intent。`docs/INVARIANTS.md` I-25 的全称命题「每个参与余额计算的非-journal 表都有 DB 级 guard」因此是**假的**，而且没有任何门禁能发现它是假的（`mutation_guards_test.go` 只测已有 guard 的五张表；`grant_coverage_test.go` 用与 migration 相同的谓词重新派生 append-only 集合，结构上不可能发现「这张表本该 append-only」）。

另有一条**功能性 fail-closed 事故**：`webhook_nonces` 的清理是 schema 自称「唯一被允许的 DELETE」，但 §14 的 GRANT 循环从不授予 DELETE（§14 header 原文「Nobody gets DELETE, anywhere」）。实测 `ledger_app` 执行该语句直接 `permission denied`，而 `TryRecordNonce` 把它当致命错误上抛 —— 在按设计做了 role 分离的部署里，**每一个入站 webhook 回调都 500**。绕过它的最省事修法（不 wire nonce recorder，这是被静默允许的 nil 分支）恰好关掉重放保护。

## 发现

### [Critical] `classifications.balance_role` 的 `'' -> <role>` 合法跃迁可把用户侧「非可支配」余额一次性变成可支配可提现

- **位置**：guard `postgres/sql/migrations/001_baseline.up.sql:1136-1139`；ACL `:1494`；受害分类 `presets/templates.go:73`；金额来源模板 `presets/templates.go:197-206`；消费点 `postgres/ledger_store.go:1281-1285`、`postgres/reserver_store.go:256-258`；契约声明 `docs/INVARIANTS.md:1073-1077`
- **判定**：CONFIRMED（trigger 放行已实跑验证；余额语义由代码路径确认）
- **失效场景**：`fee_expense`（shipped withdrawal preset）是 **user-side、debit-normal、balance_role=''** 的分类。`withdraw_fee` 模板每次提现都 `DR fee_expense (user)`，所以每个 holder 的 `fee_expense` 余额 = 他历史累计支付的手续费，正数。`sumBalancesByRoleWithQueries` 只按 `balance_role` 分桶（`role == ''` 直接 `continue`），而 `Reserve` 的 `availableBase` 就是 `roleSums[available]`。攻击者用 `ledger_app` 执行一条 `UPDATE classifications SET balance_role='available' WHERE code='fee_expense'` —— guard 明确放行（`OLD.balance_role = ''`）—— 此后每个 holder 的历史手续费总额立即计入 available、可 Reserve、可走提现模板离开系统。举例：holder 累计付过 1,200 USDT 手续费，其 available 从 0 变成 1,200，提现 1,200 走的是完全正常、**带有效签名**的 `withdraw_confirm` journal。
  更一般地：该跃迁**没有时间边界、也不限定哪个分类**。任何 consumer 自定义的 user-side `balance_role=''` 分类（escrow / unvested / accrued 之类，classification-driven 是本库的卖点）都可以被一条 UPDATE 提升为 available，敞口 = 该分类的全网用户侧余额。
- **最小复现**：① 装 withdrawal preset，一个 holder 走若干次带 fee 的提现；② 读 `GET /balances/{holder}/{currency}/breakdown`，记下 available；③ 以 `ledger_app` 执行上面那条 UPDATE；④ 重读 breakdown → available 增加了该 holder 的 `fee_expense` 余额；⑤ `POST /reservations` 该增量 → 成功。
- **为什么现有机制没拦住**：设计档 A2 的修法把「role↔role 互换」关掉了，但把 `'' -> role` 当成**安装期一次性 expand 升级**放行（I-25 原文「used once when a deployment starts opting a classification into the breakdown」），guard 里没有任何东西把它限制在安装期或限制在无历史余额的分类上。`Reserve` 的 `RequireVerifiedBalance` 闸也拦不住（`postgres/reserver_store.go:174-193`）：它遍历**当前** role=available 的分类并校验其 entries 授权，而这些 entries 是真的、签名是真的 —— 被改的是分桶语义，不是账目。reconcile 的 `journal_dr_cr` / 全局会计等式与 balance_role 无关，也不会告警。

### [Critical] per-journal 授权签名的**输入**存放在 `ledger_app` 可 UPDATE 的表里 —— 攻击者不伪造签名，让应用替他签

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:804-815`（`deposit_addresses`，无 guard）、`:206-224`（`entry_templates` / `entry_template_lines`，无 guard）；ACL `:1494`；归属读取 `service/onchain.go:622`、`:638`；签名 `service/onchain.go:930-937` → `ledger.go:229-234` → `postgres/ledger_store.go:766-789`
- **判定**：CONFIRMED（两张表的 UPDATE 已实跑验证；签名链路已通读）
- **失效场景**：`IngestDeposit` 用 `Registry.GetByAddress(addr)` 反查 holder，然后把 `da.AccountHolder` 原样交给 `AuthorizeTemplate` 签名。攻击者以 `ledger_app` 执行 `UPDATE deposit_addresses SET account_holder = <自己的 holder> WHERE address = '<受害者地址>'`（实测通过）。受害者下一笔真实链上充值（例如 50,000 USDT）被记入攻击者账户，产生的 journal 是**经 Attestor 正常签名、auth_status='signed'**、per-journal 验签通过、attestation 链覆盖、`entry_attestations.auth_verdict='authorized'` 的一条完全合法记录。solvency 两边等量增加，托管资产对账也不失衡（钱确实进来了，只是记错了人）。
  第二条同类路径：`UPDATE entry_template_lines SET holder_role='system'` / `classification_id=...` / `entry_type=...`（实测通过）—— `EntryTemplate.Render` 按这些列决定钱进哪个账户、哪个方向（`core/template.go:135-160`），`renderTemplate` 每次都从 DB 现读模板，`Authorize` 对渲染**结果**签名。改模板 = 改所有未来 journal 的 posting intent，且每一条都带有效签名。
- **最小复现**：① 装 crypto-deposit add-on，给 holder A 注册充值地址；② `UPDATE deposit_addresses SET account_holder=B WHERE address='<A 的地址>'`；③ 向该地址推一笔 sighting（或让 watcher 扫到）；④ `GET /balances/B` 出现该金额，`GET /journals/{uid}` 的 `auth_status` = `signed`，`ledger-cli verify` 报 VERIFIED。
- **为什么现有机制没拦住**：设计档 §7 的边界声明只覆盖「拿到 DB 写权限的攻击者**无法签名**」（§7.4），从没检查「签名覆盖的字段是从哪里读来的」。canonical digest 在 uid-space 覆盖 holder / 分类 / 金额（§7.2），但 holder 与分类是从可写配置表解析出来的，签名因此**认证的是应用读到了什么，而不是业务上发生了什么**。I-25 只列了 5 张表，`deposit_addresses` 和 `entry_template_lines` 都不在其中；`mutation_guards_test.go` 只测那 5 张，`grant_coverage_test.go` 结构上无法发现缺失的 guard（见下一条）。

### [Critical] `ledger_app` 缺 `webhook_nonces` 的 DELETE 权限 —— 按设计做 role 分离的部署里每个入站 webhook 都 500

- **位置**：SQL `postgres/sql/queries/webhook_subscribers.sql:28`；调用 `postgres/webhook_subscriber_store.go:69-72`；handler `server/handler_webhooks.go:52-62`；schema 自述「唯一被允许的 DELETE」`postgres/sql/migrations/001_baseline.up.sql:752-753`；GRANT 循环不给 DELETE `:1491-1495`；§14 原文「Nobody gets DELETE, anywhere」`:1442`
- **判定**：CONFIRMED（实跑：一次性 postgres:17 容器 + 真 `001_baseline` + 真 `ledger_app` 连接 → `ERROR: permission denied for table webhook_nonces`；ACL 实测为 `INSERT,SELECT,UPDATE`）
- **失效场景**：`TryRecordNonce` 第一件事就是 `DeleteExpiredWebhookNonces`，失败直接 `return false, err`；`handleWebhookCallback` 把它 `httpx.Error(w, err)` → 19999 / 500。于是在 `ledgerd` 用 `ledger_app` 凭证跑的部署里，`POST /api/v1/webhooks/{channel}` **100% 失败**。对 crypto-deposit push 路径，这意味着所有推送式充值入账停摆（外部 scanner 见 500 会无限重试）。
  安全后果比可用性更糟：最省事的「修复」是不调 `SetWebhookNonceRecorder`（`server/handler_webhooks.go:22-27` 明确允许 nil = 关闭检查），而那正好把窗口内重放保护整个关掉 —— HMAC 时间窗只拒绝 stale 重放，窗口内（±5min，`channel/onchain/evm.go:21`）的原样重放签名有效。
- **最小复现**：跑 `001_baseline` → `ALTER ROLE ledger_app WITH PASSWORD ...` → 用 `ledger_app` 连接执行 `DELETE FROM webhook_nonces WHERE seen_at < now() - interval '15 minutes';` → `permission denied`。（已实跑。）
- **为什么现有机制没拦住**：全部 DB 测试用 `postgrestest.SetupDB` 的 migration-runner（超级用户）连接，`webhook_subscriber_store` 的测试同样如此 —— 没有任何测试从 `ledger_app` 连接调 `TryRecordNonce`。`postgres/roles_test.go` 确实开了 `ledger_app` 连接，但只测了 journal 相关操作（`:95-96` 只断言 `DELETE FROM journal_entries` **被拒**是好事），没有覆盖「哪些正常路径需要 DELETE」。`grant_coverage_test.go:96` 的 `assertGrants`（`roles_test.go:292-310`，`ElementsMatch` 精确集合）**把这个缺失固定成了期望值**。

### [Major] §14 的「权限从 trigger 派生」对**意图**是 fail-open，I-25 的全称命题为假且无门禁

- **位置**：派生逻辑 `postgres/sql/migrations/001_baseline.up.sql:1471-1496`（只匹配 `ledger_block_mutation()` 且 `tgtype & 16`）；自述 fail-closed `:1449-1462`；I-25 全称命题 `docs/INVARIANTS.md:1065-1070`；镜像同一谓词的 pin `postgres/grant_coverage_test.go:113-130`
- **判定**：CONFIRMED（下列每条 UPDATE 均以 `ledger_app` 实跑通过）
- **失效场景**：派生只把「挂了 `ledger_block_mutation()` 的 UPDATE trigger」读成 append-only；**没有 trigger 一律读成「可 UPDATE 是意图」**。于是所有只在散文里被声明为不可变的表拿到了 UPDATE。实测清单（`ledger_app` 连接，全部成功）：
  - `account_policies`（`:696`）—— 冻结/关闭/`min_balance` 是 `Reserve` 与 `PostJournal` 的唯一强制点（`postgres/reserver_store.go:234-245`、`postgres/account_policy_enforce.go:86-125`）。实测 `status: frozen → active` + `min_balance → -1000000` 成功。风控冻结、透支下限都可被一条 UPDATE 取消。
  - `account_policy_changes`（`:715`）—— 上一条的审计留痕。实测 `old_state`/`new_state` 可被改写。它的同类兄弟 `period_closes` / `checkpoint_rebuilds` 都拿到了 append-only trigger，它没有 —— 同一类表两种待遇。
  - `bookings`（`:536`）—— `journal_id` 在 schema §7 与 `CLAUDE.md` 里都写作 **set-once**，无 trigger。实测把 `journal_id` 指向另一条 journal + 改 `amount` 100 → 999999 + `status → confirmed` 成功。对照：`reservations.journal_id` 有 set-once guard（`:1181-1184`）。
  - `events`（`:572`）—— 「状态跃迁的原子记录」+ 对外投递队列。实测 `amount → 999999`、`to_status → rejected`、`journal_id` 改指、`delivery_status → delivered` 全部成功。下游消费方按 event payload 记账。
  - `reservation_settlement_legs`（`:501`）—— partial settle 的幂等台账。实测 `amount` / `idempotency_key` 可改写。
  - `currencies.exponent`（`:161`）—— 声明精度，`checkAmountPrecision` 的唯一依据。可改写。
  - `deposit_addresses` / `entry_template_lines` —— 见上一条 Critical。
- **为什么现有机制没拦住**：`grant_coverage_test.go:113-130` 用与 migration **完全相同**的 `information_schema.triggers` 谓词重新派生 append-only 集合，再断言 ACL 与之一致 —— 两边同源，它只能发现「新表连 GRANT 都没写」，结构上**不可能**发现「这张表本该有 guard 却没有」或「这张表需要 DELETE」。`postgres/mutation_guards_test.go` 只覆盖已有 guard 的 5 张表。所以 I-25 的全称命题（「Every table that participates in balance computation ... has a DB-level guard」）在文档里成立、在门禁里不存在。契约 §1.4 意义上的「缺测试」发现。

### [Major] `cmd/ledgerd` / compose / Helm 默认把整个 integrity-hardening wave 关着，且不告警

- **位置**：`cmd/ledgerd/main.go:125-127`（`ledger.New(pool, WithLogger, WithMetrics)` —— 无 `WithAttestor`）、`:148-172`（worker 只 `SetFullReconciler` / `SetEventDeliverer`，无 `SetAttestor`、无 `SetPartitionService`）；HTTP reserve 入口 `server/handler_reservations.go:14-20`、`:82-88`（`createReservationRequest` 无 `require_verified_balance` 字段，handler 从不设它）；`docker-compose.yml:30`；`deploy/helm/ledger/values.yaml:76-91`
- **判定**：CONFIRMED（grep + 通读；`RequireVerifiedBalance` 全仓非测试引用只有 `examples/tamper-evident`）
- **失效场景**：consumer 按仓库自带的交付物部署时：
  1. **P5 关闭** —— 没有 Attestor，每条 journal 落库 `auth_status='unsigned_no_attestor'`，`auth_digest`/`auth_signature` 为空。M5 的伪造入账（直接 SQL 插一对 balanced entry）在这种部署下**没有任何机制能拒绝**，而 P5 存在的全部理由就是它。
  2. **P6/P7 关闭** —— `SetAttestor` 从不调用，`ledger_attestations` / `entry_attestations` 永远为空，删行检测与外部锚不存在。
  3. **Wave 2 提现闸不可达** —— `RequireVerifiedBalance` 在 HTTP wire 上没有入口，service 模式的每一次 Reserve 都走信任 checkpoint 的路径（C2 的修法在 service 模式里等于没修）。
  4. **P1 role 分离默认关闭** —— `docker-compose.yml:30` 把 `ledgerd` 的 `DATABASE_URL` 指向 `POSTGRES_USER: ledger`（DB owner）；Helm 默认 `databaseUrlKey: ""`，注释明说 serving pods 与 migration Job 用**同一个**能 `CREATE ROLE` 的连接。§12 header 自己写着「these triggers without role separation are advisory」——在默认部署里它们就是 advisory：那个凭证可以 `DROP TRIGGER`，也可以 `CREATE OR REPLACE FUNCTION ledger_block_mutation() ... RETURN NEW`（§14 `:1520-1527` 亲自点名的最隐蔽路径）。
  5. **全程无告警** —— `DevCreditEnabled` 有 `slog.Warn`（`server/server.go:315-317`），「账本在无签名/无 attestation/无 role 分离状态运行」一句都没有。`working-agreements` §3：不可区分即静默失败。这里「防篡改整套没开」与「开了」在运行时输出上没有任何差别。
- **最小复现**：`docker compose up` → `POST /api/v1/journals` 一笔 → `SELECT auth_status, auth_digest FROM journals` → `unsigned_no_attestor`, `''`；`SELECT count(*) FROM ledger_attestations` → 0；日志里没有任何相关 warn。
- **为什么现有机制没拦住**：所有签名 / attestation 测试都自己 wire Attestor（`service/attestation_test.go`、`postgres/auth_pin_test.go`），`examples/tamper-evident/main.go` 也自己 wire —— 没有任何测试或门禁断言「shipped composition root 里这些是接上的」。设计档 §14 把这些归为「消费方必须自己提供」，这个划分本身没错；错的是**同一个仓库里的 `cmd/ledgerd` 就是一个消费方**，它没提供，也没说自己没提供。

### [Major] 分区维护路径要求应用持有 owner 权限，而 owner 的 `TRUNCATE` 绕开 append-only trigger

- **位置**：`ledger.go:730`（`NewPartitionStore(s.pool)` —— 就是 app pool）；`postgres/partition_store.go:138-198`（`DETACH PARTITION` + `TRUNCATE journal_entries_default` + `ATTACH`）；`docs/RUNBOOK.md:550-553`（「needs a `ledger_owner`-backed connection, not the app pool」）；`postgres/roles_test.go:193`（测试用 `ownerPool`）
- **判定**：CONFIRMED（实跑：`ledger_app` → `CREATE TABLE ... PARTITION OF` = `permission denied for schema public`、`DETACH` = `must be owner of table journal_entries`、`TRUNCATE journal_entries_default` = `permission denied`；`ledger_owner` → `DELETE FROM journal_entries WHERE created_at='2035-01-15'` 被 trigger 拒绝，紧接着 `TRUNCATE journal_entries_default` **成功删掉那 2 条真实 entry**，无任何 trigger 触发）
- **失效场景**：两条路只能选一条，两条都坏：
  - **(a) 按设计用 `ledger_app`**：分区 job 每次都失败（只在 12h 周期日志里留一行 error，`service/worker.go:271-277`），horizon 永不推进，所有新 entry 落进 `journal_entries_default`；等到需要 `RebalanceDefault` 修复时，那条路也是 owner-only，同样跑不了。
  - **(b) 让 app pool 拿 owner 凭证**（RUNBOOK 隐含要求、compose/Helm 默认已经如此）：app 凭证泄露即等于 owner 泄露，§12 的全部 trigger 与 §14 的 ownership 分离同时失效。而 owner 手上有一个**trigger 完全拦不到**的删除原语：schema 里没有任何 `BEFORE TRUNCATE` statement trigger，`TRUNCATE journal_entries_default` 直接抹掉 entry（已实测）。`journal_entries` 的 no-DELETE guard 是这个 schema 仅存的几条硬保证之一，`TRUNCATE` 从旁边走过去了。
- **最小复现**：见上「判定」里的实跑序列。补 (a)：以 `ledger_app` 起 `ledgerd` 并 `SetPartitionService`，等分区 job 跑 → `permission denied for schema public`。
- **为什么现有机制没拦住**：`postgres/roles_test.go:167-193` 自己开了一个 `ownerPool` 喂给 `NewPartitionStore`，并在注释里写「PartitionService (running as `ledger_owner`...)」—— 断言了一种**生产代码无法组装出来的部署形态**（`Worker()` 只从 `s.pool` 构造）。这与本轮契约 §1.4 点名的 `SetLocalPoller` 事故是同一形状：测试做了消费方做不到的接线，绿灯因此不说明任何事。

### [Major] 检测层自己的游标可被 `ledger_app` 改写，且「扫了 0 个」上报为 `Complete=true, Passed=true`

- **位置**：表 `postgres/sql/migrations/001_baseline.up.sql:967-973`（无 guard，ACL 给 UPDATE）；`service/reconcile.go:588`（读游标）、`:620-624`（`len(pairs)==0` → `break`）、`:669-672`（`partialReason==""`）、`:706-720`（走 else 分支，报 complete）、`:456`（metric = `Passed && Complete`）
- **判定**：CONFIRMED（`UPDATE reconcile_scan_cursors SET after_holder = 9223372036854775807, lap_dirty=false` 以 `ledger_app` 实跑成功；后续逻辑通读）
- **失效场景**：checkpoint 漂移检查（C2 的唯一检测手段）从持久化游标恢复。攻击者把 `after_holder` 设到 `INT64_MAX` 并把 `lap_dirty` 清成 false，则 `ListCheckpointAccountsPage` 返回 0 行 → `break` → `partialReason` 为空 → `result.Complete = true`、`Passed = true`，finding 文案写「checkpoint scan complete: 0 account/currency pairs verified this run」，`ReconcileCheckResult` metric 报绿。攻击者在每次调度前重置一次游标，就能在污染 checkpoint 的整个窗口里让检测层持续报绿。
  即使不考虑攻击者：**`scanned == 0` 与「全量扫完且干净」在机器可读结论上不可区分**（都是 `Passed && Complete`）—— P0 修的是「截断/跳过被折算成通过」，这条是同一形状的残留：零覆盖被折算成完整覆盖。
- **最小复现**：① 污染一个 checkpoint（`UPDATE balance_checkpoints SET balance = balance + 777`）；② `UPDATE reconcile_scan_cursors SET after_holder=9223372036854775807, after_currency=9223372036854775807, lap_dirty=false WHERE check_name='checkpoint_balance'`；③ `POST /reconcile/full` → check #2 返回 `passed=true, complete=true`，metric 绿。
- **为什么现有机制没拦住**：I-25 不含 `reconcile_scan_cursors`（它「不参与余额计算」，字面上对 —— 但它决定余额是否**被检查**）。P0 的 `Complete` 语义只在「被 limit 截断 / 超时 / check 未运行」三种情况置 false，没有「扫到 0 行但表里有行」这一档。

### [Major] `ledger_ro`（BI / Metabase 角色）能读出站 webhook HMAC 密钥 —— 只读凭证升级成对下游的完整性能力

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:1503`（`GRANT SELECT ON ALL TABLES ... TO ledger_ro`）、`:787`（`webhook_subscribers.secret`）；outbound 签名 `service/delivery/webhook.go`
- **判定**：CONFIRMED（实跑：`ledger_ro` 连接 `SELECT url, secret FROM webhook_subscribers` 返回明文 secret）
- **失效场景**：设计档 §3 把 `ledger_ro` 定位成「那次事故应该走的 role」，并把它唯一的已知缺陷记为「比理想状态宽 —— 能读全库明细」（保密性，non-goal 1）。但它还能读**每个 subscriber 的 outbound HMAC 签名密钥**。拿到它的攻击者可以向任意消费方伪造**签名有效**的事件投递（例如「充值已确认，金额 X」），把一个纯读凭证变成对下游系统的完整性攻击能力。这不在 non-goal 1 覆盖范围内 —— non-goal 1 说的是「数据被读走」，不是「读到密钥后可以伪造」。
- **最小复现**：以 `ledger_ro` 连接 → `SELECT url, secret FROM webhook_subscribers;` → 用该 secret 按 `delivery/webhook.go` 的方案给该 url 签一条伪造 event payload。
- **为什么现有机制没拦住**：`grant_coverage_test.go` 断言 `ledger_ro` 在**每张表**上恰好有 `SELECT` —— 也就是说这条被固定成了期望行为。RUNBOOK `:559-566` 的待办只提「scope down 到聚合视图」，没有把「密钥列必须永不进只读角色的可见面」单独列出来（`custody.md`：凭证不落可失陷面）。

### [Major] 一个 write-scope API key 就能铸造与真实充值不可区分的入账，签名机制不覆盖这一层

- **位置**：`server/routes.go:110-111`（`POST /journals`、`POST /journals/template` 在 `ScopeWrite` 组）；`server/handler_journals.go:180-213`（`handlePostTemplate` 对 `req.TemplateCode` **无 allowlist**）；对照 `server/handler_devcredit.go:14-27`（专用端点刻意收窄了 template code + admin scope + `ENV=dev`）
- **判定**：CONFIRMED（通读路由与 handler）
- **失效场景**：`ScopeWrite` key 可以 `POST /journals` 提交任意 `JournalInput`（任意 holder / 分类 / 金额），或 `POST /journals/template` 指定任意 template code。于是它可以铸造 `DR user.main_wallet / CR system.custodial` —— 与真实充值**逐字段相同**，且由 ledger 自己签名（`auth_status='signed'`）、attestation 覆盖、验签通过、solvency 两边等量增加因而不失衡。这正是设计档 M5 的场景，只是发起点从「一条 SQL」变成「一次 HTTP 请求」。API key 比 DB 凭证更容易泄露（在每个 consumer 的配置里、每次请求都上线）。
  附带一条 scope 不一致：dev-credit 专用端点要求 `ScopeAdmin` + `ENV=dev`，但 `dev_credit` bundle 一旦装上，同一个 template 通过 `POST /journals/template` 用 **write** scope 就能执行 —— 专用端点的三重门禁被通用端点绕过。`cmd/ledgerd` 只在 `DevCreditEnabled` 时装 bundle（`cmd/ledgerd/main.go:140-141`），所以生产路径上关得住；库模式 consumer 自行调 `InstallDevCreditPreset` 就不成立了。
- **最小复现**：用一个 `write` scope key `POST /api/v1/journals`，entries = `[{holder: 1, class: main_wallet, debit, 1000000}, {holder: -1, class: custodial, credit, 1000000}]` → 201，`auth_status=signed`，`GET /platform/solvency` 无异常。
- **为什么现有机制没拦住**：设计档 §1 的威胁模型只有「app DB 凭证」「DB owner」「备份/只读副本」「app 进程 + 签名能力」四行 —— **没有「app 的客户端凭证（API key）」这一行**。§7.4 声称签名把门槛「从一条 SQL 抬到拿下应用运行时」，实际门槛是「拿到一个 write API key」。I-26 只说「签名特性启用时每个 journal 携带有效签名」，而通过 API 铸造的伪造记录**确实**携带有效签名 —— invariant 成立，保护不成立。这是文档需要诚实写出的边界，不是代码缺陷本身。

### [Minor] `001_baseline` 复用同名既有 role 而不校正其属性，I-22 可在共享集群上静默为假

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:104-121`（三个 `IF NOT EXISTS ... CREATE ROLE`，无 `ALTER ROLE` 兜底）
- **判定**：CONFIRMED（实跑：`ALTER ROLE ledger_app SUPERUSER` → 在同集群新库跑一遍 `001_baseline` → 跑完 `rolsuper` 仍为 `t`）
- **失效场景**：role 是**集群级**对象。把 ledger 装到一个已经存在 `ledger_app` / `ledger_owner` / `ledger_ro` 的集群（前一次安装、另一个租户、运维手工建过）时，baseline 跳过创建并沿用既有属性 —— 包括 `SUPERUSER` / `CREATEROLE` / 已知密码。I-22（「`ledger_app` 无 DDL」）与 §12 的「dropping a trigger requires ownership, and the application does not have it」在这种安装上直接为假，而安装过程一句提示都没有。
- **最小复现**：见「判定」。
- **为什么现有机制没拦住**：`TestLedgerAppIsLeastPrivilege` 只在 testcontainers 的**全新**集群上跑，那里三个 role 必然由 baseline 亲自创建。「role 已存在」这条分支没有任何测试。

### [Minor] `ledger_owner` / `ledger_ro` 以 `LOGIN` 无密码创建

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:107`、`:115-118`；RUNBOOK 说明 `docs/RUNBOOK.md:545-548`
- **判定**：CONFIRMED（实跑确认 `rolcanlogin=t`、`rolpassword IS NULL`）
- **失效场景**：在 `trust` / `peer` 认证的 `pg_hba.conf` 下（`POSTGRES_HOST_AUTH_METHOD=trust` 在 dev/CI 极常见），任何能连到端口的对端可以直接以 `ledger_owner` 登录 —— 即拿到 DDL + 全部 trigger 管理权 + `CREATE OR REPLACE ledger_block_mutation()`。按 RUNBOOK 描述，`ledger_owner` 只在 migration 时用，而 migration 是由 **bootstrap** 凭证跑的（baseline 自己就是这么设计的），所以 `ledger_owner` 的 `LOGIN` 属性在既定工作流里**没有用途**，只是敞口。仓库自带的 `docker-compose.yml` 用密码认证，因此不直接命中；条件性风险，故定 Minor。
- **为什么现有机制没拦住**：没有测试或文档断言「三个 role 必须有密码或必须 NOLOGIN」。`docs/RUNBOOK.md:545-548` 只说「密码 out-of-band 设」，没说「不设就等于在 trust 环境下开放」。

### [Minor] `/metrics` 在业务端口上无鉴权、无限流，且无法关闭鉴权豁免

- **位置**：`server/server.go:393-398`（在 router 之前拦截，注释自述「completely bypassing auth and rate-limit middleware」）；`cmd/ledgerd/main.go:212`（`srv.SetMetricsHandler(promMetrics.Handler())`）；`deploy/helm/ledger/values.yaml:93-97`（`metrics.enabled: true, port: 8080, path: /metrics`）
- **判定**：CONFIRMED（通读）
- **失效场景**：任何网络可达的调用者无凭证即可拉取 `/metrics`。标签里没有 holder id（已核 `observability/prometheus.go`），所以不泄露个体数据，但泄露聚合业务量（按 journal type 的过账笔数、reconcile 各 check 的通过/失败、`sweep_unattributed` 金额、`deposit_review_required` 计数）。同时它绕过限流，是一个无鉴权的放大面。`SetMetricsHandler` 没有提供「要求鉴权」的选项，consumer 只能选「开」或「不开」。
- **为什么现有机制没拦住**：这是刻意设计（Prometheus 抓取不便持 API key），但 Helm 默认把它开在与业务 API **同一个** 8080 端口/Service 上，而不是单独的 metrics 端口 —— 默认值让「内网抓取」变成了「与公网 API 同面」。

### [Minor] holder 面把内部 journal type code 直接发给终端用户

- **位置**：`postgres/sql/queries/holder.sql:41`（`jt.code AS kind`）；`core/holder.go:29-31`；`server/handler_holder.go:133`、`:253`；对照 `presets/devcredit.go:47-53`（明确要求 holder 面「must not narrate how the balance was produced」）
- **判定**：CONFIRMED（通读）
- **失效场景**：holder-token 客户端（终端用户的浏览器）收到的每一行交易带 `kind` = 内部 journal type code：`dev_credit`、`deposit_confirm_pending`、`deposit_release_overage`、`withdraw_fee`。`dev_credit` 尤其命中 `user-facing-surfaces.md`：`KindLabel` 被专门设计成中性的 "Credit adjustment" 正是为了不告诉用户「这笔余额是凭空造的」，而紧挨着的 `kind` 把这件事说了出来。字段 doc 说 `Kind` 是「product-side i18n / label overrides 的 anchor」—— 这个用途成立，但它同时是 wire 上暴露给终端的实现命名。
- **为什么现有机制没拦住**：没有针对 holder 响应体的「内部标识词不得出现」断言（`user-facing-surfaces.md` 建议的那类护栏），而 `KindLabel` 的脱敏是靠人在 preset 里逐个填 `DisplayLabel` 完成的。

## 移交

- **[→ A 金融正确性]** `sumBalancesByRoleWithQueries`（`postgres/ledger_store.go:1281-1285`）跳过 `balance_role=''`，而 `BalanceBreakdown.Total = available + locked + pending`（`:1257`）—— role-less 的用户侧余额（`fee_expense`）既不在任何桶里也不在 Total 里。`GetBalance` 单维度读与 breakdown 的「总额」是两个不同的数，请核这是否是刻意的、以及 holder 面 Total 与账务真值的差额语义。
- **[→ A 金融正确性]** `currencies.exponent` 可 UPDATE（本报告 Major 一条）后，`checkAmountPrecision` 的判定会对历史与未来 journal 不一致 —— 声明精度与存储精度的关系归 A。
- **[→ B 并发]** `check_journal_currency_balance()`（`:1225-1250`）用 `pg_temp` 的 `ON COMMIT DELETE ROWS` 临时表在事务内去重。请核：连接池上「同一连接、上一个事务已建过该临时表」时 `CREATE TEMP TABLE IF NOT EXISTS` 的 NOTICE 与 `INSERT ... ON CONFLICT DO NOTHING` 的 `FOUND` 语义（我实跑时看到 `NOTICE: relation "ledger_balance_checked" already exists, skipping`），以及 `RETURN NULL` 在 constraint trigger 下的行为是否会漏掉某个 journal 的检查。
- **[→ B 并发]** `TryRecordNonce` 每次调用都先跑一条全表 `DELETE ... WHERE seen_at < ...`（`postgres/webhook_subscriber_store.go:70`），高频入站下是竞争与写放大来源。
- **[→ C 消费方表面]** `RequireVerifiedBalance` 在 `core.ReserveInput` 上存在，但 HTTP wire shape 没有它 —— 一个只读文档的 service-mode 消费方无法发现提现闸不可用。同类：`Worker()` 不接受 owner pool，`SetPartitionService` / `SetAttestor` 在 `ledgerd` 里没被调用而库文档把它们当默认能力描述。
- **[→ E 测试可信度]** 三个具体的「假绿」：`grant_coverage_test.go:113-130`（与被测 migration 同源派生谓词，结构上自证）；`roles_test.go:167-193`（用生产无法组装的 `ownerPool`）；`assertGrants` 把 `webhook_nonces` 缺 DELETE 固定成期望值。另请核 `mutation_guards_test.go` 是否只覆盖已有 guard 的 5 张表。
- **[→ F 结构]** `001_baseline.up.sql` 不能用裸 `psql` 跑通（末尾 `GRANT ... ON public.schema_migrations` 依赖 golang-migrate 先建表；`SET LOCAL` 依赖外层事务）。安装只有一条通路且没有文档化的手工 fallback。

## 我没能验证的

- **P5 签名在真实 `RunInTx` 路径下的端到端行为**：我通读了 `Authorize` → `PostAuthorized` 与 `auth_status` 三态，但没有实跑一次带 Attestor 的完整入账，因此「篡改 `deposit_addresses` 后产生的 journal 确实是 `auth_status='signed'` 且 `ledger-cli verify` 报 VERIFIED」这一步是**代码推理**而非实测。链路（`service/onchain.go:930-937` → `ledger.go:229-234` → `postgres/ledger_store.go:766-789`）本身已确认，结论对我不确定的只是最后一句 verify 输出。
- **`entry_attestations.auth_verdict = ''` 的 fallback 语义**：schema 注释（`:930-940`）说 caller 会退回 live check，我读了 `service/attest_verify.go` 的 verdict 枚举但没有走完提现闸消费该缓存的完整路径，因此没有验证「`''` 是否真的在每个消费点都退回 live check 而不是被当作 pass」。这一条如果有洞会是 Critical，建议 Team Lead 单独指派。
- **`hmac.Equal` 之前的 hex 大小写 / 长度差异**（`channel/onchain/evm.go:69-71`）：比较的是 hex 字符串而非解码后的字节，我判断不构成漏洞（不匹配即拒），但没有测非常规编码输入。
- **`isUnauthenticatedPath` 的路径前缀匹配与 chi 路由的交互**（`server/middleware_auth.go:158-168`）：我推理 `/api/v1/holder/../journals` 会在 chi 树匹配阶段 404（chi 默认不做 path cleaning，也不解析 `..`），因此不构成 auth bypass，但**没有实跑**这个请求。若要确证，一个 `httptest` 请求即可。
- **Merkle / RFC 6962 实现本身**：P7 的编码与包含证明正确性我完全没看（另有 golden vector 与 Lead 的第三实现对拍记录），只审了「`merkle_root` 是否进签名域」这一节的文档主张。
- **`ledger_owner` 无密码 + `trust` 认证的实际可利用性**：我确认了 `rolcanlogin=t` 且 `rolpassword IS NULL`，但没有起一个 `POSTGRES_HOST_AUTH_METHOD=trust` 的实例实证登录成功。Postgres 语义上确定，但标注为未实跑。
- **库模式 consumer 的真实 wiring**：本报告对「integrity wave 默认关闭」的判断基于 `cmd/ledgerd` + compose + Helm。`examples/tamper-evident` 确实把全套接上了，所以库模式 consumer**能**接对；我没有评估「一个只读 README 与 docs/frontend.md 的 consumer 有多大概率接对」——那属于 territory C。

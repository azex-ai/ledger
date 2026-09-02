# 审计报告 — threat-model

> 基线 commit `fd87cae`。方法与纪律见 `docs/plans/2026-09-02-deep-audit-contract.md`
> 与 `docs/plans/2026-08-25-financial-audit-contract.md` §1 / §3。
> 本报告里标 CONFIRMED 且写「实跑」的条目，都是在一个一次性 `postgres:17` 容器上
> 按顺序应用 `001`–`015` 全部 migration、以真实 `ledger_app` / `ledger_ro` / `ledger_owner`
> 连接执行攻击语句得到的结果（容器已销毁；仓库文件未做任何修改）。

## 摘要

上一轮的 DB 层缺口大部分被真的堵上了 —— `deposit_addresses`、`currencies.exponent`、
`bookings`/`events` 的 set-once、`webhook_nonces` 的 DELETE、`ledger_ro` 读 HMAC 密钥，
逐条实跑复验，修复都接在真实路径上而不是只在测试里。但这一轮暴露的是同一个方法论缺陷的第三次复发：
**「从目录派生」的纪律被写进了注释，实现却仍是手工名单**。审计触发器挂在手点的 4 张配置表上、
受保护模板是手点的 4 个 code、ownership 清扫是 001 里的一次性循环 —— 三处都留下了同形的洞。
最重的一个是 `dev_credit`：库里唯一一个凭空造可支配余额的模板，不在 `ProtectedTemplateCodes()`
里，于是一个 `write` scope key 在任何 ENV 下都能绕开 `/dev/credits` 的三重门禁去铸币。

DB 层还有两条结构性的：migration 007 的两个 `SECURITY DEFINER` 分区函数**并不属于 `ledger_owner`**
（I-35 白纸黑字宣称它们属于），而是属于跑 migration 的 bootstrap 凭证 —— 因为 001 的 ownership 清扫
是文件末尾的一次性循环，002–015 建的 4 张表 / 4 个序列 / 9 个函数全都没被扫到，且没有任何门禁看这件事。
连带发现：007 的第一条语句 `ALTER ROLE ... NOSUPERUSER` **只有 superuser 能执行**，
所以 RUNBOOK:648 认可的「CREATEROLE 非 superuser」bootstrap（托管 Postgres 的标准形态）
会卡死在 007，008–015 全部不生效。

「篡改之后能不能复盘」这个上一轮的共同盲区，答案是**一半**：证据现在被记录了，但
（a）`account_policies` —— 唯一的 DB 侧风控开关 —— 既没进审计触发器，其守卫的白名单又恰好放行了
`status` / `min_balance` / `enforce_min_balance` 这三个开关本身；
（b）两张新审计表 `ledger_app` 有 INSERT，可以伪造 `changed_by` 与 `changed_at`；
（c）全仓没有任何一个查询、store 方法、`ledger-cli` 子命令或 RUNBOOK 条目读这三张表，只有测试读。

---

## 发现

### [Critical] `dev_credit` 模板不在 `ProtectedTemplateCodes()` 里 —— 一个 write scope key 在任何 ENV 下都能铸造可支配、可提现的无支撑余额

- **位置**：`presets/protected_templates.go:37-44`（默认保护集，只有四个 deposit code）；
  `presets/devcredit.go:32`（`DevCreditTemplateCode = "dev_credit"`）、`:70-77`（模板腿：
  `DR main_wallet(user) / CR dev_credit(system)`）；`server/handler_journals.go:244`（唯一的拦截点，
  查的是 code 名单）；`server/routes.go:112`（`POST /journals/template` 在 `ScopeWrite` 组）；
  `ledger.go:659-664`（`InstallDevCreditPreset`，**无任何 ENV 门禁**）；
  对照 `server/handler_devcredit.go:41-44` + `server/server.go:222-227`（专用端点的三重门禁）
- **判定**：CONFIRMED（通读完整调用链：路由 → scope → protected-code 查表 → `ExecuteTemplate`；
  并 grep 确认 `IsSystem` 检查只存在于 `server/handler_journals.go:212-232`，
  `core/` `postgres/` `service/` 里没有第二处）
- **失效场景**：
  1. 消费方在某个阶段调用了 `svc.InstallDevCreditPreset(ctx)`（这是库文档指定的、唯一的启用方式，
     `ledger.go:651-653` 原文「a deployment gains the ability to mint balance out of nothing only by
     naming it here, explicitly」）。模板行从此常驻 DB，与 ENV 无关。
  2. 之后即使 `ENV=production`、`DEV_CREDIT_ENABLED` 未设、`/api/v1/dev/credits` 稳定返回
     `FeatureNotEnabled`，任何 **write** scope 的 API key 依然可以：
     ```
     POST /api/v1/journals/template
     {"template_code":"dev_credit","holder_id":42,"currency_uid":"...",
      "idempotency_key":"x1","amounts":{"amount":"1000000"}}
     ```
     → 201。落账 `DR main_wallet(42) / CR dev_credit(system)`。`main_wallet` 的 `balance_role`
     是 `available`（`presets/templates.go:69`），也就是 `Reserve` 唯一会花的桶 —— 这笔钱立刻可预留、
     可结算、可走提现流程。若消费方接了 Attestor，这笔 journal 还会带一个**有效签名**。
  3. 两个端点的防线形状不同，这是根因：`handlePostJournal` 用的是**结构性**判据
     （实时读 `classifications.is_system`，`server/handler_journals.go:212-232`），
     `handlePostTemplate` 用的是**手工 code 名单**。`dev_credit` 分类 `IsSystem: true`
     （`presets/devcredit.go:43`），所以手写路径拦得住它，模板路径拦不住。
- **最小复现**：见上第 2 步。无需 admin scope、无需 `ENV=dev`、无需 `DevCreditEnabled`。
- **为什么现有机制没拦住**：
  `server/handler_journals_template_test.go:108` 的 `TestPostTemplate_DefaultProtectsDepositCodes`
  遍历的是 `presets.ProtectedTemplateCodes()` 自己 —— 与被测实现同源，**结构上不可能**发现
  「某个本该被保护的模板不在名单里」。这正是上一轮在 `grant_coverage_test.go` 上修掉的那个形状
  （「用与 migration 相同的谓词重新派生」），在 HTTP 层原样复现了一次。
  没有任何测试遍历「已安装的模板 → 它的腿是否触及 `is_system` 分类 → 是否被保护或被显式豁免」。
  I-38 的措辞（`docs/INVARIANTS.md:2792-2794`）把保证限定在 "deposit-shaped accounting"，
  字面上没被违反 —— 但它自己的 M-2 修订注记（`:2763-2772`）给的理由
  （「库自己 ship 的、库自己知道危险的 code，不能等部署方记得去配」）逐字适用于 `dev_credit`，
  而这条恰恰是库里唯一一个**其 doc comment 明说就是凭空造钱**的模板。
  `README.md:886` 对外的措辞是 "a leaked `write`-scope key cannot mint deposit-shaped accounting
  through either endpoint" —— 一个只读 README 的消费方会得出比事实更强的结论。

---

### [Major] 007 的两个 `SECURITY DEFINER` 分区函数不属于 `ledger_owner`，而属于跑 migration 的 bootstrap 凭证；I-35 的断言为假且无门禁

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:1578-1582`（ownership 清扫循环，
  位于 001 文件末尾，**只在 001 里跑过一次**）；
  `postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:114`、`:179`
  （两个 `GRANT EXECUTE ... TO ledger_app`）；
  断言原文 `docs/INVARIANTS.md:2425-2427`「Both functions are owned by `ledger_owner` and run with
  its privileges regardless of the caller」；007 header 同样断言（`:80-83`）
- **判定**：CONFIRMED（实跑。以 bootstrap 超级用户跑完 001–015 后：
  `SELECT proname, pg_get_userbyid(proowner), prosecdef FROM pg_proc ...` 返回
  `ledger_create_monthly_partition | boot | t`、`ledger_rebalance_default_partition | boot | t`；
  另有 7 个 guard/audit 函数（`ledger_account_policies_guard` / `ledger_bookings_guard` /
  `ledger_events_guard` / `ledger_log_config_table_change` /
  `ledger_log_reconcile_scan_cursor_change` / `ledger_block_column_mutation` /
  `ledger_reject_unknown_normal_side` / `ledger_signed_amount` / `ledger_signed_delta`）
  以及 4 张表 + 4 个序列（`config_table_changes`、`reconcile_scan_cursor_changes`、
  `reservation_operation_receipts`、`booking_transition_receipts` 及其 `_id_seq`）同样 owner=bootstrap）
- **失效场景**：001 的 ownership 清扫是文件末尾的一次性 catalogue sweep，002 之后新建的对象全部没有被扫到。
  001 自己的注释（`:1536-1544`）逐字描述过这个失效模式：
  「a table created by a migration that ran AFTER the ownership sweep never got swept …
  Sweeping the catalogue instead of a list of names is what makes both classes impossible here」
  —— 它防住了 001 内部的那次，没防住 001 之后的每一次。后果分两层：
  - **SECURITY DEFINER 的提权面**：这两个函数以其 owner 的权限运行，`ledger_app`（威胁模型假定已泄露的那个凭证）
    持有 EXECUTE。若 bootstrap 是 superuser（见下一条：007 实际上**要求** superuser），
    那么泄露的 app 凭证手上就有两个 **superuser 权限**的入口，而不是设计以为的 `ledger_owner` 权限。
    007 header 用来论证「blast radius shrinks」的整段推理，前提不成立。
  - **guard 函数的 owner**：001 自己（`:1527-1535`）把函数 ownership 明确写成「part of the tamper-evidence
    story」，理由是 owner 可以 `CREATE OR REPLACE` 掉 `ledger_block_mutation` 的函数体让所有守卫静默失效。
    现在 006/010 建的 5 个 guard/audit 函数的 owner 是 bootstrap 凭证 —— 而 001（`:1430-1436`）自己
    承认 bootstrap 凭证保有对 `ledger_owner` 的永久 ADMIN OPTION、应当在安装后轮换或退役。
    对象归它所有，`DROP ROLE` 会直接失败，这条退役建议在当前 schema 下做不到。
- **最小复现**：全新库跑 `postgres.Migrate(url)` → `SELECT relname, pg_get_userbyid(relowner) FROM pg_class
  WHERE relnamespace='public'::regnamespace AND relkind IN ('r','S') AND pg_get_userbyid(relowner) <> 'ledger_owner';`
  返回 8 行；`pg_proc` 同理返回 9 行。
- **为什么现有机制没拦住**：全仓 grep `relowner|proowner|pg_get_userbyid|OWNER TO` —— 除了 001 的 up/down
  两个文件，**没有任何测试或 migration 提到 ownership**。`grant_coverage_test.go` 覆盖 ACL 与序列，
  不覆盖 ownership，也不覆盖函数的 EXECUTE ACL。I-35 的三条 pin
  （`TestLedgerAppInsertsIntoPartitionCreatedAfterGrant` /
  `TestPartitionMaintenanceRejectsUnshapedPartitionNames` /
  `TestPartitionFunctions_SearchPathIncludesPgTemp`）验的是「能不能调通」「名字校验」「search_path」，
  没有一条验「owner 是谁」—— 而在 testcontainers 里 runner 就是超级用户，所以就算加了断言也要小心：
  正确的断言是 `proowner = 'ledger_owner'::regrole`，那条现在会红。

---

### [Major] migration 007 只有 SUPERUSER 能执行；RUNBOOK / README 认可的 CREATEROLE bootstrap 会卡死在 007，008–015 全部不生效

- **位置**：`postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql:23-25`
  （三条 `ALTER ROLE ... NOSUPERUSER ...`）；`docs/RUNBOOK.md:647-651`
  （「The connection that runs `001_baseline` must be able to `CREATE ROLE` (superuser, or a role with the
  `CREATEROLE` attribute) … Every migration after `001` runs as `ledger_owner` and needs no elevated
  privilege beyond what it already holds」）；`postgres/migrate.go:47-76`（`Migrate` 全程只吃一个 URL，
  没有任何切换连接的机制）
- **判定**：CONFIRMED（实跑两次：
  ① 以 `ledger_owner` 执行 `ALTER ROLE ledger_app NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`
  → `ERROR: permission denied to alter role / DETAIL: Only roles with the SUPERUSER attribute may change
  the SUPERUSER attribute`；
  ② 新建一个 `CREATEROLE CREATEDB` 非 superuser 角色 `mgr` 并 `GRANT ledger_app/ledger_owner/ledger_ro TO mgr
  WITH ADMIN OPTION`，同一条语句 → **同样的错误**；而 `ALTER ROLE ledger_app NOCREATEDB` 单独执行成功
  —— 失败点确实是 `NOSUPERUSER` 这个属性本身，即使它是个 no-op）
- **失效场景**：RUNBOOK 明说 bootstrap 可以是「CREATEROLE 非 superuser」，这正是托管 Postgres 的标准形态
  （RDS 的 master、Cloud SQL、Neon、Supabase 都不给 superuser）。这类部署上 `Migrate()` 会在 007 报
  `SQLSTATE 42501`，golang-migrate 把库标 dirty，**007 到 015 一条都不生效**。留下的是 pre-007 姿态：
  - `ledger_ro` 仍能 `SELECT secret FROM webhook_subscribers`（007 第 2 节没跑）
  - 两个分区 `SECURITY DEFINER` 函数不存在 → worker 的分区 job 每 12h 失败一次，
    horizon 不前进，新 entry 全落 `journal_entries_default`
  - 008 的 `journal_entries.id` 列级 INSERT 收窄没跑 → 伪造 id 的跨分区攻击仍然开着
  - 014 的 `webhook_subscribers` 写面收窄没跑 → `ledger_app` 仍可 INSERT 攻击者的 subscriber URL
  而 007 header 的三段解释都不提这条前置条件，运维看到「permission denied to alter role」最省事的处置
  就是给 bootstrap 加 superuser —— 一个比原问题更糟的结果。
  RUNBOOK 那句「Every migration after `001` runs as `ledger_owner`」在代码里也没有对应机制：
  `Migrate(databaseURL string)` 只有一个 URL。
- **最小复现**：见「判定」②。或：以 CREATEROLE-only 角色对空库跑 `postgres.Migrate`。
- **为什么现有机制没拦住**：`postgres/roles_test.go:801` 的
  `TestRoleAttributeHardeningResetsPreExistingPrivileges` 用 testcontainers 的**超级用户** pool 直接
  `pool.Exec(ctx, "ALTER ROLE ledger_app NOSUPERUSER ...")`，注释写着「This is migration 007's own
  statement, re-applied -- what a fresh install onto this already-populated cluster would run」。
  它证明的是「这条语句由 superuser 发出时有效」，不是「消费方按 RUNBOOK 用 CREATEROLE 凭证能装上」。
  契约 §1.4 的「测试做了消费方做不到的接线」在这里精确成立。

---

### [Major] `account_policies` 的守卫白名单恰好放行了它唯一要保护的三个风控开关，且这张表没有审计触发器

- **位置**：`postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:42`
  （`mutable := ARRAY['status','min_balance','enforce_min_balance','note','updated_at']`）；
  006 header `:8`（自述这张表是 "the only DB-enforced freeze/overdraft floor"）；
  强制点 `postgres/account_policy_enforce.go:85-92`（closed）、`:93-96` + `:99-107`（frozen）、
  `:110-126`（min_balance）；审计触发器名单 `006:287-307`（只有 currencies / classifications /
  journal_types / entry_templates 四张）
- **判定**：CONFIRMED（实跑。种一行 `status='frozen', min_balance=0, enforce_min_balance=true` 的策略，
  以 `ledger_app` 执行
  `UPDATE account_policies SET status='active', min_balance=-1000000, enforce_min_balance=false WHERE account_holder=42;`
  → 成功；回读确认 `active / -1000000.000000000000000000 / f`；
  `SELECT count(*) FROM account_policy_changes` = 0、`SELECT count(*) FROM config_table_changes` = 0）
- **失效场景**：上一轮 threat-model 报告点名的攻击原文是「`status: frozen → active` + `min_balance → -1000000`
  实测成功。风控冻结、透支下限都可被一条 UPDATE 取消」。006 给这张表装了守卫，
  但守卫只锁 identity 列（`account_holder` / `currency_id` / `classification_id`），
  **那条攻击语句逐字仍然成功**。`enforceAccountPolicies` 是 `PostJournal` 路径上唯一的 DB 侧风控点，
  被解除后：冻结账户可以继续出金；`min_balance` 变成 `-1000000` 后
  `after.LessThan(minBalance)` 永不成立，账户可透支到一百万。
  这是一个真实的权衡（`UpsertAccountPolicy` 确实要写这些列），不是实现失误 —— 失误在**第二层**：
  006 把 `account_policies` 排除在 `config_table_changes` 审计触发器之外，理由写在 `006:250-256`：
  「account_policies is the only table in this family with an application-level audit trail
  (account_policy_changes)」。而那张审计表是**由应用在同一事务里写的**，走裸 SQL 的攻击者根本不写它。
  于是这张表成了唯一一张「守卫放行 + 无审计」的风控表：既拦不住，也看不见。
- **最小复现**：见「判定」。
- **为什么现有机制没拦住**：`postgres/roles_test.go:474-498` 的 `TestAccountPoliciesGuard` 有两个子测试，
  第二个 `"legitimate SetPolicy-shaped update still works"`（`:491-497`）**断言这条攻击必须成功**
  （`status='active', min_balance=5, enforce_min_balance=false` → `require.NoError`）。
  测试是对的（合法路径确实要能写），但它同时把「攻击者也能写」固定成了期望行为，
  而没有任何一条断言要求这次写留下痕迹。审计覆盖是 006 手点的 4 张表，不是从
  「哪些表有部分（白名单）守卫」派生出来的 —— 派生的话会得到 9 张
  （`classifications` / `reservations` / `journals` / `account_policies` / `bookings` / `events` /
  `currencies` / `journal_types` / `entry_templates`）。

---

### [Major] 006 新建的两张审计表对 `ledger_app` 开放 INSERT，`changed_by` / `changed_at` 可任意伪造

- **位置**：`postgres/sql/migrations/006_threat_model_guard_coverage.up.sql:229`
  （`GRANT SELECT, INSERT ON public.reconcile_scan_cursor_changes TO ledger_app`）、
  `:273`（`config_table_changes` 同样）；触发器函数 `ledger_log_config_table_change` /
  `ledger_log_reconcile_scan_cursor_change` 均为 **invoker rights**（非 `SECURITY DEFINER`，
  实测 `prosecdef = f`），所以这个 INSERT 授权是它们能工作的前提
- **判定**：CONFIRMED（实跑，以 `ledger_app`：
  `INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by, changed_at)
   VALUES ('currencies','{}','{}','ledger_owner', now() - interval '30 days');` → 成功；
  回读该行 `changed_by = ledger_owner`、`changed_at = 2026-08-03`；
  `reconcile_scan_cursor_changes` 同样成功。
  两张表的 UPDATE / DELETE 确实被 `ledger_block_mutation()` + REVOKE 挡住，已实测拒绝）
- **失效场景**：006 引入这两张表的目的是回答上一轮 §9 的「谁、何时、改了什么」。
  真实记录删不掉、改不了 —— 这部分成立。但攻击者可以**追加**：
  - 把自己的改动伪装成 `ledger_owner`（或任何字符串）在 30 天前做的；
  - 用噪声淹没审计表（`ledger_app` 也持有 `_id_seq` 的 USAGE），让事后按时间/表名筛查失去意义。
  取证的价值在于「记录可信」，一个攻击者可写的审计表把「谁」这一列变成了不可采信的字段。
  006 header `:262-266` 自己承认 `current_user` 在每个部署里都是 `ledger_app`、因而不识别业务 actor
  —— 但没有意识到攻击者可以直接绕过 `DEFAULT current_user` 显式赋值。
- **最小复现**：见「判定」。
- **修复方向**（一句话，因为它便宜）：把两个 trigger 函数改成 `SECURITY DEFINER`（owner 必须是
  `ledger_owner` —— 见上面的 ownership 那条），然后 `REVOKE INSERT ... FROM ledger_app`；
  `grant_coverage_test.go` 里这两张表的期望值从 `{SELECT, INSERT}` 变成 `{SELECT}`。
- **为什么现有机制没拦住**：`postgres/roles_test.go:672-760` 的两个审计测试断言的是
  「合法 UPDATE 会产生审计行」+「审计行不可 UPDATE/DELETE」，没有一条断言
  「`ledger_app` 不能凭空插一行」。

---

### [Major] §9 的取证层没有任何读者：无查询、无 store 方法、无 CLI 子命令、无 RUNBOOK 条目

- **位置**：`config_table_changes` / `reconcile_scan_cursor_changes` / `account_policy_changes`
  三张表；全仓引用见下；`cmd/ledger-cli/main.go:119-141`（命令表：`balance` / `balances` /
  `journals` / `journal` / `trace` / `reconcile` / `solvency` / `trial-balance` / `health` / `verify`）
- **判定**：CONFIRMED（grep 全仓，排除 `web/` 与 `migrations/`：
  `config_table_changes` 与 `reconcile_scan_cursor_changes` 的非-migration 引用只出现在
  `docs/INVARIANTS.md` 与 `postgres/grant_coverage_test.go` / `postgres/roles_test.go`；
  `postgres/sql/queries/` 下 0 命中；`docs/RUNBOOK.md` 下 0 命中；`cmd/ledger-cli/` 下 0 命中）
- **失效场景**：`cmd/ledger-cli` 的定位是「read-only investigation CLI」（`CLAUDE.md` 文件表原文），
  RUNBOOK 是给 on-call 的操作手册。发生事故后要回答「谁改了决定钱去哪的规则」，
  运维手上没有任何工具、也没有任何文档告诉他这三张表存在 —— 只能自己去翻 schema。
  上一轮 README §3 把这条列为「共同盲区」并建议补审计表；006 补了写入侧，读取侧一行都没有。
  按 `working-agreements.md` §3 的判据：证据被记录了，但「有人篡改过配置」与「从未发生」
  在**任何被暴露的界面上**仍然不可区分。
- **最小复现**：`ledger-cli --help` → 没有任何 audit / config-history 命令；
  `grep -rn "config_table_changes" docs/RUNBOOK.md` → 无输出。
- **为什么现有机制没拦住**：没有门禁要求「新增的审计表必须有一个读取面」。
  I-45（`docs/INVARIANTS.md:2494-2540`）描述了写入侧的机制，它的 pin 也只验写入侧。

---

### [Major] `ledger_app` 保有 `webhook_subscribers.secret` 的 SELECT —— 泄露的 DB 凭证可以向每个下游订阅方伪造**签名有效**的事件投递

- **位置**：`postgres/sql/migrations/014_webhook_subscriber_app_grant.up.sql:24-28`
  （显式声明 attack #2 不在本次修复范围）；外发签名 `service/delivery/webhook.go`；
  威胁模型表 `docs/plans/2026-08-21-tamper-evident-ledger-design.md:63-70`（四行攻击者能力，
  没有「下游系统完整性」这一列）
- **判定**：CONFIRMED（实跑：以 `ledger_app` `SELECT name, secret FROM webhook_subscribers;`
  → 返回明文 `s3cr3t-hmac-key`。同一语句以 `ledger_ro` 执行 → `permission denied for table
  webhook_subscribers`，007 的列级授权确实生效；`ledger_ro` 只读非密列成功）
- **失效场景**：007 已经把这条从 `ledger_ro` 上摘掉了，理由写得很清楚：
  「Reading it does not just disclose data, it hands a read-only credential the ability to forge
  signed event deliveries to any subscriber」。**同一句话对 `ledger_app` 一字不改地成立**，
  而 `ledger_app` 正是整个威胁模型假定已泄露的那个凭证。
  后果是这个 DB 凭证的爆炸半径越出了本账本：攻击者可以给任意下游发一条 HMAC 校验通过的
  「充值已确认，金额 X」，下游按事件记账。设计档 §1 的 non-goal 1（保密性）覆盖不到这里 ——
  non-goal 说的是「数据被读走」，不是「读到密钥后可以伪造」；007 自己就是这么区分的。
  014 把它列为 "NOT closed here"，修法（把 webhook 密钥移出 DB，与 `core.Attestor` 同一原则）
  也写下来了，但威胁模型表里没有这一行，`docs/INVARIANTS.md` 里也没有对应的诚实声明。
- **最小复现**：以 `ledger_app` 读 secret → 按 `service/delivery/webhook.go` 的方案对任意 payload 签名 →
  POST 到该 subscriber 的 url。
- **为什么现有机制没拦住**：`grant_coverage_test.go:239-243` 把 `webhook_subscribers` 对
  `ledger_app` 的 `SELECT`（表级、含 secret 列）固定成期望值，注释原文
  「SELECT stays table-level (outbound delivery must read url + secret to sign)」——
  正确的现状描述，但也意味着任何收窄它的尝试都会先让这条 pin 变红。

---

### [Major] RUNBOOK 的两条角色指引与 007 之后的代码相反；照做会重新装回 007 刚拆掉的 TRUNCATE 旁路

- **位置**：`docs/RUNBOOK.md:669-672`（「**Partition creation runs as `ledger_owner`.**
  `CREATE TABLE ... PARTITION OF` is DDL; whatever process runs `PartitionService.EnsureUpcoming`
  … needs a `ledger_owner`-backed connection, not the app pool」）；
  `docs/RUNBOOK.md:650`（「Every migration after `001` runs as `ledger_owner`」）；
  实际代码 `ledger.go:847`（`NewPartitionStore(s.pool)` —— 就是 app pool）+
  `postgres/partition_store.go:92,150`（只调两个 `SECURITY DEFINER` 函数，不发 DDL）；
  I-35 `docs/INVARIANTS.md:2419-2452`
- **判定**：CONFIRMED（通读三处；`grep -n "CREATE TABLE\|DETACH\|TRUNCATE\|ATTACH" postgres/partition_store.go`
  只命中注释，没有一条真实 DDL）
- **失效场景**：007 存在的全部理由就是让服务侧进程**不再需要** owner 凭证 ——
  I-35 的「Why」段落用实跑证据说明：owner 池的 `TRUNCATE journal_entries_default`
  会绕过 `journal_entries` 的 no-DELETE trigger（TRUNCATE 不触发行级触发器）。
  RUNBOOK 仍然在指挥运维给 worker 一个 `ledger_owner` 连接。照做的部署，
  `journal_entries` 的 append-only（I-2，这个 schema 仅存的几条硬保证之一）就重新变成 advisory，
  而且这次是**按文档做的**。
  另一条（`:650`）在上面已单列 —— 007 根本无法以 `ledger_owner` 运行。
- **最小复现**：读 `docs/RUNBOOK.md:669-672` 与 `ledger.go:847`。
- **为什么现有机制没拦住**：RUNBOOK 与代码之间没有任何机器校验。仓库里有 openapi ↔ Go 的双向门禁、
  README API surface 门禁、INVARIANTS pin 存在性门禁 —— 唯独 RUNBOOK 的操作指引没有。

---

### [Major] 入站 webhook 重放缓存全仓无人接线、无文档、nil 时静默；README 把 ±5min 时间窗说成全部的重放防护

- **位置**：`server/handler_webhooks.go:20-28`（`WebhookNonceRecorder` + `SetWebhookNonceRecorder`，
  doc 原文 "Nil disables the check"）、`:53`（`if s.webhookNonces != nil`）；
  `README.md:884`（"**Webhook replay**: HMAC payload is `<timestamp>.<body>`; timestamps outside
  ±5 minutes are rejected." —— 全文对 nonce 缓存只字不提）；
  签名窗口 `channel/onchain/evm.go:21`（`signatureFreshness = 5 * time.Minute`）
- **判定**：CONFIRMED（grep 全仓，排除 `web/`：`SetWebhookNonceRecorder` 的引用只有
  它自己的定义（`server/handler_webhooks.go:28`）、`server/server.go:62` 的字段注释、
  和 `CHANGELOG.md:517`。**`ledger.go`、`examples/`、`docs/`、`README.md` 里 0 命中**）
- **失效场景**：按仓库自带的任何一个 example 或 README 装配的消费方，`s.webhookNonces` 恒为 nil，
  于是 ±5 分钟窗口内的原样重放（同一 body、同一 timestamp、同一签名）**每一次都通过签名校验**，
  直接进入 `IngestDeposit` / `Transition`。是否造成重复入账取决于下游幂等键
  （`deposit-{chain_id}-{tx_hash}-{txlog_seq}`，migration 002 header 的说法），
  也就是说这层防御在设计上被判定为「必要的第二层」，实际交付里从来没被启用过。
  没有任何 `slog.Warn` —— 而同一个文件所在的 `newServer`（`server/server.go:492-503`）
  为 `DevCreditEnabled`、`AllowSystemClassificationPost`、auth 关闭、CORS 全开
  各写了一条启动 warn。这里「防护开着」与「防护关着」在运行时输出上没有任何差别。
- **最小复现**：按 `examples/` 装配 → 抓一次合法回调 → 5 分钟内原样重发 → 200（而非 409 replayed）。
- **为什么现有机制没拦住**：上一轮把这个 nil 分支写进了报告（`threat-model.md:36`），
  Wave 2 的处置是给 `webhook_nonces` 补 DELETE 授权（migration 002）——
  修好了「用得起来」，没有修「有人把它接上」。这与 C1 / `Worker.Subscribe` 是同一形态：
  **能力存在于库里，缺席于消费方真正会走的那条路径。**

---

### [Minor] `ledger_rebalance_default_partition` 的日期区间不受任何约束，`ledger_app` 可一次性制造永久性分区膨胀

- **位置**：`postgres/sql/migrations/007_...up.sql:125-177`（函数体，`p_first` / `p_last` 无校验）、
  `:179`（`GRANT EXECUTE ... TO ledger_app`）；对照 `013_partition_function_hardening.up.sql:60-100`
  （对**兄弟函数** `ledger_create_monthly_partition` 补了 name↔range 一致性校验，
  理由是「EXECUTE on the function is itself a ledger_app-reachable capability」）
- **判定**：CONFIRMED（实跑：以 `ledger_app`
  `SELECT array_length(ledger_rebalance_default_partition('2020-01-01','2021-12-01'),1)` → `24`。
  随后 `pg_class` 中 `journal_entries_y2020%` 有 60 个关系 = 12 张分区表 × 5（表 + pkey + 3 索引））
- **失效场景**：区间每多一个月就多一张分区表（5 个关系）。`ledger_app` 无法 `DROP` 它们（owner-gated），
  所以这是一个单向的、需要 DBA 介入清理的可用性缺陷 —— 013 给兄弟函数写的正是这句话
  （"a persistent availability defect requiring manual DBA intervention"）。
  同时函数体的 `DETACH` / `ATTACH` 取 `ACCESS EXCLUSIVE` 锁，循环调用即写面全停。
  合法调用方（`postgres/partition_store.go:145-152`）传的区间总是有界的 —— 这正是 013 用来论证
  「仍然要补校验」的同一个前提。
- **为什么现有机制没拦住**：013 只审了 name 参数那一个函数，没有把同一条推理推广到唯一一个
  接受自由日期区间的函数。

### [Minor] I-22 的「`ledger_app` 不能创建任何对象 / 不能 TRUNCATE」在 007 之后为假，pin 只试直接 DDL

- **位置**：`docs/INVARIANTS.md:1030-1038`（"it cannot `DROP`, `TRUNCATE`, `ALTER`, manage triggers,
  or create any object, anywhere in the schema"）；反例 `007:114,179` 的两个 EXECUTE 授权
- **判定**：CONFIRMED（实跑：`ledger_app` 执行
  `SELECT ledger_create_monthly_partition('journal_entries_y2031m01','2031-01-01','2031-02-01')` → `t`，
  即 `CREATE TABLE` 成功；`ledger_rebalance_default_partition` 内部的
  `TRUNCATE journal_entries_default` 同样由 `ledger_app` 触发）
- **失效场景**：I-22 是「机器可检验的契约」这一纪律的载体，它现在陈述了一个已被后续 migration 推翻的全称命题。
  实质风险由上面的 ownership 与区间两条承担；这一条的危害是**误导下一个读者**：
  一个基于 I-22 做威胁建模的人会得出「app 凭证泄露不产生 DDL 面」的错误结论。
  I-35 补充了受控入口的存在，但 I-22 的措辞没有相应收敛，两条 invariant 互相矛盾。
- **为什么现有机制没拦住**：`postgres.TestLedgerAppIsLeastPrivilege`（`postgres/roles_test.go:59`）
  逐条尝试 `TRUNCATE` / `DROP TRIGGER` / `ALTER TABLE` / `CREATE TABLE` 的**裸语句**，
  不尝试 `ledger_app` 持有 EXECUTE 的那两个函数。断言与命题之间差了一个「经由函数」的维度。

### [Minor] `grant_coverage_test.go` 结构上要求每张新表把全表 SELECT 给 `ledger_ro` —— 复制出 007 刚修掉的那个暴露

- **位置**：`postgres/grant_coverage_test.go:266`（`assertGrants(t, pool, "ledger_ro", table, []string{"SELECT"})`，
  唯一的逃逸口是把表名加进 `:135` 的 `roColumnScoped`）；被修掉的先例
  `007:47-56`（`REVOKE SELECT ON webhook_subscribers FROM ledger_ro` + 列级 GRANT）
- **判定**：CONFIRMED（读代码；实测确认 `ledger_ro` 对当前 34 张表全部持表级 SELECT，
  仅 `webhook_subscribers` 例外）
- **失效场景**：下一张携带密钥类列的表（一个 signer 配置、一个 channel 凭证）落地时，
  这条 pin 会**要求**作者给 `ledger_ro` 全表 SELECT，否则测试红 —— 除非作者恰好想到去
  `roColumnScoped` 加一行。默认方向是敞开，而不是收紧。
  上一轮的教训（BI 只读角色不该看见任何密钥列）被写进了 007 的一次性修复，没有被写进门禁的默认。
- **为什么现有机制没拦住**：门禁的期望值是硬编码的 `{"SELECT"}`，不是从「这张表有没有敏感列」派生的。

### [Minor] `TryRecordNonce` 对 42501 的容错降级完全无痕

- **位置**：`postgres/webhook_subscriber_store.go:85`
  （`if err := s.q.DeleteExpiredWebhookNonces(ctx); err != nil && !isInsufficientPrivilege(err) { ... }`
  —— 被容忍的那条分支里没有任何 `slog` / metric）
- **判定**：CONFIRMED（通读；函数体内无日志调用）
- **失效场景**：一个缺 migration 002 授权的库（见上面 CREATEROLE 那条：007 卡住不会影响 002，
  但一个只跑到 001 的库会）会永久不清理 nonce 表，表无界增长，而运行时输出与正常部署一模一样。
  doc comment 承认了这个代价（"at the cost of a cache that stops shrinking"），
  但 `working-agreements.md` §3 的第二条要求是「降级必须落痕」。这里没有。
- **修复方向**：容忍分支里打一次 `slog.Warn`（或用一个 `sync.Once` 避免每请求刷屏）。

### [Minor] `Config.AllowSystemClassificationPost` 的 doc 声明了一个 `LoadConfig` 从不读的环境变量

- **位置**：`server/server.go:185`（doc 写 `AllowSystemClassificationPost (ALLOW_SYSTEM_CLASSIFICATION_POST)`）；
  `server/server.go:293-304`（`LoadConfig` 组装 `Config` 字面量，没有这个字段，也没有
  `os.Getenv("ALLOW_SYSTEM_CLASSIFICATION_POST")`）
- **判定**：CONFIRMED（grep 全仓 `ALLOW_SYSTEM_CLASSIFICATION_POST` —— 只在那一行 doc 注释里出现）
- **失效场景**：方向是 fail-closed（设了也不生效，守卫保持开启），所以不是安全洞。
  但一个用 `server.New()`（走 `LoadConfig`）的部署，若确有理由手写 system-side journal，
  会设这个环境变量、看不到启动 warn、然后收到 403 而不知道为什么 —— 而排查时 doc 说环境变量存在。

### [Minor] 没有任何门禁断言「每条 chi 路由都带鉴权/授权中间件」；`/api/v1/holder/` 的豁免是一次字符串前缀匹配

- **位置**：`server/middleware_auth.go:158-168`（`isUnauthenticatedPath`，
  `strings.HasPrefix(path, webhookPathPrefix) || strings.HasPrefix(path, holderPathPrefix)`）；
  已有的路由遍历基础设施 `server/openapi_contract_test.go:754-776`（`chi.Walk`，
  回调已经拿得到中间件链但只用了 method+route）
- **判定**：CONFIRMED（当前路由表实际是干净的 —— `routes.go` 里 `/holder/*` 的六条全在
  `s.holderTokenAuth` 组内，`/holders/{holder}/...`（复数）与 `/holder-tokens` 都不匹配
  `"/api/v1/holder/"` 这个带尾斜杠的前缀。缺的是门禁而非当前缺陷）
- **附带验证（回答上一轮「我没能验证的」）**：路径穿越**不构成鉴权绕过**。
  用一个隔离的 chi 程序复现了本仓的中间件形状，逐一发送
  `/api/v1/holder/../journals`、`/api/v1/holder/%2e%2e/journals`、`/api/v1/holder/%2E%2E/journals`、
  `/api/v1/holder/..%2Fjournals`、`/api/v1/holder/./../journals`：
  **鉴权中间件确实被跳过（`X-Auth: SKIPPED`），但 chi 全部返回 404**，没有一个能落到
  `/api/v1/journals` 的 handler 上。所以今天是安全的 —— 安全**只因为**没有一条路由能被这样命中，
  而不是因为有机制保证。
- **失效场景**：未来任何一条注册在 `/api/v1/holder/` 之下、但没被放进 holder-auth 组的路由，
  会是完全无鉴权的；任何一条注册在三个 scope 组之外的路由，会是「有 key 即可，无 scope 检查」的。
  `chi.Walk` 已经在仓库里跑着，把中间件链一起断言的成本接近于零。

### [Minor] `deposits` / `withdrawals` 两张死表保留着 `ledger_app` 的 SELECT/INSERT/UPDATE 且无任何守卫

- **位置**：ACL 见附录矩阵；`postgres/grant_coverage_test.go:207,220`（列在 `reviewed` 里，
  注释原文 "deposits/withdrawals (history, nothing reads or writes them)"）
- **判定**：CONFIRMED（`grep -rln "CreateWithdrawal\|GetWithdrawal\|UpdateWithdrawal"` 排除
  `postgres/sqlcgen/` 后 **0 个调用方**；`deposits` 同理。sqlc 生成的 10 个 `*Withdrawal*` 方法全是死代码）
- **失效场景**：不构成当前攻击面（无人读 → 篡改无效果），但它们是两张名字最像「钱」的表，
  在事故取证时会误导调查者；且一旦将来有人「顺手」把它们接回读路径，就直接继承了一张无守卫、
  无审计、全可写的表。属于 → H structure 的清理项，这里只记录 ACL 事实。

---

## 上轮修复复核

按契约要求，对落在 D territory 的上轮条目逐条问三件事：修复是否接到真实路径 / 拆掉哪个 pin 会红 /
有没有在别处复制出同形缺陷。

| 上轮条目 | 接到真实路径？ | 拆掉会红的 pin | 同形缺陷 |
|---|---|---|---|
| **C2** 配置表守卫（`7ec13ff` / migration 003） | ✅ 真。实跑复验：`UPDATE currencies SET exponent=18` → `check_violation`；`UPDATE deposit_addresses SET account_holder=1`（表里有行）→ `check_violation`；`UPDATE currencies SET is_active=false` 放行且写了一行 `config_table_changes` | 拆掉 trigger：`TestClassificationsGuard_*`（`mutation_guards_test.go:26,41`）、`TestConfigTableChangesAudited`（`roles_test.go:718`）；拆掉 ACL：`grant_coverage_test.go` 的 `updateRevoked` 分支 | ⚠️ `TG_ARGV` NULL 那个**字面形态**已无兄弟（全仓 `TG_ARGV` 只剩 `003:72` 一处且带 COALESCE；006 的三个守卫用 `ARRAY[...]` 常量）。但那条的**方法论形态**（「验了算子，没验输入」= 手工名单 vs 从目录派生）复发了三次，见上面的 Critical、account_policies、grant_coverage/ledger_ro 三条 |
| **C4** `webhook_nonces` DELETE（`068afb4` / migration 002） | ✅ 真。实跑：以 `ledger_app` `DELETE FROM webhook_nonces` 成功，且它是全 schema 唯一持 DELETE 的表 | `grant_coverage_test.go:102` 的 `deleteAllowed` map：拿掉 GRANT 即红 | ⚠️ 修复只让缓存**能用**，没让它**被用** —— `SetWebhookNonceRecorder` 全仓零调用方（上面 Major 一条）。另：容错分支静默（Minor 一条） |
| **M-2** 受保护模板 + is_system 守卫（`8e503b3`） | ✅ 真。两个 handler 都在生产路径上：`handlePostTemplate:244`（默认集非空）、`handlePostJournal:172-177`（默认关闭 = 拒绝，opt-out 有启动 warn） | `TestPostTemplate_DefaultProtectsDepositCodes`、`TestPostJournal_RejectsSystemClassificationByDefault` | ❌ **是**。默认集是手工 4 个 code，漏了 `dev_credit`（Critical）。且 `TestPostTemplate_DefaultProtectsDepositCodes` 遍历被测实现自己的返回值 —— 与上一轮 `grant_coverage_test` 的「同源派生」是同一个洞，在 HTTP 层重开了一次 |
| **M-6** advisory lock 命名空间（`8e503b3`） | 不在本 territory，见 → B | — | — |
| **M-8** openapi 契约门禁完整性（`8e503b3`） | ✅ 真（两条 completeness 测试确实遍历 spec 的 `paths`），且 `chi.Walk` 反向门禁也在 | `TestOpenAPIContract_EveryRouteIsDocumented` 等 | ⚠️ 同一个 `chi.Walk` 基础设施**没有**用于鉴权面（Minor 一条）：路由的「有没有文档」被机器守着，「有没有鉴权」没有 |
| **grant_coverage 三分类** | ✅ 真，且是这一轮 DB 层最扎实的一处：未分类的新表直接 `t.Fatalf`（`grant_coverage_test.go:246-250`），三分类对**表**是穷尽的（实跑 34 张表全绿） | 加一张不分类的表即红 | ❌ 覆盖面有三个洞：**(a) 完全不看 ownership**（本轮最重的 DB 发现由此逃逸）；**(b) 完全不看函数的 EXECUTE ACL**（007 给 `ledger_app` 的两个 EXECUTE 没有任何门禁）；**(c) 排除 `relispartition`**，某张分区被单独错授的话看不见。另外 `ledger_ro` 的期望值硬编码成全表 SELECT（Minor 一条） |
| **上轮 Major：分区维护要 owner 权限 / owner TRUNCATE 绕守卫** | ✅ 代码侧真修了（`ledger.go:847` 用 app pool；`partition_store.go` 只调函数不发 DDL；实跑 `ledger_app` 建分区成功） | `TestLedgerAppInsertsIntoPartitionCreatedAfterGrant`（已改成单 app pool，不再用生产组装不出的 ownerPool） | ❌ 但修复本身带进两条新的：函数 owner 不是 `ledger_owner`（Major）；`RUNBOOK.md:669-672` 仍在教运维给 owner 池（Major） |
| **上轮 Major：检测层游标可被改写** | ✅ 真。DB 侧加了 AFTER UPDATE 审计（006）+ `lap_scanned` 累计计数（010），Go 侧 `service/reconcile.go:796,881-889` 用 `lapScanned < total` 判定 | 拆掉 `lap_scanned` 比较：`TestReconcileScanCursorChangesAudited` 只验审计行，真正证伪的应是 service 层的 lap-coverage 测试 | ⚠️ 010 header 诚实写下了残留（同时伪造 `after_holder` + `lap_scanned` 仍可绕过，靠审计留痕兜底）—— 但那个「留痕」没有任何读者（Major 一条），且审计行本身可被 `ledger_app` 伪造（Major 一条）。两者叠加后，「detection, not prevention」这个姿态实际上退回到了「记录，但既不可信也没人看」 |
| **上轮 Major：`ledger_ro` 能读 webhook HMAC 密钥** | ✅ 真。实跑：`ledger_ro` `SELECT name, secret` → `permission denied`；`SELECT name, url` → 成功；`SELECT *` → 拒绝（列级授权正确，REVOKE 表级也做了） | `TestLedgerRoCannotReadWebhookSecret`（`roles_test.go:762`）+ `grant_coverage_test.go` 的 `roColumnScoped` 列级断言 | ❌ **是**：同一条推理对 `ledger_app` 成立却被 014 显式留开（Major 一条）；且门禁把「新表给 `ledger_ro` 全表 SELECT」固定成了默认期望（Minor 一条） |
| **上轮 Major：write scope key 铸造入账（`TemplateCode` 无 allowlist）** | ✅ 部分。四个 deposit code 默认受保护；handwritten 路径加了结构性 `is_system` 检查 | 见 M-2 行 | ❌ **是** → Critical（`dev_credit`） |
| **上轮 Minor：`CREATE ROLE IF NOT EXISTS` 不校正属性** | ✅ 真（007:23-25 三条 `ALTER ROLE`） | `TestRoleAttributeHardeningResetsPreExistingPrivileges`（`roles_test.go:801`） | ❌ **是**，而且是本轮第二重的一条：这条修复引入的语句**只有 superuser 能执行**，把 RUNBOOK 认可的 CREATEROLE bootstrap 挡在门外（Major）。pin 用超级用户 pool 直接执行语句，看不见这件事 |
| **上轮 Minor：`ledger_owner` / `ledger_ro` 无密码 LOGIN** | ⚠️ 未修，且 007:20-24 显式记录了「LOGIN is deliberately left alone」的理由（三个 role 都要 LOGIN）。密码仍靠 out-of-band（`RUNBOOK.md:667-670`）。可接受的拍板，不重复报 | — | — |
| **上轮 Minor：holder 面泄露内部 journal type code** | ✅ 真修（M-7 / I-44）。`postgres/sql/queries/holder.sql:68` 现在输出 `COALESCE(NULLIF(jt.holder_kind,''),'other')`，值域是固定的产品词表（deposit/withdrawal/transfer/fee/adjustment/other），不再是 `jt.code` | — | 逐字段复核 `server/handler_holder.go:122-158` 的三个响应体：`currency_uid` / `uid` / `reversal_of_uid` 都是 UUID（I-18 uid-only 合规），`kind` 已词表化，`kind_label` 走 `DisplayLabel` 回退链，`memo` 来自 journal metadata。**未发现新的内部标识泄露**。错误面也干净：`pkg/httpx/response.go:169-176` 的 `Error()` 写的是 `bizcode.DisplayMessage(ae.Code)`（按码取的固定文案），内部 error 原文只进 `slog.Error`；未知码回退 `"An unexpected error occurred"`（`pkg/bizcode/errors.go:175-183`）。handler 里那些含 uid / 分类名的详细 403 文案（如 `handler_journals.go:224`）**不会**上线，只进日志 |

---

## 模型外场景的爆炸半径（各一句）

| 场景 | 爆炸半径 |
|---|---|
| **迁移凭证（bootstrap）泄露** | 等价于 superuser：它对 `ledger_owner` 保有永久 ADMIN OPTION（`001:1430-1436` 自己承认），且在当前 schema 下**还额外**拥有 002–015 建的 4 张表 / 4 个序列 / 9 个函数（含两个 `SECURITY DEFINER` 与全部 006/010 的守卫与审计函数），可以 `CREATE OR REPLACE` 掉守卫函数体让所有 append-only 保证静默失效 —— 而 001 建议的「安装后轮换/退役该凭证」因为它持有对象而无法直接 `DROP ROLE`。 |
| **备份泄露** | 保密性全失（全部账务明细 + `webhook_subscribers.secret`），完整性不受影响（签名与 attestation 一起备份，验签仍能识别改写）；设计档 non-goal 1 已覆盖前半，**未覆盖备份里的 HMAC 密钥**。 |
| **只读副本 / `ledger_ro` 泄露** | 007 之后止于保密性：全库明细可读，但 `webhook_subscribers.secret` 已被列级 REVOKE 挡住，无法升级为对下游的伪造能力。⚠️ 前提是 007 装上了 —— CREATEROLE-only 部署卡在 007，这条豁免不成立。 |
| **R2 anchor 凭证泄露** | 可发布一个更高 seq 的伪造 head：`Head()`（`anchors/r2/r2.go:190-217`）只读 key 的**最新版本**，从不枚举历史版本，所以 Object Lock 买到的是「旧版本还在」而不是「篡改会被发现」—— 攻击者若同时持有 DB 写凭证，可以截断尾部并发布匹配的新 head，`verify` 会报 VERIFIED。（属 → C，此处只给爆炸半径。） |
| **签名密钥（Attestor）泄露** | 设计档 §1 non-goal 2 明确不防；本轮复核确认密钥确实从不入库（全 schema 的 secret 类列只有 `webhook_subscribers.secret` 一列，见附录）。 |

---

## 移交

- **[→ C tamper-evident]** `anchors/r2` 的 `Head()` 只读 key 的最新版本，不枚举 Object Lock 保留的历史版本 —— 请核「anchor 能检测尾部截断」这条主张在只有一个 R2 凭证被泄露时是否还成立（`anchors/r2/r2.go:190-217`）。
- **[→ C tamper-evident]** `authdev` 的包 doc 说自己是 production-ready，`anchordev` 的说自己是 dev/test only。同一个 `dev` 后缀两种生产状态，`CLAUDE.md` 文件表也只对后者写了警告 —— 命名会误导消费方往任一方向判断。
- **[→ C tamper-evident]** `ledger_app` 对 `journals` 持表级 INSERT（含 `auth_status` / `auth_digest` / `auth_signature` 三列），所以裸 SQL 可以插一条自称 `auth_status='signed'` 但签名无效的 journal。请核在什么路径上会被识破、以及 `verify` 与提现闸各自的判定。
- **[→ B 并发]** `ledger_rebalance_default_partition` 函数体里的 `INSERT INTO journal_entries SELECT * FROM journal_entries_default` 会重新触发 `trg_check_journal_currency_balance`（deferred constraint trigger）。请核跨分区搬运一个 journal 的部分 entry 时该聚合是否仍按 journal_id 全表聚合（而不是只看本次插入的行）。
- **[→ B 并发]** `TryRecordNonce`（`postgres/webhook_subscriber_store.go:85`）每次调用先跑一条全表 `DELETE ... WHERE seen_at < ...` —— 高频入站下的写放大与锁竞争（上一轮已移交，未见处置）。
- **[→ H structure]** `deposits` / `withdrawals` 是完整的死表（10 个 sqlc 方法零调用方），却保有全部 ACL；`server/server.go:185` 文档了一个 `LoadConfig` 从不读的环境变量。
- **[→ I operability]** 三张审计表（`config_table_changes` / `reconcile_scan_cursor_changes` / `account_policy_changes`）没有任何 metric、CLI 命令或 RUNBOOK 条目；`docs/RUNBOOK.md:650,669-672` 的两条角色指引与 007 之后的代码相反。
- **[→ I operability]** 限流器只按 socket 对端 IP 分桶（`server/middleware_ratelimit.go:28-35`），`TrustedProxyCIDRs` 默认为空 —— 挂在负载均衡后面时全体调用方共用一个桶（100 mutations/min 全局），是自伤型可用性陷阱。

---

## 我没能验证的

- **`InstallDevCreditPreset` → `POST /journals/template` 的端到端实跑**。Critical 那条是通读完整调用链
  （路由 → scope 组 → `s.protectedTemplateCodes` 查表 → `ExecuteTemplate`）加 grep 确认
  `IsSystem` 检查在 `core/` `postgres/` `service/` 中不存在得出的，**没有真的起一个 server 打这个请求**。
  只读纪律下我不能加测试文件；建议 Team Lead 用一条 httptest 直接证伪（`newProtectedTemplateServer(nil, nil, ...)`
  + `postTemplateBody(presets.DevCreditTemplateCode)`，若返回 201 即坐实）。
- **golang-migrate 在 007 失败后的确切落盘状态**。我确认了 `ALTER ROLE ... NOSUPERUSER`
  在 CREATEROLE-only 角色下必然报 42501，但没有真的用 CREATEROLE 凭证跑一次完整的 `Migrate()`
  来观察它把 `schema_migrations` 标成 dirty 的哪个版本、以及重试行为。
  「008–015 不生效」是从 golang-migrate 的语义推出的，不是实测。
- **本报告的 ownership 结论对「非 superuser bootstrap」这一分支**。我的实测环境 runner 是超级用户
  （因为 CREATEROLE-only 根本装不完）。若将来 007 被改成可由 CREATEROLE 执行，
  对象 owner 仍会是那个 CREATEROLE 角色而非 `ledger_owner` —— 结论方向不变，但具体权限面会不同。
- **重放攻击的下游后果**。我确认了 nonce recorder 全仓无接线，但没有走完
  「重放一条 deposit sighting → `IngestDeposit` 的派生幂等键是否真的吸收它」这条链
  （`service/onchain.go`，属 → G）。所以「重放是否导致重复入账」我给不出结论，只给出
  「窗口内重放一定进得来」。
- **`Memo` 字段的内容来源**。`holder.sql` 的 memo 取自 journal metadata；我没有追查
  metadata 在各 preset / orchestration 里被写入了什么，所以「holder 面不泄露内部实现」
  这个结论对 `memo` 只覆盖到「字段本身不是内部标识」，不覆盖「消费方往里写了什么」。
- **`hmac.Equal` 之前的 hex 大小写**（`channel/onchain/evm.go:84-86`，比较的是 hex 字符串而非解码后字节）。
  与上一轮结论相同：不构成漏洞（不匹配即拒），但会让一个发送大写 hex 的外部扫描器永远被拒，
  且这条兼容性约束没有写进 `docs/api.md` 的 webhook 章节。未实测。
- **`ledger_ro` 无法按名直读 007 之后新建的分区**（实测 `permission denied for table
  journal_entries_y2020m01`，经父表则正常）。对 BI/取证是否构成实际障碍，取决于消费方怎么查，未评估。

---

## 附录 A · 逐表 ACL × trigger 矩阵

数据来源：一次性 `postgres:17` 容器，按序应用 `001`–`015` 后从
`has_table_privilege` / `pg_trigger` 直接查询（`relispartition` 的分区已排除，见附录 B）。
trigger 列格式 `触发器名:函数名:事件`，`I`=INSERT `U`=UPDATE `D`=DELETE。

| 表 | `ledger_app` | `ledger_ro` | 守卫 / 审计 trigger | 备注 |
|---|---|---|---|---|
| `account_policies` | S,I,**U** | S | `ledger_account_policies_guard:U` | ⚠️ 白名单放行 status/min_balance/enforce_min_balance；**无审计** → Major |
| `account_policy_changes` | S,I | S | `ledger_block_mutation:U,D` | 006 补齐 |
| `balance_checkpoints` | S,I,**U** | S | — | 设计如此（不可信缓存，靠 reconcile 检测）；无审计 |
| `balance_snapshots` | S,I,**U** | S | — | 无守卫无审计无检测（M4 未处置） |
| `booking_transition_receipts` | S,I | S | `ledger_block_mutation:U,D` | 006 补齐 |
| `bookings` | S,I,**U** | S | `ledger_bookings_guard:U` | journal_id set-once + settled_amount 单调；**无审计** |
| `chain_cursors` | S,I,**U** | S | — | 决定哪些区块被扫；无守卫无审计 → G |
| `checkpoint_rebuilds` | S,I | S | `ledger_block_mutation:U,D` | |
| `classifications` | S,I,**U** | S | `ledger_classifications_guard:U` + `ledger_log_config_table_change:U` | normal_side 不可变 + balance_role 单向升级 |
| `config_table_changes` | S,**I** | S | `ledger_block_mutation:U,D` | ⚠️ INSERT 可伪造 changed_by/changed_at → Major |
| `currencies` | S,I,**U** | S | `ledger_block_column_mutation('is_active'):U` + audit | exponent 已锁（上轮 Major 已修，实跑复验） |
| `deposit_addresses` | S,I,**U** | S | `ledger_block_column_mutation():U`（白名单为空） | 上轮 `TG_ARGV` NULL bug 已修，实跑复验拒绝 |
| `deposits` | S,I,**U** | S | — | 死表（0 调用方）→ Minor |
| `entry_attestations` | S,I | S | `ledger_block_mutation:U,D` | |
| `entry_template_lines` | S,I | S | `ledger_block_column_mutation():U` | UPDATE 已 REVOKE |
| `entry_templates` | S,I,**U** | S | `ledger_block_column_mutation('is_active'):U` + audit | |
| `events` | S,I,**U** | S | `ledger_events_guard:U` | 只放行投递列 + journal_id set-once；**无审计** |
| `ingest_dead_letters` | S,I,**U** | S | — | |
| `journal_entries` | **S 表级 + I 列级（不含 id）** | S | `ledger_block_mutation:U,D`、`check_journal_currency_balance:I,U,D` | 008：`INSERT` 收窄到列级，实跑确认带 id 的 INSERT 被 42501 拒 |
| `journal_types` | S,I,**U** | S | `ledger_block_column_mutation('display_label','is_active','holder_kind'):U` + audit | |
| `journals` | S,I,**U** | S | `ledger_journals_block_arbitrary_update:U`、`ledger_block_mutation:D` | 只放行 event_id 的 NULL→非NULL；**auth_* 列在 INSERT 面上无限制** → 移交 C |
| `ledger_attestations` | S,I | S | `ledger_block_mutation:U,D` | |
| `period_closes` | S,I | S | `ledger_block_mutation:U,D` | |
| `reconcile_scan_cursor_changes` | S,**I** | S | `ledger_block_mutation:U,D` | ⚠️ 同 `config_table_changes`，INSERT 可伪造 |
| `reconcile_scan_cursors` | S,I,**U** | S | `ledger_log_reconcile_scan_cursor_change:U`（仅审计，无守卫） | 006/010 的刻意选择，理由已写在 migration 里 |
| `registration_rescans` | S,I,**U** | S | — | |
| `reservation_operation_receipts` | S,I | S | `ledger_block_mutation:U,D` | 006 补齐 |
| `reservation_settlement_legs` | S,I | S | `ledger_block_mutation:U,D` | 006 补齐 |
| `reservations` | S,I,**U** | S | `ledger_reservations_guard:U` | 维度不可变 + settled_amount 单调 + 状态白名单；**无审计** |
| `rollup_queue` | S,I,**U** | S | — | |
| `schema_migrations` | —（无任何权限） | S | — | ⚠️ `ledger_ro` 有 SELECT（无害） |
| `system_rollups` | S,I,**U** | S | — | 平台侧汇总，无守卫无审计 |
| `webhook_nonces` | S,I,U,**D** | S | — | 全 schema 唯一的 DELETE（migration 002），实跑确认 |
| `webhook_subscribers` | **S 表级（含 secret）**；U 列级（仅 last_status_code/last_error/last_attempt_at）；INSERT 已 REVOKE | **列级 SELECT，不含 secret** | — | 014（写侧）+ 007（读侧）。`ledger_app` 仍读 secret → Major |
| `withdrawals` | S,I,**U** | S | — | 死表（0 调用方）→ Minor |

**「ACL 说可写、trigger 没拦、且该表决定记账结果或提现资格」的表**（本轮回答）：

1. `account_policies` —— 有 trigger 但白名单恰好放行三个风控开关；**且是唯一直接决定提现资格的表** → Major
2. `balance_checkpoints` —— 决定 `GetBalance` 与 `Reserve` 读到的余额，无守卫无审计（设计上以「不可信 + 可重算」承担，但检测层本身有上面的残留）
3. `system_rollups` / `balance_snapshots` —— 决定平台侧汇总与历史快照，无守卫无审计无检测
4. `chain_cursors` —— 决定哪些区块会被扫描，即哪些真实充值会被看见 → 移交 G
5. `webhook_nonces` —— 决定重放是否被识别（且是唯一持 DELETE 的表）

上一轮 `TODO.md` §4 说的「22 张有 UPDATE 无守卫」，现在是 **13 张**
（`balance_checkpoints` `balance_snapshots` `chain_cursors` `deposits` `ingest_dead_letters`
`registration_rescans` `rollup_queue` `system_rollups` `webhook_nonces` `withdrawals`
`webhook_subscribers`(已列级收窄) `reconcile_scan_cursors`(有审计无守卫)
`schema_migrations`(app 无权限，仅列出)）—— 其中被 `grant_coverage_test.go:186-222` 的
`reviewed` 名单显式认领为「有意识的决定」的是 11 张。**三分类对表是穷尽的**：
任何新表若不落进 `appendOnly` / `updateRevoked` / `reviewed` 三者之一，测试直接 `t.Fatalf`
（`:246-250`），这是本轮 DB 层最扎实的一处门禁。它的三个盲区（ownership / 函数 ACL / 分区）
见上面对应条目。

## 附录 B · 函数层（`grant_coverage_test.go` 完全不覆盖的一层）

| 函数 | owner（实测） | SECURITY DEFINER | `ledger_app` 可 EXECUTE |
|---|---|---|---|
| `ledger_create_monthly_partition(text,date,date)` | **bootstrap runner** | 是 | **是**（007:114） |
| `ledger_rebalance_default_partition(date,date)` | **bootstrap runner** | 是 | **是**（007:179） |
| `ledger_account_policies_guard` | **bootstrap runner** | 否 | trigger |
| `ledger_bookings_guard` | **bootstrap runner** | 否 | trigger |
| `ledger_events_guard` | **bootstrap runner** | 否 | trigger |
| `ledger_log_config_table_change` | **bootstrap runner** | 否 | trigger |
| `ledger_log_reconcile_scan_cursor_change` | **bootstrap runner** | 否 | trigger |
| `ledger_block_column_mutation` | **bootstrap runner** | 否 | trigger |
| `ledger_reject_unknown_normal_side` / `ledger_signed_amount` / `ledger_signed_delta` | **bootstrap runner** | 否 | — |
| `ledger_block_mutation` / `ledger_classifications_guard` / `ledger_reservations_guard` / `ledger_journals_block_arbitrary_update` / `check_journal_currency_balance` | `ledger_owner` ✅ | 否 | trigger |

分界线正好落在 001 与 002 之间 —— 001 建的对象被文件末尾的 sweep 扫到了，之后每一个都没有。
同样未被 sweep 扫到的表 / 序列：`config_table_changes(+_id_seq)`、
`reconcile_scan_cursor_changes(+_id_seq)`、`reservation_operation_receipts(+_id_seq)`、
`booking_transition_receipts(+_id_seq)`。
另：`ledger_rebalance_default_partition` 创建的每一个新分区也归函数 owner 所有
（实测 `journal_entries_y2020*` 全部 owner=bootstrap），且不带任何 `ledger_ro` 授权。

## 附录 C · secret 类列全量（回答「还有哪些 secret 列对 `ledger_ro` 可读」）

对 `information_schema.columns` 按 `secret|key|token|passwd|password|signature|digest|hmac|seed|priv`
正则扫全 schema，命中 18 列。其中真正的密钥只有一列：

- **`webhook_subscribers.secret`** —— `ledger_ro` 已被 007 列级挡住（实跑确认）；`ledger_app` 仍可读（Major）。

其余 17 列全部是 `idempotency_key`（幂等键，非密钥）、`entry_template_lines.amount_key`（模板参数名）、
以及签名产物本身（`journals.auth_digest/auth_key_id/auth_signature`、
`ledger_attestations.batch_digest/signature/key_id/auth_verdict_digest`）—— 这些是**公开可验证的输出**，
不是密钥。签名私钥与 anchor 凭证确实从不入库，与设计档 §0「签名密钥位置」的要求一致。

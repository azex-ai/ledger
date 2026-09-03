# 独立复审 · 安装机制与角色/凭证边界（install-roles）

**审计员**：独立安全审计（第一次接触本仓库）
**日期**：2026-09-03（SGT）
**范围**：`postgres.Migrate` 安装机制、三角色（`ledger_owner` / `ledger_app` / `ledger_ro`）的 ACL × trigger × 函数权限边界、五类凭证（迁移凭证 / 应用凭证 / 只读凭证 / R2 对象存储凭证 / 备份文件）的 blast radius。
**读过的材料**：`postgres/sql/migrations/001..028`、`postgres/migrate.go`、`postgres/*_test.go`（仅作为「护栏覆盖到哪」的证据）、`anchors/r2/r2.go`、`service/attest_verify.go`、`cmd/ledger-cli/main.go`、`CLAUDE.md`、`docs/INVARIANTS.md`、`docs/RUNBOOK.md`、`docs/DR.md`、`docs/plans/2026-08-21-tamper-evident-ledger-design.md`。
**按要求未读**：`docs/audits/` 下任何既有报告、`docs/plans/2026-09-02-*`、`docs/plans/2026-09-03-*`。因此本报告可能与既有轮次重叠；重叠不代表转述，全部结论都在容器里重测过。
**实验环境**：`postgres:17.10`（aarch64，容器 `r3iroles-pg`，端口 15923），非 superuser、`CREATEROLE` 的 bootstrap 角色 `boot` 拥有目标库；主仓只读，实验在 worktree `_worktrees/ledger/r3-install-roles` 完成，容器与临时 runner 已销毁。

---

## 摘要

安装机制在**新集群 + 非 superuser CREATEROLE 凭证**这条主路径上是成立的：`Migrate` 一次跑通，所有对象归 `ledger_owner`，`ledger_app` 的每一条「拆守卫」语句（`DROP TRIGGER` / `DISABLE TRIGGER` / `CREATE OR REPLACE` 守卫函数 / 夺 owner / `TRUNCATE` / 写审计表 / 改角色 / `SET ROLE` / `session_replication_role` / `pg_read_file`）在容器里逐条实测**全部被拒**（附录 B）。GRANT/ownership/函数 ACL 矩阵与 RUNBOOK 的描述一致，`ledger_ro` 读不到 `webhook_subscribers.secret`（列级 REVOKE 生效）。

但本轮找到 **2 个 Critical** 与 **5 个 Major**，其中最要紧的一条是：

> `ledger_app` 只需一条 `CREATE TEMP TABLE`，就能让 **I-24（DB 层逐笔借贷平衡）** 完全失效，并成功提交一笔凭空多出 999,999 的单边分录。

原因是同一类结构问题：**SECURITY DEFINER 函数全部钉了 `search_path = public, pg_temp`，而 9 个 SECURITY INVOKER 守卫函数一个都没钉**（附录 C），其中 `check_journal_currency_balance()` 恰好是唯一一个会去读关系（`journal_entries`）并且把去重状态放在调用方可写的 `pg_temp` 里的函数。仓库现有的护栏 `TestPartitionFunctions_SearchPathIncludesPgTemp` 只枚举了那 2 个 SECURITY DEFINER 分区函数，所以这个缺口不可能被现有 gate 发现。

第二类问题是**「上限/边界的参照物由被威胁的那个凭证自己写」**，出现两次：021 给 `ledger_rebalance_default_partition` 加的 120 个月上限，被一条 `ledger_app` 自己插入的远期 `created_at` 行绕过（实测一次调用建了 286 个分区 / 1716 个关系）；024 给 `anchor_observations` 加的「不得超过本地链高」上限，被 `ledger_app` 自己插入的一行伪造 `ledger_attestations`（seq=888888）抬高（实测两条 INSERT 即把 verify 永久焊死成 TAMPERED，且两张表都拒绝任何人 DELETE）。

第三类是安装机制在**共享集群**形态下的真实断裂：三个角色已存在时，非 superuser CREATEROLE 凭证装第二个 ledger 库会**死在 001 内部**（`must be able to SET ROLE "ledger_owner"`，库被标 dirty at 1），而 `Migrate` 那段写得很清楚的三选一 preflight 报错**跑在 001 之后**，永远看不到这个场景。

---

## 凭证 × blast radius

| 凭证 | 能做什么（实测） | 边界在哪 |
|---|---|---|
| ① **迁移/安装凭证**（`boot`：非 superuser + `CREATEROLE` + 库 owner） | **完全等同 owner，且永久、随时**：`GRANT ledger_owner TO boot WITH INHERIT TRUE` → `SET ROLE ledger_owner` → `DROP TRIGGER journal_entries_no_update` ✅ → `TRUNCATE journal_entries` ✅（不触发行级 trigger，实测把全部分录抹掉）→ `TRUNCATE schema_migrations` ✅。另外它对**三个角色都持 `ADMIN OPTION`**（创建者自动获得，实测 `admin_option=t`），因此可 `ALTER ROLE ledger_app/ledger_ro/ledger_owner PASSWORD ...` 直接接管这三个凭证 | 无窗口限制。001 头部只把这条残留描述成「对 `ledger_owner` 的 ADMIN OPTION」，实测范围是三个角色 + 改密码（见 m3）。这是本系统事实上的 root，必须当 root 管理（离线保存 / 轮换 / 由 superuser 收回三个 ADMIN OPTION） |
| ② **应用凭证 `ledger_app`** | 无任何 DDL / ownership / DELETE（附录 B 全拒）。但可：**提交单边不平衡分录**（C1/C2，破 I-1/I-24 写时防线）；插入任意 `ledger_attestations` 行（无签名校验）→ 焊死 verify（M4）；用远期 `created_at` 撬动分区批量创建（M3）；`DELETE FROM webhook_nonces` 清空重放缓存（002 授予，纵深上仍有 idempotency key）；读全库业务数据**含 `webhook_subscribers.secret`**（表级 SELECT，出站 webhook HMAC 密钥）；自由指定 `journal_entries.created_at`（列级 INSERT 覆盖该列，无 CHECK 约束） | I-22 的「不拥有任何对象」这层是真的、经得起逐条实测；破的是**建立在 trigger 语义之上**的两条不变量（I-24 的写时防线、024 的 anchor 记忆上限）。检测层仍在：`reconcile --full` 的 `journal_dr_cr` 实测报出 `passed:false`（前提是 reconcile 从未被攻陷的会话跑） |
| ③ **只读凭证 `ledger_ro`** | 全库 SELECT（含 `journals` / `bookings` / `deposit_addresses` / `config_table_changes` / `schema_migrations`）。写入实测全拒（`INSERT INTO currencies` / `UPDATE classifications` → 42501）。`webhook_subscribers.secret` 读不到（列级 REVOKE 生效，实测 `permission denied for table webhook_subscribers`，而 `count(*)` 可跑） | 泄漏成本 = 全量业务数据披露（无密钥材料：全库无私钥/口令列，附录 D）。可 `CREATE TEMP TABLE`，但无写权限故不可利用。副作用见 m4：用这个角色跑 `reconcile --full` 会因权限把一项检查判红 |
| ④ **R2 对象存储凭证**（`GetObject`+`PutObject`，按 RUNBOOK 范围） | 无法回退 head（一 seq 一对象 + `If-None-Match` 布局按声称工作）。但可 `PutObject` 一个高 seq 对象 → `Head()` 取 `MAX(seq)` → `VerifyLedger` 永久 `TAMPERED`（`anchor knows about seq X but the DB chain only reaches Y`），且**该对象在 Object Lock 保留期内谁都删不掉**，`anchor_observations` 又会把这个高 seq 记成永久记忆 → 没有回到 `VERIFIED` 的路径（M5） | 无法伪造/隐藏账目（需要签名密钥），但可**永久毁掉告警的信噪比**。RUNBOOK 把它写成 "possible-but-loud"，没写它不可逆、也没有恢复流程 |
| ⑤ **备份文件** | 全量业务数据披露；**不含**签名密钥、不含角色口令（`pg_dump` 不带 globals；schema 内无密钥列）。篡改后的备份恢复回来仍会被 attestation 链 / anchor 比对识别（设计意图成立） | 真正的缺口在恢复侧：`DR.md` 自己记录了「恢复进全新集群会对每条角色 GRANT 报 `role "ledger_app" does not exist`，用 `--no-owner` 无害带过」——那意味着**恢复出来的库可能整套 owner/ACL/角色隔离都没了**，而 §5 的验收步骤只跑 `reconcile`/`solvency`/`verify`，仓库也没有任何面向运维的 ACL/ownership/trigger 矩阵自检入口（`ledger-cli` 无此子命令）。I-22 只在 CI 的新装容器上被 pin，恢复后/人工 GRANT 后无人复核（m7） |

---

## 安装机制：三种形态实测

| 形态 | 结果 |
|---|---|
| **A. 全新集群 + 非 superuser CREATEROLE**（RUNBOOK 主推） | ✅ `MIGRATE OK`。001 建三角色 → 全部对象转 `ledger_owner` → 002..N 在一条 `SET ROLE ledger_owner` 的连接上跑完。收尾状态：`pg_auth_members` 只剩创建者自动 ADMIN 行（`admin_option=t, inherit=f, set=f`）×3，`schema_migrations` 上留给 runner 的 `arD`，`ALTER DEFAULT PRIVILEGES FOR ROLE boot ... TO ledger_owner`，`public` 对 PUBLIC 已收回 |
| **B. 共享集群（三角色已存在）** | ❌ **死在 001 内部**：`postgres: migrate: baseline: migration failed: must be able to SET ROLE "ledger_owner"`，`schema_migrations` = `(1, dirty=t)`（对象因单事务全部回滚，只剩这张表）。见 M1。superuser 事先 `GRANT ledger_owner TO boot WITH SET TRUE` 后再跑即成功 —— 即缺的只是一步，不是权限墙 |
| **B'. 共享集群 + 某角色已带 SUPERUSER** | ✅ 护栏按设计触发（007 的 fail-closed 报错，指名角色 + 给出 `ALTER ROLE ledger_app NOSUPERUSER` + 建议换集群）。但报错把「持有的属性」印成了「要施加的子句」，诊断句读起来是反的（m1） |
| **C. 单凭证部署（应用池与迁移同一凭证）** | 分三种：<br>· 凭证 = `ledger_app`：❌ 死在 golang-migrate 的 `CREATE TABLE IF NOT EXISTS "public"."schema_migrations"`（42501），**不是** `Migrate` 文档承诺的「三选一」actionable 报错（m2）<br>· 凭证 = `ledger_owner`（RUNBOOK 明列的第 2 种）：❌ 在**全新库**上同样死在 `schema_migrations` DDL —— 新库里 `ledger_owner` 没有 `CREATE ON SCHEMA public`（那条 GRANT 由 001 自己发放）。已装好的库上可用（m2）<br>· 凭证 = bootstrap，且另有会话在线：✅ 拒绝（报错文案与 RUNBOOK 一致）。**但该拒绝可被 `application_name` 绕过**（M2） |

---

## 发现

### [Critical] C1 · `pg_temp` 关系遮蔽让 DB 层逐笔平衡（I-1 / I-24）完全失效

**状态**：CONFIRMED（容器实测，两次，非 superuser `ledger_app`）

`check_journal_currency_balance()` 是 SECURITY INVOKER 且 `proconfig = (none)`（附录 C），函数体里 `FROM journal_entries` 是不限定的。PostgreSQL 对**关系名**隐式优先搜索 `pg_temp`，而 `ledger_app` 持有默认的 `TEMPORARY` 权限（013 的注释自己写明了这一点）。于是：

```sql
-- 以 ledger_app 连接
CREATE TEMP TABLE journal_entries (journal_id bigint, currency_id bigint, entry_type text, amount numeric);
BEGIN;
INSERT INTO public.journals (...) VALUES (..., total_debit=999999, total_credit=999999, ...);
INSERT INTO public.journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
  VALUES (<该 journal>, 42, <USD>, <main_wallet>, 'debit', 999999);   -- 单边，无对冲
COMMIT;   -- 实测：COMMIT 成功
```

对照组（不建临时表，同样的两条 INSERT）实测被拒：`ERROR: journal 1 has unbalanced entries by currency`。加上临时表之后延迟约束 trigger 在 COMMIT 时读到的是攻击者那张空的 `pg_temp.journal_entries`，`EXISTS(...)` 为假，直接放行。落库结果（`public.journal_entries`）：holder 42 凭空多出 999,999 的借方净额。

这正是 I-24 的 threat C1（"an attacker with a leaked app DB credential ... can issue a direct SQL INSERT into journal_entries"）所声称已经关闭的攻击，也是 I-24「两条互不信任的机制」里 DB 那一条的唯一实现。

**缓解现状**：批量检测层仍然工作 —— 实测 `ledger-cli reconcile --full` 的 `journal_dr_cr` 报 `passed:false / 1 journal/currency pair(s) fail per-journal balance`，`global_dr_cr_equality` 与 `accounting_equation` 也报红。所以这是「写时预防被完全绕过，事后检测仍在」，而不是静默无痕。

**修复方向**：给 `check_journal_currency_balance()` 加 `SET search_path = public, pg_temp`（与 6 个 SECURITY DEFINER 函数一致），或把函数体里所有关系名限定为 `public.*`。**注意这只修 C1，不修 C2。**

---

### [Critical] C2 · 平衡守卫的去重集合放在调用方可写的 `pg_temp`，可被预置绕过

**状态**：CONFIRMED（容器实测，独立于 C1，会话内未做任何关系遮蔽）

同一函数用 `CREATE TEMP TABLE IF NOT EXISTS ledger_balance_checked (...) ON COMMIT DELETE ROWS` 做事务内按 `journal_id` 去重，`INSERT ... ON CONFLICT DO NOTHING` + `IF NOT FOUND THEN RETURN NULL`。攻击者可以先自己建一张**同名、默认 `ON COMMIT PRESERVE ROWS`** 的临时表并预填 id：

```sql
CREATE TEMP TABLE ledger_balance_checked (journal_id bigint primary key);
INSERT INTO ledger_balance_checked SELECT generate_series(1,10000);   -- journals_id_seq 对 ledger_app 可读，id 可预测
-- 之后同一会话内任意单边分录：trigger 命中冲突 → NOT FOUND → RETURN NULL，聚合检查一次都不跑
```

实测输出 `NOTICE: relation "ledger_balance_checked" already exists, skipping`，随后不平衡分录（holder 77，借方 7）提交成功。

**为什么必须与 C1 分开修**：`IF NOT EXISTS` 与 `INSERT` 无论 `search_path` 怎么钉，都仍然解析到 `pg_temp` 里那张表（临时表只能在 `pg_temp`）。把 C1 的 `search_path` 钉上之后，C2 依旧成立。可行的修法：① 去重键改用事务局部 GUC / `pg_temp` 之外的载体（例如 `set_config('ledger.balance_checked.<id>','1',true)`），或 ② 建表时校验拿到的关系确实是本函数期望的形状与 `ON COMMIT` 语义（`pg_class.relpersistence` + `reloptions`/`on_commit` 不可从 catalog 直接读，故 ② 较脆），或 ③ 放弃去重、改用 `AFTER ... FOR EACH STATEMENT` 的普通 trigger + 显式 journal 级聚合（约束 trigger 必须 FOR EACH ROW，但普通 statement trigger 可以，代价是不再延迟到 COMMIT）。倾向 ①。

---

### [Major] M1 · 共享集群上的第二次安装死在 001 内部，报错不可行动，库被标 dirty

**状态**：CONFIRMED（实测两次，一次用真实创建者残留形状 `admin=t, inherit=f, set=f`）

001 靠 `SET LOCAL createrole_self_grant='set'` + `CREATE ROLE` 拿到 `ledger_owner` 的 SET 成员资格，而这**只在它真的创建了该角色时**发生。三角色已存在（同一集群装第二个 ledger 库；Aaron 本机共享 `dev-postgres` 正是这个形状，I-47 的集群锁存在本身就是为这个场景）时 `IF NOT EXISTS` 跳过 `CREATE ROLE`，section 14 的 `ALTER TABLE ... OWNER TO ledger_owner` 立刻失败：

```
postgres: migrate: baseline: migration failed: must be able to SET ROLE "ledger_owner" (column 0) in line 1: -- 001_baseline.up.sql ...
```

（后跟整份 1600 行迁移文件的回显。）`schema_migrations` 留下 `(1, dirty=t)`，需要人工 force。`Migrate` 里那段写得非常清楚的三选一 preflight（`prepareLedgerOwnerIdentity` / `assertSoleSessionOnCredential`）**在 `applyBaseline` 之后**才跑，所以对这个场景一句话都说不上。RUNBOOK 只覆盖了「已存在的角色带 SUPERUSER 等属性时 007 会拦」，没写「已存在的角色本身就让 001 装不上」。

补充实测：如果 bootstrap 凭证连创建者的 `ADMIN OPTION` 也没有了（例如运维按 001 头部的建议让 superuser 收回，或执行了 `revokeLedgerOwner` 错误信息里给的 `REVOKE ledger_owner FROM <runner>`，见 m3），失败点前移到 keepsake 那步：`permission denied to grant role "ledger_owner"`。

**修复方向**：把「让本连接能 act as `ledger_owner`」的安排（`prepareLedgerOwnerIdentity` 那套 ADMIN-OPTION 自授）提到 `applyBaseline` **之前**，对 001 也生效；或在 001 里检测「角色已存在且我没有 SET 成员资格」并 `RAISE` 一句带 remedy 的话（`GRANT ledger_owner TO <runner> WITH SET TRUE` 需 superuser 或持 ADMIN OPTION 者执行）。

---

### [Major] M2 · 单凭证部署的拒绝守卫可用 `application_name` 一行绕过

**状态**：CONFIRMED（两半分别实测）

`assertSoleSessionOnCredential` 用 `application_name IS DISTINCT FROM 'azex-ledger-migrate'` 排除「自己的连接」。这个键完全由客户端设置。

半 A（实测）：留一条 `boot` 会话执行 `SET application_name='azex-ledger-migrate'` 并挂住，`Migrate` 返回 **`MIGRATE OK`**，002..N 全跑完；把同一条会话的名字换成 `myapp`，则如期拒绝（报错文案与 RUNBOOK 一致）。

半 B（实测，定时脚本）：`boot` 的一条**先于迁移存在**的会话，T0 时 `SET ROLE ledger_owner` → `permission denied`；另一条连接（模拟 Migrate 的窗口安排）`GRANT ledger_owner TO boot WITH SET TRUE, INHERIT FALSE` 之后，**同一条从未重连的老会话**在 T6 `SET ROLE ledger_owner` 成功（`current_user=ledger_owner, session_user=boot`），窗口关闭后再次失败。即成员资格是集群级、按语句检查的，与「连接是新的还是旧的」无关。

两半合起来：一个持有迁移凭证的会话，只要把 `application_name` 设成那个常量，就同时得到「不被拒绝」和「整个 run 期间可 `SET ROLE ledger_owner` 并 `DROP TRIGGER`」。这恰好是这个守卫存在的唯一目的所要防的部署形态。`postgres/migrate_window_subject_test.go` 对**中途新建连接**的残留写得很诚实（并且 pin 了），但没有任何 pin 覆盖「老会话伪装 application_name 使拒绝失效」。

**注意不要高估**：对「自己创建了三角色」的 bootstrap 凭证来说，它本来随时可以靠 ADMIN OPTION 把 owner 拿回来（① 的 blast radius），窗口带来的增量有限。真正的损失是 fail-closed 变成了**静默 fail-open**：RUNBOOK 说这条守卫「makes the common case fail loudly instead of quietly」，而它的排除键是被审计对象自己能写的数据。

**修复方向**：每次 `Migrate` 生成随机 nonce，`application_name = "azex-ledger-migrate-" || <nonce>`，用等值/前缀匹配排除自己 —— 攻击者无法预测 nonce；同时保留对「同名但非本 run」连接的计数（那正是需要被拒绝的东西）。

---

### [Major] M3 · 021 的 120 个月上限被一条攻击者自己插入的远期 `created_at` 行绕过

**状态**：CONFIRMED（非 superuser `ledger_app`）

021 给 `ledger_rebalance_default_partition(p_first, p_last)` 加了月对齐 + 顺序 + `max_months=120` 三道参数校验（实测有效：`('2026-09-01','2050-06-01')` → `spans more than 120 months`），但函数随后按 default 分区里**实际存在的行**把范围向外扩，并且这段扩张按注释是「must stay uncapped」。`journal_entries.created_at` 没有任何 CHECK，`ledger_app` 持有该列的列级 INSERT。于是：

```sql
-- 一对平衡分录，created_at = '2050-06-15'（合法落进 default 分区）
SELECT array_length(ledger_rebalance_default_partition('2026-09-01','2026-12-01'),1);  -- 参数在上限内
--> 286
-- pg_class 中 journal_entries_y% 关系数：1716
```

一次「合规」调用建了 286 个分区（每个分区 = 表 + pkey + 3 索引），全程持 `journal_entries` 的 ACCESS EXCLUSIVE，且 `ledger_app` 无法 DROP 任何一个 —— 021 自己的话：「a one-way availability defect needing a DBA」。把 `created_at` 写成 4000 年即约 2.4 万个分区。

**修复方向**：扩张时只为 default 分区里**真实出现的月份**建分区（函数已经拿到 min/max，改成 `SELECT DISTINCT date_trunc('month', created_at)` 即可），空月一个都不建；这样扩张仍然「uncapped」（不会让过期 horizon 变得不可恢复），但 DDL 量由真实数据的月份基数决定而不是由攻击者选的跨度决定。另可给 `created_at` 加合理上界（trigger，而非 CHECK —— CHECK 不能用 `now()`）。

---

### [Major] M4 · 024 的「不得超过本地链高」上限，参照物由同一个凭证写入 → 红灯仍可被焊死

**状态**：CONFIRMED（非 superuser `ledger_app`，两条 INSERT）

024 的目的写得很明确：不让「a red light one INSERT can weld on」。它把 `anchor_observations` 的写入改成 SECURITY DEFINER 函数并收回直接 INSERT（实测有效：`INSERT INTO anchor_observations ...` → `permission denied for table anchor_observations`；`ledger_record_anchor_observation(..., 999999, ...)` → 按设计拒绝）。但那道上限是 `SELECT COALESCE(MAX(seq),0) FROM ledger_attestations`，而 `ledger_app` 对 `ledger_attestations` 持 `ar`（INSERT），DB 层对签名不做任何校验：

```sql
INSERT INTO ledger_attestations (uid, seq, entry_count, batch_digest, prev_root, root_hash, signature, key_id)
  VALUES (gen_random_uuid(), 888888, 0, ''::bytea, ''::bytea, ''::bytea, ''::bytea, 'x');   -- 实测成功
SELECT ledger_record_anchor_observation(gen_random_uuid(), 888888, ''::bytea);              -- 实测成功
DELETE FROM anchor_observations;              -- ERROR: DELETE on anchor_observations is not allowed
DELETE FROM ledger_attestations WHERE seq=888888;  -- ERROR: DELETE on ledger_attestations is not allowed
```

此后 `VerifyLedger` 的 `anchorSeq < lastObserved` 对每一次未来的运行都为真 → 永久 `TAMPERED`；两张表都是 append-only（trigger 对**任何角色**拒绝 DELETE，包括 `ledger_owner`，除非它先 DROP 自己的守卫 trigger），所以没有干净的回退路径 —— 024 自己描述的失败形态原封不动地成立，只是需要两条 INSERT 而不是一条。024 的收益陈述「the worst a leaked credential can now record is the true current chain height」不成立，因为 chain height 也由该凭证决定。

**修复方向**：上限不能取自被威胁的表。可选：① 用「attestation 行必须携带能被 `AuthVerifier` 验过的签名」作为记忆的前置（DB 端只能做形状检查，真正的比对得放到写入路径的 SECURITY DEFINER 函数之外），② 把 `ledger_attestations` 的写入也收成 owner-written 的 SECURITY DEFINER 入口（与 020/024 同形），并在其中校验 `seq = MAX(seq)+1` 与 `prev_root` 链接 —— 这样 seq 只能一步一步涨，`MAX(seq)` 重新变成一个可信参照，③ 为 `anchor_observations` 提供一条**可审计、owner-only、留痕**的作废路径（与 027 的 `ledger_unlink_event_journal` 同形），否则任何误记都是永久的。

---

### [Major] M5 · R2 凭证可把外部验证永久毁成 TAMPERED，且无恢复流程

**状态**：CONFIRMED（代码层，未跑 MinIO/R2 —— 见「我没能验证的」）

`Head()` 取前缀下 `MAX(seq)` 再读那一个对象；`Publish` 用 `If-None-Match: "*"` 防覆盖。`anchors/r2` 的包注释把「高 seq 注入」明确列为 "deliberately left possible-but-loud"，理由是 `VerifyLedger` 会把 `anchorSeq > maxSeqSeen` 报成 TAMPERED，「forward jump cannot hide anything」。这个推理对**保密性/完整性**是对的，对**可用性与告警可信度**是错的：

- 该对象在 Object Lock 保留期内无法删除（连管理员都不行；ledger 侧凭证按 RUNBOOK 又刻意不带 `DeleteObject`）；
- `AttestationService.catchUpAnchor` 会把这个高 seq 写进 `anchor_observations`（M4 的同一张表），此后即便对象消失，`anchorSeq < lastObserved` 也会永久成立；
- RUNBOOK 的「Verify status semantics」只定义了 `TAMPERED` 的含义，没有任何「anchor 被注入后如何回到可用状态」的流程。

净效果：只拿到 R2 写凭证（不接触数据库）的攻击者可以让 verify 永久报警，把真实篡改藏在告警疲劳后面。

**修复方向**：`Head()` 对「seq 远超本地链高」的对象不要直接采信为 head（例如只接受 `<= localChainHead + N` 的 seq，超出者作为**独立告警项**报出而不污染 head 与 `anchor_observations`）；同时在 RUNBOOK 写一条恢复流程（换 `Config.Key` 前缀 = 换 anchor 身份 + 记录旧前缀作为取证材料，并为 `anchor_observations` 提供 M4-③ 的作废路径）。

---

### [Minor] m1 · 007 的 fail-closed 报错把「持有的属性」印成了「要施加的子句」

**状态**：CONFIRMED（实测：预置 `ALTER ROLE ledger_app SUPERUSER` 后安装）

```
ledger: role ledger_app already exists on this cluster with the NOSUPERUSER attribute and this migration credential cannot remove it. ... (ALTER ROLE ledger_app NOSUPERUSER) ...
```

角色实际持有的是 `SUPERUSER`。`RAISE` 的第二个参数用了 `clauses[i]`（清除子句）而不是属性名。remedy 语句是对的，诊断句是反的 —— 对一条唯一价值就是 actionable 的安装期报错，值得修：多带一个 `attrs`→显示名 数组（`SUPERUSER` / `CREATEDB` / …），诊断位用它，remedy 位继续用 `clauses[i]`。

### [Minor] m2 · 两条 RUNBOOK 明列的迁移凭证形态在全新库上做不到，且报错不是承诺的那条

**状态**：CONFIRMED

- 凭证 = `ledger_app`：`Migrate` 文档与 RUNBOOK 都说「Any other credential is refused **before a single migration runs**, with a message naming all three ways out」。实测得到的是 golang-migrate 建 `schema_migrations` 时的裸 42501。那条 actionable 报错只有「能建 `schema_migrations` 的凭证」才够得到 —— 而指错 `MIGRATE_DATABASE_URL` 到应用凭证恰好是最常见的误配。
- 凭证 = `ledger_owner`（RUNBOOK 明列的第 2 种、以及「must migrate in-process 就用 owner」的建议）：**全新库上不可能**，`ledger_owner` 在新库里没有 `CREATE ON SCHEMA public`（那条 GRANT 由 001 自己发）。实测同样死在 `schema_migrations` DDL。已装好的库上可用。

**修复方向**：把凭证能力 preflight（能否建/写 `schema_migrations`、能否 act as owner、`postgres` 库可否 CONNECT）挪到 `waitForDatabase` 之后、`applyBaseline` 之前一次做完（与 M1 同一处改动）；RUNBOOK 的 `ledger_owner` 选项加上「仅适用于已安装的库」。

### [Minor] m3 · 安装凭证的残留比文档描述的更宽，且文档给的收回动作是单向的

**状态**：CONFIRMED

- 001 头部与 RUNBOOK 把残留描述为「对 `ledger_owner` 的永久 ADMIN OPTION」。实测 `pg_auth_members`：对 **三个角色** 都是 `admin_option=t`（`ledger_owner` / `ledger_app` / `ledger_ro`）。因此该凭证还能 `ALTER ROLE ... PASSWORD` 直接接管应用与只读凭证（实测成功，并用改后的口令登录 `ledger_app` 完成后续测试），也能把这三个角色授予任何第三方角色。
- `revokeLedgerOwner` 的错误信息让运维手工执行 `REVOKE ledger_owner FROM <runner>`。实测（以 superuser 执行）该语句会把**创建者那条 ADMIN 行一并删掉**（PG 16+ 语义：不带 `GRANTED BY` 时按执行者的权限撤销所有可撤销的成员行）。安全上是好事，但此后同集群的新安装会撞上 M1 的另一种失败（`permission denied to grant role "ledger_owner"`），而没有任何文档提示这是单向操作。

**修复方向**：RUNBOOK 的「retire the bootstrap credential」加上确切的三条 `REVOKE ADMIN OPTION FOR ledger_owner|ledger_app|ledger_ro FROM <runner>`（superuser 执行），并说明执行后同集群再装库需要先由 superuser 补 `GRANT ... WITH SET TRUE`。

### [Minor] m4 · `reconcile --full` 在只读凭证上会因权限把一项检查判红，且 `complete:true`

**状态**：CONFIRMED

以 `ledger_ro` 跑 `ledger-cli reconcile --full`：

```json
{"name":"checkpoint_balance","passed":false,"complete":true,
 "findings":[{"description":"checkpoint scan complete: 0 account/currency pairs verified this run"},
             {"description":"checkpoint scan cursor reset failed",
              "detail":"... ERROR: permission denied for table reconcile_scan_cursors (SQLSTATE 42501)"}]}
```

同一命令以 `ledger_app` 跑：`checkpoint_balance` `passed:true`。即「BI/报表专用角色」跑不干净这条运维命令，而输出把「因权限没跑成」和「数据不符」都表达成 `passed:false / complete:true`，`full_coverage` 仍为 `true`。这会把运维推向「干脆给调查工具一个写凭证」，与最小权限相反。

**修复方向**：游标写入拿到 42501 时把该检查标成 `complete:false`（未运行 ≠ 失败，也 ≠ 通过），并在 RUNBOOK 明说 `reconcile --full` 需要 `ledger_app`（或提供 `--no-resume` 只读模式）。

### [Minor] m5 · `docs/INVARIANTS.md` 指向三个已不存在的迁移文件

**状态**：CONFIRMED

`044_journal_balance_trigger.up.sql`（I-1 与 I-24 用它作为「机制在哪」的唯一指针）、`027_currency_exponent.up.sql`、`048_ledger_attestations_merkle_root.up.sql` 在 001 压平后都不存在了。I-24 还在讲「044 不是追溯的、018 之前的行不受 trigger 覆盖」——对全新安装（现在唯一的安装形态）这段叙事已经不成立。读者按文档去核 C1 涉及的那个机制会直接找不到文件。

**修复方向**：把这三处指针改成 `001_baseline.up.sql` 的节名（section 12 / section 4），历史留在 git。

### [Minor] m6 · `search_path` 护栏只覆盖 2 个 SECURITY DEFINER 分区函数（C1 得以存活的直接原因）

**状态**：CONFIRMED（读测试 + catalog 对照）

`TestPartitionFunctions_SearchPathIncludesPgTemp` 硬编码枚举 `ledger_create_monthly_partition` / `ledger_rebalance_default_partition`。附录 C 显示 public 下 20 个函数里 6 个 SECURITY DEFINER 都钉了 `public, pg_temp`（含 020/024/027 新增的，做得很好），而 9 个 SECURITY INVOKER 守卫函数全部 `proconfig = (none)`。021 引入的 `function_acl_test.go`（EXECUTE 白名单）也不看 `proconfig`。

**修复方向**：把这条 gate 改成从 catalog 枚举 `pg_proc`（与 001 section 14「问数据库而不是问记忆」同一原则）：public 下每个函数要么 `proconfig` 含 `search_path=...pg_temp`，要么在一个显式豁免名单里；新函数落地即被覆盖。

### [Minor] m7 · 没有任何面向运维的「ACL / ownership / trigger 矩阵」自检入口

**状态**：CONFIRMED（`cmd/ledger-cli` 子命令清单 + DR.md §4/§5）

I-22 的全部证据都在 CI 的新装容器上（`grant_coverage_test.go` / `object_ownership_test.go` / `function_acl_test.go`）。生产库经过一次逻辑恢复（DR.md 自己记录了 `--no-owner` + 角色缺失的告警）、一次人工 GRANT、或一次 owner 级抢救之后，没有任何命令能回答「这套隔离现在还成立吗」。

**修复方向**：把那三个测试的断言抽成一个只读的 `ledger-cli verify-acl`（或 `health --acl`），DR.md §5 的验收清单加一步。

---

## 我没能验证的 / 已排除的假设

**已实测排除（负面结果，写下来省下一次）**：

- **`pg_temp` 函数遮蔽不成立**。我原以为可以用 `CREATE FUNCTION pg_temp.ledger_journal_unlink_is_authorized() ... 'SELECT true'` + `SET search_path = pg_temp, public` 骗过 027 的两个 set-once 守卫（它们是 SECURITY INVOKER 且 `proconfig=(none)`，调用该谓词时不限定 schema）。实测**失败**：PostgreSQL 对函数/操作符名把 `pg_temp` 排在最后（用一对同名 `public.probe()` / `pg_temp.probe()` 探针验证：`search_path = pg_temp, public` 下仍解析到 `public`）。因此 027 的第 2 层守卫成立，`ledger_app` 拿到的是 `permission denied for function ledger_journal_unlink_is_authorized`。**可利用的只有关系遮蔽（C1），不是函数遮蔽。**
- `ledger_app` 的「拆守卫」语句逐条实测全部被拒（附录 B），包括 `SET session_replication_role = replica`（`permission denied to set parameter`）与 `pg_read_file`。
- `ledger_ro` 无任何写权限、读不到 `webhook_subscribers.secret`（列级 REVOKE 生效）。
- 全库无密钥材料列（附录 D）：`journals.auth_signature/auth_key_id`、`ledger_attestations.signature/key_id` 都只是签名与 key **标识**，私钥不在库里 —— 「拿到备份就能伪造签名」不成立。

**没能验证（明确列出，不要当成通过）**：

- **M5 未在真实 R2/MinIO 上跑过**。结论来自 `anchors/r2/r2.go`（`Head` 取 `MAX(seq)`）+ `service/attest_verify.go`（`anchorSeq > maxSeqSeen` → TAMPERED；`anchorSeq < lastObserved` → TAMPERED）+ Object Lock 语义。高 seq 注入的可行性我没有实际 PutObject 验证过（本轮无 R2 凭证，也没起 MinIO）。
- **备份/恢复未做演练**。⑤ 的 blast radius 与 m7 的推论基于 `DR.md` 自述与 schema/ACL 观察，没有真的 `pg_dump`→虚拟集群 `pg_restore` 走一遍，因此「恢复后 owner/ACL 具体丢成什么样」我给不出实测矩阵。
- **没跑 Go 测试套件**（`go test` 一次都没执行）。我只跑了 `postgres.Migrate` 的一个临时 runner 和 `ledger-cli`。因此「现有测试是否会因我的建议而变红」未知。
- **C1/C2 的检测时效未评估**。已确认 `reconcile --full` 会报出来，但没评估默认调度频率、是否有人值守、以及「攻击者同时持有应用进程（而非仅 DB 凭证）时能否连 reconcile 会话一起遮蔽」——后者在同一进程内是可能的，我没验。
- **`chains/evm`、`anchors/r2` 两个子模块的代码只读了 `r2.go`**，其余未看。
- **一个我制造的测试污染，已修正但值得说明**：为验证 007 的属性护栏我曾 `ALTER ROLE ledger_app SUPERUSER`，之后的两个实验（M3 分区、M4 anchor）第一遍是在污染状态下跑的。两者都在 `ALTER ROLE ledger_app NOSUPERUSER` 之后**重跑过一遍**，本报告引用的数字（286 分区 / 1716 关系；888888 的 anchor weld；`anchor_observations` 直接 INSERT 被拒）全部来自重跑。C1/C2/附录 B 的实验时间点早于该污染。
- 我没有测「以某个 `boot` 的成员角色连接」是否也能在窗口里 `SET ROLE ledger_owner`（守卫按 `usename = current_user` 计数，成员角色不会被计入）。这是个构造出来的配置，优先级低，但守卫的过滤条件确实不覆盖它。

---

## 附录 A · 表 × ACL × trigger 矩阵（新装库实测，`postgres:17.10`）

owner 一律 `ledger_owner`。ACL 记法：`a`=INSERT `r`=SELECT `w`=UPDATE `d`=DELETE `D`=TRUNCATE `x`=REFERENCES `t`=TRIGGER。「守卫」列为该表上非 internal 的 trigger。

| 表 | ledger_app | ledger_ro | 守卫 trigger |
|---|---|---|---|
| `journal_entries`（分区父） | `r` + 8 列级 `a`（不含 `id`，008/I-42） | `r` | `no_update`/`no_delete`（`ledger_block_mutation`）、`trg_check_journal_currency_balance`（CONSTRAINT, DEFERRED）|
| `journal_entries_default` / `_y2026m09..y2027m01` | 同上（列级 `a` 已克隆到每个分区） | `r` | 继承父表行级 trigger |
| `journals` | `arw` | `r` | `no_delete`、`no_arbitrary_update`（白名单 `event_id`）、`journals_audit`(AFTER UPDATE) |
| `events` | `arw` | `r` | `events_mutation_guard`、`events_audit` |
| `bookings` | `arw` | `r` | `bookings_mutation_guard`、`bookings_audit` |
| `reservations` | `arw` | `r` | `reservations_mutation_guard` |
| `classifications` | `arw` | `r` | `classifications_mutation_guard` |
| `account_policies` | `arw` | `r` | `account_policies_mutation_guard` |
| `ledger_attestations` | `ar` | `r` | `no_update`/`no_delete` ← **M4 的入口：INSERT 开放且无签名校验** |
| `entry_attestations` | `ar` | `r` | `no_update`/`no_delete` |
| `anchor_observations` | `r` | `r` | `no_update`/`no_delete`（024 收回了 INSERT 与序列 USAGE，实测生效）|
| `config_table_changes` | `r` | `r` | `no_update`/`no_delete`（D-M4：app 不能伪造 `changed_by`，实测 INSERT 被拒）|
| `reconcile_scan_cursor_changes` | `r` | `r` | 同上 |
| `reconcile_scan_cursors` | `arw` | `r` | 变更日志 trigger |
| `period_closes` / `checkpoint_rebuilds` / `account_policy_changes` / `booking_transition_receipts` / `reservation_operation_receipts` / `reservation_settlement_legs` / `entry_template_lines` | `ar` | `r` | `no_update`/`no_delete` |
| `balance_checkpoints` / `balance_snapshots` / `rollup_queue` / `system_rollups` / `currencies` / `journal_types` / `entry_templates` / `chain_cursors` / `deposit_addresses` / `deposit_reorgs` / `registration_rescans` / `ingest_dead_letters` | `arw` | `r` | 部分带列白名单守卫（003/006）|
| `webhook_nonces` | `arwd` ← 唯一被授予 DELETE 的表（002） | `r` | — |
| `webhook_subscribers` | `r`（**含 `secret`**） | 列级 `r`，`secret` 被 REVOKE；`last_*` 三列另有 `w` | — |
| `deposits` / `withdrawals` | `r`（021 收回了 INSERT/UPDATE） | `r` | — |
| `schema_migrations` | 无 | `r` | — ；`boot=arD`（golang-migrate 的两次版本写入） |

序列：`ledger_app` 普遍 `rU`，`ledger_ro` `r`；`anchor_observations_id_seq` / `config_table_changes_id_seq` / `reconcile_scan_cursor_changes_id_seq` 对 `ledger_app` 无授权（与「owner-written」一致）。schema：`USAGE` 给三角色 + runner，`CREATE` 只给 `ledger_owner`，`PUBLIC` 已全部收回。

## 附录 B · 以 `ledger_app`（非 superuser）实跑的「拆守卫」矩阵

| 语句 | 结果 |
|---|---|
| `DROP TRIGGER journal_entries_no_update ON journal_entries` | ❌ must be owner of relation |
| `ALTER TABLE journal_entries DISABLE TRIGGER ALL` | ❌ must be owner of table |
| `CREATE OR REPLACE FUNCTION ledger_block_mutation() ... RETURN NEW` | ❌ permission denied for schema public |
| `DROP FUNCTION ledger_block_mutation()` | ❌ must be owner of function |
| `ALTER FUNCTION ledger_events_guard() SET search_path = pg_temp` | ❌ must be owner of function |
| `ALTER TABLE journals OWNER TO ledger_app` | ❌ must be owner of table |
| `TRUNCATE journal_entries` / `TRUNCATE journal_entries_default` | ❌ permission denied for table |
| `DELETE FROM journal_entries` | ❌ permission denied for table |
| `INSERT INTO config_table_changes ...` | ❌ permission denied for table |
| `INSERT INTO anchor_observations ...` | ❌ permission denied for table |
| `ALTER ROLE ledger_app CREATEROLE` | ❌ permission denied to alter role |
| `SET ROLE ledger_owner` | ❌ permission denied to set role |
| `GRANT ledger_owner TO ledger_app` | ❌ only roles with ADMIN option may grant |
| `SET session_replication_role = replica` | ❌ permission denied to set parameter |
| `CREATE TABLE evil(x int)`（public） | ❌ permission denied for schema public |
| `SELECT pg_read_file('/etc/passwd')` | ❌ permission denied for function |
| `SELECT ledger_unlink_event_journal(...)` | ❌ permission denied for function |
| `UPDATE events SET journal_id = NULL`（027 的 set-once） | ❌ permission denied for function `ledger_journal_unlink_is_authorized` |
| **`CREATE TEMP TABLE t_probe(x int)`** | ✅ 成功 → C1/C2 的前提 |
| **`CREATE FUNCTION pg_temp.probe_fn() ...`** | ✅ 成功（但函数遮蔽不可利用，见「已排除」） |
| **`INSERT INTO ledger_attestations (seq=888888, 全空签名)`** | ✅ 成功 → M4 |
| **`DELETE FROM webhook_nonces`** | ✅ 成功（002 授权；纵深上仍有 idempotency key） |
| **`INSERT INTO journal_entries (..., created_at='2050-06-15')`** | ✅ 成功 → M3 |
| **单边分录 + `CREATE TEMP TABLE journal_entries`** | ✅ COMMIT 成功 → **C1** |
| **单边分录 + 预置 `ledger_balance_checked`** | ✅ COMMIT 成功 → **C2** |

## 附录 C · 函数 × owner × SECURITY DEFINER × `proconfig` × EXECUTE

| 函数 | SECDEF | `proconfig` | EXECUTE |
|---|---|---|---|
| `ledger_create_monthly_partition(text,date,date)` | ✔ | `search_path=public, pg_temp` | owner, **app** |
| `ledger_rebalance_default_partition(date,date)` | ✔ | `search_path=public, pg_temp` | owner, **app** |
| `ledger_record_anchor_observation(uuid,bigint,bytea)` | ✔ | `search_path=public, pg_temp` | owner, **app** |
| `ledger_log_config_table_change()` | ✔ | `search_path=public, pg_temp` | owner |
| `ledger_log_reconcile_scan_cursor_change()` | ✔ | `search_path=public, pg_temp` | owner |
| `ledger_unlink_event_journal(uuid)` | ✔ | `search_path=public, pg_temp` | owner |
| `ledger_journal_unlink_is_authorized()` | ✘ | `search_path=public, pg_temp` | owner |
| **`check_journal_currency_balance()`** | ✘ | **(none)** | owner ← **C1/C2** |
| `ledger_block_mutation()` / `ledger_block_column_mutation()` / `ledger_journals_block_arbitrary_update()` / `ledger_classifications_guard()` / `ledger_reservations_guard()` / `ledger_account_policies_guard()` / `ledger_bookings_guard()` / `ledger_events_guard()` | ✘ | **(none)**（这 8 个函数体不读关系，故当前不可利用；`ledger_events_guard`/`ledger_bookings_guard` 调用的谓词靠「函数名不走 pg_temp」侥幸成立） | owner |
| `ledger_resweep_ownership()` | ✘ | (none) | owner |
| `ledger_signed_amount` / `ledger_signed_delta` / `ledger_reject_unknown_normal_side` | ✘ | (none)（纯函数，不读关系） | owner, app, ro |

全部函数 owner = `ledger_owner`；021 的全表 `REVOKE ALL FROM PUBLIC` 生效（无一条 `(default: PUBLIC EXECUTE)`）。

## 附录 D · 敏感列清点（用于 ③⑤ 的 blast radius）

`secret|key|token|password|signature|digest|private|salt|nonce` 命中的列共 20 个：`webhook_subscribers.secret`（唯一真正的共享密钥，已对 `ledger_ro` 列级 REVOKE）、`webhook_nonces.nonce`、各表 `idempotency_key`、`journals.auth_{digest,signature,key_id}`、`ledger_attestations.{signature,key_id,batch_digest,auth_verdict_digest}`、`reservation_*.auth_*`。**没有任何私钥/口令列** —— 签名密钥在库外（应用侧），这也是 M4/M5 里「伪造签名做不到、焊死告警做得到」这个不对称的来源。

---

## 建议的处置顺序

1. **C1 + C2**（同一个函数，两处独立机制）—— 这是唯一一条在不接触 owner 权限的前提下就能凭空造钱的路径。
2. **m6**（把 `search_path` gate 改成 catalog 枚举）—— 否则 C1 会以另一个函数的形式回来。
3. **M4 / M5**（两条「永久红灯」）—— 它们让 tamper-evidence 的告警不再可信，而告警是 C1/C2 之后唯一剩下的防线。
4. **M1 + m2**（安装 preflight 提到 001 之前）—— 一处改动同时修掉共享集群断裂与两条 actionable-报错承诺。
5. **M2**（`application_name` → per-run nonce）、**M3**（只为真实存在的月份建分区）。
6. **m1 / m3 / m4 / m5 / m7**（文档与报错精度、运维自检入口）。

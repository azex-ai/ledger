# R3 复核 · install-roles 发现逐条重测（main `5a11533` / 代码 `0e7512a`）

**复核人**：同一名独立审计员（2026-09-03 `install-roles.md` 的作者）
**日期**：2026-09-04（SGT）
**被复核对象**：`5a11533`（`docs:` only；代码等于 `0e7512a`），schema 版本 30
**方法**：全新 `postgres:17.10` 容器（`r3recheck-pg`，端口 15931），非 superuser + `CREATEROLE` 的 bootstrap 角色 `boot` 拥有目标库，`postgres.Migrate` 从空库跑到 30；每条发现用**原报告的同一 PoC / 同一 mutation** 重放。只读代码，未修改任何被复核文件（两处反向确认用的临时 mutation 已还原，`git status` 干净）。
**判据**：零 Critical 复发。

---

## 结论

**判据未通过：1 条新的 Critical。** 原报告的 14 条发现里 **10 条完全闭合、3 条部分闭合、1 条未闭合（Minor）、0 条原样复发**；但 C1/C2 的修复（migration 030 §2 把聚合搬到 `journals` 上的延迟约束触发器 + 按 `journals.xmin` 跳过逐行检查）引入了一条**新的、同一不变量（I-1 / I-24 写时防线）的完整绕过**：

> `ledger_app` 在事务开头加一句 **`SET CONSTRAINTS ALL IMMEDIATE`**（任何角色都可执行，不需要任何权限），即可提交单边分录 —— 实测提交成功，凭空多出 999,999；并且可以给一笔**已经提交过、账面平衡的历史 journal** 追加 777 的单边借方。

原因：`SET CONSTRAINTS ... IMMEDIATE` 把 `journals` 上那个延迟触发器的触发时机从 COMMIT 提前到「`INSERT INTO journals` 语句结束」——那一刻这笔 journal **还没有任何分录**，聚合因此空过；随后每一条分录的逐行触发器都因为 `journals.xmin = 本事务 xid` 而跳过，COMMIT 时再没有任何东西复查。

三种形态全部实测成功（`SET CONSTRAINTS ALL IMMEDIATE`、只点名 `trg_check_journal_balance_on_journal IMMEDIATE`、以及对历史 journal 用 `event_id` UPDATE 夺取 xmin），对照组（同样语句、不加 `SET CONSTRAINTS`）被正确拒绝。仓库全库 grep **没有任何一处提到 `SET CONSTRAINTS`**，030 新增的 7 条 pin 也没有一条发出该语句，所以现有测试套件不可能看见它。详见下面 **N1**。

其余修复的质量很高，尤其：`REVOKE TEMPORARY FROM PUBLIC` 的三层纵深（pin 里刻意 `withTemporaryGrantedBack` 只测第 1、2 层）、`migrateRun` 用 backend pid 取代 `application_name` 作为自我排除键、M1 的 preflight 提前到 001 之前且失败时「什么都没应用、连 `schema_migrations` 都不存在」、M3 把 DDL 量从「写入方选的跨度」变成「数据的月份基数」（286 → 1 个分区）。

---

## 逐条表

| 原编号 | 原结论（2026-09-03） | 复核方法 | 复核结论 | 残余 |
|---|---|---|---|---|
| **C1** | `CREATE TEMP TABLE journal_entries` 遮蔽 → 单边分录提交成功（999,999） | 原 PoC 原样重放（ledger_app over TCP） | **已闭合**。第 3 层：`CREATE TEMP TABLE` → `permission denied to create temporary tables in database "ledger"`（`datacl` 实测 `{=c/boot,boot=CTc/boot}`，PUBLIC 只剩 `c`）。把 TEMPORARY 授回 PUBLIC 后再试，第 1/2 层仍拦（错误来自 `ledger_assert_journal_balanced`）。全库 32 个函数除 2 个申报豁免外全部 `search_path=public, pg_temp` | 见 **N1**（同一不变量被新机制以另一路径绕过） |
| **C2** | 预置 `pg_temp.ledger_balance_checked` → 聚合一次不跑 | 原 PoC（`CREATE TEMP TABLE ledger_balance_checked` + `generate_series`） | **已闭合**。建表本身被拒；且该 memo 结构已被 030 整体删除，无可预置之物 | 同上 |
| **M1** | 共享集群第二次安装死在 001 内部（`must be able to SET ROLE "ledger_owner"`），库被标 dirty at 1 | 同一集群第二个库 `ledger2`，同一 `boot` 凭证（残留形状 `admin=t, inherit=f, set=f`） | **已闭合**。`MIGRATE OK`，version 30 clean，43 张表；跑完 `set_option` 回到 `f`（临时成员资格已收回）。子形态：`REVOKE ledger_owner FROM boot CASCADE` 后再装 → fail-closed，报错点名三条出路并给出确切 `GRANT ... WITH SET TRUE, INHERIT FALSE`，且**什么都没应用**（`schema_migrations` 表都不存在，无 dirty 需要 force） | 无 |
| **M2** | 一条 `SET application_name='azex-ledger-migrate'` 让拒绝守卫失效，run 变 `MIGRATE OK` | 原 PoC：挂一条 `boot` 会话冒名，再对全新库跑 Migrate | **已闭合**。现在如期拒绝，并把冒用的名字当**标签**打印出来（排除键换成 `migrateRun` 记录的 backend pid 集合）。假阳性对照：杀掉该会话后同一条命令 `MIGRATE OK`，说明 Migrate 自己的多连接churn 没被误计。守卫覆盖面还扩大了：现在 001 阶段也受它约束 | 无 |
| **M3** | 一条 2050 年的 `created_at` 行让 in-cap 调用建出 286 个分区 / 1716 个关系 | 原 PoC：种一对 2050-06 的平衡分录 → `ledger_rebalance_default_partition('2026-09-01','2026-12-01')` | **已闭合**。over-cap 直调仍被 120 月上限拒；in-cap 调用返回 5（4 个 in-cap 月 + 1 个稀疏月），关系数 30 → 36，只多出 `journal_entries_y2050m06` 一张表 + 5 个从属关系 | 线性残余：每伪造一个**不同月份**仍多一个分区（1 行 = 1 分区，不再放大），且 `ledger_app` 仍无法 DROP。原报告建议的 sparse 修法已采纳，此残余是其固有下限 |
| **M4** | 伪造 `ledger_attestations.seq=888888` 抬高链高 → `ledger_record_anchor_observation` 接受 → verify 永久 TAMPERED 且任何人不可 DELETE | 原 PoC 两条 INSERT，另加「32 字节合规字段」变体 | **已闭合**。两种形态都被 `ledger_attestations_insert_guard` 拒（`seq must extend the chain by one (chain head is 0, got 888888)`）；ceiling 调用照旧拒。因此「焊死到任意高 seq」不复存在 | 部分残余（029 自述、且属 money-out M-4 域）：在空链上伪造 **seq=1**（genesis prev_root + 32 字节垃圾 + 任意签名）仍被接受，实测成功；此后 remembered ceiling 最高只能是 1。恢复路径 `ledger_discard_attestations_from` 已存在且对 `ledger_app` 正确拒绝（owner-only） |
| **M5** | R2 高 seq 注入 → verify 永久 TAMPERED，Object Lock 下不可删，无恢复流程 | 代码复核（`anchors/r2/r2.go` `Head`=MAX(seq)、`service/attest_verify.go` 两处判定）+ 文档复核（RUNBOOK §20）+ DB 侧 ceiling 实测 | **部分闭合**。注入本身仍可能（设计选择，不变），但：① DB 侧的记忆不再会被焊死（M4 闭合 + §20 给出 `remembered <= chain_head` 的判别 SQL）；② RUNBOOK 新增 **§20「The external anchor was poisoned」** 完整恢复流程（先轮换 token → 保留旧前缀作取证 → 换 `r2.Config.Key` 前缀 → 等 `catchUpAnchor` 追平 → 明确告知窗口期会持续 TAMPERED 且不要误读），并记录了原报告没发现的次级症状：`catchUpAnchor` 在 `latest.Seq <= anchorSeq` 时提前返回，等于**静默停止外部锚定**而 `anchor_lag_seqs` 仍是 0 | 未在真实 R2/MinIO 上验证（本轮同样没有 R2 凭证）；恢复流程未做演练。注入仍是「可做、吵闹、需人工换前缀」 |
| **m1** | 007 的 fail-closed 报错把「持有的属性」印成清除子句（角色持 SUPERUSER，报错说 "with the NOSUPERUSER attribute"） | 原 mutation：`ALTER ROLE ledger_app SUPERUSER` 后装新库 | **未闭合**，原样复发（逐字相同）。007 第 136-138 行仍是 `role_name, clauses[i], ...`；无后续 migration 纠正 | 仍为 Minor。修法不变：给 `attrs` 配一张显示名数组，诊断位用它、remedy 位继续用 `clauses[i]` |
| **m2** | 两条 RUNBOOK 明列的迁移凭证形态在全新库上做不到，且报错不是文档承诺的那条 | 分别以 `ledger_app` / `ledger_owner` 对全新库跑 Migrate | **部分闭合**。`ledger_app`：现在在第一条语句之前就拿到 actionable 报错（三条出路 + "Nothing has been applied"）✅。`ledger_owner`：**仍旧**死在 golang-migrate 的 `CREATE TABLE IF NOT EXISTS "public"."schema_migrations"` → 裸 `permission denied for schema public`（新库里 ledger_owner 没有 `CREATE ON SCHEMA public`，那条 GRANT 由 001 自己发） | ① RUNBOOK 仍把「`ledger_owner` itself」列为可用凭证而未注明「仅对已安装的库」（§20 之外，第 952 / 1041 / 1049 行）；② `ledger_app` 那条报错的**诊断语**假定了「同集群已有另一个 ledger 库」，而最常见的真实原因是「MIGRATE_DATABASE_URL 指错到了应用凭证」——remedy 正确，归因误导 |
| **m3** | 安装凭证的残留比文档描述的宽（对**三个**角色都持 ADMIN OPTION → 可改三者口令）；文档给的 `REVOKE ledger_owner FROM <runner>` 是单向操作且会让后续安装以不可行动的错误失败 | 重测 `pg_auth_members`；重跑 CASCADE 撤销后的安装 | **部分闭合**。后半（单向撤销之后的失败）已经不再是陷阱：现在 fail-closed + 精确 remedy + 零副作用（见 M1 子形态）。前半仍未记录：`admin_option=t` 依旧在 `ledger_owner`/`ledger_app`/`ledger_ro` 三行上，001 头部与 RUNBOOK 仍只提 `ledger_owner` 一个，且都没写「退役该凭证需要 superuser 收回三条 ADMIN OPTION，否则它可随时改三个角色的口令」 | 文档条目 |
| **m4** | `reconcile --full` 以 `ledger_ro` 跑会因权限把 `checkpoint_balance` 判红，且 `complete:true` | 原命令重跑 | **未闭合**，原样复发：`passed:false / complete:true` + `checkpoint scan cursor reset failed ... permission denied for table reconcile_scan_cursors` | 仍为 Minor。42501 应折成 `complete:false`（未运行 ≠ 失败），或在 RUNBOOK 写明该命令需要 `ledger_app` |
| **m5** | INVARIANTS.md 指向 3 个已不存在的迁移文件（044 / 027_currency_exponent / 048） | 重跑路径存在性检查 | **已闭合（按设计）**。`027_currency_exponent` 已消失；`044` 与 `048` 仍是死路径，但两处都紧跟着**活指针**（I-1 现在写明「mechanism 在 `001_baseline` section 12 as amended by `030_...`」），读者不再被卡住；I-24 已按 030 的新机制整节重写 | 死路径作为沿革保留 —— 可接受，不再单列 |
| **m6** | search_path 护栏只覆盖 2 个 SECURITY DEFINER 分区函数（C1 得以存活的直接原因） | 读新 gate + 实跑 + 反向 mutation | **已闭合**。`TestGuardFunctionSearchPath_EveryFunctionIsPinned` 从 `pg_proc` 枚举，等值断言 `search_path=public, pg_temp`；豁免只有 2 个 SQL inlinable 函数，且 `..._ExemptionsCannotReadRelations` 反过来约束豁免的形状（LANGUAGE sql + IMMUTABLE + 非 SECDEF + 无 trigger 引用 + 源码无 FROM/JOIN/DML）。反向确认：删掉一条 `ALTER FUNCTION` → gate 立刻变红并点名该函数 | 无（但见 N1：gate 覆盖的是 `proconfig`，覆盖不到「触发时机」这一类语义） |
| **m7** | 没有面向运维的 ACL/ownership/trigger 矩阵自检入口 | 复核 `cmd/ledger-cli` 子命令表 | **未闭合**（子命令新增 `dead-letters` / `reorgs`，仍无 ACL 自检；`health` 不读 catalog） | 仍为 Minor |
| — | — | — | **N1（新）**：`SET CONSTRAINTS ALL IMMEDIATE` 绕过 I-1/I-24 写时防线 —— **Critical** | 见下 |

统计：**已闭合 10**（C1 C2 M1 M2 M3 M4 m5 m6 + M1 子形态 + m3 后半）／**部分闭合 3**（M5 m2 m3）／**未闭合 3 个 Minor**（m1 m4 m7）／**原样复发 0**／**新 Critical 1**。

---

## N1 · `SET CONSTRAINTS ALL IMMEDIATE` 让 030 的新平衡守卫空过（Critical，CONFIRMED）

**凭证**：`ledger_app`（非 superuser，实测 `rolsuper=f`），通过 TCP 连接，无任何额外授权。

### PoC 1 · 新 journal（最短路径）

```sql
BEGIN;
SET CONSTRAINTS ALL IMMEDIATE;                       -- 唯一新增的一句
INSERT INTO public.journals (journal_type_id, idempotency_key, total_debit, total_credit, uid)
  VALUES (<t1>, 'rc-imm', 999999, 999999, gen_random_uuid());
INSERT INTO public.journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
  VALUES (<该 journal>, 42, <USD>, <main_wallet>, 'debit', 999999);   -- 单边，无对冲
COMMIT;                                              -- 实测：COMMIT 成功
-- rc-imm 的净额：+999999.000000000000000000
```

**对照组**（同样两条 INSERT，去掉 `SET CONSTRAINTS`）实测被拒：
`ERROR: journal 3 has unbalanced entries by currency`（来自 `ledger_assert_journal_balanced` ← `ledger_check_journal_balance`）。所以差别只在那一句。

### PoC 2 · 只点名新触发器

```sql
BEGIN;
SET CONSTRAINTS trg_check_journal_balance_on_journal IMMEDIATE;
... 同上 ...
COMMIT;   -- 实测成功，net = +7
```

说明问题精确定位在 030 §2 新增的那个 `journals` 级触发器，而不是「ALL」这个宽泛动作。

### PoC 3 · 篡改一笔**历史上已平衡**的 journal（最危险形态）

tx1 先诚实提交一笔平衡 journal（`rc-honest`，debit 100 / credit 100，净额 0，COMMIT 干净）。tx2：

```sql
BEGIN;
SET CONSTRAINTS ALL IMMEDIATE;
INSERT INTO public.bookings (...);                    -- 为下面的 event 准备 FK
INSERT INTO public.events (...);
UPDATE public.journals SET event_id = <该 event> WHERE idempotency_key='rc-honest';
   -- journals 级检查此刻立即触发：这笔 journal 当前是平衡的 → 通过
   -- 同时 UPDATE 刷新了 xmin，本事务从此「拥有」这笔历史 journal
INSERT INTO public.journal_entries (...) VALUES (<rc-honest>, 50, <USD>, <main_wallet>, 'debit', 777);
   -- 逐行触发器：journals.xmin = 本事务 xid → 跳过
COMMIT;   -- 实测成功
-- rc-honest 的净额：0 → +777
```

这正是 030 §2 注释里点名要保留逐行触发器去防的那种形态（"the backstop for entries appended to a journal THIS transaction did not write -- which is the direct-SQL tampering shape"）：攻击者只要先用一条**合法的 `event_id` 回填**把 xmin 变成自己的，那条 backstop 就自愿让路。注意 030 已经想到了「UPDATE 也会刷新 xmin」并因此让 journals 级触发器同时覆盖 UPDATE —— 那个补丁在**延迟**语义下是正确的，在 `IMMEDIATE` 语义下就反过来变成了攻击的一部分（UPDATE 时检查、之后追加）。

### 机制

030 §2 把「每笔 journal 检查一次」从「调用方可写的 pg_temp memo」换成了「`journals` 上的延迟约束触发器 + 用 `journals.xmin` 判断是否已排队」。前半的**排队时刻**由调用方控制：`SET CONSTRAINTS ... IMMEDIATE` 对任何 `DEFERRABLE` 约束触发器都可用、不需要任何权限、且「mode change 对本事务后续语句持续有效」。在 IMMEDIATE 模式下：

1. `INSERT INTO journals` 语句结束时 journals 级触发器就跑聚合 —— 此时该 journal **零条分录**，`EXISTS(... HAVING sum<>0)` 为假 → 视为平衡；
2. 之后每条分录的逐行触发器都命中 `journals.xmin = pg_current_xact_id()` → `RETURN NULL` 跳过；
3. COMMIT 时两个触发器都已「跑过」，无人复查。

跳过条件的自述安全方向是「答错只会让聚合多跑一次，绝不会让它对一条本事务没写的行跳过」—— 这句在**时序被调用方改动**之后不成立：行确实是本事务写的，但为它排队的那次检查已经在正确的数据出现之前用掉了。

### 为什么现有测试看不见

- 全仓库（`postgres/` `docs/` `core/` `service/`）grep `SET CONSTRAINTS` = **0 命中**。
- 030 新增的 7 条 pin（`postgres/guard_function_search_path_test.go`）都在默认的 DEFERRED 模式下跑；其中 `TestBalanceGuard_NoOpUpdateCannotAdoptAnOldJournal` 已经在测「用 UPDATE 夺 xmin」这条路，但因为没有 `SET CONSTRAINTS`，journals 级触发器在 COMMIT 时如期抓到了它 —— 只差一句就会红。
- 影响面刚好等于这两个触发器：本 schema 里 `tgdeferrable = true` 的非 internal 触发器只有 `trg_check_journal_balance_on_journal`（journals）与 `trg_check_journal_currency_balance`（journal_entries 及其全部分区），二者都属 I-1/I-24。

### 缓解现状（与 C1/C2 时相同）

批量检测层仍然工作：`ledger-cli reconcile --full` 的 `journal_dr_cr` 实测报 `passed:false / 4 journal/currency pair(s) fail per-journal balance`（涵盖 N1 造出的 3 笔与反向确认造出的 1 笔）。所以 N1 同样是「写时预防被完全绕过、事后检测仍在」，不是静默无痕；`global_dr_cr_equality` / `accounting_equation` 也报红。

### 修复方向（按我的偏好排序）

1. **逐行触发器不再无条件信任「journals 级检查会在之后跑」**。跳过条件需要的是「聚合尚未为这笔 journal 消耗掉」，而 `SET CONSTRAINTS` 的状态在 SQL 里读不到 —— 所以把去重键换成一个**调用方不可预置、也不可读写**的载体：owner 所有的 UNLOGGED memo 表，键 `(pg_current_xact_id(), journal_id)`，只由**改成 SECURITY DEFINER 的触发器函数本身**写入，且不对 `ledger_app` 授予任何 EXECUTE。这样既保住 030 实测的 O(N) 成本曲线，也不再依赖任何时序假设。
2. **journals 级检查拒绝把「零分录」当平衡**：一笔 `chk_journal_nonzero`（`total_debit > 0`）为真却没有任何分录的 journal 在任何时刻都不合法，`RAISE` 即可。这一条单独就能杀死 PoC 1/2，但**杀不掉 PoC 3**（历史 journal 在 UPDATE 时刻确实平衡），所以只能当纵深，不能当修复。
3. **回退到「逐行无条件聚合」**（删掉 xmin 跳过）。030 已经量过代价：2-6 条腿的真实 preset 免费，2000 条腿 5.7x（363ms → 2268ms）—— 而 2000 条腿的 journal 本身就是攻击形态。作为一次可立即落地的止血是划算的。
4. 无论选哪条，**新增一条 pin：在 `SET CONSTRAINTS ALL IMMEDIATE` 下重跑 C1/C2 的三种形态**。这类「调用方可改变约束求值时机」的缺口应该进 gate，而不是靠下一位读者想起来。

---

## 反向确认记录（拆掉修复 → pin 变红 → 已还原）

| # | 拆的是什么 | 预期 | 实测 |
|---|---|---|---|
| R1 | 容器内把 030 之前的守卫装回去（`check_journal_currency_balance` 恢复 unpinned + 未限定 `FROM journal_entries` + `pg_temp` memo；`DROP TRIGGER trg_check_journal_balance_on_journal`；`GRANT TEMPORARY ... TO PUBLIC`） | C1 原 PoC 重新成功 → 证明我的 PoC 仍然致命，闭合来自 030 而不是测试漂移 | ✅ `rev-c1` 单边 1234 提交成功。随后把 030 的函数体、触发器与 `REVOKE TEMPORARY` 在容器里恢复，后续所有测量都在恢复后的状态上做 |
| R2 | 源码 mutation A：从 030 §1 删掉 `ALTER FUNCTION ledger_events_guard() SET search_path = ...` 一行 | 目录枚举 gate 变红并点名该函数 | ✅ `--- FAIL: TestGuardFunctionSearchPath_EveryFunctionIsPinned`，消息点名 `ledger_events_guard()`。已 `cp` 还原 |
| R3 | 源码 mutation B：注释掉 `CREATE CONSTRAINT TRIGGER trg_check_journal_balance_on_journal` | C1/C2 的行为 pin 变红 | ✅ `--- FAIL: TestBalanceGuard_DedupSetCannotBePreSeeded`、`--- FAIL: TestBalanceGuard_NoOpUpdateCannotAdoptAnOldJournal`。已 `cp` 还原 |

还原后 `git status --short` 干净；7 条相关 pin 全绿（`go test ./postgres/ -run 'TestGuardFunctionSearchPath|TestLedgerApp_CannotCreateTemporaryRelations|TestBalanceGuard' -count=1 -v` → 全部 PASS）。

---

## 顺带核对（非原报告条目，均为正向结果）

- **安装形态 A（全新集群 + 非 superuser CREATEROLE）**：`MIGRATE OK`，version 30 clean；`datacl` 显示 PUBLIC 只剩 CONNECT。
- **`ledger_app` 拆守卫抽查（新 HEAD）**：`DROP TRIGGER trg_check_journal_balance_on_journal` / `ALTER TABLE journals DISABLE TRIGGER ALL` / `DROP TRIGGER ledger_attestations_insert_guard` / `SET ROLE ledger_owner` / `CREATE TEMP TABLE` / `ledger_rewind_chain_cursor` / `ledger_discard_attestations_from` / 直接 `INSERT INTO anchor_observations` —— **逐条被拒**。
- **`ledger_app` 的函数 EXECUTE 集合**：只多了 `ledger_assert_journal_balanced`（读 `journal_entries` 后要么返回要么 raise，`ledger_app` 本来就能 SELECT 该表），其余仍是原来 5 个；029 新增的 owner-only 门（rewind / discard / repair 授权谓词）都没有对 app 授权。
- **029 的两个新授权谓词**（`ledger_template_line_repair_is_authorized` / `ledger_chain_cursor_rewind_is_authorized`）沿用 027 已验证的「事务局部 GUC **且** `pg_has_role(current_user,'ledger_owner','USAGE')`」双条件；GUC 自身谁都能 `set_config`，但角色那半 `ledger_app` 过不去（原报告已实测函数名不走 `pg_temp`，此路不通）。

---

## 我没能验证的

- **M5 仍未在真实 R2 / MinIO 上跑过**（本轮无 R2 凭证，未起 MinIO）。§20 的恢复流程是读出来的，没有演练：尤其「换前缀后窗口期持续 TAMPERED、追平后自愈」这一段没有实测。
- **备份 / 恢复仍未演练**，m7（恢复后 ACL 是否还成立、有无自检入口）因此仍只是代码与文档层面的判断。
- **只跑了 7 条相关 pin**，没有跑全量 `go test ./...`；因此不能断言我的 N1 修复建议不会与别处冲突，也不能断言 Wave 5 的其它改动全绿（那是 gates 的职责）。
- **N1 的其它可能形态没有穷举**：我测了 3 种，没有测「`SET CONSTRAINTS` + 分区表直插」「子事务 / `SAVEPOINT` 与 xmin 的交互」「`SET CONSTRAINTS` 之后再 `SET CONSTRAINTS ALL DEFERRED` 的来回切换」。前两者我预期结论不变（跳过条件同一条），但没有测就是没有测。
- **未评估 N1 的检测时效**：只确认 `journal_dr_cr` 会报，没有评估调度频率与值守。
- 本轮所有实验都在我自己的 worktree + 一次性容器（已销毁）里；主仓与其它 worktree 未触碰。

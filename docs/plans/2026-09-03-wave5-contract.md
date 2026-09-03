# Wave 5 — 独立复审（R3）发现的整改契约

> 2026-09-03 · Status: **RUNNING（六条任务并行）** · 基线 = main `c854c6e` 之后。
> 源：`docs/audits/2026-09-03-independent-review/*.md`（五个零前情 agent）。纪律沿用 `2026-09-02-remediation-contracts.md` §0（兄弟扫描——本轮明确：**粒度 = 算式项 × 谁能写这张表；对每张表分别问 INSERT / UPDATE / DELETE 三条路径**）、§3、§5、§6。

## 0. R3 的核心发现：三个此前三轮都没问过的问题

| # | 问题 | 命中 | 为什么前三轮看不见 |
|---|---|---|---|
| R3-1 | **INSERT 路径既不拦也不留痕**：24 个 BEFORE UPDATE + 13 个 BEFORE DELETE 守卫、6 个 AFTER UPDATE 审计，INSERT 期只有一个平衡约束触发器。往 `entry_template_lines` 追加一行让诚实充值多认 N 倍；往 `bookings` 插一行让 recheck job 签出凭空入账；往 `chain_cursors`/`ledger_attestations` 插行让检测倒置 | money-out C-1 C-2 M-1 M-4；onchain-ops C-1；install-roles M3 M4 | 三轮的守卫工作都围绕「篡改已存在的行」（UPDATE/DELETE）；「追加一行合法形状的行」从未被列为攻击面 |
| R3-2 | **SECURITY INVOKER 守卫函数未钉 `search_path`**：`ledger_app` 持默认 TEMPORARY 权限，`CREATE TEMP TABLE journal_entries` 遮蔽真表，借贷平衡触发器失效；其去重集合本身活在 `pg_temp`，可预置绕过 | install-roles C1 C2 | 上一轮只给 SECURITY DEFINER 函数钉了 `search_path`（013），gate 只枚举 2 个分区函数 |
| R3-3 | **README 可运行门禁只跑 4/24 个代码块**，Quick Start 两个块不编译、且教的是「迁移凭证 = 运行时凭证」 | consumer C1 C2 | D-surface 的 gate 按锚点挑块；gates-fixes 的「未分类块红」只覆盖新增块 |

## 1. 任务（文件独占域）

| 任务 | 覆盖 | 独占文件 | 号段 |
|---|---|---|---|
| **W5-insert** INSERT 路径的守卫与审计 | money-out C-1 C-2 M-1 M-4；install-roles M3 M4；onchain-ops C-1 | 新 migration **029**：① 配置表（`entry_templates` `entry_template_lines` `classifications` `currencies` `journal_types` `account_policies` `deposit_addresses` `webhook_subscribers`）与状态表（`bookings` `reservations` `chain_cursors` `ledger_attestations` `anchor_observations`）加 **BEFORE INSERT 守卫或 AFTER INSERT 审计**：配置表的 INSERT 只允许经 owner-only SECURITY DEFINER 写入函数（与 020 审计表同形）或必须写审计行；`bookings` 的 INSERT 必须来自 `Booker.CreateBooking`（约束：`status` 必须是 lifecycle initial、`metadata` 不得含 `block_number` 等确认字段——由 trigger 校验）；`chain_cursors` 加审计 + 单次前进上限 + 只能由 scanner 事务（同事务内有 ingest 或 `scan_evidence` 行）推进；`ledger_attestations` INSERT 必须 seq = max+1 且 prev_root 匹配。② `audit_trail_guard_test` 的豁免表按 INSERT/UPDATE/DELETE 三列分别派生。③ 周期性回看重扫最近 N 块（幂等键吸收重复）作为 `chain_cursors` 前跳的检测层 | `postgres/sql/migrations/029_*`、`postgres/audit_trail_guard_test.go`、`postgres/grant_coverage_test.go`、`service/onchain.go`（仅回看重扫）、`docs/INVARIANTS.md` 新 **I-66 I-67** | migration 029、I-66 I-67 |
| **W5-searchpath** 守卫函数 `search_path` 与 pg_temp | install-roles C1 C2 | migration **030**：全部 SECURITY INVOKER 守卫/约束函数 `SET search_path = pg_catalog, public`（**不含 pg_temp**）；`check_journal_currency_balance` 的去重集合从 pg_temp 移到事务内 `pg_advisory_xact_lock` + 会话级临时结构不可预置的形态（或改为无去重的逐 journal 聚合，实测成本）；`REVOKE TEMPORARY ON DATABASE FROM ledger_app`（评估影响：应用是否依赖临时表）；gate 从「2 个分区函数」改为「目录派生：所有 trigger 函数与 SECURITY DEFINER 函数的 proconfig 必含固定 search_path」 | `postgres/sql/migrations/030_*`、`postgres/function_acl_test.go`、`postgres/*search_path*_test.go`、I-24 措辞 | migration 030、**I-68** |
| **W5-readme** README 与门禁 | consumer C1 C2 M1–M10 | README 全部 24 个 Go 块**必须编译**（gate 抠出每个块编译；不能运行的标 `<!-- readme-gate: compile-only -->`）；Quick Start 拆 `MIGRATE_DATABASE_URL`/`DATABASE_URL`，`svc.Worker` 双返回值，`srv` 的真实挂法；`AssertRuntimeRole` 进 Quick Start 且 `StartupReport.Warnings` 对「运行时凭证是 owner/superuser」出 warning（这一条改 `service/worker.go`/`ledger.go`，独占）；pending bundle 闸、`SetDepositIngester`、api.md 缺章节、BREAKING 缺条目、`anchors/r2` 可 `go get` 口径 | `README.md`、`readme_runnable_test.go`、`docs/api.md`、`docs/BREAKING.md`、`docs/COOKBOOK.md`、`ledger.go`（仅 runtime-role warning 接线）、`service/worker.go`（仅 StartupReport 新 warning）、`server/handler_bookings.go`（F-M1 500→4xx） | — |
| **W5-onchain-ops** | onchain-ops C-2 M-1…M-9 | dead-letter 的 metric / CLI / RUNBOOK / 重放路径；`auto_reverse` 至少 N 次连续观测；decimals × exponent 交叉校验；orphaned broadcast 恢复步骤；RUNBOOK §8 §15 修正；`ResolveReorg` 接线与文档；`chain_cursor_lag` 停摆形态；三个 job 存活/失败指标 | `service/onchain*.go`、`chains/evm/**`、`cmd/ledger-cli/`、`docs/RUNBOOK.md`、`core/metrics.go`（追加 + NopMetrics）、`observability/` | **I-69** |
| **W5-money-misc** | money-out M-2 M-3；install-roles M1 M2 M5 | 伪造 `reversal_of` 链接的剩余口（M-2，看 I-51 rule 漏了什么）；I-11 gated 路径 role 过滤的真 pin（M-3）；共享集群第二次安装可行动报错（M1）；`application_name` 绕过守卫 → 改按 `backend_type`/`pid` 集合而非可自报的 application_name（M2）；R2 凭证钉死 TAMPERED 的恢复流程（M5） | `postgres/ledger_store.go`、`postgres/reversal_fraction_store.go`、`postgres/reserver_store.go`（仅 role 过滤 pin）、`postgres/migrate.go`、`service/attest_verify.go`、RUNBOOK 对应节 | — |
| **W5-gates** | gates-pins F-1…F-9 + PLAUSIBLE + 互斥测试 + 24 条未 mutation 不变式 + 6 类未验证门禁面 | 新增 `postgres/*_pin_test.go`（约束直击 pin、append-only 触发器目录派生 pin）、`core/invariants_pins_test.go`（登记表改解析声明 + 只许缩）、I-50/I-61/I-18/web census 门禁改 AST/类型派生、`govulncheck` 覆盖 submodule、Docker 缺失 fail-closed、`.github/workflows/**` | — |

## 2. 合并顺序与门
W5-searchpath（030）与 W5-insert（029）先后合（号段顺序）；其余并行。全部合入后 **R3 五名复审员各自对自己的发现做一次复核**（同一 agent、同一 worktree、只读），零 Critical 复发才进入发版评估。

## 3. 通过标准
同 `2026-09-03-independent-review-contract.md` §3。

## 4. 整改期间的拍板（lead）

| # | 问题 | 拍板 |
|---|---|---|
| 5.1 | `bookings.metadata` 在 INSERT 期合法带 `block_number`（recheck 依赖它），任务书那条「metadata 不含确认字段」约束会打死诚实充值 | 不做；bookings/reservations INSERT 只钉形状 + AFTER INSERT 审计 |
| 5.2 | money-out C-2（插一行 confirming/pending booking 被 recheck 自动签出）无法在 DB 层预防（recheck 同时扫 pending 与 confirming） | 预防归 **W5-onchain-ops**：recheck 对任何推进到 confirmed 的 booking 无条件核链上包含（N 次连续观测）且金额/地址/token 与链上日志一致，核不到进 review；029 只留审计痕迹 |
| 5.3 | `entry_template_lines` 的 `amount_key` 在出厂模板里本就重复，「不得重复」不可用 | 改用「父 template 必须是本事务创建」的同事务约束（`CreateTemplate` 与其 lines 同事务；`InstallTemplatePresets` 对已存在模板只校验不更新），能阻止而非只记录 C-1 |
| 5.4 | money-out M-2：I-51 三条规则在应用写路径无漏，漏在读侧无条件采信 raw INSERT 的 `reversal_of` 行 | 扩成「用时门」：`cumulativeReversedByDimension` 只采信自身通过 rule 1–3 的行，不通过的不计入并写 Finding |
| 5.5 | install-roles C2 的候选修法①（事务内 GUC）同样可被 `SET LOCAL` 预置（w5-searchpath 实测） | 由 w5-searchpath 在其余候选里按实测成本选 |
| 5.6 | 六个 worker 于 2026-09-03 13:21 同时撞会话额度上限，全部有未提交改动 | 额度重置后以 SendMessage 续做（先 WIP commit）。**协议追加**：worker 每完成一个子项立即 commit，不攒 |

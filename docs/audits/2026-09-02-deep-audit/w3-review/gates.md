# W3 对抗式复审 — 门禁与测试可信度

> 角色：security-reviewer（只读，只写本文件，未改动任何被评审代码）
> 基线：`676ae7e`（评审开始时的 main HEAD；期间 lead 又推了 docs-only 的 `0edad7b`，不影响任何结论）
> 评审对象：`git diff fd87cae..HEAD` 里**新增或改动的门禁与 pin**
> 方法：契约 §1.4 三问 + 实跑 mutation。**全部 mutation 在独立 worktree
> `/Users/aaron/projects/_worktrees/ledger/w3-gates`（分支 `w3-gates`）里做**，主工作树全程只读——
> 这是 lead 在 `lead-verification.md`「流程发现」里定的下轮规矩（F 的 19 个污染窗口的直接后果）。
> 每条结论都能在下方表里找到对应行；标「读码」的没有实跑，不要当成实测背书。

---

## 摘要

本轮整改新加的门禁**数量和设计水平都比上一轮高一个量级**：`grant_coverage` 的「新表不分类直接 fail」、
`object_ownership` 的 catalogue sweep、`precision_schema` 的机械派生、`route_authorization` 的
chi.Walk 中间件链、openapi 的双向类型比对——我实跑了 12 条控制组 mutation，**全部真的会红**。

但本轮的核心主题是「门禁守形状不守语义」，而**这批新门禁自己大面积复现了同一个形态**。
我做了 **34 次 mutation**（22 条盲区 + 12 条控制组），找到 **23 个盲区**。三条最要紧：

1. **`PUT /accounts/{holder}/policy` 从 admin 降到 write scope，`server` / `core` / 根包全绿。**
   那是全仓唯一的 DB 强制冻结 / 最低余额闸（`audit_trail_guard_test.go` 自己的注释这么说的）。
   新建的 `TestRouteAuthorization_EveryRouteIsGuarded` 只检查中间件链里**存在**
   `requireScope`/`requireCapability`，不检查是**哪个** scope；唯一能看出降权的
   `TestAuth_ScopeEnforcement` 是手写的 4 条路由抽样，12 条 scope 路由里 8 条没抽到。
   这是 D-C1（write-scope key 可铸币）在**权限边界**层的同形复发，而本轮为「路由有没有鉴权」
   专门建的门禁看不见它。

2. **I-32（提现验签闸）的六条 pin 换成 `core.TestIsUserAccount`，四条 INVARIANTS 门禁全绿。**
   `TestInvariantsPinsReferenceEnforcedSymbols` **检测到了**这个不匹配，但把它降级成
   `t.Log`——因为 I-32 在 `citationStyleGapInvariants` 白名单里。我把**全部 63 条** invariant 的
   pin 都换成同一条无关测试，实测：**只有 10 条会红**（I-8/10/17/29/33/47/55/56/59/63），
   28 条降级 advisory，25 条被静默跳过。本轮新造的 money-path invariant（I-32 / I-49 / I-52 /
   I-53 / I-54）**全部在不阻塞的那 53 条里**。而且那份「只许缩不许扩」的白名单是**纯 prose**：
   我往里加 `"I-59"` 和 `"I-8"`（十条阻塞项里的两条），core 包全绿。

3. **两条「破坏性变更必须落文档」的门禁在 CI 里一条都不可能红。**
   `TestAPISurface_BreakingChangesAreDocumented` 拿工作树的快照 diff `HEAD` 的快照——作者一提交
   两边就相等，而**提交后的状态正是 CI 唯一见到的状态**。实测：改导出签名 + 重生成快照 + 提交，
   不写 `BREAKING.md`，门禁 PASS。另一条 `TestChangelogListsBreakingGoAPIChanges` 依赖
   `git describe --tags`，而 `.github/workflows/` 全仓**没有任何 `fetch-depth` / `fetch-tags`**，
   `actions/checkout` 默认 `--depth 1` 无 tag——我用 `git clone --depth 1` 实测
   `git describe --tags` 直接 `fatal: No names found`，即该门禁在 CI 恒 `t.Skip`。

第二类系统性问题：**新门禁大量用「单行正则 / 子串计数」实现语义判据**，于是
「换个写法就隐身」成了通用逃逸路径——多行 `CASE`（sign authority）、`let {` 代替 `const {`
（web query-consumption）、`//go:build` 不在第一行（CI 门禁）、大写 `PG_ADVISORY_LOCK`
（advisory lock）、把 `aria-label` 写进注释（hardening ratchet）、把 `.MutGauge(` 写进注释
（emission coverage）、两行拆开的 `GRANT ledger_owner` / `TO x`（migration elevation）。
这些不是刁钻构造：**多行 CASE 是 SQL 的自然排版，license 头在 build 约束上方是 Go 的常规写法。**

---

## Gate × Mutation 结果表

「红」= 门禁按预期拦住（好）。「绿」= 门禁本该抓到却没抓到（盲区）。

### A. INVARIANTS pin 门禁（`core/invariants_pins_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M4 | I-32 的**全部** Pinned by bullet 换成 `` `core.TestIsUserAccount` `` | **绿**。`PinsReferenceEnforcedSymbols` 打出 advisory 行（I-32 在白名单）→ 不阻塞 |
| M4b | 同样操作用在 I-50（**不在**白名单） | **绿**。I-50 的 Enforced by 没有 backtick 化的可解析符号 → 整节被 `len(leaves)==0 continue` 静默跳过 |
| M4c | **63 条 invariant 全部**换成同一条无关 pin | **10 条红 / 28 条 advisory / 25 条静默跳过**。红的是 I-8 I-10 I-17 I-29 I-33 I-47 I-55 I-56 I-59 I-63 |
| M5 | `citationStyleGapInvariants` 加 `"I-59"` + `"I-8"`（十条阻塞项里的两条） | **绿**。全仓无任何测试断言这张白名单的内容或大小；「只许缩」是注释里的 GOVERNANCE 段，不是机器检查 |
| — | 空 Pinned by / 不可解析 pin | lead 已实测红（`d2940b7` 退回 → `9c7d249`），我不重复；我读码确认 `pinnedBlockHasResolvableCitation` 覆盖这两形态 |

### B. Go 导出面 / 破坏性变更门禁（`api_surface_test.go`、`changelog_breaking_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M10a | `ledger.go:1174` `Service.Ping` 加变参 `_ ...string`（对调用方源码兼容，快照里是 CHANGED），**不重生成快照** | **红** — `TestAPISurface_MatchesSnapshot` |
| M10b | 同上，`UPDATE_API_SURFACE=1` 重生成快照，**不改 `BREAKING.md`**，未提交 | **红** — `BreakingChangesAreDocumented` 点名 `ledger.Service.Ping` |
| M10c | 同上，**git commit 之后**再跑 | **绿**。工作树快照 == `HEAD` 快照 → delta 为空。CI 只见提交后状态 → 该门禁在 CI 结构性恒绿 |
| M10d | 同一提交跑 changelog 门禁 | **红**（`ledger.go` 的方法签名在其 scope 内） |
| M11 | `core.Metrics` 新增接口方法 `MutGauge(float64)`（+ NoopMetrics + Prometheus 实现） | changelog 门禁**绿** — `exportedSymbols` 只记 `TypeSpec` 名字（signature 恒为 `"type"`），**接口方法集变化对它不可见**，而接口方法正是该门禁 doc comment 自陈的动因（`core.Metrics` 32 个方法） |
| M23 | `git clone --depth 1 file://…` 后跑 `git describe --tags --abbrev=0 --match 'v[0-9]*'` | `fatal: No names found` → changelog 门禁 `t.Skip`。`.github/workflows/` 无 `fetch-depth`/`fetch-tags`（grep = 0） |

### C. CI 配置门禁（`core/ci_workflow_gates_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M6a | 新文件 `chains/evm/mut_nightly_test.go`，第 1 行 `//go:build nightly` | **红** |
| M6b | 同文件，`//go:build nightly` 上方加**一行注释**（Go 合法，license 头的常规写法） | **绿**。`TestCustomBuildTagsAppearInCIOrMakefile` 只读 `strings.SplitN(src,"\n",2)[0]` |
| M7 | 把 `go-verify.yml` 里根模块**整个 `go test -race … ./...` step** 换成 `echo skipped` | **绿**。三条 CI 门禁 + core 全包绿。F-M6 的修法把两个 caller 统一指向同一文件（形状），但**没有任何门禁管那个共享文件的 step 内容**（语义） |

### D. 迁移 / 权限 / schema 门禁（`postgres/`）

| # | mutation | 结果 |
|---|---|---|
| M1a | 023 追加单行 `GRANT ledger_owner TO current_user;` | **红** |
| M1b | 同一语句**拆成两行**（`GRANT ledger_owner` / `  TO current_user;`） | **绿**。`TestMigrationsDoNotManageLedgerOwnerMembership` 逐行 `re.MatchString(line)`，跨行语句隐身 |
| M16 | migration 024：`journals` 加 `mut_fee_amount BIGINT` + `mut_tip_amount TEXT` + `mut_rate_vector NUMERIC(20,8)[]`，全 NOT NULL | **绿**。`TestSchema_NumericColumnsAreExactly30_18` 只扫 `data_type='numeric'`；`bigint`/`text`/`ARRAY` 三种金额列都在population 之外——包括发现里点名的那个 `(20,8)`，只要包成数组就隐身 |
| M17 | 同一 migration 换成 `NUMERIC(20,8)` 标量 + `DOUBLE PRECISION` + 一个 nullable `TEXT` | **红 ×3**（precision / float / I-7 nullable 三条各自命中，报名到列） |
| M18a | migration 024 `CREATE TABLE mut_probe(...)`，不 grant、不 resweep | **红 ×3**（ownership 关系+序列、grant coverage 未分类、序列 grant） |
| M18b | 新配置表 `mut_fee_rules(code, bps NUMERIC(30,18))`，**给 ledger_app SELECT/INSERT/UPDATE、调 resweep、在 `grant_coverage_test.go` 的 `reviewed` 里加一行、既不加 guard trigger 也不加 audit trigger** | **绿**。`TestPartialGuardTablesAreAudited` 的 population 是「带非 blanket 的 BEFORE UPDATE row trigger 的表」——**完全不加 guard 的表让所有 UPDATE 无痕通过，却在 population 之外**。I-58 的「guard 放行的表都要有取证轨迹」可以靠「不加 guard」满足 |
| M19 | migration 024 建 `SCHEMA mut_ns` + `SECURITY DEFINER FUNCTION mut_ns.mut_escalate()`（内容 `GRANT ALL ON ALL TABLES IN SCHEMA public TO ledger_app`），`GRANT EXECUTE` 给 ledger_app，调 resweep | **绿**。`TestObjectOwnership_*` / `TestFunctionACL*` / `TestLedgerAppIsLeastPrivilege` / `TestRoleAttributes` 全部 `WHERE n.nspname='public'`。ledger_app 调一次这个函数就拿到全库 DML |
| M20a | `holder.sql` 加 `SELECT pg_advisory_lock(...)` | **红** |
| M20b | 同上改成 `SELECT PG_ADVISORY_LOCK(...)`（SQL 大小写不敏感，执行完全一致） | **绿**。`blockingSessionLock` 正则没有 `(?i)`（对比：migration elevation 门禁有 `(?i)`，sign authority 先 `ToLower`——就这一条漏了） |

### E. 符号权威门禁（`postgres/sign_authority_gate_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M2a | `holder.sql` 加单行 `SUM(CASE WHEN je.entry_type='debit' THEN je.amount ELSE -je.amount END)` | **红**（报错文案还专门写了 "Do not skip this by making the line unmatchable"） |
| M2b | 同一表达式**换行排版**（`CASE je.entry_type` / `WHEN 'debit' THEN je.amount` / `ELSE -je.amount` / `END` 各一行） | **绿**。`isBareEntryTypeArithmetic` 要求 `entry_type` + `amount` + `case` 出现在**同一行**。多行 CASE 是 SQL 的自然排版，也就是说门禁警告的那件事本身是默认逃逸路径 |
| M2c | 同一新 query **完全不带** `balance_role NOT IN ('','memo')` 过滤 | **绿**。`HolderVisibleMoneyPredicateHasOneSpelling` 只钉「已存在的拷贝拼法一致 + 每文件条数」，**加一条忘了这个谓词的新 holder 查询**（A-M3 的原始形态）不在它的判据里 |
| M3 | `postgres/rollup_adapter.go` 加 `func mutSignedAmount(side core.NormalSide, …) { if string(side) == "debit" { … } }` | **绿**。`normalSideBranchRE` 只匹配常量标识符 `core.NormalSideDebit/Credit`；`type NormalSide string`，所以按字面值比较是一个完整可用的第二实现 |
| — | 附带：mutation P5 我在 `postgres/sqlcgen/reconcile.sql.go` 里重新引入了裸 CASE，符号门禁两条都不扫 `sqlcgen/`（Go 侧 SkipDir、SQL 侧只扫 `sql/queries/*.sql`）。只有 CI 的 `sqlc diff` 能间接兜住 | 读码 |

### F. OpenAPI 契约门禁（`server/openapi_{contract,params,types}_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M8 | `GET /snapshots` 的 `currency_uid`：`required: true → false`，`schema` 从 `{string, uuid}` 改成 `{integer, int64}`；`start` 也改成 `required: false` | **绿**。`ParamsMatchGoHandlers` 只比**名字集合**；`specParam.required` 被解析进结构体后**从未被断言**；`EveryParamHasTypedSchema` 只要求 `type` 键**存在**，不与 Go 侧比对。这就是 H-M1 那个「spec 生成的 client 每次调用都 400」的第二形态 |
| M9a | `Currency.code` 加 `format: date-time`（Go 侧是普通 `string`） | **绿**。`format` 只在 Go 类型是 `time.Time` 时校验；反方向（spec 声称一个 Go 类型产不出的 format）无检查——**正是 H-M2 `Booking.expires_at` 的原始形态**，可以静默回归 |
| M9b | `Currency.name` 的 `type: string` 整个删掉，只留 `description` | **绿**。`assertSchemaMatchesGoType` 按 `primaryType` 分支，无 type 又无 properties 的属性穿过所有 case，什么都不查。parameters 侧有 `EveryParamHasTypedSchema`，**schema 属性侧没有对应门禁** |
| M9c | `Currency.code` 改成 `type: integer`（控制组） | **红** |
| M24 | `server.currencyResponse` 加 `MutSecretHint string \`json:"mut_secret_hint"\``，openapi 不改 | **红**（H-m11 的响应双向确实成立，`CurrencyEnvelope` / `CurrencyListEnvelope` 各自命中） |

### G. 路由鉴权门禁（`server/route_authorization_gate_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M21 | `POST /api/v1/templates` 从 admin group 挪到 write group | **绿**（`server` 全包） |
| M22 | `PUT /api/v1/accounts/{holder}/policy` 从 admin group 挪到 write group | **绿**（`server` + `core` + 根包全绿） |
| M25 | `routes.go:150` 把 admin group 的 `requireScope(ScopeAdmin)` 改成 `requireScope(ScopeRead)` | 新门禁**绿**；被**旧的** `TestAuth_ScopeEnforcement`（`server_test.go:2003`）抓到——但那是手写抽样（只覆盖 `/journals`、`/classifications`、`/periods/close`、`/balances/*`），M21/M22 动的两条都不在抽样里 |

### H. 文档可运行门禁（`readme_runnable_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M13 | README 第 13 个 ```go 块（`svc.Templates().CreateTemplate` 配方）的第二条腿 `AmountKey` 从 `"amount"` 改成 `"total"`——编译过、运行必错（渲染时取不到该 key） | **绿**。README 有 24 个 ```go 块，`extractREADMEGoBlock` 用**字面 anchor** 只跑其中 5 个；另外 19 个没有任何运行期门禁。本仓自己偏爱的「枚举 + 强制分类 fail-closed」模式（`grant_coverage` / sign authority）没有用在这里 |

### I. 可观测性发射覆盖（`observability/emission_coverage_test.go`）

| # | mutation | 结果 |
|---|---|---|
| M11c | `core.Metrics` 新增 `MutGauge`，无任何调用点 | **红** |
| M11d | 在 `postgres/rollup_adapter.go` 追加**一段注释**，里面出现 `s.metrics.MutGauge(1)` | **绿**。`scanNonTestGoSource` 把整包源码拼成一个字符串跑 `\.Name\(` 正则，**注释和字符串字面量一并计入**。该文件 doc comment 自陈「false-positive 方向是安全的（只会让本该失败的测试通过）」——对一个**覆盖率**门禁来说，这正是危险方向 |
| M12 | `postgres/ledger_store.go:1005` 唯一的 `s.metrics.JournalPosted(...)` 调用注释掉（文本仍在） | 发射门禁**绿**；被行为 pin `ledger_metrics_wiring_test.go` 抓到（**红**）→ 该条金额路径有真实兜底 |
| — | 普查：`core.Metrics` 的 41 个方法里，有 **4 个在任何 `_test.go` 里零出现**（除反射式 embed 测试与 observability 自身）：`DepositReorgDetected` / `DepositReviewRequired` / `RegistrationRescanFailed` / `SweepUnattributed`。这四个的生产调用点删掉后，只有发射门禁可能注意到，而它可以被一行注释满足 | 读码 + 机械普查 |

### J. web 门禁（`web/packages/ledger-react/test/`）

| # | mutation | 结果 |
|---|---|---|
| M14a | `CurrenciesPage.tsx` 改成 `const currencies = Array.isArray(useCurrencies().data) ? …`（内联调用），删掉 `<ErrorState>` 分支 | query-consumption **绿**，但一条 page-render 测试因重复调用报错 → 判为「无效 mutation」，重做 |
| M14b | 同一页：`const { data, isLoading, isError, refetch }` → **`let { data, isLoading }`**，删掉整个 `<ErrorState>` 分支和它的 import | **绿**。39 个测试文件 / 203 个用例全过。`findViolations` 的 Case 1 正则是 `const\s*\{`，`let {` 隐身；Case 2 只在绑定到裸标识符时生效。失败的请求现在渲染成「No currencies yet」——正是 J-1/J-2/J-3 的形态 |
| M15 | `TemplatesPage.tsx`（J-12 专门加固过的那页）删掉 9 个 `aria-label` 属性里的 8 个，再加一行注释把 `aria-label` 这个词重复 8 次 | **绿**。`censusFor` 数的是**文件文本里的子串出现次数**，注释一并计入；per-skin floor 和新加的 `SHADCN_MIN_RATIO_OF_HEROUI` 都过。整套 web 203 用例全过 |
| — | `styles.test.ts` / `build-artifacts.test.ts` 读 `dist/`：缺 `dist` 时 throw（fail-closed，好），但 `dist` **过期**时它们校验的是**上一次构建的产物**（fail-open）。`npm test` 不产 `dist`，这一条只能靠 `web/CLAUDE.md` 的「先 build」口头约定 | 读码（未 build，见「未能验证」） |

### K. 「拆掉被测实现仍绿」——10 条最靠近钱的新 pin 实测

| # | pin | 我拆掉的实现 | 结果 |
|---|---|---|---|
| P1 | `postgres.TestListJournals_CursorOrderIsNewestFirst` | `sqlcgen/journals.sql.go` 的 `listJournalsCursor` 改回 `id > cursor ORDER BY id ASC` | **红** |
| P2 | `postgres.TestCheckpointDimensions_RejectOrphanFK` | migration 022 删掉 `fk_checkpoints_classification` | **红** |
| P3 | `core.TestFreeformFieldLimits` | `core/limits.go` `validateFreeformFields` 首行 `return nil` | **红**（含 bookings/transitions 那条子用例） |
| P4 | `postgres.TestJournalEntries_RejectsNaNAmount` | migration 018 的 `chk_journal_entries_amount_not_nan` 改成 `CHECK (true)` | **红**（注：实际是被 balance trigger 先拦下，pin 因断言约束名而红——结论成立，但它红的原因不是它以为的那个） |
| P5 | `postgres.TestSettlementNettingViolations_ReportsTheSameSignAsGetBalance` | `sqlcgen/reconcile.sql.go:470` 的 `ledger_signed_amount(...)` 换回裸 `CASE WHEN entry_type='debit'` | **红**，报文直接给出 `net=-40 vs 40` |
| P6 | `postgres.TestPostJournal_IdempotentReplayNeverInsertsUnsignedRow` | `ledger_store.go:362` 去掉 `replay: true`（保留 status 取自存量行） | **绿** |
| P6b | 同上 | 再把 `ledger_store.go:1237` 的 fail-closed `if auth.replay {` 改成 `if false {` | **绿**，且 **`go test ./postgres/` 全包绿（133s）**。m-6 修复的**两个**机制（显式 replay 标记 + 插入路径 fail-closed 拒绝）都可以拆掉；pin 断言的是修复前就已经成立的**结果**（没有新行、status 不是 unsigned），不是修复引入的**机制** |
| P7 | `postgres.TestListHolderTransactions_PageCostDoesNotGrowWithTheTable` | migration 023 删掉 `idx_entries_account_journal` | **红**（`page one read 24090 rows`） |
| P8 | `postgres.TestMetadata_DedupeIsTheUniqueConstraint` | 未实跑 | 读码：断言的是约束行为（重复 create → `ErrConflict`），形状正确 |
| P9 | `server.TestRouteAuthorization_EveryRouteIsGuarded` | 见 G 段 M21 / M22 | **绿** |
| P10 | `server.TestOpenAPIContract_ResponseTypesMatchGoStructs` | 见 F 段 M24 | **红** |

---

## 发现

### Critical

**C-1 · 路由鉴权门禁看不见 scope 降权，`PUT /accounts/{holder}/policy` 降到 write scope 全绿**
- 位置：`server/route_authorization_gate_test.go:80-85`（`chainContains(chain, "requireScope") || chainContains(chain, "requireCapability")`）；`server/routes.go:150` admin group；`server/server_test.go:1975-2011`（手写 4 条抽样）
- 实跑：M22（`PUT /accounts/{holder}/policy` 挪进 write group）→ `server` + `core` + 根包全绿；M21（`POST /templates` 同样操作）→ `server` 全包绿
- 为什么要紧：`account_policies` 是全仓**唯一** DB 强制的冻结 / `min_balance` 底线
  （`postgres/audit_trail_guard_test.go:88-101` 自陈；`postgres/account_policy_enforce.go` 是执行点）。
  一个 write-scope key 拿到它就能解冻账户、把最低余额下调，然后提现。
- 门禁为什么没拦：新门禁问的是「这条路由**有没有**鉴权中间件」，不是「是**哪一级**」；
  能看出级别的 `TestAuth_ScopeEnforcement` 是手写清单，12 条 scope 路由里抽了 4 条，
  8 条 admin 路由（journal-types ×2、templates ×2、currencies ×2、accounts policy、deposit tolerance）没抽到。
- 修法：门禁应从 chi.Walk 拿到的中间件里**取出 scope 值**（`requireScope` 返回的闭包可用一张
  route→期望 scope 的表来钉，那张表是唯一该手写的东西，其余从 router 派生），
  或者反过来——`TestAuth_ScopeEnforcement` 改成对 `chi.Walk` 出来的**全部**路由做三 key 矩阵，
  用 `grant_coverage` 的「未分类直接 fail」形状。

**C-2 · 「pin 必须真的碰它声称的机制」这条门禁，对 63 条 invariant 只在 10 条上阻塞；本轮新造的 money-path invariant 全在不阻塞的那一侧；且它的豁免白名单可以静默扩大**
- 位置：`core/invariants_pins_test.go:645-654`（`citationStyleGapInvariants`，32 项）、`:663-666`
  （`len(leaves)==0 → continue`，静默跳过）、`:686-694`（advisory 走 `t.Log`）
- 实跑：M4（I-32 六条 pin → `core.TestIsUserAccount`，四条门禁全绿，只多一行 advisory 日志）；
  M4c（63 条全换 → 红 10 / advisory 28 / 静默跳过 25）；M5（白名单加 `"I-59"`+`"I-8"` → core 全绿）
- 名单事实：不阻塞的 53 条里包含 **I-32**（提现验签闸）、**I-49**（`min(V,E)`）、**I-52 / I-53**（onchain 幂等键）、
  **I-54**，即 Wave 1 三条 Critical 的落地 invariant；还包含 I-26~I-31 整个防篡改族。
- 门禁自陈「一次跑出 146 处不匹配、不该一次爆掉全文档」是合理的工程判断——**问题在收尾**：
  白名单的「只许缩不许扩」写成了注释里的 GOVERNANCE 段（`working-agreements.md` §5 的反面教材），
  而 25 条「Enforced by 里没有 backtick 化符号 → 整节跳过」的情况**连 advisory 都不打**，
  从输出上完全看不出这一节没被检查（§3：未运行 ≠ 通过）。
- 修法（按代价排序）：① 白名单加一条锁定测试：断言其内容等于一个硬编码的期望集合，
  且**长度只能减不能增**（用 `len(citationStyleGapInvariants) <= 32` + 内容 diff 报名）；
  ② 静默跳过的 25 条改成 advisory + 计数断言（`skipped <= N`，N 只降不升）；
  ③ Wave 3 收口时优先把 I-32 / I-49 / I-52 / I-53 / I-54 的 Enforced-by 符号 backtick 化并出白名单。

**C-3 · 两条破坏性变更文档门禁在 CI 里都不可能红**
- 位置：`api_surface_test.go:121-164`（diff 对象是 `HEAD` 的快照 vs 工作树的快照）；
  `changelog_breaking_test.go:41-44`（`lastReleaseTag()` 失败即 `t.Skipf`）；
  `.github/workflows/go-verify.yml`（`actions/checkout@v7`，**无** `fetch-depth` / `fetch-tags`）
- 实跑：M10c（改签名 + 重生成快照 + commit → PASS）；M23（`git clone --depth 1` → `git describe --tags` = `fatal: No names found`）
- 合起来的后果：**接口方法签名/方法集的破坏性变更可以带着全绿 CI 落地且无任何文档条目**——
  M11 证明 changelog 门禁对接口方法结构性不可见（只记 `TypeSpec` 名字），
  M10c 证明 `BREAKING.md` 要求只在「工作树已改、还没提交」这一个瞬间存在。
  而这两条门禁的动因原文就是 `core.Metrics` 的方法增长（`api_surface_test.go:6-11`）。
- 修法：`BreakingChangesAreDocumented` 的 baseline 换成**上一个 release tag**（和 changelog 门禁同源）
  或 `origin/main`，不是 `HEAD`；CI 的 checkout 加 `fetch-depth: 0`（或 `fetch-tags: true`），
  并且 changelog 门禁的 `t.Skipf` 改成「在 CI 环境（`CI=true`）下 skip 视为失败」——
  否则「gate 跑不了」还是等于「gate 通过」。

### Major

**M-1 · 共享 verify workflow 的**内容**无门禁**：F-M6 把 `ci.yml` / `go-release.yml` 统一 `uses:`
同一个文件（形状），`TestReleaseWorkflowUsesSameVerifyAsGoVerify` 只比对两个 `uses:` 字符串相等。
实跑 M7：删掉 `go-verify.yml` 里根模块**整个 `go test` step**，三条 CI 门禁 + core 全绿。
`TestGoWorkModulesAllCoveredByGoVerify` 也只要求某个 step 带对应的 `working-directory:`，
不管那个 step 跑什么（`echo` 也算覆盖）。修法：给 `go-verify.yml` 的 step 集合加一份快照/清单断言
（至少：每个 module × {vet, lint, build, test} 的存在性从 `go.work` 派生），
或断言每个 `working-directory` 步骤的 `run` 以 `go test`/`go build`/`go vet` 开头。

**M-2 · sign authority SQL 门禁的判据是「同一行」，多行 `CASE` 是默认逃逸路径**：
实跑 M2b。`isBareEntryTypeArithmetic` 要求 `entry_type`+`amount`+`case` 同行。
`balance_trends.sql` 那个「第 18 处实现、唯一方向错的那个」如果按常规换行排版写，这个门禁一开始就抓不到。
门禁自己的报错文案写着「Do not skip this by making the line unmatchable」——换行就是。
修法：按 `-- name:` 切块，在**整块**文本（去注释、`ToLower`、压平空白）上判 `entry_type` + `case` 共现。

**M-3 · holder-visible 谓词门禁钉的是「已有拷贝的拼法」，不是「该有谓词的地方有没有」**：
实跑 M2c。往 `holder.sql` 加一条按 holder 聚合的新 query、完全不写 `balance_role NOT IN ('','memo')` → 绿。
A-M3 的原始事故（四份拷贝改了一份）被钉住了；**第五份忘写**没有被钉住，而那正是同一个错误的下一次形态。
修法：把「holder 可见金额投影」这件事收成一个 SQL view / 函数（一个实现，其余查询 join 它），
或至少给门禁加一条：`holder.sql` 里任何 `FROM journal_entries` 的聚合 query 必须命中 canonical 谓词或显式豁免。

**M-4 · sign authority Go 门禁只认常量标识符，不认字面值**：实跑 M3。
`type NormalSide string`，所以 `if string(side) == "debit"` 是一个完整可用的第二实现，
`normalSideBranchRE` 看不见。同类：把常量存进局部变量再比较也隐身。
另：Go 侧豁免是**整文件**粒度（`service/rollup.go` / `service/reconcile.go` / `core/types.go`），
这三个文件里新写的任何符号分支都被预先批准。
修法：正则补 `NormalSide` 类型的字面值比较（`== "debit"` / `== "credit"` 且同表达式涉及 normal_side）；
豁免粒度降到函数或行（带 `//nolint`-style 标记）。

**M-5 · I-6 的 schema 半边只盯 `data_type='numeric'`，金额列换类型即隐身**：实跑 M16。
`BIGINT`（wei 风格）、`TEXT`（字符串金额）、`NUMERIC(20,8)[]`（数组，`data_type='ARRAY'`）三种全绿。
`financial.md` 的红线是「金额禁 float」，但「金额禁非 NUMERIC(30,18)」在这三个方向上没有门禁。
修法：加一条按**列名**派生的检查（`%amount%|%balance%|%total_debit%|%total_credit%|%bps%|%rate%|%fee%`
必须是 `numeric(30,18)`），并把 `ARRAY` 的 element type 展开（`information_schema.element_types`）。
`information_schema.columns` 也不含 materialized view 的列——今天没有 matview，加了要补。

**M-6 · 审计 trigger 派生门禁可以靠「不加 guard」绕过**：实跑 M18b。
`TestPartialGuardTablesAreAudited` 的 population = 「带非 blanket 的 BEFORE UPDATE row trigger 的表」。
一张新配置表**完全不加 guard**（全量 UPDATE 放行、零取证轨迹）不在 population 里，
而 `grant_coverage_test.go` 的四分类里 `reviewed` 就是这条路的入口——加一行即通过。
006 号 migration 手工点名的四张配置表被审计了；**今天新加的第五张配置表只需要一行 `reviewed` 条目**。
修法：population 改成从**权限**派生——「ledger_app 持有 UPDATE 且没有 blanket 拒绝 guard」的表
必须要么有 audit trigger、要么在一份带理由的名单里。

**M-7 · ownership / least-privilege 全族门禁只扫 `public` schema**：实跑 M19。
一个建在 `mut_ns` 里、由 migration runner（常见安装下是 superuser）拥有的 `SECURITY DEFINER` 函数，
内容是给 ledger_app 授全库 `ALL`，`GRANT EXECUTE` 给 ledger_app → `TestObjectOwnership_*`、
`TestObjectOwnership_SecurityDefinerFunctionsRunAsLedgerOwner`、`TestFunctionACL*`、
`TestLedgerAppIsLeastPrivilege`、`TestRoleAttributes` 全绿。
I-57 / I-35 的机器检查因此有一个 schema 维度的洞，而它防的正是「函数 owner 能 CREATE OR REPLACE 掉所有 guard」。
修法：把 `nspname='public'` 换成「除 `pg_catalog` / `information_schema` 之外的所有 schema」，
并加一条「public 之外不得存在非系统 schema」或「新 schema 必须登记」的断言。

**M-8 · openapi 参数门禁只比名字**：实跑 M8。`required` 解析了但从不断言，`schema.type` 只查存在。
一个 `required: false` + `type: integer` 的 `currency_uid`（handler 硬必填、按 uuid 字符串用）全绿——
和 H-M1 原始事故（spec 生成的 client 每次 400）同一个用户可见结果。
修法：`required` 与「handler 缺失即 400」的行为对齐（至少：spec 侧 `required` 与
`openapi_params_test` 已经解析出来的那个字段做一次显式断言 + 一份 route→必填集的派生源），
参数 `schema.type` 参照 `openapi_types_test.go` 的 `assertSchemaMatchesGoType` 与 Go 侧解析方式比对。

**M-9 · openapi schema 属性可以「无 type」隐身，`format` 只单向校验**：实跑 M9a / M9b。
属性写成 `{description: …}`（无 type、无 properties）→ 穿过 `assertSchemaMatchesGoType` 所有 case，零检查；
Go `string` 字段标 `format: date-time` → 绿，而这正是 H-M2 `Booking.expires_at` 的原始形态。
修法：给 schema 属性补一条 `EveryPropertyHasTypedSchema`（parameters 侧已有对应门禁）；
`format` 建立双向表（`date-time`/`date` ⇒ Go 必须是 `time.Time`；`uuid` ⇒ 走 uid 约定；等）。

**M-10 · emission coverage 是文本扫描，注释即可满足；且 4 个 metrics 方法零测试引用**：
实跑 M11d（注释里写 `.MutGauge(1)` → 绿）、M12（注释掉唯一调用点 → 发射门禁绿，靠行为 pin 兜住）。
`DepositReorgDetected` / `DepositReviewRequired` / `RegistrationRescanFailed` / `SweepUnattributed`
在任何 `_test.go` 里零出现——这四条只有发射门禁一层，而那层可被一行注释满足。
修法：改用 `go/ast` 只统计 `*ast.CallExpr` 的 `SelectorExpr`（本仓已有三处同款 AST 扫描可复用），
并给这四个方法各补一条行为 pin（`ledger_metrics_wiring_test.go` 是现成的形状）。

**M-11 · migration 自提权门禁是逐行正则**：实跑 M1b。`GRANT ledger_owner` / `  TO x;` 两行拆开即隐身。
同族更宽的洞：该正则只认 `ledger_owner` 的 GRANT/REVOKE，一条 `ALTER ROLE ledger_app SUPERUSER`
不在它的判据里（今天由 `TestRoleAttributes` 在 migration 跑完后兜住，但那是另一条门禁的顺带效果，
不是这条门禁的承诺）。修法：按语句（以 `;` 切分、剥注释）而非按行匹配；
正则扩到 `ALTER ROLE … (SUPERUSER|CREATEROLE|CREATEDB|REPLICATION|BYPASSRLS)` 与 `GRANT … TO ledger_app`。

**M-12 · README 24 个 go 块只跑 5 个，其余 19 个无运行期门禁**：实跑 M13。
把 `CreateTemplate` 配方改成两条腿读不同 `AmountKey`（编译过、运行必错）→ 全绿。
E-M2 修的是「其中一个块」，不是「块这一类」。
修法：枚举全部 ```go 块，每个块必须要么被某个 anchor 覆盖、要么在一份带理由的豁免表里
（本仓 `grant_coverage` 的 fail-closed 形状），非 DB 类的块至少做编译门禁。
另：`runGoProgram` 只看退出码，README 里若出现 `if err != nil { fmt.Println(err) }` 这种吞错写法，
运行失败也是 exit 0。

**M-13 · web query-consumption 门禁的两条 case 都可以用 `let {` 绕过**：实跑 M14b。
`CurrenciesPage` 改 `let { data, isLoading }` 并删掉整个 `<ErrorState>` 分支 → 39 文件 203 用例全过，
失败请求渲染成「No currencies yet」。skin-parity 按构造看不见（且 `BEHAVIORAL_TOKENS` 有意排除 `isError`）。
另一个层面的洞：门禁只要求 `isError` 这个**标识符在文件里出现过**，不要求它被渲染成错误态——
除 `BalancesPage` 趋势图（J-24 那条行为测试）以外，多数页面的 `isError` 分支没有行为测试。
修法：Case 1 正则加 `(const|let|var)`；再加一条「destructure 了 `isError` 就必须有一处 JSX 消费它」
（或直接对每个 page 做「query 报错 → 期望出现 ErrorState」的行为测试，`BalancesPage` 已有模板）。

**M-14 · hardening ratchet 数的是文本子串，注释一并计入**：实跑 M15。
`TemplatesPage`（J-12 专门加固的那页）删 8 个真 `aria-label`、加一行含 8 次该词的注释 → 全绿。
per-skin floor 和新加的 `SHADCN_MIN_RATIO_OF_HEROUI` 都过。且 census 是**目录总量**，
一页减少可以被另一页增加抵消（per-page 无 floor）。
修法：census 前剥掉注释与字符串字面量；`aria-label` 改数**属性出现**（`aria-label={` / `aria-label="`）；
floor 下沉到 per-page。

### Minor

- **m-1 · `advisory_lock_shape_pin` 正则漏 `(?i)`**（实跑 M20b）。SQL 大小写不敏感，
  `PG_ADVISORY_LOCK(...)` 执行完全一致。同族的另两个门禁都做了大小写归一，只这一条没有。
- **m-2 · 同名门禁两份实现，失败策略相反**：`TestCLAUDEMdFileLayoutPathsExist` 同时存在于
  `claude_md_paths_test.go:16`（`package ledger`，缺 CLAUDE.md → `t.Fatalf`）与
  `readme_docs_consistency_test.go:144`（`package ledger_test`，缺 CLAUDE.md → `t.Skip`）。
  同一个判据两份实现、一个 fail-closed 一个 fail-open，且 `go test -v` 输出里同名两次。
  这正是本轮反复在治的「一个决策一个地方」。建议删掉 `t.Skip` 那份。
- **m-3 · `roles_test.go` 的 SUPERUSER 窗口只修了一半，注释却说修完了**：
  `postgres/roles_test.go:860-864` 取的是**集群迁移锁**，注释（`:850-855`）说这让「本测试与每个并发 Migrate
  互斥」——对；但它**不与 ACL 断言互斥**。实测普查：另有 6 个文件做 42501/permission-denied 断言
  （`audit_trail_guard_test.go`、`journal_entry_id_uniqueness_test.go`、
  `journal_entries_partition_grant_test.go`、`migrate_bootstrap_role_test.go`、
  `role_check_test.go`、`runbook_emergency_recovery_test.go`），**没有一个取这把锁**。
  F 报的假红风险（跨 package 并行时 ledger_app 短暂是 superuser）仍然在，注释里的「Both are fixed」是高估。
- **m-4 · web 读 `dist/` 的门禁在 `dist` 过期时 fail-open**：缺 `dist` 会 throw（好），
  但 `npm test` 不产 `dist`，`dist` 过期时 `styles.test.ts` / `build-artifacts.test.ts` 校验的是上一次构建。
  建议这两个文件在断言前比对 `dist` 与 `src` 的 mtime（或直接在 test setup 里跑 build）。
- **m-5 · sign authority 两条门禁都不扫 `sqlcgen/` 与 `sql/migrations/`**：我在 P5 里正是在
  `sqlcgen/reconcile.sql.go` 重新引入了裸 CASE（门禁不扫），只有 CI 的 `sqlc diff` 能间接兜住；
  migration 里定义的 SQL 函数 / view 若手搓符号也在扫描范围外。
- **m-6 · `journals.sql` 内部注释自相矛盾**：`ListRecentJournals` 的注释（`sql/queries/journals.sql:120-127`）
  说 "ListJournalsCursor above, which walks ASCENDING from a cursor"，而紧邻上方的
  `ListJournalsCursor` 注释与查询本体都已改成 NEWEST FIRST / `ORDER BY id DESC`。
  H-m3 的修复漏改了这一段，读者会得到相反的事实（`feedback_verify_the_original_not_the_retelling`）。
- **m-7 · `TestJournalEntries_RejectsNaNAmount` 红的原因不是它以为的那个**：拆掉
  `chk_journal_entries_amount_not_nan` 后它确实红，但拦下写入的是 balance trigger（SQLSTATE 23514
  "unbalanced entries by currency"），pin 因断言错误文案包含 `not_nan` 而失败。
  即这条 pin 现在同时依赖两个机制，单独看不出 CHECK 是否还在。建议改成直接查
  `pg_constraint` 里该约束存在 + 单腿写入被拒。

### 测试之间互斥的断言

按 lead 的第二个问题逐条查了，**没有找到两条测试钉住严格矛盾行为的实例**。最接近的三个：

1. **m-3**（`roles_test.go` 的 SUPERUSER 窗口 vs 6 个文件的 ACL 断言）——不是断言矛盾，
   是**集群级共享状态上的互斥需求没有被互斥机制覆盖**，窗口期内双方对 `ledger_app` 的属性要求相反。
   这是本轮唯一一条真的「两组测试要求同一份全局状态的相反取值」。
2. **m-2**（同名门禁两份实现，fail-open vs fail-closed）——同一判据的两种失败策略。
3. `service/reconcile_full_test.go:248/281`（无 AuthVerifier 15 条检查 / 有则 16 条）与
   `docs/DR.md:202`（"grown from 13 to 15 checks"）——DR 明说不给准确数，口径可自洽，
   但 lead 在 `1d4cb45` 前后已因这组计数断言红过一次 main；两处硬编码的数字仍分居 test 与 doc，
   建议 DR 侧改为引用而非复述（不列为发现）。

---

## 我没能验证的

- **`actions/checkout@v7` 的默认 fetch 行为**我是从「仓库里没有任何 `fetch-depth`/`fetch-tags`」+
  本机 `git clone --depth 1` 的 `git describe` 失败推出来的（契约 §1.5：不编造）。
  **要闭环需要看一次真实 CI run 的日志**，确认 `TestChangelogListsBreakingGoAPIChanges` 打的是 SKIP。
  我全程只读代码库，没有查 GitHub。
- **`web` 的 `dist/` 门禁（`styles.test.ts` / `build-artifacts.test.ts`）没有 mutation**：
  worktree 里没有 `dist`，跑 build 才能测，判为性价比不足（结论 m-4 是读码）。
- **`postgres/metadata_dedupe_test.go`、`period_barrier_pin_test.go`、`service/reconcile_check1_test.go`、
  `postgres/idempotency_match_internal_test.go`、`postgres/deposit_reorg_store_test.go`、
  `server/{config_coverage,api_doc_examples,error_wire,holder_envelope,readiness,middleware_idempotency_params}_test.go`**
  只读了，没有实跑反转。K 段那 10 条我按「离钱近 + 可单点拆」挑的，不是随机抽样。
- **只有 P6b 一条做了全包 `go test ./postgres/`（133s，无 `-race`）**；其余 mutation 都用了
  `-run` 过滤，所以「绿」的严格含义是「被点名的门禁 + 同包被过滤到的测试都没红」，
  不是「全仓无任何测试会红」。M14b / M15 是例外——web 侧我跑了整套（39 文件 203 用例）；
  M22 跑了 `server` + `core` + 根包全包。
- **`chains/evm` / `anchors/r2` 两个子模块的新门禁一条都没碰**（`reader_test.go`、
  `token_decimals_test.go` 是本轮新增，未评审）。
- **没有验证任何 mutation 的下游真实危害**：C-1 我论证到「write-scope key 能改冻结/最低余额闸」，
  依据是 `routes.go` 的分组和 `account_policy_enforce.go` 的存在，**没有实跑一次
  「write key → PUT policy → 解冻 → 提现」的端到端**。
- **`citationStyleGapInvariants` 的 25 条「静默跳过」我是用 M4c 的输出反推的**
  （63 − 38 检测到 = 25），没有逐条确认每一条的 Enforced-by 为何不可解析。

---

## 工作树状态

全部 mutation 在 `/Users/aaron/projects/_worktrees/ledger/w3-gates`（`wt create ledger w3-gates`）
里进行，每次改一个文件、跑完立即 `git checkout --` 恢复；M10 那次为了证明「提交后门禁恒绿」
在 worktree 里建了一个临时 commit，验证完 `git reset --hard HEAD~1`。
主工作树**全程只读**，本文件是唯一写入。

```
$ git -C /Users/aaron/projects/ledger status --porcelain
?? docs/audits/2026-09-02-deep-audit/w3-review/

$ git -C /Users/aaron/projects/_worktrees/ledger/w3-gates status --porcelain
(空)
$ git -C /Users/aaron/projects/_worktrees/ledger/w3-gates log --oneline -1
676ae7e docs: archive the TODO as an index, note contract status entering Wave 3
```

> worktree 用完请 `wt remove ledger w3-gates`（`hive.md`：不允许「先留着以后再说」）。
> 我保留它是为了 lead 复核时能原地重放任一条 mutation；复核完即可清理。

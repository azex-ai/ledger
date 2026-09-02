# 审计报告 — H · structure-and-contract

> 基线 commit `fd87cae`。只读评审，未修改任何被审文件。
> 纪律沿用 `docs/plans/2026-08-25-financial-audit-contract.md` §1 / §3。
> 本轮无 Bash 权限，所有判定来自源码通读；凡涉及运行时代价的量级一律标 PLAUSIBLE。

## 摘要

六边形边界本身是干净的：`core/` / `presets/` 对 `net/http` / `pgx` / `slog` / `chi` / `bizcode` / `httpx`
零 import（唯一第三方是 `decimal` 与 `x/crypto/sha3`），env 读取只出现在 `server.LoadConfig` 与
`main` 包，`RunInTx` 的克隆边界处理得很扎实。问题全部集中在**契约面**，且呈同一个形状：
上一轮把「Go ↔ openapi」这条链补上了门禁（`server/openapi_contract_test.go`），但门禁只覆盖了
**「$ref 引用的 requestBody/response 的顶层字段名」**这一个切片——**query 参数、字段类型/格式、
inline schema、嵌套对象、出站事件 payload 全部在门禁之外**，而这四类里每一类都已经有实证漂移：
`GET /snapshots` 的三个查询参数全错（照 spec 调必 400）、`Booking.expires_at` 在默认路径上吐空串
而 spec 承诺 `date-time`、`POST /journals/{uid}/reverse` 的 `idempotency_key` 被文档标成 required
而 Go struct 根本不读、出站 webhook 用 `encoding/json` 直接序列化 `core.Event` 绕过了
`pkg/httpx` 那个专门为「不要吐 `+08:00`」而写的 UTC extension。**`docs/api.md`（散文）在三处
query 参数上是对的，而 `docs/openapi.yaml`（唯一被 codegen 消费的机器契约）是错的**——不是含糊，
是 SoT 那一份坏了，且两份文档之间没有任何比对。

schema 侧最要紧的一条是**分区键与查询模式不匹配**：`journal_entries` 按 `created_at` 月分区，
而余额读路径（`ListComputedBalancesForHolders`，`GetBalance`/`GetBalances`/`BatchGetBalances`/
breakdown 的共同底座）只有 `id > watermark` 谓词、没有任何 `created_at` 条件，因此**永远无法分区裁剪**；
更要命的是它的 `populated` CTE 对该 holder 的**全历史** entries 做 `DISTINCT`，把 checkpoint+delta
本来要省掉的那部分成本又加了回来。

演化能力上有一个结构性空洞：**Go 导出面没有任何破坏性变更检测**。`readme_api_surface_test.go`
只管 `*Service` 方法是否出现在 README 表里，`core` 的接口与类型（真正的消费契约）无人看守——
`core.Metrics` 已有 32 个方法，每加一个信号都是一次对非嵌套实现方的静默 API 破坏。

## 发现

---

### [Major] openapi 的 query / path 参数完全在契约门禁之外，已有三处确诊漂移；`GET /snapshots` 按 spec 调用必然 400

- **位置**：
  - `docs/openapi.yaml:295-298`（`/entries` 声明 `name: currency, required: true`）vs `server/handler_journals.go:439-443`（`currencyUID := q.Get("currency_uid")`，缺失即 400）
  - `docs/openapi.yaml:312-326`（`/balances/{holder}` 声明 **零** query 参数，summary 写 "All balances for one holder across currencies"）vs `server/handler_balances.go:31-35`（`currency_uid` 缺失即 400）
  - `docs/openapi.yaml:1250-1264`（`/snapshots` 声明 `holder` / `currency`(required) / `from` / `to`）vs `server/handler_system.go:186-212`（读的是 `holder` / `currency_uid` / `start` / `end`，且 `start`/`end` 缺失即 400）
  - 门禁范围证据：`server/openapi_contract_test.go:259-380` 三个注册表只覆盖 requestBody / response 的 schema；`:688-743`（`Every2xxHasSchema`）与 `:789-819`（`EveryRouteIsDocumented`）只看 method+path，**全文件没有一处读取 `parameters`**
- **判定**：CONFIRMED
- **失效场景**：消费方用 `openapi-typescript`（或任何按 spec 手写的客户端）调 `GET /snapshots?holder=1&currency=<uid>&from=2026-08-01&to=2026-08-31`：`currency` 不被读取 → handler 走到 `currencyUID == ""` → `400 {"code":10001,...,"text":"..."} `；即使补上 `currency_uid`，`from`/`to` 仍然不被读取，`start`/`end` 为空 → 再次 400。三个参数名全错，且 spec 把 `currency` 标成 `required: true`，等于**照文档写的客户端 100% 不可用**。`/balances/{holder}` 反向：spec 说不用带参数、summary 明说「across currencies」，实际必须带 `currency_uid`，一带就只返回一个币种。
- **最小复现**：`npx openapi-typescript docs/openapi.yaml` 生成类型，按生成的 `paths['/snapshots']['get']['parameters']['query']` 构造请求 → 400。
- **为什么现有机制没拦住**：`openapi_contract_test.go` 的四个契约检查都以 `components.schemas` 或 `paths.<p>.<m>.responses` 为输入，`parameters` 数组从未被读过。`docs/api.md:521 / 531 / 846` 三处的散文**是对的**（`currency_uid`、`start`、`end`），说明代码行为一直没变、是 openapi 单方面写错的——**而没有任何门禁比对 `docs/api.md` 与 `docs/openapi.yaml`**，两份文档可以长期互相矛盾。

---

### [Major] 契约门禁只比字段名、不比类型与 format；`Booking.expires_at` 在默认路径上就吐 `""` 而 spec 承诺 `date-time`

- **位置**：`server/handler_bookings.go:95-97`（`if !op.ExpiresAt.IsZero() { resp.ExpiresAt = ... }`，`bookingResponse.ExpiresAt string json:"expires_at"`，**无 `omitempty`**，见 `:55`）；`docs/openapi.yaml:2286`（`Booking.required` 含 `expires_at`）+ `:2303`（`expires_at: { $ref: Timestamp }` → `type: string, format: date-time`）；门禁只取字段名：`server/openapi_contract_test.go:178-196`（`goJSONFieldNames` 只解析 tag 名）、`:235-248`（`assertSpecSubsetOfGo` 只做 key 集合包含）
- **判定**：CONFIRMED
- **失效场景**：`POST /bookings` 不传 `expires_at`（`createBookingRequest.ExpiresAt` 是可选的，`handler_bookings.go:25`），booking 的 `expires_at` 落库为 `'epoch'`（`001_baseline.up.sql:550` 的 DEFAULT），读回时 `op.ExpiresAt.IsZero()` 为真 → 响应体是 `"expires_at": ""`。任何按 spec 生成客户端并把该字段喂给日期解析器（`new Date(x)` → `Invalid Date`、Zod `z.string().datetime()` → 抛错、Go `time.Parse(time.RFC3339, "")` → error）都会失败，而这是**不设过期时间的普通 booking 的常规路径**，不是边角。
- **最小复现**：`POST /api/v1/bookings {"classification_code":"deposit","account_holder":1,"currency_uid":"...","amount":"1","idempotency_key":"k"}` → `GET /api/v1/bookings/{uid}` → `data.expires_at === ""`。
- **为什么现有机制没拦住**：这正是门禁的设计边界——它比对的是 key 集合，`expires_at` 两边都有，所以绿。同一个盲区还覆盖：`Booking.metadata` spec 写 `additionalProperties: true` 而 Go 是 `map[string]string`（spec 比代码宽）、`ReserveInput.expires_in_sec` 若某天从 `int64` 改成 `string` 也不会红——上一轮那个 `expires_in`/`expires_in_sec` bug 是**名字 + 类型**双错，修复只钉住了名字那一半。

---

### [Major] inline（非 `$ref`）schema 逃逸全部三层门禁；`POST /journals/{uid}/reverse` 文档把 `idempotency_key` 标成 required，而 Go struct 根本不读它

- **位置**：`docs/openapi.yaml:230-239`（inline requestBody，`required: [reason, idempotency_key]`）vs `server/handler_journals.go:50-52`（`reverseJournalRequest` 只有 `Reason`）；逃逸机制见 `server/openapi_contract_test.go:519-528`（`schemaRefIn` 对无 `$ref` 的节点返回 `""` → 整个 operation 被跳过）与该函数注释自陈「that gap is a different, larger finding than M-8, not silently expanded to cover here」
- **判定**：CONFIRMED
- **失效场景**：消费方按 spec 发 `POST /journals/{uid}/reverse {"reason":"refund","idempotency_key":"my-key-1"}`；`idempotencyHeaderAliasMiddleware`（`server/middleware_idempotency.go:64-71`）甚至会把 header 里的 key 也注进 body——然后 `httpx.Decode[reverseJournalRequest]` 把这个字段**静默丢弃**（`json` 解码忽略未知字段）。真实幂等键是 `ReverseJournal` 自己派生的 `fmt.Sprintf("reversal:%s:%s", journalUID, reason)`（`core/interfaces.go:59-63`）。消费方以为自己控制着这笔冲销的幂等语义，实际不控制：换一个 `reason` 重试 = 换一个幂等键。这是 money-path 上的契约谎言。
  逃逸面不止这一处，同为 inline 因而**从未被任何检查看过**的 requestBody 还有：`/journals/{uid}/reverse-partial`(`:264-275`)、`/reservations/{uid}/settle`(`:474-490`)、`/settle-partial`(`:521-536`)、`/finalize`(`:563-578`)、`/release`(`:604-619`)、`/deposits/{uid}/review/reject`(`:775-783`)、`/balances/batch`(`:381-393`)、`/journal-types`(`:1028-1038`)、`/currencies`(`:1148-1165`)、`/reconcile/account`(`:1211-1220`)、`/holder-tokens`(`:1457-1466`)、`/dev/credits`(`:1587-1604`)——**12 个 requestBody，其中 5 个是 reservation 的结算/释放路径**。
- **最小复现**：把 `docs/openapi.yaml:236` 的 `required` 改成 `[reason, idempotency_key, nonexistent_field]` 并加一个 `nonexistent_field` property，跑 `go test ./server/...` → 全绿。
- **为什么现有机制没拦住**：`TestOpenAPIContract_EveryRequestBodySchemaIsRegistered`（`:613`）的完备性检查建立在 `everyRequestBodySchemaRef`（`:532`）之上，而后者只收集 `$ref` 名——inline schema 既不进「已注册」也不进「未注册」集合，是第三态：不可见。完备性门禁自己有一个完备性缺口。

---

### [Major] 出站事件契约：webhook 用 `encoding/json` 直接序列化 `core.Event`，绕过 `pkg/httpx` 的 UTC 强制；且这个 payload 没有任何机器可校验的 schema

- **位置**：`service/delivery/webhook.go:235`（`payload, err := json.Marshal(evt.Event)`，import 的是标准库 `encoding/json`，`:9`）；对照 `pkg/httpx/response.go:28-39`（私有 jsoniter API + `utcTimeExtension`）与 `:41-47` 的注释原文——「pgx v5 decodes timestamptz into time.Local … so a deployment with TZ=Asia/Singapore would silently emit `+08:00` on every `_at` field」；`core/event.go:26`（`OccurredAt time.Time json:"occurred_at"`，无任何 UTC 归一）；`postgres/` 全目录读路径无 `.UTC()` 归一（grep 结果只有写侧 `time.Now().UTC()`）
- **判定**：CONFIRMED（机制）/ PLAUSIBLE（具体偏移取决于部署 TZ，我没有实跑）
- **失效场景**：部署进程 `TZ=Asia/Singapore` 时，HTTP 响应里的 `occurred_at` 是 `2026-09-02T04:00:00Z`（`utcTimeExtension` 强制），而**同一个事件**推给 webhook 订阅者的 body 里是 `2026-09-02T12:00:00+08:00` —— 违反 `api-contract.md` §5 与 `working-agreements.md` §6「机器契约面一律 UTC，绝不把 `+08:00` 写进 API 响应」。订阅者若按 `Z` 后缀做字符串比对/去重/排序会得到两套值。
  第二层问题：这个 payload 的形状是 `core.Event` 的 json tag（`core/event.go:11-36`），与 `docs/openapi.yaml:2359-2383` 的 `Event`（对应 `server/handler_bookings.go:60-75` 的 `eventResponse`）**是两个不同的 Go 类型**——`eventResponse.OccurredAt` 是 `string`（`.UTC().Format(RFC3339)`），`core.Event.OccurredAt` 是 `time.Time`；`core.Event` 还多带 `Attempts`/`MaxAttempts`/`NextAttemptAt`（用 `json:"-"` 挡住了，但这层保护同样只靠人记得写 tag）。**出站事件是这个库最重要的对外契约之一，却没有任何 openapi component / JSON Schema 描述它**，因此也没有任何「消费者容忍未知字段」的可校验约定（`api-contract.md` §8 / §10）。
- **最小复现**：`TZ=Asia/Singapore go run ./examples/event-subscribe`（改成 webhook 路径）或直接对 `delivery.WebhookDeliverer.sendHTTP` 打断点看 payload；亦可单测：构造 `core.Event{OccurredAt: time.Date(..., time.FixedZone("SGT", 8*3600))}` 后 `json.Marshal`，断言输出以 `Z` 结尾 → 红。
- **为什么现有机制没拦住**：UTC 强制被实现为 `pkg/httpx` 的一个 jsoniter extension，**作用域是「走 httpx 的 HTTP 响应」**。事件投递不走 httpx。这是「一个机制被正确实现，然后没有被接到另一条真实出口上」的同形复现——而且没有任何测试断言过出站 payload 的时间格式。

---

### [Major] `required:` 补丁只覆盖了 8 个 schema，7 个 2xx 响应 schema 至今全字段 optional，其中包括 `solvent` / `overall_passed` / `full_coverage` 这三个安全信号

- **位置**：缺 `required:` 的 component schema —
  `AccountPolicy`（`docs/openapi.yaml:2209`）、`DepositTolerancePlanResult`（`:2681`）、`BookingTrace`（`:2732`）、`PlatformBalance`（`:2749`）、`SolvencyReport`（`:2768`）、`BalanceTrendPoint`（`:2783`）、`ReconcileReport`（`:2805`，连同其嵌套的 `checks[]` 与 `findings[]`）。
  对照上一轮补过的 8 个（`docs/audits/2026-08-25-financial-engineering/TODO.md:313-322`：Balance / Reservation / Booking / Event / DepositAddress / ReconcileResult / SystemRollup / BalanceBreakdown）。
  这 7 个都被 2xx 引用：`:672`(`AccountPolicyListEnvelope`) `:656` `:198` `:1351` `:1387` `:1404` `:1441` `:1244`。
- **判定**：CONFIRMED
- **失效场景**：`openapi-typescript` 把无 `required` 的 object 的**每个** property 生成为 `T | undefined`。于是消费方拿到的是
  `report.overall_passed?: boolean` / `report.full_coverage?: boolean` / `solvency.solvent?: boolean`。
  `ReconcileReport` 的 `full_coverage` 与 `CheckResult.complete` 是这个库为「未运行 ≠ 通过」专门引入的信号
  （`core/reconcile_extra.go:17-25` 的注释把这条讲得很清楚），把它们生成成 `boolean | undefined`
  正好把「没跑」和「跑了没过」在类型层重新混在一起——`if (!report.full_coverage)` 与
  `if (report.full_coverage === false)` 会得到不同结论，而编译器不再帮忙区分。
- **最小复现**：`npm run -w @azex/ledger-react codegen` 后看 `src/client/schema.ts` 里 `SolvencyReport`/`ReconcileReport` 的字段全部带 `?`。
- **为什么现有机制没拦住**：`required:` 是人工逐个补的（上一轮 `03e2867` 一批 8 个），**没有任何测试断言「每个被 2xx 引用的 object schema 必须有 `required`」**——而同一个文件里已经存在这种完备性检查的模板（`TestOpenAPIContract_EveryResponseEnvelopeSchemaIsRegistered`、`TestOpenAPIContract_Every2xxHasSchema`）。这是 `working-agreements.md` §5「能被结构强制的不要靠记忆」的一次现成反例：模式已经有了，这一条没套上去。

---

### [Major] 分区键 `created_at` 与余额读路径不匹配：balance 读没有任何分区裁剪谓词，且 `populated` CTE 对 holder 全历史做 DISTINCT，抵消 checkpoint+delta 的意义

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:337`（`PARTITION BY RANGE (created_at)`）、`:370-372`（三个继承索引，无一以 `created_at` 打头）；`postgres/sql/queries/checkpoints.sql:40-71`（`ListComputedBalancesForHolders`，注释自陈「This is the batch primitive behind GetBalances, BatchGetBalances, and role breakdowns」——**全查询没有一个 `created_at` 谓词**）；`:160-172`（`ListBalancesAt`：`WHERE je.effective_at < $1` 且**无 `currency_id` 谓词**，因此 `idx_entries_currency_effective (currency_id, effective_at)` 无法作范围扫描）
- **判定**：CONFIRMED（schema/查询事实）/ PLAUSIBLE（性能量级——本轮无法跑 `EXPLAIN`）
- **失效场景**：每一次余额读（含 `Reserve` 在持 advisory lock 期间的可用余额计算）都要在**每一个月分区**上各走一次 `idx_entries_account_id`。分区数随部署月数单调增长（`service/partition.go` 的 job 还会持续预建未来分区），所以一次余额读的固定开销随时间线性上升，且**永远不会回落**——分区裁剪本来是解决这件事的机制，但裁剪需要分区键上的谓词，而余额路径的谓词是 `id > last_entry_id`。
  更进一步，`populated` CTE（`checkpoints.sql:44-49`）是 `SELECT DISTINCT je.account_holder, je.classification_id FROM journal_entries WHERE account_holder = ANY(...) AND currency_id = ...`——**没有 `id > checkpoint` 限制**，即扫该 holder 的全部历史 entries 只为求出「他碰过哪些 classification」。checkpoint+delta 设计的全部意义是「只读 watermark 之后的行」，这个 CTE 把「读全历史」又加了回来。
  `ListBalancesAt`（trial balance / as-of 的底座）更彻底：无 `currency_id`、无 `created_at`，对全表 GROUP BY。
- **最小复现**：不必真跑——读 `checkpoints.sql:44-71` 的谓词集合，与 `001_baseline.up.sql:337` 的分区键取交集，交集为空。要量化则 `EXPLAIN (ANALYZE, BUFFERS)` 该查询，看 `Append` 节点下的分区数。
- **为什么现有机制没拦住**：`postgres/benchmarks_test.go` 的 bench 在 testcontainer 上跑，那里只有 001 引导阶段建的 4~5 个分区（`001_baseline.up.sql:346-360` 的四个月 horizon），**分区数正好小到看不出这个问题**。没有任何测试或 bench 在「几十个分区」的形态下测过余额读，也没有任何断言检查「余额查询必须能分区裁剪」。

---

### [Major] Go 导出面没有任何破坏性变更检测；`core.Metrics`（32 方法）每加一个信号就是一次对消费方实现的静默 API 破坏

- **位置**：`readme_api_surface_test.go:80-96`（只遍历 `package ledger` 根目录文件里 receiver 为 `Service` 的导出方法，且只断言 README 里出现 `` `svc.<Name>(` `` 这个字符串）；`core/metrics.go:24-128`（`Metrics` 接口，32 个方法，`:93-127` 的 Onchain 一块是后加的）；`core/interfaces.go` 全部 port；CI 无 `apidiff` / `gorelease` / `go/packages` 类检查（`.github/workflows/ci.yml` 五个 job：vet / test / vulncheck / sqlc-diff / build）
- **判定**：CONFIRMED
- **失效场景**：① 给 `core.Metrics` 加第 33 个方法 —— 任何**没有嵌入 `NoopMetrics`** 的消费方实现立刻编译失败。`core/metrics.go:14-23` 用注释建议嵌入，但这是约定不是强制（Go 没有 sealed interface），且这个库已经这样加过一批（Onchain 六个方法）。② 改 `core.ReserveInput` 的字段名、给 `core.Reserver` 加方法、改 `core.JournalWriter` 的签名 —— 全部零门禁。③ `readme_api_surface_test.go` 连**包级导出函数**都不看：`ledger.New` / `ledger.Migrate` / `WithLogger` / `WithMetrics` / `WithAttestor` / `idempotency.go` 的导出项改签名，测试照绿。④ `server` / `service` / `presets` / `channel` / `authdev` / `anchordev` / `anchortest` / `observability` 八个导出包完全无人看守。
- **最小复现**：在 `core/interfaces.go` 的 `Reserver` 里加一个方法并在 `postgres/reserver_store.go` 实现它 → `go test ./...` 全绿，而任何自建 `core.Reserver` 实现（mock、内存实现、代理）的下游都会崩。
- **为什么现有机制没拦住**：`deployment.md` 要求「哪些改动会破坏现有消费方，必须有机制发现」。HTTP 面有 `openapi_contract_test`，DB 面有 migration + `grant_coverage_test`，**Go 面只有一个 README 表格的字符串匹配**。而 Go 库模式是这个仓库的主消费形态（`CLAUDE.md` 开篇即「importable library or standalone HTTP service」）。

---

### [Major] `chains/evm` 与 `anchors/r2` 作为独立 module 目前不可被外部消费；且发版流水线不跑它们的测试（日常 CI 跑）

- **位置**：`chains/evm/go.mod:6`（`require github.com/azex-ai/ledger v0.0.0-00010101000000-000000000000`）+ `:47`（`replace github.com/azex-ai/ledger => ../..`）；`anchors/r2/go.mod:10-11` + `:78,84`（同形，且额外 `require github.com/azex-ai/ledger/anchors/r2/internal/miniotest v0.0.0-000...`，一个 **`internal/` 路径下、外部永远不可 import 的模块**）；`.github/workflows/go-release.yml:11-12`（只匹配 `v*` tag）+ `:44-48`（verify 只跑根模块的 vet/build/test）；对照 `.github/workflows/ci.yml:63-73`（日常 CI **专门**为两个子模块各加了独立 test step，并在注释里写明「Without these steps chains/evm and anchors/r2 ship with tests that CI never runs, which is the shape this repo keeps paying for」）
- **判定**：CONFIRMED
- **失效场景**：① `replace` 指令**只在 main module 生效**，消费方拿不到。消费方 `go get github.com/azex-ai/ledger/anchors/r2@latest` 先撞上「没有 `anchors/r2/v*` tag」（`go-release.yml` 从不打子模块 tag）；即便手工指定一个 commit，模块图里会出现 `github.com/azex-ai/ledger v0.0.0-00010101000000-000000000000` 与不可获取的 `.../internal/miniotest` —— 只有在消费方**自己也直接 require 根模块的某个真实版本**（MVS 选高者）的情况下才碰巧能解析。`CLAUDE.md` 把 `anchors/r2` 列为「Cloudflare R2 + Object Lock anchor. **Separate Go module**」的可消费产物，但这条消费路径今天走不通。
  ② 发版门禁比日常门禁松：打 `v0.6.1` 时 `anchors/r2` / `chains/evm` 的测试与 vet 一次都不跑。这两个模块里有 `anchortest` 一致性套件和 MinIO testcontainer 测试——正是最需要在发版点被执行的东西。
- **最小复现**：`GOWORK=off go build ./...`（在 `anchors/r2/` 外的临时消费模块里 import `github.com/azex-ai/ledger/anchors/r2`）→ 版本解析失败。发版侧：读 `go-release.yml:44-48` 与 `ci.yml:63-73` 的 step 差集。
- **为什么现有机制没拦住**：`ci.yml` 的注释证明这个 failure mode（「检查存在，没人执行」）**已经被识别并在日常 CI 里修过一次**，但修复没有复制到 `go-release.yml`——同一份仓库里同一个形状，第二个出口没接上。见「上轮修复复核」。

---

### [Major] `core.Metrics` 把内部 `currencies.id` 递给消费方实现，并作为 Prometheus label `currency_id` 发布 —— I-18 明文禁止的 core 接口内部 id 泄露，两个 I-18 门禁都看不见

- **位置**：`core/metrics.go:75`（`BalanceDrift(classCode string, currencyID int64, ...)`）、`:89`（`NegativeBalanceDetected(classCode string, currencyID int64)`）、`:90`（`ReconcileGap(currencyID int64, ...)`）、`:91`（`ReservedAmount(currencyID int64, ...)`）；落地：`observability/prometheus.go:233,238,243,248`（label 名字面量 `"currency_id"`）、`:403-419`；规则原文：`docs/INVARIANTS.md:790-794`（「Internal `BIGSERIAL`/`IDENTITY` ids … appear in **no public contract**: … and not in the library-mode Go API (`core` types **and interfaces** speak uids exclusively)」）
- **判定**：CONFIRMED
- **失效场景**：消费方要实现 `core.Metrics`（这是库明确邀请的扩展点，`core/metrics.go:10-11`「Inject Prometheus, OpenTelemetry, or DataDog implementation」）就必须接收并处理 `currencies.id` 这个内部自增主键——它跨 dump/restore 不稳定、暴露写入顺序与基数，正是 I-18 的「Why」段（`INVARIANTS.md:808-811`）列举的三件事。库自带的实现进一步把它写成 Prometheus label，于是**运维面板与告警规则被焊死在内部主键上**：换一次数据库重建（或把 currency 表重灌）所有历史 series 断裂。
- **最小复现**：`curl :9090/metrics | grep currency_id` → `ledger_balance_drift{class="main_wallet",currency_id="3"}`。
- **为什么现有机制没拦住**：I-18 的两个 pin（`server/contract_pin_test.go:41`、`core/no_internal_ids_test.go:61`）都调用 `idschema.ScanGoFilesForBannedKeys`，而它的实现（`internal/idschema/idschema.go:145`）是一条正则 `json:"([a-z0-9_]+)[,"]` —— **只能看见 struct 的 json tag**。接口方法的参数不是 struct 字段、没有 json tag，因此结构上不可见。`INVARIANTS.md:834-838` 却声称该 pin「scans every exported type declared in `core/*.go` directly」，这句描述与机制不符（详见下一条）。

---

### [Minor] I-18 core 侧门禁的真实覆盖是「带 json tag 的字段」，而 INVARIANTS 声称是「every exported type」；in-tree 已有无 tag 的内部 id 类型，门禁绿

- **位置**：`internal/idschema/idschema.go:141-169`（`ScanGoFilesForBannedKeys`，逐行正则匹配 `json:"..."`）；`core/attestation.go:132-141`（`AttestedEntry{ EntryID, JournalID, AccountHolder, CurrencyID, ClassificationID int64 }`，**导出类型、零 json tag**）、`:446-449`（`AttestedLeaf{ EntryID int64 }`）；声称：`docs/INVARIANTS.md:834-838`
- **判定**：CONFIRMED
- **失效场景**：`core.AttestedEntry` / `core.AttestedLeaf` 是导出的 core 类型且携带四个内部 id，按 I-18 的全称措辞它们是违规；按类型自己的注释（`core/attestation.go:129-131`）它们是**有意的例外**（digest 必须绑定内部 id）。问题不在这两个类型，而在于：**这个例外既不在 I-18 文本里，也不在门禁的 allowlist 里，它只是碰巧不带 json tag 所以看不见**。后果是门禁无法区分「有意例外」与「有人忘了」——新加一个不带 tag 的 `CurrencyID int64` 到任何 core 类型上，两个 pin 都不会红。
- **最小复现**：在 `core/` 新建一个 `type Foo struct{ ClassificationID int64 }`（无 tag），跑 `go test ./core/ -run TestNoInternalIDFieldsInCoreTypes` → 绿。
- **为什么现有机制没拦住**：回归 pin `TestNoInternalIDFieldsInCoreTypes_CatchesPlantedViolation`（`core/no_internal_ids_test.go:74-93`）种的 fixture **自己带 json tag**（`:83`），所以它证明的是「带 tag 的违规能被抓」，恰好绕开了这个盲区。这是 §1.4「测试做的准备工作，真实场景也会做吗」的一个实例。

---

### [Minor] `docs/openapi.yaml` 的 `info.version` 是 `0.4.0`，CHANGELOG 已到 `0.6.0`；同一文件里写了「bump both together」的规则，没有任何门禁

- **位置**：`docs/openapi.yaml:9-10`（「`info.version` tracks the Go module's release tag (see CHANGELOG.md) — bump both together, not independently.」）+ `:16`（`version: "0.4.0"`）；`CHANGELOG.md:17`（`## [0.6.0] — 2026-08-27`）
- **判定**：CONFIRMED
- **失效场景**：落后两个 minor 版本。消费方从 spec 生成的客户端会自我标注为 0.4.0 API；排查线上问题时按 `info.version` 去 CHANGELOG 对照会看错一整段（含 0.5.1/0.6.0 的破坏性变更）。
- **为什么现有机制没拦住**：规则写在注释里，靠人记得。可机器化（一条测试比对 `info.version` 与 CHANGELOG 最新标题即可），未做。

---

### [Minor] `GET /journals` 的排序方向：spec 说 descending，实现是 ascending

- **位置**：`docs/openapi.yaml:114`（「List journals (cursor-paginated, **descending id**)」）vs `postgres/sql/queries/journals.sql:96-100`（`ListJournalsCursor`：`WHERE id > cursor_id ORDER BY id ASC`），经 `postgres/query_provider.go:88-109` 直接透出
- **判定**：CONFIRMED
- **失效场景**：按文档实现「最新在前」列表的消费方拿到的是最旧在前，且 cursor 语义是「大于」而非「小于」，翻页方向整体相反。对照 `/holder/transactions` 确实是 DESC（`postgres/sql/queries/holder.sql:34-36` `j.id < cursor ORDER BY j.id DESC`），所以同一个 API 里两条 cursor 分页方向相反、只有一条被文档说对。
- **为什么现有机制没拦住**：`summary` 是散文，无门禁；契约门禁不检查语义。

---

### [Minor] 同一个 API 里并存两种「不分页 list」的 wire 形状，其中一种绕过了全部 wire struct 反射

- **位置**：`server/response.go:18-21`（`PagedResponse[T]` → `{list, next_cursor: null}`）vs `server/handler_holder.go:228` 与 `:291`（`httpx.OK(w, map[string]any{"list": out})` → `{list}`，**无 `next_cursor` 键**）；spec 侧一致地分成两派：`HolderBalanceListEnvelope`(`docs/openapi.yaml:1744-1755`) / `HolderHoldListEnvelope`(`:1795-1806`) 只有 `list`，其余 12 个 list envelope 都有 `next_cursor`
- **判定**：CONFIRMED
- **失效场景**：消费方写一个通用的「取下一页」helper 时，`data.next_cursor` 在两条 holder 路由上是 `undefined`（键不存在），在其他路由上是 `null`——`api-contract.md` §6 要求的正是「一个可判等的哨兵」，现在有两种拼写。次生问题：`map[string]any` 不是 struct，`TestOpenAPIContract_ListEnvelopeItemsMatchGoStructs`（`server/openapi_contract_test.go:370`）注册的 `holderBalanceResponse{}` / `holderHoldResponse{}` 只覆盖 **item**，**envelope 那一层完全没有 Go 类型可反射**——这两条路由的外层形状是无门禁的。
- **为什么现有机制没拦住**：上一轮把 `next_cursor` 修成 `*string` 是在 `PagedResponse` 与 `holderTransactionsPage` 两个 struct 上做的；用 map 直接写的两条路由不在那次改动的视野里。

---

### [Minor] `Config.AllowSystemClassificationPost` 的文档写了环境变量名，`LoadConfig` 从不读它

- **位置**：`server/server.go:185`（注释「AllowSystemClassificationPost (**ALLOW_SYSTEM_CLASSIFICATION_POST**) opts a deployment out…」）vs `:251-309`（`LoadConfig` 读了 8 个 env，其中没有这一个；`:294-304` 构造 `Config{}` 时也未赋该字段）
- **判定**：CONFIRMED
- **失效场景**：走 `server.New()` / `LoadConfig()` 的部署设了 `ALLOW_SYSTEM_CLASSIFICATION_POST=true` 也**不会生效**，字段恒为 `false`。方向是安全的（默认关闭 = 保守），所以不构成安全洞；但文档承诺的开关不存在，运维会以为自己关了/开了一个其实没接线的闸。
- **为什么现有机制没拦住**：`server/config_test.go` 未覆盖该字段的 env 读取（它不存在，没法覆盖）；没有「Config 每个字段都要么被 LoadConfig 读、要么显式标注仅程序化设置」的检查。

---

### [Minor] `settled_amount: { nullable: true }` 是 OpenAPI 3.0 关键字，在声明为 3.1.0 的 spec 里无意义

- **位置**：`docs/openapi.yaml:1`（`openapi: 3.1.0`）+ `:2166`（`settled_amount: { $ref: ".../Decimal", nullable: true }`）；对照同文件里正确的 3.1 写法 `type: [string, "null"]`（`:1783` 等 12 处 `next_cursor`）
- **判定**：CONFIRMED
- **失效场景**：3.1 里 `nullable` 不是 JSON Schema 关键字，生成器会忽略它。作者的意图（该字段可为 null）在生成类型里表达不出来；而 Go 侧其实是 `*string` + `omitempty`（`server/handler_reservations.go:58`），wire 上是**缺席**而非 null，所以三方（意图 / spec / 实现）各说一套。
- **为什么现有机制没拦住**：契约门禁只比 key 集合，不做 spec 版本合法性校验；CI 里没有 `spectral` / `openapi-spec-validator` 之类的 lint。

---

### [Minor] `core.ReconcileReport` 是唯一被直接当 HTTP wire 类型使用的 core 类型，且其嵌套结构不在契约门禁覆盖内

- **位置**：`server/handler_system.go:172-179`（`report, err := s.fullReconciler.RunFullReconciliation(...)` → `httpx.OK(w, report)`，report 是 `*core.ReconcileReport`）；`core/reconcile_extra.go:10-55`（core 类型带 json tag）；门禁注册处 `server/openapi_contract_test.go:316`（`{"ReconcileReportEnvelope", core.ReconcileReport{}}`）；覆盖边界 `:131-138`（`envelopeDataObjectNames` 只读 `data` 的**顶层** `properties`）
- **判定**：CONFIRMED
- **失效场景**：① 领域类型直接充当 wire 契约：改 `core.CheckResult` 的任一 json tag 就是一次 HTTP 破坏性变更，而其他每个端点都有 `server` 本地的 `*Response` 类型做缓冲。② `checks[]` 与 `findings[]` 在 spec 里是 inline 嵌套对象（`docs/openapi.yaml:2821-2845`），`envelopeDataObjectNames` 不下钻，因此 `name` / `passed` / **`complete`** / `checked_at` / `findings[].description` / `findings[].detail` 六个字段**两个方向都没有契约覆盖**——其中 `complete` 正是「部分覆盖绝不读作通过」这条设计的载体。
- **为什么现有机制没拦住**：门禁刻意只做一层（注释 `:126-130` 说明它会解析 `data` 上的 `allOf`，但不递归 properties）。

---

### [Minor] `balance_checkpoints` / `rollup_queue` / `balance_snapshots` 的 `currency_id` / `classification_id` 没有外键，`system_rollups` 有

- **位置**：`postgres/sql/migrations/001_baseline.up.sql:396-406`（`balance_checkpoints`，裸 `BIGINT NOT NULL`）、`:408-418`（`rollup_queue`）、`:427-435`（`balance_snapshots`）vs `:439-445`（`system_rollups`，两列都 `REFERENCES`）
- **判定**：CONFIRMED
- **失效场景**：同一份 schema 对同类维度列采用了两套完整性策略。缺 FK 的三张表可以持有指向已删除/不存在 currency 或 classification 的行，只能靠 `orphan_*` reconcile 检查事后发现（`postgres/sql/queries/reconcile.sql`）。这三张表恰好是余额物化路径，`AggregateCheckpointsByClassification`（`checkpoints.sql:199-206`）会把孤儿行汇总进 `system_rollups`。
- **为什么现有机制没拦住**：`001_baseline` 的注释解释了 `account_policies.currency_id = 0` 的通配符例外（`:676-681`），但没有解释为什么 checkpoint 三兄弟不加 FK；`grant_coverage_test.go` 管的是 ACL 分类而不是 FK 覆盖。

---

### [Minor] holder 读面三条路径都对该 holder 的全历史做扫描 / DISTINCT，且两条无分页上限

- **位置**：`postgres/sql/queries/holder.sql:27-37`（`page_journals` CTE：`SELECT DISTINCT j.id … WHERE je.account_holder = $1 … ORDER BY j.id DESC LIMIT n` —— 没有任何索引能同时服务 `account_holder` 过滤 + `journal_id` 去重 + `j.id DESC` 排序，现有 `idx_entries_account_id` 是 `(account_holder, currency_id, classification_id, id)`，`journal_id` 不在其中，见 `001_baseline.up.sql:370`）；`:110-119`（`ListHolderCurrencies`：`SELECT DISTINCT … WHERE je.account_holder = $1`，全历史）；`:92-108`（`ListHolderHolds`：无 LIMIT）
- **判定**：CONFIRMED（索引/查询事实）/ PLAUSIBLE（代价量级）
- **失效场景**：`GET /holder/transactions` 的**每一页**（含第 10 页）都要把该 holder 的全部 entries 读出来再排序取 20 条——分页不会随深度变便宜。`GET /holder/balances` 先 `ListHolderCurrencies` 全历史 DISTINCT，再按币种扇出 breakdown。`GET /holder/holds` 与 `/holder/balances` 都没有 LIMIT，返回全集。
- **为什么现有机制没拦住**：holder surface 的测试都在几十行数据量上跑；无 bench，无「list 端点必须有上限」的结构检查。

---

### [Minor] `idempotencyHeaderAliasMiddleware` 的 `params` 嵌套分支在当前 HTTP 面没有任何对应的 request struct

- **位置**：`server/middleware_idempotency.go:57-62`（「Template execution nests the key under "params"」）vs `server/handler_journals.go:38-48`（`postTemplateRequest` 的 `idempotency_key` 是**平铺**的）；全仓唯一带 `json:"params"` 的是 `core/template.go:55`（`TemplateExecutionRequest`，`TemplateBatchExecutor` 的库内类型，无 HTTP 路由）
- **判定**：CONFIRMED
- **失效场景**：注释描述的行为已经不存在（模板执行早就平铺了 key）。分支仍在，且是无条件的：任何 POST body 只要顶层有一个名为 `params` 的对象，header 里的 `Idempotency-Key` 就会被注进 `params` 里而**不是**顶层，导致 handler 读不到 → 报 "idempotency_key is required"。今天没有这样的端点，但这是一颗按「字段名撞车」触发的雷。
- **为什么现有机制没拦住**：中间件测试只覆盖平铺路径；没有测试断言「`params` 分支对应哪个真实端点」。

---

### [Minor] 响应方向门禁单向（spec ⊆ Go），Go 新增响应字段永远不会被任何机制发现

- **位置**：`server/openapi_contract_test.go:235-248`（`assertSpecSubsetOfGo`）+ `:20-33` 的自陈理由
- **判定**：CONFIRMED
- **失效场景**：给任一 `*Response` struct 加一个字段：Go 侧发出去了，`docs/openapi.yaml` 不知道，`schema.ts` 不知道，`@azex/ledger-react` 的类型里没有它，TS 消费方拿不到（且 `codegen:check` 也不会红，因为 openapi 没变）。字段就这样长期存在于 wire 上而不存在于契约里。
- **为什么现有机制没拦住**：这是刻意取舍（`journalResponse` 的 `omitempty` 字段被多个 envelope 复用，纯反射无法静态判断）。取舍本身合理，但**没有任何补偿机制**（例如 `omitempty` 字段显式 allowlist + 其余字段双向校验）。

---

## 上轮修复复核

对 `structure.md` 与 Wave 1/1.5/2 落在本 territory 的修复，逐条问三件事：接到真实路径了吗 / 拆掉会不会红 / 有没有在别处复制出同形缺陷。

**1. `next_cursor` 必须序列化成字面 `null`（上轮 Major）— 修复成立，但覆盖不全**
`server/response.go:18-21` 的 `PagedResponse[T].NextCursor *string`（无 `omitempty`）与 `handler_holder.go:144-149` 的 `holderTransactionsPage` 都改对了；spec 侧 12 处 `next_cursor` 全部改成 `type: [string,"null"]`，并新增 `TestOpenAPIContract_NextCursorIsNullable`（`openapi_contract_test.go:412-428`）递归遍历、不再靠两种硬编码形状猜——**拆掉 `cursorPtr` 会红**（回归 pin 在 `response_test.go`）。
**缺口**：`handler_holder.go:228`、`:291` 两条路由用 `map[string]any{"list": out}` 直接出，既没有 `next_cursor` 也没有 Go struct 可反射，落在修复视野之外（见上文 Minor）。

**2. `openapi_contract_test.go` 双向门禁（上轮的核心修复）— 接到真实路径了，但覆盖面比它自己声称的窄**
它确实跑在 `go test ./...`（无 path filter），确实会在 `expires_in` / `expires_in_sec` 那种漂移上红，`EveryRequestBodySchemaIsRegistered` / `EveryResponseEnvelopeSchemaIsRegistered` / `Every2xxHasSchema` / `EveryRouteIsDocumented` 四层完备性检查也都是「从产物派生、不靠手维护清单」的正确形状。**但它的输入集合只有 `components.schemas` 与 `paths.*.responses`**：query/path 参数（Major 第 1 条，3 处实证漂移）、字段类型与 format（Major 第 2 条）、inline schema（Major 第 3 条，12 个 requestBody）、嵌套对象（Minor `ReconcileReport`）四类全在外面。**这不是「修复失效」，是「修复的边界比读者以为的窄」**——`openapi_contract_test.go:1-33` 的文件头注释确实诚实地写了取舍，但 `docs/audits/2026-08-25-financial-engineering/README.md:41-43` 的总结「反射 Go wire struct 对照 `docs/openapi.yaml`，双向报」读起来覆盖面要宽得多。

**3. `required:` 列表（`03e2867`）— 只补了 8 个，7 个仍缺，且没有留下防复发的检查**
见上文 Major 第 5 条。补丁本身正确，但它是一次性人工清点；同一个文件里已经有四个「完备性 = 从产物派生」的模板可套（这正是上一轮 README 自称的最大收获「把靠人记得换成机器会拦」），这一条没套上。**下一个新增的 response schema 默认仍然是全 optional。**

**4. I-18 门禁抽进 `internal/idschema`（board #28）— 去重成功，覆盖面被文档高估**
两份 ~55 行拷贝确实合成了一份，`BannedKeys` 的三条 sanity floor（无 migration 文件 / 零 surrogate 表 / 少于 10 条）是好的 fail-closed 设计，两个回归 pin 也都能对种下的 fixture 变红。**但机制是 `json:"..."` 正则**（`idschema.go:145`），因此：① 接口方法参数看不见 → `core.Metrics` 的 `currencyID int64` 泄露至今无人发现（Major 第 9 条）；② 无 json tag 的导出 core 类型看不见 → `core.AttestedEntry` 在树里且门禁绿（Minor）。而 `docs/INVARIANTS.md:834-838` 写的是「scans every exported type declared in `core/*.go` directly」——**描述与机制不符**，这正是 §1.4 要防的那类「读起来比实际强」的门禁。两个回归 pin 种的 fixture 都自带 json tag，所以它们**结构上不可能**发现这个盲区。

**5. M-7 `HolderTxKind` 词表 — 端到端接住了**
`core.HolderTxKind` 的六值词表、`journal_types.holder_kind`（migration 012）、`COALESCE(NULLIF(jt.holder_kind,''),'other')`（`holder.sql:68`，把内部「未打标」状态挡在 wire 之外）、openapi 的 `enum`（`:1762`、`:2475`）、`SetHolderKind` 的可重打标能力（`core/interfaces.go:344-353`）—— 五处一致，`I-44` 的措辞与实现也对得上。**没找到缺口。** 唯一可议：`JournalType.holder_kind` 的 enum 允许 `""`（`:2475`）而 `HolderTransaction.kind` 的 enum 不允许（`:1762`），两处不同是**有意且正确**的（一个是配置读回、一个是产品面），文档也写明了。

**6. `NewFromDeps`（上轮 Minor：21 个同形位置参数无编译期保护）— 修复存在，但没接到唯一的示范路径**
`server.NewFromDeps` + `Deps` struct 已落地并有 pin（`server/deps_test.go:40-70`，含「旧构造器仍然 panic」的显式断言）。**但仓库里唯一的完整装配示例 `examples/fullstack/backend/main.go:220-242` 仍然用 22 参数的 `NewWithConfig`**，而 `New` / `NewWithConfig` 都没有 `Deprecated:` 标记（`server/server.go:311,347`）。消费方读示例学到的仍是旧形式——「机制在库里、示例里没有」，与上一轮 README §2 主题一同形，只是方向反过来。

**7. `ledger-react-publish.yml` 补 `codegen:check`（上轮 Major）— 修复成立，但同形缺陷在 `go-release.yml` 上原样存在**
`ledger-react-publish.yml:34` 确实补上了，注释还写明了理由（「this release pipeline … never re-ran it」）。**同一形状在 Go 发版侧没修**：`ci.yml:63-73` 专门为 `chains/evm` / `anchors/r2` 各加了 test step 并在注释里点名这个 failure mode，而 `go-release.yml:44-48` 的 verify 只跑根模块——打 tag 时这两个子模块的测试与 vet 一次都不跑（Major 第 8 条）。**「发版门禁比日常门禁松」这条上一轮已经诊断并修过一次的病，在另一条流水线上完好无损。**

---

## 移交

- **A（financial-correctness）**：`ListBalancesAt`（`postgres/sql/queries/checkpoints.sql:160-172`）无 `currency_id` 谓词也无 `created_at` 谓词，与 `idx_entries_currency_effective` 的列序不匹配 —— 我只判定了索引不可用，没判断它对 as-of 余额**正确性**有无影响。
- **B（concurrency）**：Major 第 6 条的分区不裁剪发生在 `Reserve` 持 advisory lock 的窗口内（余额计算在锁内），锁持有时长随分区数增长 —— 并发影响归 B。
- **C（tamper-evident）**：`entry_attestations` 的 PK 是 `(entry_id)` 单列（`001_baseline.up.sql:1014-1021`），它假设 `journal_entries.id` 全局唯一；而 migration 008 自己承认这个唯一性**不是约束保证的**，只是「ledger_app 拿不到 id 列的 INSERT 权限 + 单一序列」，并点名 PITR 后序列回退是现实触发路径。这条对存证覆盖的含义（同一 entry_id 覆盖两行）请 C 判。
- **D（threat-model）**：`webhook_subscribers.secret` 以明文 TEXT 存库（`001_baseline.up.sql:787`），而该表 `ledger_app` 有 UPDATE 且无 mutation guard；出站签名密钥与 DB 同失效域。
- **I（operability）**：`observability/prometheus.go` 用 `decimalToFloat` 把 decimal 转 float64 做 gauge —— Prometheus 结构上只能是 float64，我认为这是不可避免而非违规，但金额进 float 这件事应由 I 明确记一笔。另外 `server` 包全程直接用 `slog`（`server.go:493,496,499,502,516`）而不是注入的 `core.Logger`，消费方无法接管 HTTP 层日志。
- **J（web-ledger-react）**：Major 第 5 条（7 个 schema 缺 `required`）会直接体现在 `web/packages/ledger-react/src/client/schema.ts` 的生成类型上；Major 第 2 条的 `expires_at: ""` 会流到前端的日期渲染。

---

## 我没能验证的

- **没有跑任何命令**（本次无 Bash 权限）。所有「会红/会绿」的判断是读代码推演，不是实测。特别是：Major 第 6 条的性能量级完全没有 `EXPLAIN` 支撑，我只确证了「谓词集合与分区键无交集」这个静态事实。
- **`web/packages/ledger-react/src/client/schema.ts` 我没有打开**。三方一致的第三方（TS 生成物）只从 `package.json` 的 `codegen:check` 脚本定义与两个 workflow 推断，没有逐字段核对生成结果。
- **openapi.yaml 全文 2852 行没有逐端点逐字段核完**。我逐字核了：全部 `parameters`（这是本轮主要产出之一）、全部 `components.schemas` 的 `required` 有无、全部 12 个 `next_cursor`、以及 reservations / journals / entries / balances / snapshots / bookings / events / holder / platform / reports 这十组的响应字段。**未逐字核**：`/webhooks/{channel}` 的 `oneOf` 三分支、`/deposits/*` 三个端点、`/holders/{holder}/deposit-address` 两个端点的响应字段与 `handler_onchain.go` / `handler_deposit_reviews.go` 的实际 struct。
- **`postgres/sql/queries/` 31 个文件里我只读了 6 个**（checkpoints / journals / holder / trial_balance / audit_lists 及 reservations 的片段）。索引对照表（附录 B）只覆盖这 6 个文件命中的表；`reconcile.sql`、`integrity_*.sql`、`platform_balances.sql`、`bookings.sql`、`events.sql` 的 WHERE/ORDER BY 与索引是否匹配，**我没有看**。
- **migration 002–015 我只完整读了 008**，其余只看了文件名。因此「后续 migration 有没有新增索引/外键改变附录 B 的结论」这一点我只能保证 001 与 008 的部分；`015_checkpoint_updated_at_index` 明显新增了一个索引，我没有把它并入附录 B 的分析。
- **`deployment.md` 三步（expand → migrate → contract）在 002–015 里有没有实例** —— 我没有逐个 migration 判定。已确证的只有：001 的文件头明确说明「一次性压平 53 个 migration、放弃 N-1 兼容性证明」，其依据 `ledger-no-compat-constraint` **是一个 agent memory 文件名，不在仓库里**（全仓 grep 只有三处引用、零处定义），因此「这个例外何时结束、由谁判定」在代码库里没有可核的记载。破坏性变更清单目前维护在 `docs/audits/2026-08-25-financial-engineering/TODO.md` §10，是一份审计残务文件而不是发版流程的一部分。
- **`anchors/r2` 的外部可消费性**我是按 Go 模块解析规则推演的（`replace` 只在 main module 生效、`v0.0.0-00010101000000-000000000000` 是 replace 占位伪版本、无子模块 tag），**没有真的建一个 scratch 消费模块跑 `GOWORK=off go build`**。CLAUDE.md 的 Gotchas 段落记录过一次类似的 scratch 实测，本轮没有复现。

---

## 附录 A · 端点契约合规矩阵

列含义：**包络** = `{code,message,data}` 且 message/data 互斥；**snake** = 字段 snake_case；**uid** = 无内部 BIGSERIAL；**金额** = 字符串；**时间** = RFC3339 UTC；**分页** = `api-contract.md` §6 形状；**幂等** = 写操作有幂等键；**spec** = 与 `docs/openapi.yaml` 一致。
`—` = 不适用；`?` = 本轮未逐字核。

| # | 路由 | 包络 | snake | uid | 金额 | 时间 | 分页 | 幂等 | spec |
|---|---|---|---|---|---|---|---|---|---|
| 1 | `GET /system/health` | ✅ | ✅ | — | — | — | — | — | ✅ |
| 2 | `GET /system/ready` | ✅ | ✅ | — | — | — | — | — | ✅ |
| 3 | `GET /system/balances` | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ 恒 null 的 cursor 形 | — | ✅ |
| 4 | `GET /journals` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ cursor | — | ❌ 方向写反（Minor） |
| 5 | `POST /journals` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ✅ body | ✅ |
| 6 | `POST /journals/template` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ✅ body/header | ✅ |
| 7 | `POST /journals/deposit-tolerance` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ✅ | ⚠️ 响应 schema 无 `required` |
| 8 | `GET /journals/{uid}` | ✅ | ✅ | ✅ | ✅ | ✅ | — | — | ✅ |
| 9 | `POST /journals/{uid}/reverse` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ⚠️ 派生键，body 里的被丢弃 | ❌ **Major 3** |
| 10 | `POST /journals/{uid}/reverse-partial` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ✅ | ⚠️ inline，无门禁 |
| 11 | `GET /entries` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ cursor | — | ❌ **Major 1**（`currency` vs `currency_uid`） |
| 12 | `GET /balances/{holder}` | ✅ | ✅ | ✅ | ✅ | — | ⚠️ 恒 null | — | ❌ **Major 1**（必填参数未文档化） |
| 13 | `GET /balances/{holder}/{currency}` | ✅ | ✅ | ✅ | ✅ | — | — | — | ✅ |
| 14 | `GET /balances/{holder}/{currency}/breakdown` | ✅ | ✅ | ✅ | ✅ | — | — | — | ✅ |
| 15 | `POST /balances/batch` | ✅ | ✅ | ✅ | ✅ | — | ⚠️ 恒 null | — | ⚠️ inline，无门禁 |
| 16 | `GET /reservations` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ cursor | — | ✅ |
| 17 | `POST /reservations` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ✅ | ✅（上轮修复后一致） |
| 18 | `POST /reservations/{uid}/settle` | ✅ | ✅ | ✅ | ✅ | — | — | ✅ 必填 | ⚠️ inline |
| 19 | `POST /reservations/{uid}/settle-partial` | ✅ | ✅ | ✅ | ✅ | — | — | ✅ 必填 | ⚠️ inline |
| 20 | `POST /reservations/{uid}/finalize` | ✅ | ✅ | ✅ | — | — | — | ✅ 必填 | ⚠️ inline |
| 21 | `POST /reservations/{uid}/release` | ✅ | ✅ | ✅ | — | — | — | ✅ 必填 | ⚠️ inline |
| 22 | `PUT /accounts/{holder}/policy` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ❌ 无幂等键 | ⚠️ 响应无 `required` |
| 23 | `GET /accounts/{holder}/policies` | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ 恒 null | — | ⚠️ 响应无 `required` |
| 24 | `POST /holders/{holder}/deposit-address` | ✅ | ✅ | ✅ | — | ✅ | — | 天然幂等 | ? |
| 25 | `GET /holders/{holder}/deposit-address` | ✅ | ✅ | ✅ | — | ✅ | — | — | ? |
| 26 | `GET /deposits/reviews` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ cursor | — | ? |
| 27 | `POST /deposits/{uid}/review/approve` | ✅ | ✅ | ✅ | ✅ | ✅ | — | 声明幂等 | ? |
| 28 | `POST /deposits/{uid}/review/reject` | ✅ | ✅ | ✅ | ✅ | ✅ | — | 声明幂等 | ⚠️ inline |
| 29 | `GET /bookings` | ✅ | ✅ | ✅ | ✅ | ⚠️ 空 booking 的 `expires_at` 为 `""` | ✅ cursor | — | ❌ **Major 2** |
| 30 | `POST /bookings` | ✅ | ✅ | ✅ | ✅ | ⚠️ 同上 | — | ✅ | ❌ **Major 2** |
| 31 | `GET /bookings/{uid}` | ✅ | ✅ | ✅ | ✅ | ⚠️ 同上 | — | — | ❌ **Major 2** |
| 32 | `POST /bookings/{uid}/transition` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ✅ 必填（I-3） | ✅ |
| 33 | `GET /events` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ cursor | — | ✅ |
| 34 | `GET /events/{uid}` | ✅ | ✅ | ✅ | ✅ | ✅ | — | — | ✅ |
| 35 | `POST /webhooks/{channel}` | ✅ | ✅ | ✅ | ✅ | ✅ | — | nonce 缓存 | ⚠️ `oneOf` inline，未核 |
| 36 | `GET /classifications` | ✅ | ✅ | ✅ | — | ✅ | ⚠️ 恒 null | — | ✅ |
| 37 | `POST /classifications` | ✅ | ✅ | ✅ | — | ✅ | — | ❌ 无幂等键 | ✅ |
| 38 | `POST /classifications/{uid}/deactivate` | ✅ | ✅ | ✅ | — | — | — | 天然幂等 | ⚠️ inline |
| 39 | `GET /journal-types` | ✅ | ✅ | ✅ | — | ✅ | ⚠️ 恒 null | — | ✅ |
| 40 | `POST /journal-types` | ✅ | ✅ | ✅ | — | ✅ | — | ❌ 无幂等键 | ⚠️ inline（`holder_kind` 未被门禁核） |
| 41 | `POST /journal-types/{uid}/deactivate` | ✅ | ✅ | ✅ | — | — | — | 天然幂等 | ⚠️ inline |
| 42 | `GET /templates` | ✅ | ✅ | ✅ | — | ✅ | ⚠️ 恒 null | — | ✅ |
| 43 | `POST /templates` | ✅ | ✅ | ✅ | — | ✅ | — | ❌ 无幂等键 | ✅ |
| 44 | `POST /templates/{code}/preview` | ✅ | ✅ | ✅ | ✅ | — | — | — | ✅ |
| 45 | `POST /templates/{uid}/deactivate` | ✅ | ✅ | ✅ | — | — | — | 天然幂等 | ⚠️ inline |
| 46 | `GET /currencies` | ✅ | ✅ | ✅ | — | — | ⚠️ 恒 null | — | ✅ |
| 47 | `POST /currencies` | ✅ | ✅ | ✅ | — | — | — | ❌ 无幂等键 | ⚠️ inline |
| 48 | `POST /currencies/{uid}/deactivate` | ✅ | ✅ | ✅ | — | — | — | 天然幂等 | ⚠️ inline |
| 49 | `POST /reconcile` | ✅ | ✅ | ✅ | ✅ | ✅ | — | — | ✅ |
| 50 | `POST /reconcile/account` | ✅ | ✅ | ✅ | ✅ | ✅ | — | — | ⚠️ inline requestBody |
| 51 | `POST /reconcile/full` | ✅ | ✅ | ✅ | — | ✅ | — | — | ⚠️ 无 `required`、嵌套无门禁 |
| 52 | `GET /snapshots` | ✅ | ✅ | ✅ | ✅ | ✅ date | ⚠️ 恒 null | — | ❌ **Major 1**（三个参数名全错） |
| 53 | `GET /audit/journals` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ cursor | — | ✅ |
| 54 | `GET /audit/bookings/{uid}/trace` | ✅ | ✅ | ✅ | ✅ | ⚠️ 见 #29 | — | — | ⚠️ 无 `required` |
| 55 | `GET /audit/journals/{uid}/reversals` | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ 恒 null | — | ✅ |
| 56 | `GET /platform/balances` | ✅ | ✅ | ✅（key 是 code） | ✅ | — | — | — | ⚠️ 无 `required` |
| 57 | `GET /platform/solvency` | ✅ | ✅ | ✅ | ✅ | — | — | — | ⚠️ 无 `required`（含 `solvent`） |
| 58 | `GET /balances/trends` | ✅ | ✅ | ✅ | ✅ | ✅ date | ⚠️ 恒 null | — | ⚠️ 无 `required` |
| 59 | `POST /holder-tokens` | ✅ | ✅ | ✅ | — | ✅ | — | — | ⚠️ inline |
| 60 | `GET /holder/balances` | ✅ | ✅ | ✅ | ✅ | — | ❌ `{list}` 第三种形状、无上限 | — | ✅（spec 与之一致地偏离契约） |
| 61 | `GET /holder/transactions` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ cursor | — | ✅ |
| 62 | `GET /holder/holds` | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ 同 #60 | — | ✅（同上） |
| 63 | `GET /holder/deposit-address` | ✅ | ✅ | ✅ | — | ✅ | — | — | ? |
| 64 | `POST /holder/deposit-address` | ✅ | ✅ | ✅ | — | ✅ | — | 天然幂等 | ? |
| 65 | `POST /dev/credits` | ✅ | ✅ | ✅ | ✅ | ✅ | — | ✅ 必填 | ⚠️ inline |
| 66 | `POST /periods/close` | ✅ | ✅ | ✅ | — | ✅ | — | ❌ 无幂等键 | ✅ |
| 67 | `GET /periods/closes` | ✅ | ✅ | ✅ | — | ✅ | ⚠️ 恒 null | — | ✅ |
| 68 | `GET /reports/trial-balance` | ✅ | ✅ | ✅ | ✅ | ✅ | — | — | ✅ |
| — | **出站** webhook POST（`service/delivery`） | ❌ 裸 `core.Event`（SSE/WS 类豁免包络，但字段规则仍适用） | ✅ | ✅ | ✅ | ❌ **Major 4** | — | 事件 uid 去重 header | ❌ 无 schema |

**统计**：68 条 REST 路由 + 1 条出站契约。包络合规 68/68（上轮探针裸 JSON 的 Minor 已修，`handler_system.go:26-32` 走 `httpx.Error`）。
spec 一致性：❌ 6 条（含 4 类系统性成因）、⚠️ 22 条（inline / 无 `required` / 未核）、✅ 40 条。
「恒 null 的 cursor 形」11 条：技术上不违反 §6（是 cursor 形且到底），但这些端点其实是不分页全量返回，形状选得容易误导。

---

## 附录 B · 索引 ↔ 查询模式对照（限本轮读过的 6 个 query 文件）

| 表 | 现有索引（`001_baseline.up.sql`） | 命中的查询与谓词 | 判定 |
|---|---|---|---|
| `journal_entries` | `idx_entries_account_id (account_holder, currency_id, classification_id, id)` `:370` | `ListComputedBalancesForHolders`（`checkpoints.sql:44-69`）：`account_holder = ANY / currency_id = / classification_id = / id > cp` | 列序匹配，**但无分区裁剪谓词**；`populated` CTE 还额外全历史 DISTINCT → **Major 6** |
| `journal_entries` | `idx_entries_journal (journal_id)` `:371` | `ListJournalEntries`（`journals.sql:55`）、`VerifyJournalBalanced`（`:141`）、`check_journal_currency_balance` trigger（`:1250-1251`） | 匹配；仍跨全部分区 |
| `journal_entries` | `idx_entries_currency_effective (currency_id, effective_at)` `:372` | `TrialBalanceRows`（`trial_balance.sql:15-16`）：`currency_id = AND effective_at <=` | **匹配**（这是它建出来的目的） |
| `journal_entries` | 同上 | `ListBalancesAt`（`checkpoints.sql:170`）：**只有** `effective_at < $1` | ❌ 无 `currency_id` 前缀 → 索引不可用，全表 GROUP BY |
| `journal_entries` | 同上 | `SumGlobalDebitCreditByCurrency`（`checkpoints.sql:151-158`）：无 WHERE，`GROUP BY currency_id, entry_type` | 全表扫描，设计如此（全局对账） |
| `journal_entries` | 无 `(account_holder, journal_id)` 索引 | `ListHolderTransactionRows` 的 `page_journals`（`holder.sql:27-37`）：`account_holder = AND j.id < cursor ORDER BY j.id DESC LIMIT n` | ❌ 无索引可同时服务过滤+去重+DESC 排序 → 每页全量扫 + 排序（Minor） |
| `journal_entries` | 同上 | `ListHolderCurrencies`（`holder.sql:113-119`）：`account_holder =` + DISTINCT | ⚠️ 走 `idx_entries_account_id` 前缀，但读全历史（Minor） |
| `balance_checkpoints` | PK `(account_holder, currency_id, classification_id)` `:404` | `GetBalanceCheckpoint` / `GetBalanceCheckpoints` / upsert（`checkpoints.sql:1-33`） | 匹配 |
| `balance_checkpoints` | 无索引服务 `MIN(updated_at)` | `GetCheckpointMaxAgeSeconds`（`checkpoints.sql:135-137`） | ⚠️ 全表聚合（`015_checkpoint_updated_at_index` 疑似正为此而加，**本轮未读**） |
| `rollup_queue` | `uq_rollup_queue_pending_dimension` `:422` / `idx_rollup_queue_pending (created_at, id) WHERE processed_at IS NULL` `:425` | `EnqueueRollup` ON CONFLICT（`:82`）、`DequeueRollupBatch`（`:88-96`：`processed_at IS NULL AND claimed_until … ORDER BY created_at, id`） | 匹配；`failed_attempts < 10` 是索引后过滤，可接受 |
| `reservations` | `idx_reservations_account_status … WHERE status='active'` `:490` / `idx_reservations_expired … WHERE status='active'` `:491` / `idx_reservations_account_created (account_holder, created_at DESC)` `:498` | `ListHolderHolds`（`holder.sql:107`：`account_holder = AND status IN ('active','settling')`） | ⚠️ 两个 partial 索引只覆盖 `active`，`settling` 落回 `idx_reservations_account_created`（无 status 前缀）→ 按 holder 扫全部预留行再过滤 |
| `journals` | PK `(id)` | `ListJournalsCursor`（`journals.sql:96-100`：`id > cursor ORDER BY id ASC`） | 匹配 |
| `journals` | `idx_journals_created` `:320` | `ListJournalsByTimeRange`（`audit_lists.sql:27-31`：`created_at >= / <=` + `ORDER BY id ASC`） | ⚠️ 过滤走 `idx_journals_created`，排序按 `id` → 需额外排序或退回 PK 扫描（本轮未跑 EXPLAIN） |
| `journals` | `idx_journals_reversal_of … WHERE NOT NULL` `:321` | `ListReversalsByOriginalJournalID`（`journals.sql:36`）、`GetReversalChain` 递归 CTE（`audit_lists.sql:48-93`） | 匹配 |
| `journals` | `uq_journals_uid` `:323` / `idempotency_key UNIQUE` `:297` | `GetJournalByUID` `:213`、`GetJournalByIdempotencyKey` `:29` | 匹配 |
| `journals` | `idx_journals_event … WHERE NOT NULL` `:322` | 本轮未在读过的 query 文件里找到使用点 | ? 可能冗余，未确认 |
| `bookings` | `idx_bookings_holder_class (account_holder, classification_id, status)` `:568` / `uq_bookings_channel_ref` `:564` / `idx_bookings_expires` `:569` | `bookings.sql` **本轮未读** | ? |
| `events` | `idx_events_delivery_pending (next_attempt_at) WHERE delivery_status='pending'` `:597` / `idx_events_booking` `:598` | `TraceBookingEvents`（`audit_lists.sql:37`）匹配 `idx_events_booking`；投递轮询本轮未读 | 部分匹配 |
| `entry_attestations` | PK `(entry_id)` `:1020` / `idx_entry_attestations_seq` `:1023` | `integrity_attestations.sql` **本轮未读** | ? （唯一性假设问题已移交 C） |

**冗余/缺失小结**（限已读范围）：疑似缺 —— `journal_entries (account_holder, journal_id)`（holder 交易分页）、`reservations` 覆盖 `settling` 的 status 索引；疑似冗余 —— `idx_journals_event`（未见使用点）；结构性 —— 分区键 `created_at` 与所有余额路径的谓词集合无交集。

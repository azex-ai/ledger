# ledger v0.4 — API 契约对齐（api-contract.md 收编）

**Created**: 2026-07-03
**Status**: Approved 2026-07-03（§6 全部拍板；uid 范围按 Aaron 裁定扩大到全部对外契约）
**权威源**: `~/.claude/rules/api-contract.md`（26-07-03 拍板的跨边界数据契约唯一 SoT）
**前置**: v0.3.1 已发布。**按全新 lib 对待（Aaron 2026-07-03 裁定）：不存在「存量数据」概念**——
不设计任何回填/兼容路径，dev 库可随时重建；迁移只需对空库正确。
**下游**: armatrix 升级计划（`armatrix-docs/plans/2026-07-03-ledger-v0.3-upgrade.md`）
**改为直接对齐本批终态（v0.4.0），只适配一次**——该文档的前置条件与适配清单在本批完成后同步修订。

---

## 0. 差距审计结论（2026-07-03，对照 api-contract.md 逐条）

| 条款 | 现状 | 判定 |
|---|---|---|
| §2 snake_case | 全库零 camelCase | ✅ 不动 |
| §4/§5/§7 金额字符串/RFC3339/枚举 | 符合 | ✅ 不动 |
| §1 包络 | 成功 `code=0`（应为 200）；错误缺 `data: null` | P1 修 |
| §6 分页 | 列表字段 `data`（应为 `list`），嵌套成 `data.data` | P1 修 |
| §3 只暴露 uid | 全 API 暴露内部 BIGSERIAL id，schema 无 uid 列 | P2 修（主工程） |
| §9 Idempotency-Key header | 用 body 字段 | §6 拍板点 1 |

## 1. P1 — 包络与分页（小改，先行合入）

**包络**（`pkg/httpx/response.go`）：

- `OK`/`Created`：`Code: 0 → 200`（`message` 维持 "ok"/"created"）
- 错误响应补 `data: null`：`ErrorBody{Code, Message, Data: nil, Retryable}`——字段顺序 code/message/data/retryable；`retryable` 是契约外的 additive 扩展，保留（消费方容忍未知字段，§8）
- 前端判断语义从「code==0」改「code==200」——`@azex/ledger-react` 的 client.ts 同步，`web/` dashboard 同步

**分页**（`server/response.go`）：

- `PagedResponse.Data → List`（JSON tag `list`），消灭 `data.data` 嵌套 → `data.list`
- `next_cursor` 命名已符合；`limit` 参数已符合；ledger 无 offset 分页场景，不引入
- openapi.yaml（两份）+ docs/api.md 同步

## 2. P2 — uid：对外标识符切换（主工程）

### 原则（Aaron 2026-07-03 裁定：主键 id 不出现在任何对外契约）

- **uid = UUIDv7 字符串，是唯一对外标识**——HTTP 路径/响应体/webhook 负载，
  **以及 library mode 的 Go 公开 API**（core 类型、接口方法参数/返回、facade）。
- **内部 BIGSERIAL id 降级为纯存储细节**：只用于 FK、join、advisory lock、
  checkpoint/cursor 序；**不出现在任何 exported 契约**——core 实体类型删除 `ID int64`
  字段，接口签名全部收 uid，store 层做一次 uid→内部 id 解析。
- **机制型字段例外**（不是实体引用，是运行机制）：`BalanceCheckpoint.LastEntryID`、
  rollup 队列、分页 cursor 的内部序——保留 int64，doc comment 注明 operational
  internal，不作为查找句柄提供。
- `journal_entries` 无独立 uid（实体身份跟随 journal）；entry 响应不再输出行 id。

### 涉及实体（migration 031，单步）

`journals`、`journal_entries`（跟随 journal，无独立 uid）、`bookings`、`events`、
`reservations`、`classifications`、`journal_types`、`entry_templates`、`currencies`、
`account_policies`、`period_closes`

```sql
-- 无存量前提：列直接 NOT NULL、无 DEFAULT 兜底——uid 一律由 Go 侧 UUIDv7 生成，
-- 不存在两种来源的 uid（插入路径漏传即报错，好事）。
ALTER TABLE journals ADD COLUMN uid UUID NOT NULL;
CREATE UNIQUE INDEX uq_journals_uid ON journals (uid);
-- 其余实体同型。
```

### Go 侧

- 依赖：`github.com/google/uuid`（v1.6+ 原生 `uuid.NewV7()`）——ledger 首个 ID 生成依赖，进 root go.mod
- core 实体类型：`ID int64` 删除、`UID string` 加入（JSON tag `uid`）；交叉引用字段
  全部换 uid（`Booking.JournalUID *string`、`Event.BookingUID`、`Journal.ReversalOfUID`、
  `JournalInput.EventUID`、`TransitionInput.BookingUID`、`SettleInput.ReservationUID`...）
- 接口签名收 uid：`GetBooking(ctx, uid string)`、`ReverseJournal(ctx, journalUID, reason)`、
  `Transition`/`Settle`/`SettlePartial`/`FinalizeSettlement`/`Release` 同理
- store：insert 生成 V7；读写路径入口做一次 uid→内部 id 解析（unique 索引点查）；
  sqlc 加 `*ByUID` 查询族
- **HTTP 层**：路径全部换 uid——`/journals/{uid}`、`/bookings/{uid}/transition`、
  `/reservations/{uid}/settle-partial`...
- **响应体**：无任何 `id` 键；关联引用 `journal_uid`/`reservation_uid`/`booking_uid`/
  `reversal_of_uid`；cursor 分页游标继续基于内部序（base64 不透明串，不暴露语义，合规）
- **出站 webhook 事件负载**：`event.uid` + `booking_uid` + `journal_uid`；
  消费者去重键从 `X-Ledger-Event-ID` 换 `X-Ledger-Event-UID`（header 同步改名）
- **ledger-cli / examples / COOKBOOK**：全部换 uid 口径（CLI 保留 `--internal-id`
  运维逃生舱，直查存储层，不经公开 API）
- **service/ 内部编排**（rollup/reconcile/snapshot/expiration）继续用内部 id——
  它们是 composition root 内的机制，不是消费者契约

### 前端

- `@azex/ledger-react`：types.ts 全部 Item 换 `uid: string`；client.ts 路径换 uid；
  hooks 的 queryKey 换 uid —— 一次性破坏（npm 包 minor bump `ledger-react-v0.2.0`）
- `web/` dashboard 页面跟随

### 不变量

- **I-18（新增）**：每个对外实体行有且仅有一个不可变 uid；uid 在 API 表面是唯一标识，
  内部 id 永不出现在任何响应体 / 路径 / webhook 负载。
  Pin：全 handler 响应体 JSON 断言无 `"id":` 键（server 集成测试遍历端点做机械扫描）
  + uid 幂等（同一行重复读 uid 恒定）

## 3. P3 — 收尾

- openapi 两份全量同步（路径参数、schema、examples）；`info.version → 0.4.0`
- docs/api.md、COOKBOOK、INVARIANTS、RUNBOOK 里的示例与查询全部换 uid 口径
- CHANGELOG `[0.4.0]`（Breaking：包络 code、分页字段、uid 切换、header 改名）
- tag `v0.4.0` + `ledger-react-v0.2.0`

## 4. 实施顺序与规模

| Phase | 内容 | 规模 | 依赖 |
|---|---|---|---|
| P1 | 包络 + 分页 + 前端适配 | 小（半天内） | 无 |
| P2 | migration 031 + core/store/HTTP/webhook/前端 uid 切换 | 大（一个完整批次，可拆 3 个 agent：schema+store / HTTP+webhook / 前端） | P1 合入后 |
| P3 | 文档 + 发版 | 小 | P2 |

全程金融硬约束不动：append-only、双分录、幂等语义、decimal 精度与 v0.3 全部不变量继续有效（I-1~I-17 的 pin 测试是本批的回归安全网）。

## 5. armatrix 计划同步修订（本批完成后）

`armatrix-docs/plans/2026-07-03-ledger-v0.3-upgrade.md` 改为「upgrade to v0.4.0」：

- 前置条件：`v0.3.0` → `v0.4.0`
- 适配清单追加：**ledger_adapter 持有的 ledger 引用列 bigint → uuid**（reservation/
  journal/event 引用；无存量，直接改列类型 + 调用点换 uid 字符串），以及接口签名跟随
- armatrix 自己的 API 契约工作（包络统一 + Twitter camelCase 边界归一）保持在该计划内

## 6. 拍板点

1. ✅ **Idempotency-Key（拍板通过）**：body 字段为 SoT，HTTP 层接受 `Idempotency-Key`
   header 别名（header 有且 body 缺 → 注入；两者同存且不一致 → 400）。
2. ✅ **uid 全覆盖（Aaron 裁定，推翻原推荐）**：主键 id 不出现在任何对外契约——
   包括 library mode 的 Go 公开 API。armatrix adapter 需一次 bigint→uuid 列适配
   （无存量，代价可控）；每次调用多一次 unique 索引点查，可接受。
3. ~~存量 uid 回填~~ **已消解**（无存量裁定）：uid 全部 Go 侧 UUIDv7，无回填、无双来源。

---

## 附：P2 执行进度（2026-07-03，供接续会话使用）

已完成（编译通过）：
- P1 已合入 main（commit 3118bac）。P2 在 main 工作树进行中（未提交；有 stash 备份 "wip-backup"）。
- migration 031（uid 列+唯一索引，NOT NULL 无 DEFAULT）；全部 INSERT 查询带 uid 参数；
  Deactivate*/GetCurrency 等查询已改 `WHERE uid=$1`；entry 列表查询 JOIN journals 取 journal_uid；
  新增 GetJournalUIDByID / GetEventUIDByID / GetEventIDByUID / *ByUID 查询族 / *Dims 缓存查询。
- `core/` 全包 uid 化并编译通过（实体 UID、交叉引用 *UID、接口签名、AuditFilter.Cursor 改 string）。
- `service/` 全包编译通过（ClassificationLister 端口改 ClassificationDims/CurrencyIDByUID/CurrencyUIDByID；
  expiration/reconcile/snapshot/system_rollup 已适配；delivery 用 PendingEvent{InternalID,Event}，
  header 改 X-Ledger-Event-UID）。
- `postgres/`：dims.go（per-pool id<->uid 缓存）、convert.go（newUID/uidToPG/pgToUID、
  journalFromRow(ctx,dims,q)、entryCore、classification/journalType/currency/template/
  accountPolicy/periodClose mapper 已 uid 化）；classification/currency/template/period/
  account_policy 五个 store 已完成。

待做（按序）：
1. postgres/ledger_store.go：加 dims 字段；postJournalWithQueries 顶部把 EntryInput 的
   currency/classification uid 一次性 resolve 成 resolvedEntry（内部 id+exponent+normalSide），
   下游 precision/policy/locks/insert 全改用 resolved；JournalTypeUID/EventUID/ReversalOfUID
   resolve；GetBalance(holder, currencyUID, classificationUID)；ReverseJournal(uid)；
   ExecuteTemplate 路径；journalFromRow 调用点带 (ctx, s.dims, q)。
2. postgres/precision.go / account_policy_enforce.go：签名改收 resolvedEntry（含 exponent/
   normalSide/内部 id），删除对 core.EntryInput 内部 id 的引用。
3. postgres/idempotency_match.go：entries 比对改在 resolved-id 空间或 uid 空间做一致比较。
4. postgres/booking_store.go / event_store.go（GetPendingEvents 返回 delivery.PendingEvent，
   转换需要 booking/journal uid join 或查询）/ reserver_store.go（uid 签名）/
   reversal_fraction_store.go / pending_store.go / audit_store.go / query_provider.go /
   platform_balance_store.go / rollup_adapter.go（新增 ClassificationDims/CurrencyIDByUID/
   CurrencyUIDByID 实现 + snapshot/trends/trial/reconcile 查询的 uid 边界）。
5. server/（handler 路径 {uid}、响应无 id 键）、ledger.go facade、cmd/ledger-cli、examples。
6. 测试大军：internal/postgrestest seed helpers 改为“插入带 uid 并返回 uid 字符串”，
   全部测试的 `.ID`→`.UID`、字段名 CurrencyID:→CurrencyUID: 等机械迁移。
7. I-18 pin（响应体无 "id":" 键的机械扫描测试）+ openapi 全量重写 + ledger-react uid 化 +
   CHANGELOG [0.4.0] + tag v0.4.0 / ledger-react-v0.2.0；armatrix 计划改“直升 v0.4.0”。

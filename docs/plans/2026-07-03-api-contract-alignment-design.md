# ledger v0.4 — API 契约对齐（api-contract.md 收编）

**Created**: 2026-07-03
**Status**: Draft — pending Aaron review（§6 三个拍板点）
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

### 原则

- **内部 BIGSERIAL id 保留**（FK、join、advisory lock、checkpoint 序都依赖它），只是**从对外表面消失**
- **uid = UUIDv7 字符串**，每个对外可见实体一列：`uid UUID NOT NULL UNIQUE`
- **作用域 = ledger 的对外表面**：HTTP 路径参数、响应体、出站 webhook 事件负载。
  **Library mode 的 Go 接口保持 int64**（armatrix 这类进程内消费者持有的是内部引用，
  不是「外部 API」；它们自己的对外 API 用它们自己的 uid）——见 §6 拍板点 2

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
- core 类型加 `UID string` 字段（JSON tag `uid`）；insert 路径由 store 生成 V7
- store 加按 uid 查询：`GetJournalByUID` / `GetBookingByUID` / ...（sqlc）
- **HTTP 层**：路径全部换 uid——`/journals/{uid}`、`/bookings/{uid}/transition`、
  `/reservations/{uid}/settle-partial`...；handler 内 uid→内部 id 解析一次后走原逻辑
- **响应体**：`id` 字段删除，输出 `uid`；关联引用同步（`journal_uid`、`reservation_uid`、
  `booking_uid`、`reversal_of_uid`）；cursor 分页的游标继续基于内部 id（base64 不透明串，
  不暴露语义，合规）
- **出站 webhook 事件负载**：`event.uid` + `booking_uid` + `journal_uid`；
  消费者去重键从 `X-Ledger-Event-ID` 换 `X-Ledger-Event-UID`（header 同步改名）
- **ledger-cli**：查询参数接受 uid（运维排查仍可用 `--internal-id` 逃生舱直查内部 id）

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
- 适配清单追加：无（armatrix 走 library mode，Go 接口 int64 不变；HTTP 包络/uid
  变更不触及它）——**这正是把两批合并成一次升级的收益**
- armatrix 自己的 API 契约工作（包络统一 + Twitter camelCase 边界归一）保持在该计划内

## 6. 拍板点

1. **Idempotency-Key：body 字段 vs header（契约 §9）**。ledger 现状是 body 字段
   `idempotency_key`，且「同 key + 同 payload 幂等返回 / 同 key + 异 payload → ErrConflict」
   的比对语义天然绑定 body。**推荐：ledger 保持 body 字段为 SoT，HTTP 层额外接受
   `Idempotency-Key` header 作为别名**（header 存在且 body 缺失时注入 body；两者同时存在
   且不一致 → 400）——契约 §9 的精神（每次写操作显式幂等）满足，比对语义不丢。
   若你要严格 header-only，payload 比对改为「key → 请求体哈希」存表，工程量 +1 张表。
2. **Library mode 接口保持 int64**（uid 只在 HTTP/webhook 表面）。推荐如此——armatrix
   的 ledger_adapter 零改动；反之全接口切 uid 会让每个进程内调用多一次 uid→id 解析。
3. ~~存量 uid 回填~~ **已消解**（无存量裁定）：uid 全部 Go 侧 UUIDv7，无回填、无双来源。

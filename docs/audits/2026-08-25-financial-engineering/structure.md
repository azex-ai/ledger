# 审计报告 — structure（收窄范围：Q1 六边形边界 / Q2 api-contract 合规 / Q3 消费方破坏检测）

## 摘要

`core/` 对 `net/http`/`pgx`/`slog`/`chi` 是干净的，观测性（Logger/Metrics）走接口注入，六边形边界本身没破。但**两个composition root（`ledger.go` 库门面 与 `cmd/ledgerd/main.go` 服务二进制）在能力接线上确认存在分叉**：P5/P6 整套"防篡改"子系统（per-journal 签名 + 批量 attestation + 提现验证闸）在库门面上可以被消费方装配，但 `ledgerd` 从未调用 `ledger.WithAttestor`、从未 `worker.SetAttestor`、HTTP 层也没有任何 endpoint 能设置 `ReserveInput.RequireVerifiedBalance` —— 这意味着如果这个 HTTP 服务是唯一部署形态，整个 tamper-evident 设计在生产里形同虚设，且没有任何测试或门禁会告诉你这件事（这条本身跨到 D territory 的威胁模型，已在"移交"里写明，这里只报告结构事实）。api-contract 合规上找到两类**系统性**违规：cursor 分页的 `next_cursor` 从未真正序列化为 `null`（要么被 `omitempty` 吞掉，要么退化成空字符串），以及至少一处 `openapi.yaml` 与 handler 实际字段名/类型的具体漂移（`expires_in_sec` int64 秒 vs 文档里的 `expires_in` duration 字符串）。Q3 最要紧的发现：`docs/openapi.yaml` 唯一的 CI 门禁（`codegen:check`）只回答"TS 生成物是否等于 openapi.yaml"，从未回答"openapi.yaml 是否等于 Go handler 的真实行为"——且这个门禁本身还是 path-filtered，纯 Go 改动完全不会触发它，也不在 npm 发布门禁（`ledger-react-publish.yml`）里执行。

## 发现

### [Major] `docs/openapi.yaml` 唯一的 CI 门禁只验证「TS 生成物 = spec」，从不验证「spec = Go 真实行为」；且该门禁 path-filtered、发布流水线里还缺席
- **位置**：`.github/workflows/ledger-react.yml:6-16`（`on.push.paths`/`on.pull_request.paths` 只含 `web/**`、`docs/openapi.yaml`、workflow 自身）；`web/packages/ledger-react/package.json:51-52`（`"codegen": "openapi-typescript ../../../docs/openapi.yaml -o src/client/schema.ts"`、`"codegen:check": "npm run codegen && git diff --exit-code src/client/schema.ts"`）；对照 `.github/workflows/ledger-react-publish.yml:15-49`（`verify` job 只跑 `typecheck`/`build`/`test`/产物断言，没有 `codegen:check` 这一步）
- **判定**：CONFIRMED
- **失效场景**：`codegen:check` 的实现是"用 `docs/openapi.yaml` 重新生成 `schema.ts`，diff 一下有没有变化"——它的两端都是**同一份仓库里的静态文件**，从未包含"启动 `cmd/ledgerd` 打一发真实请求，对比响应体与 spec"这类契约测试。这意味着：① 只要 PR 只改了 `server/*.go`（没碰 `web/**` 或 `docs/openapi.yaml`），`ledger-react.yml` 这个 workflow 根本不会被触发——Go 侧对 wire 格式的任何改动（重命名字段、改枚举值、加减字段）如果作者忘了同步手改 `docs/openapi.yaml`，CI 全绿，没有人会知道 spec 已经跟不上代码了；② 即使某次 PR **确实**改了 `docs/openapi.yaml`（比如手工修正一处笔误）但改得依然不对，`codegen:check` 照样通过，因为它只关心"TS 是不是从这份（可能本身就是错的）YAML 生成出来的"；③ 到了真正发布 npm 包的那一刻（`ledger-react-publish.yml`，tag 触发），连这层"至少 TS 和 YAML 自洽"的检查都不跑了——`verify` job 里没有 `codegen:check` 这一步，发布门禁比日常 PR 门禁更松。上文两条已确认的具体漂移（`next_cursor` null 语义、`expires_in` vs `expires_in_sec`）之所以能潜伏到现在，根源就是这条门禁链条从头到尾没有一个节点摸到过"Go handler 实际吐出的 JSON"。
- **最小复现**：手改 `server/handler_reservations.go`（把 `json:"account_holder"` 改名成别的），不改 `docs/openapi.yaml`，不改 `web/`，提一个 PR——`ci.yml`（Go 侧）会跑但它不检查 wire 格式漂移；`ledger-react.yml`（唯一提到 openapi.yaml 的门禁）因为 path 过滤器不匹配，根本不会触发；PR 可以合并。
- **为什么现有机制没拦住**：这本身就是"现有机制没拦住"的完整答案——不存在第三方门禁去闭合这个链条（比如：contract test 启动 `ledgerd`、打真实请求、用 `docs/openapi.yaml` 做 runtime 校验）。

### [Major] `ledgerd`（唯一的 HTTP 服务部署形态）从未接线 P5/P6 防篡改子系统，且 HTTP 层结构上无法开启提现验证闸
- **位置**：`cmd/ledgerd/main.go`（整个文件，`ledger.New(pool, ledger.WithLogger(...), ledger.WithMetrics(...))` 调用处，未见 `ledger.WithAttestor`）；对照 `ledger.go:129-134`（`WithAttestor` 定义）、`server/handler_reservations.go:14-20`（`createReservationRequest` 结构体，无 `require_verified_balance` 字段）、`core/reserve.go:73`（`ReserveInput.RequireVerifiedBalance bool`）
- **判定**：CONFIRMED
- **失效场景**：`grep -rn "WithAttestor" --include="*.go" . | grep -v _test.go` 显示唯一调用点是 `examples/tamper-evident/main.go:120`；`grep -rn "RequireVerifiedBalance" --include="*.go" .` 显示唯一把它设为 `true` 的地方同样是这个 example。任何人部署 `cmd/ledgerd`（本仓库里唯一现成的可运行服务二进制）作为生产 HTTP 服务，得到的是：① 每笔 journal 都以未签名状态写入（`ledger.go:90-91` 注释自陈"attestor == nil 是默认，每笔 journal 都不签名"）；② `worker.SetAttestor` 从未被调用，P6 批量 attestation job 永远不跑（`service/worker.go:147-152` 的 `SetAttestor` doc comment 自陈"不设置则整个 job 被跳过"）；③ HTTP `POST /reservations` 的请求体结构上就不存在能让调用方开启 `RequireVerifiedBalance` 的字段，即便消费方想要这道"提现前必须验证过签名"的闸门也做不到——必须绕开 HTTP 服务、直接用 Go 库模式手写 `core.ReserveInput{RequireVerifiedBalance: true}` 才行得通。`docs/plans/2026-08-21-tamper-evident-ledger-design.md` 整份设计文档描述的安全保证，在"跑 `cmd/ledgerd` 当生产服务"这个最直觉的部署路径下，没有一条真正生效。
- **最小复现**：`go run ./cmd/ledgerd`（配好 DATABASE_URL）→ `POST /api/v1/reservations` 建一个 reservation → 查 `journals` 表：`auth_signature`/相关签名列（若有）为空；查 `attestations` 表：从未有新行插入，因为 worker 的 attestation loop 从未注册。
- **为什么现有机制没拦住**：没有任何集成测试以"跑 `cmd/ledgerd` 主二进制"为起点去断言签名/attestation 在跑；`examples/tamper-evident` 只证明库模式下"如果你手动装配"这条路径可行，从未证明"默认可运行的服务二进制"也这样做。这正是 territory C（消费方表面）在契约里点名的同类缺陷（"对外 API 从未真实工作过，而测试是绿的"）在**服务而非库**层面的镜像。

### [Major] Cursor 分页的 `next_cursor` 从未真正序列化为 JSON `null`，系统性偏离 `api-contract.md` §6
- **位置**：`server/response.go:10-13`（`PagedResponse[T]{ NextCursor string \`json:"next_cursor,omitempty"\` }`）；`server/handler_holder.go:144-146`（`holderTransactionsPage{ NextCursor string \`json:"next_cursor"\` }`，无 `omitempty`）；`docs/openapi.yaml:1340,1481,1515,1553,1707,1900` 处处写 `next_cursor: { type: string, description: "...absent/empty when exhausted..." }`
- **判定**：CONFIRMED
- **失效场景**：`api-contract.md` §6 明文规定 cursor 分页到底时 `"next_cursor": null`（显式 null，用来给消费方一个可判等的哨兵）。本仓库两条独立实现路径都无法产出这个值：`PagedResponse[T]` 用 `string` 类型 + `omitempty`，到底时**整个字段从 JSON 里消失**；`holderTransactionsPage` 用 `string` 不带 `omitempty`，到底时序列化成 `"next_cursor": ""`（空字符串，而不是 null）。因为字段类型从来不是 `*string`，这个代码库结构上**无法表达** JSON `null`——这不是某一个 handler 漏写，是两个不同的分页响应类型都选错了字段类型。前端如果照 `api-contract.md` 写 `if (data.next_cursor === null)` 来判断翻页结束，两条路径都会误判（一个拿到 `undefined`，一个拿到 `""`，都不是 `=== null`）。`docs/openapi.yaml` 与代码在这件事上互相印证、一起错——spec 没有documented出公司统一契约要求的 null 语义，而是把代码的实际行为原样写进了文档。
- **最小复现**：任意 `GET /api/v1/journals?limit=200`（数据量小于 200 条，一页取完）→ 响应 `data.next_cursor` 要么不存在这个 key，要么是 `""`，两者都不是契约要求的 `null`。
- **为什么现有机制没拦住**：`codegen:check`（见 Q3）只保证 `schema.ts` 与 `docs/openapi.yaml` 一致，而 `docs/openapi.yaml` 本身就是照着这个（不合公司契约的）行为写的，所以这条门禁在设计上就不可能抓到这个问题——它比对的两端都在同一个错误假设下达成了一致。没有任何测试断言过 `next_cursor` 在"最后一页"场景下的**JSON wire 层**取值是字面 `null`。

### [Minor] `docs/openapi.yaml` 与 handler 实际字段存在具体、可复现的漂移（字段名 + 类型都不一致）
- **位置**：`docs/openapi.yaml:1565`（`ReserveInput.expires_in: { type: string, description: 'Go duration string (e.g. 1h, 30m).' }`）对照 `server/handler_reservations.go:19`（`createReservationRequest.ExpiresInSec int64 \`json:"expires_in_sec"\`，单位是秒的整数，字段名也不同）
- **判定**：CONFIRMED
- **失效场景**：任何按 `docs/openapi.yaml` 生成/手写客户端代码的消费方，会往 `POST /api/v1/reservations` 发一个 `{"expires_in": "1h"}` 的请求体；handler 用 `httpx.Decode[createReservationRequest]` 解码，`expires_in` 这个 key 在目标结构体里根本不存在（结构体字段是 `expires_in_sec`），JSON 解码器默默丢弃未知字段（`api-contract.md` §8 明确要求"消费方必须容忍未知字段"，这里踩的是反向坑——是**服务端**丢弃了消费方按文档发来的字段），结果是 `ExpiresInSec` 保持零值 `0`，`time.Duration(0)*time.Second = 0`，reservation 会以 0 过期时间创建（`core/reserve.go` 里 `ExpiresIn` 为 0 时的具体兜底行为本报告未验证，见"我没能验证的"）。
- **最小复现**：照 `docs/openapi.yaml` 文档发送 `POST /api/v1/reservations {"account_holder":1,"currency_uid":"...","amount":"10","idempotency_key":"k1","expires_in":"1h"}` → 对比同一请求把 `expires_in` 换成 `expires_in_sec: 3600` → 两次调用产生的 `expires_at` 不同。
- **为什么现有机制没拦住**：`codegen:check` 只诊断 `schema.ts` 是否等于 `openapi.yaml`，从不诊断 `openapi.yaml` 是否等于 Go 源码里真实的 JSON tag；这条门禁的两端压根不包含"运行中的 `ledgerd`"或"`server/handler_*.go` 源码"，所以这类漂移永远不会被 CI 感知（见 Q3 主发现）。

### [Major] `core.BalanceCheckpoint`（Go 库公开 API 的返回类型）把内部 BIGSERIAL 主键当 `currency_id`/`classification_id` 直接返回，与同一份 API 的入参、以及紧邻的姊妹类型都不一致
- **位置**：`core/checkpoint.go:10-18`（`BalanceCheckpoint{ CurrencyID int64, ClassificationID int64, ... }`）；对照 `core/interfaces.go:143`（`CheckpointIntegrityStore.RebuildCheckpoint(ctx, holder int64, currencyUID, classificationUID string, actorID int64) (*BalanceCheckpoint, error)` ——**入参**用的是 `currencyUID`/`classificationUID` 字符串）；再对照 `core/checkpoint.go:34-40`（同文件里紧挨着的 `BalanceSnapshot{ CurrencyUID string, ClassificationUID string, ... }`，正确模式）；schema 侧证据：`postgres/sql/migrations/001_baseline.up.sql:156-180`（`currencies.id BIGSERIAL PRIMARY KEY` / `classifications.id BIGSERIAL PRIMARY KEY`，各自另有独立的 `uid UUID` 列并建了 `uq_currencies_uid`/`uq_classifications_uid` 唯一索引——即这两个 id 就是 `api-contract.md` §3 说的"内部 BIGSERIAL id，对外只暴露 uid"里的那个 id）
- **判定**：CONFIRMED
- **失效场景**：`svc.CheckpointIntegrity().RebuildCheckpoint(ctx, holder, "curr-uid-xxx", "class-uid-yyy", actorID)` 这个库消费方唯一能拿到 `*BalanceCheckpoint` 的公开入口——传参时规规矩矩传了 uid 字符串，拿回来的结构体里却是内部数据库自增整数（`CurrencyID`/`ClassificationID`），且没有任何字段告诉调用方"这两个数字对应哪个 uid"。消费方如果想把这个返回值序列化给自己的前端/日志/审计记录（这正是 `RebuildCheckpoint` 文档自称的用途——"每次调用必须留下取证记录"），要么被迫自己再反查一次 `currencies`/`classifications` 表把 id 翻译回 uid，要么就把内部自增 id 原样泄露出去——这正是 `api-contract.md` §3"内部 BIGSERIAL id 永不出现在任何 API"想防的事，而且是在"含 Go API"的边界上违反的。同一个文件里的 `BalanceSnapshot`（`checkpoint.go:34-40`）恰恰示范了正确做法（`CurrencyUID string`/`ClassificationUID string`），说明这不是"这个概念没法用 uid 表达"，而是 `BalanceCheckpoint`/`RollupQueueItem`（`checkpoint.go:20-31`，同样是 `CurrencyID`/`ClassificationID int64`）这两个类型单独漏做了。
- **最小复现**：调用 `svc.CheckpointIntegrity().RebuildCheckpoint(ctx, 123, "<currency-uid>", "<classification-uid>", 0)`，打印返回值——`CurrencyID`/`ClassificationID` 是两个裸整数，与传入的 uid 字符串对不上、也无法从返回值反推出传入时用的是哪个 uid。
- **为什么现有机制没拦住**：没有测试断言过 `BalanceCheckpoint`（或它的 JSON 序列化形态）不包含内部 id；`RollupQueueItem`/`BalanceCheckpoint` 都不经过 HTTP 层（未在 `server/handler_*.go` 中出现），所以 `api-contract.md` 的"逐端点核"这条常规审计路径天然覆盖不到——它们只活在 Go 库公开 API 面，需要专门去读 `ledger.go` 导出方法的返回类型才能发现，这正是这次被点名重点核的原因。

### [Minor] Health/Ready 探针失败路径绕开了统一响应信封
- **位置**：`server/handler_system.go:22-37`（`handleHealth` 的 DB-down 分支）、`server/handler_system.go:39-50`（`handleReady` 的 not-ready 分支）——两处都是 `w.Write([]byte(`{"status":"degraded",...}`))` 之类的裸 JSON，而不是走 `httpx.Error`/`httpx.OK`
- **判定**：CONFIRMED
- **失效场景**：`api-contract.md` §1 原文"所有 REST 响应，无例外"都必须是 `{code, message, data}` 三字段信封。这两个探针端点在成功路径（`httpx.OK(w, healthResponse{...})`）遵守了信封，但失败路径（503）手写了一个完全不同形状的 body（`{"status":"degraded","db":"down"}`），既没有 `code`/`message`/`data`，也不是 bizcode 错误码体系的一部分。严格来说是信封契约的一个例外口子。
- **最小复现**：把 DB 连不上（或没启动 `IsReady`），`curl /api/v1/system/health` 与 `curl /api/v1/system/ready`，返回体不含 `code`/`message`/`data` 字段。
- **为什么现有机制没拦住**：探针端点通常不被当作"业务 API"看待，也没有消费方测试去解析它们的 body 结构（一般只看 HTTP status code），所以这个信封缺口至今没有暴露出实际影响；但契约文本本身没有为探针开例外，是一个可以指出的不一致点。

### [Minor] 部分 port 超出 golang.md 建议的 1-3 方法上限（结构可讨论，非必然是 bug）
- **位置**：`core/interfaces.go:12`（`JournalWriter`：`PostJournal`/`PostAuthorized`/`ExecuteTemplate`/`ReverseJournal`/`ReverseJournalFraction`/`AuthorizeReversal`，6 个方法）；`core/interfaces.go:248`（`ClassificationStore`：`CreateClassification`/`GetByCode`/`DeactivateClassification`/`ListClassifications`/`SetBalanceRole`/`SetDisplayLabelIfEmpty`/`SetLifecycleIfEmpty`，7 个方法）
- **判定**：CONFIRMED（方法数量是事实；是否构成"胖接口"问题是判断题，不强行定性）
- **失效场景**：这不直接导致算错钱，但正是契约点名要问的"1-3 方法之外的胖接口"。`JournalWriter` 的 6 个方法确实全部落在"journal 的写入/授权/冲销"这一个内聚职责范围内，把它们拆成更小接口会让 `postgres.LedgerStore` 这个唯一实现方需要同时满足好几个小接口——收益不明显；但对"消费方只想用其中一两个能力"的场景（比如只想 `PostJournal`，不需要冲销），没有更细粒度的接口可选，任何 mock/替代实现都必须实现全部 6-7 个方法。
- **最小复现**：无需复现，读接口定义即可确认方法计数。
- **为什么现有机制没拦住**：golang.md 的"1-3 方法"是指导性红线，没有对应的静态检查（如 `go vet` 自定义规则或 lint）强制执行，纯靠人工 review 把关。

## 移交

- 上面第一条"[Major] ledgerd 从未接线 P5/P6"本质是安全保证在生产部署下不生效——建议 D territory（threat-model）在其威胁模型评估里显式确认这一点，判断这是不是本波的 Critical（如果威胁模型的前提是"防篡改能力已经在生产跑着"，而实际没跑，威胁模型本身可能需要重写）。
- `postgres/verified_balance_store.go:140` 那条错误信息（"no AuthVerifier configured...journal cannot be confirmed authorized"）是不是在生产环境下的默认路径上被静默吞掉、还是真的会 fail-closed 传导到调用方——涉及 B（并发/事务）或 D（威胁模型）的调用链追踪，我没有走完，标在下面。
- `AccountHolder`/`ActorID`/`HolderID` 这批 `int64` 字段虽然表面看像"内部 id 泄露"，但经核实是调用方自己命名空间的外部标识（schema 里 `account_holder` 只是 `BIGINT NOT NULL CHECK <> 0`，没有对应的内部 `account_holders` 表和 FK），是一致的设计决策而非契约违规——已排除，不需要 A/D territory 重复核实。

## 我没能验证的

- `core.ReserveInput.ExpiresIn` 为 `0`（Duration 零值）时 `postgres/reserver_store.go` 的具体兜底行为——是拒绝创建、还是产出一个"已过期"或"永不过期"的 reservation？没有走完这条调用链，所以上面 [Minor] 那条"`expires_in` 文档漂移"的**具体金额/业务后果**（例如是否可能被利用来创建一个从不过期从而永久锁死资金的 reservation）还停留在"字段丢失"这一层，没有验证到"丢失后系统真正做了什么"。
- `postgres/verified_balance_store.go:140` 返回 `ErrUnauthorizedJournal` 之后，沿着 `Reserve` 的调用链是否真的会让 HTTP 层的 `POST /reservations` 返回错误（还是被某处 swallow）——没有走完。
- schema/索引/分区策略是否匹配查询模式——team lead 把本次范围收窄到三个问题，这条不在收窄后的范围内，我没有看。
- 除 `next_cursor` 与 `expires_in` 之外，`docs/openapi.yaml` 全文（约 1900+ 行）逐字段对照全部 ~50 个端点的 handler 源码——受限于本次时间，我只对分页字段（系统性问题，覆盖全部 list 端点）和 reservation 创建（team lead 点名的 attestation 相关路径）做了逐字核对，其余端点只抽查了字段命名模式（`_id` 后缀扫描）、时间格式调用点、错误信封实现，没有逐端点通读 handler body 对照 openapi.yaml 的每个 schema。
- 时间戳 UTC 一致性：`server/handler_bookings.go:91-92` 用 `.Format(time.RFC3339)` 未显式 `.UTC()`，而 `server/handler_holder.go` 多处显式 `.UTC()` 后再 Format——两者是否都能保证输出带 `Z` 后缀而非 `+08:00` 之类的 offset，取决于 pgx 对 `TIMESTAMPTZ` 列返回的 `time.Time` 的 Location 是否总是 UTC，我没有去查 pgx/pgxpool 的时区配置来确认，这条只能标 PLAUSIBLE 且未展开成正式发现。

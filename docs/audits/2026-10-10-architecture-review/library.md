# Ledger 库消费与边界审查

日期：26-10-10。基线：`d00fdebef4199d331787057127cc29a0ecb7cd87`。
Worktree：`/Users/aaron/projects/_worktrees/ledger/codex/review-library-261010-r1`；分支：`codex/review-library-261010-r1`。

本报告只审查 Go facade、HTTP 契约与权限边界、React 包消费，以及它们如何支撑通用多 currency 账本。未修改业务源码、未提交、未发布。遵循 financial-engineering profile：输入为用户目标及当前代码；产出为可复现 findings；交接给主审整合；主审回查证据后裁定；本次没有改写 Hive 规则。

## 结论

现有代码已经具备“所有可计量单位统一为 Currency、换算由外部提供、复式账本只记录结果”的主要结构，不需要推倒重建。真正需要优先修的是跨 Go/JSON/TypeScript 的身份与请求契约，以及让消费方更难把几个正确原语组合成错误资金流程。

这次找到 3 个可复现 bug，其中 holder ID 精度问题影响记账对象，优先级最高。另有 4 项扩展/设计边界，应单独排期，不能冒充已经发生的账务错误。根 Go 库和独立 npm tarball 均能够在仓库工作区之外消费，不能再沿用旧审计“主库不可独立引入”的结论。

已阅读三轮历史审计 README/disposition、R3 consumer 与 recheck，以及 `docs/plans/2026-09-07-rate-quoter-and-exchange.md`、消费优化计划。本文不重报已关闭的 root postgrestest 伪版本、模板保护、clone 守卫、请求响应错误包络等问题。

## 已确认 bug

### L-1 · P1 / Major：合法 int64 holder ID 经 React 转成另一账户 ID

**类型：跨端契约 bug。**

源码链：

- `core/booking.go:37,53`：`AccountHolder` 为 `int64`，校验仅拒 0；`core/template.go:40,132`：`HolderID int64`，仅要求用户侧 ID 为正。不存在 JavaScript 安全整数上限。
- `docs/openapi.yaml:3134`：模板输入 `holder_id` 声明为 `integer/int64`，没有安全整数范围。
- `web/packages/ledger-react/src/client/client.ts:186-196`：`postTemplateJournal` 收 `holder_id: number`，直接 `JSON.stringify`。
- shadcn `src/components/pages/JournalsPage.tsx:159-164` 和 HeroUI `src/heroui/pages/JournalsPage.tsx:180-185`：用户输入经 `parseInt`，只检查 `isNaN`。
- 同类面包括 client 的 `postJournal.entries[].account_holder`（:172）、`getBalances(holder: number)`（:223）、批量 holder（:238）、reservations（:259）。这是同一身份契约的横向问题。

**触发条件：** 宿主使用大于 `Number.MAX_SAFE_INTEGER` 的合法 int64 账户 ID，例如 `9007199254740993`；运营人员在包提供的模板记账页面输入该 ID，或消费方把 Go 返回的数字 holder 经普通 JSON 解码后回传。

**复现：** 构建后的真实 client，按页面同样的 `parseInt` 路径调用 `postTemplateJournal`，fetch 截获的请求为：

```text
intended: 9007199254740993
actual JSON holder_id: 9007199254740992
```

Go probe 同时验证 `CreateBookingInput{AccountHolder:9007199254740993,...}.Validate()` 返回 nil，证明这是账本承诺接受的身份，并非非法输入。两个 ID 均可在 Go/PG 的 int64 范围内表示。测试没有向真实数据库记账；已直接证实请求的收款/扣款对象发生改变，实际记错对象以该请求继续进入正常记账执行为条件。

**影响：** 双分录平衡检查依然能全部通过，因为变化的是账户身份而非金额。Snowflake 等大整数宿主身份若直接用于 holder，这会成为集成阻断项。

**建议：** holder 在 wire 和 TS 中统一为十进制字符串，或映射为公开 UID；内部可继续 int64。迁移同时覆盖路径参数、请求、响应、actor 等相关外部身份，并用大于 2^53 的两个相邻值做端到端负向测试。过渡期页面与 numeric wire 输入必须显式拒绝不安全整数；这能避免错账，但不等价于支持全 int64。不要只把页面的 `parseInt` 改成 `BigInt`，随后再转回 number。

### L-2 · P2 / Major：React client 的预留时长字段被 Go 静默忽略

**类型：请求契约 bug。**

源码链：

- `web/packages/ledger-react/src/client/client.ts:258-267`：`createReservation` 暴露 `expires_in?: string`。
- 已生成的 `src/client/schema.ts:3440-3442`：正确字段是 `expires_in_sec?: number`，另有 `require_verified_balance?: boolean`；但 client 方法没有消费该生成输入类型。
- `server/handler_reservations.go:14-26,100-108`：只读取 `expires_in_sec`，转换为 `time.Duration`。
- `pkg/httpx/response.go:85-93`：普通 JSON decode 允许未知字段，因此不会因 `expires_in` 报错。
- `postgres/reserver_store.go:614-624`：零时长按 15 分钟处理。

**触发条件：** TypeScript 消费方按包公开方法签名调用 `createReservation({... expires_in: "1h"})`。

**复现：** 真实构建 client 发出 `expires_in:"1h"`；该 payload 发给真实 server router/handler（Reserver 为记录输入的替身），HTTP 201，但 `ReserveInput.ExpiresIn=0s`。对照正确 `expires_in_sec:3600` 得 HTTP 201、`ExpiresIn=1h0m0s`。数据库默认分支已读码核实为 15 分钟，本 probe 不启动数据库。

**影响：** 希望预留 1 小时的宿主只持有 15 分钟，可能在业务仍执行时释放余额；希望短于 15 分钟的宿主则持有过久。请求成功，没有任何失败信号。正确 `expires_in_sec` 对直接 object literal 调用又不符合公开 TS 方法签名。

**同根功能缺口：** `require_verified_balance` 在 HTTP/生成 schema 已有，手写 `createReservation` 签名未暴露。不要误报 HTTP 本身没有该闸；这是 React client 不完整。

**建议：** client 入参直接引用 OpenAPI 生成的 `ReserveInput`；对全部 mutation 的请求入参做同方向审查。现有 `test/client/types-conform.ts` 主要验证响应对象赋值，不能证明 client 手写请求与 handler 一致。增加“真实 client 序列化 → 实际 Go handler”契约测试比再增加一份手写 mock 字段更有效。

### L-3 · P2 / Major：管理端 QueryClient 缓存没有账本实例/会话身份

**类型：有明确触发条件的缓存隔离 bug。**

源码链：

- `src/hooks/keys.ts:11-27`：键统一为 `["ledger", ...]`，`balances` 只包含 holder，不含 baseUrl 或非敏感 scope。
- `src/provider/provider.tsx:19-33`：baseUrl/API key 改变只重建 client。
- `src/provider/shell.tsx:56-64`：自有 QueryClient 随组件长期保留，也明确支持宿主注入共用 QueryClient。
- `src/server/prefetch.ts:60-68`：prefetch 使用同样无实例身份的 key。

**触发条件：** 同一管理界面切换 A/B 两个账本实例，或不同 LedgerProvider 共用宿主 QueryClient；两边都查询 holder 42。宿主设置非零 staleTime 是已有 RSC prefetch 测试和常见 hydration 形态。默认 staleTime=0 也会先暴露旧缓存直到 refetch，不能把它作为隔离保证。

**复现：** 用真实构建 `createServerLedgerClient`、`prefetchBalances`、`ledgerKeys`，同一个 `QueryClient(staleTime=60_000)` 先读 `https://a.invalid`，再读 `https://b.invalid`：

```text
callsA = 1
callsB = 0
dataForB = [{ balance: "100", source: "A" }]
```

这是通过注入 fetch 的确定性内存实验，没有联网，也不声称绕过了后端授权。问题是前端在正确切换 API client 后继续使用另一个实例的余额/分类/流水。若随后操作的是新实例，用户判断依据与实际目标不同。

**建议：** 给管理面增加显式、非秘密的 cache scope（实例 + 身份边界），统一注入 hooks、prefetch 和 invalidation key；不要把原始 API key 放入 query key。wallet 已有 `WalletClientConfig.scope` + `walletKeys` 可作为局部现有模式，但仍应说明实例也是作用域的一部分。为 A/B 实例和退出登录后登录另一身份各留一个真实 provider/hydration 测试。

## 与目标的差距：扩展项与明确的设计取舍

### 1. Go library 是完整编排入口，通用 HTTP 原语不是等价业务 API

`server/handler_bookings.go:236` 只做 Transition；`server/handler_reservations.go:142` 只做 Settle。`core/interfaces.go:163-170` 明确 Settle 不记账，只解除 hold，必须与扣费 journal 在同一个 `RunInTx` 中组合。HTTP 暴露这两个原语不意味着一个远程客户可以把跨请求的业务动作变成原子事务。当前也没有通用 HTTP Exchange/批量事务端点。

这是现有 API 的边界，不是“Settle 实现错误”。用户计划 Go + Next.js 同栈，正确近期路径是由宿主 Go backend 组合账本，再向 Next.js 提供 capture/transfer/purchase 等业务端点；浏览器不应编排“先 settle 请求成功，再 postJournal”。独立服务模式若将来要对等，应增加少量明确业务命令，而不是提供远程任意事务 DSL。

### 2. 通用 Exchange 与签名/验签余额目前无法直接组合

`exchange.go:141-146` 明示 Exchange 的事务路径写 `AuthStatusUnsignedTxMode`；要求可验签的部署仍需手写 `AuthorizeTemplate + PostAuthorized`。`ledger.go:597-609` 同样说明普通 tx journal 会使相关维度无法验证。该限制已有文档，不能冒充新隐藏 bug。

但对“拿来即用的独立库”而言，这个组合缺口很重要：最方便的兑换原语与最严格的余额可信度模式不是同一条安全路径。下一轮应从实际 capture / exchange 两三个场景抽出 prepared posting：外部授权在事务前完成，事务内重新核对绑定的模板/币种/金额/报价身份，统一获取全量有序锁并原子提交。不要让每个宿主复制排序锁、签名准备、reserve/settle 的正确组合。

### 3. Currency 统一计量，与 USD valuation 应保持不同职责

当前设计已明确 `RateQuoter` 是外部 port，账本不存 rates：`core/conversion.go:15-32`，设计 `2026-09-07-rate-quoter-and-exchange.md` 的 Decision 段。`FixedRate` 为有方向、有版本、有舍入规则的纯换算值；实际 quote 作为 journal 证据留存。这正是值得保留的边界。

建议分三层：原币数量和权属账本；USD valuation 读模型（价格来源/时间/缺失状态）；业务报价与执行。USD 汇总不应反写改变原币持仓。积分/礼物可以有 USD 展示价值，但“值多少钱”不自动授予赎回/提现权；准入与兑换方向在业务政策中表达。

现有 `FixedRate` 适合固定兑换、积分单价。真实市场 swap 的报价可能依赖数量、费用、滑点、有效期和 venue；`QuoteRate(ctx, source, target)` 不足以描述这些因素。这是范围扩展，不是固定比率 helper 的 bug。可在宿主或可选 exchange 包定义 amount-sensitive `QuoteRequest` / `Quote` / `ExecutionReceipt`，核心消费已验证的成交事实。

**边界补充：** 外部成交成功以后，本地数据库事务失败不能回滚链上/venue。宿主必须持久跟踪已提交操作、重试入账及对账补偿；prepared posting 不应承诺分布式原子成交。已提交回执恢复与禁止保存待执行金融草稿是两回事。

### 4. 可选模块发布仍需独立外部消费者闸

根模块此次独立消费成功。`chains/evm/go.mod:6` 和 `anchors/r2/go.mod:10-11` 仍含 root / test fixture 零伪版本并依赖本地 replace；`go.work` 内构建无法证明最终远程消费。R2 限制已在 `README.md:1099` 及旧审计披露，不重复计新 bug。本次没有访问 registry 验证这些可选模块当前 tag 状态，也不声称它们今天的任意消费方式都失败。

迭代应给每个可发布模块分别跑 `GOWORK=off` 的干净消费者 `tidy/build`，从真实发布版本或候选模块包验证，而不是只测试 root。这与已建立的 root `make test-consumer` 同方向。

## 已验证做得好的部分

- **根 Go 库能够独立消费。** `make test-consumer` 创建新宿主 module、关闭 go.work、执行 tidy/build/run；固定兑换 USDC、INPUT_TOKEN、ROSE → CREDITS 均通过，反向币对被拒；生产 imports 不包含 Docker/testcontainers。
- **清晰的 facade 与事务组合。** `ledger.New(pool)` 在一个位置装配 postgres；应用通过 `core` 接口调用。`RunInTx` 的同 tx executor、回滚清理、嵌套拒绝、外部调用边界均已在代码明确。数据库固定为 PostgreSQL 是已有产品选择，并非为“通用”必须再造数据库抽象。
- **外部可选生态依赖隔离。** EVM 与 R2 放在独立 Go module，核心无需直接引入 go-ethereum/S3 SDK；链监听、归集属于可选 adapter，不是 Currency 类型的必要能力。
- **服务端授权边界有结构护栏。** operator scopes 与 holder token 分面；deposit review capability 不被 admin scope 自动包含；system classification 模板默认拒绝通用入口调用。本文未重审全部安全实现，只确认这些防护仍在实际路由/handler 路径上。
- **真实 npm tarball 能消费。** 本次将 package pack 后装入独立目录，未安装 HeroUI，TypeScript 编译及 root/headless/charts/server/wallet/wallet-headless 六个入口的 runtime import 全部通过。React 包可以脱离 Next.js 项目目录；两个 skin 与 headless 共享逻辑值得继续保持。
- **契约与交付检查已经成形。** OpenAPI codegen、类型检查、build artifacts、双 skin parity、真实 server 测试均存在且本次通过。新 findings 说明要补“请求序列化、身份范围、缓存作用域”的语义检查，不是缺一套新的大审计框架。

## 建议迭代顺序

1. **先修身份与请求契约。** L-1 为最高优先；同批修 L-2，把公开 client mutation 输入从生成契约派生。新增测试要跨序列化边界，覆盖两个大整数账户与非默认 reservation TTL。
2. **建立显式缓存边界。** 修 L-3，hooks/prefetch/mutation invalidation 一起改，验证两个实例与两个 session 的隔离。
3. **完成两个真实宿主消费场景。** 例如 crypto 充值 → credits 兑换，以及计量消费 → capture/refund。用“可用余额、pending、locked、原币数量、quote evidence、幂等重放”验收全流程，而非只验证每个 store 方法。
4. **抽取安全操作门面。** 从上一步真实重复中统一原子 capture/transfer/exchange 与签名 prepared posting，保留 journal/template 低层能力。对退款明确部分比例、舍入、关联兑换两腿的契约；不要先扩成万用 DSL。
5. **再加 valuation 与市场执行 adapter。** USD valuation 是读模型；真实 swap 有独立 execution lifecycle；固定报价实现保持小。所有可发布子模块都增加无 workspace 的消费者闸。

## 验证记录与复现

所有命令在上述独占 worktree 运行，长任务显式设置了 subprocess timeout。结果：

| 验证 | 结果与限制 |
|---|---|
| `make test-consumer`（600s timeout） | 通过；GOWORK=off、tidy/build/run、三个固定兑换案例、生产依赖排除检查 |
| `npm ci`（web，600s timeout） | 成功安装；审计警告单列如下 |
| `npm run -w @azex/ledger-react build` | 通过，九个 entry 构建 |
| `npm run -w @azex/ledger-react typecheck` | 通过 |
| `npm run -w @azex/ledger-react test` | 43 文件、257 tests 通过 |
| `npm run -w @azex/ledger-react codegen:check` | 通过，没有 schema diff |
| `go test ./server/... -short -race -timeout 120s -count=1`（外层180s） | 通过，4.872s；不是整个账本数据库集成测试 |
| 独立 tarball `npm install --ignore-scripts --omit=optional` + `tsc --noEmit` + 六入口 import | 通过；HeroUI 未安装；未运行真实 Next.js 生产 build/browser UI |
| 本报告三个 probes | 均复现；Go HTTP 使用输入记录器，缓存使用真实 QueryClient + 注入 fetch，无真实资金写入 |

附带环境信号：`npm audit --omit=dev --json` 本次退出 1，报告生产依赖 2 high + 1 critical（next/sharp/source-map-js）。这里只记录工具结果，不把 advisory 数量等同于可利用漏洞；未作 exploitability 验证，且这些结果来自 web 工作区，不能直接归因于发布的 ledger-react tarball。完整输出保留在本任务 `.hive-tmp/library-audit/npm-audit.json`。因此本报告只声称上述 build/test 通过，不声称完整发布安全闸全绿。

可复用证据文件在同目录 `library-probes/`：`probe.mjs`、`probe.go.txt`、`run.py`、`output.txt`。Go probe 使用 `.txt` 避免把审计程序混入仓库 Go package 集合。运行：

```bash
cd /Users/aaron/projects/_worktrees/ledger/codex/review-library-261010-r1
# 前置：web/npm ci，随后 npm run -w @azex/ledger-react build 已完成。
mkdir -p .hive-tmp
python3 docs/audits/2026-10-10-architecture-review/library-probes/run.py
```

独立 npm 消费命令和输出保留在 `library-probes/consumer-check.py` 与 `consumer-output.txt`。执行脚本本身包含每条 subprocess 命令和 timeout，可以重跑；它只在 `.hive-tmp/library-audit` 创建 tarball/独立消费目录，不发布任何包。

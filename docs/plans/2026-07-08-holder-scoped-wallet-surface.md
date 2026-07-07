# Holder-Scoped Wallet Surface — 用户钱包读端面 + wallet 组件

> 2026-07-08 · 需求档 v0.3（v0.1 → v0.2：review 修订，补双模式挂载、聚合语义、schema 与 token 刷新，决策收口；v0.2 → v0.3：打磨——补全三端点 wire 形状、kind 判定机械规则、token 格式、排序边界、Phase 拆解与验收、See Also）
> 发起：Aaron（armatrix 集成 review 时拍板）。核心洞察：**用户钱包就是账本数据的 holder 切片**——切片投影层不该每个消费方产品各写一遍，应上游化为 ledger 的库能力。

---

## 1. 问题（来自消费方的证据）

ledger 目前对外只有一个信任域：**Bearer API key（read < write < admin 三档 scope）**，任何一档都是全账本视角。消费方产品要给终端用户展示"我的钱包"，必须自建一层投影：

1. **鉴权域转换**：产品 session 用户 → 账本 account holder；
2. **范围过滤**：全账本 → 只属于该 holder 的余额/流水；
3. **概念翻译**：journal / entry / debit / credit / classification / reservation → 用户语言（余额、充值记录、消费记录）——终端用户不该看到复式记账内部词汇。

现实代价：armatrix 的 `internal/handler/credit.go` + adapter 读路径 ≈ 100+ 行专门干这三件事；azex、dayzero 接入时会再写两遍同样的东西。三件事全部是账本的通用投影逻辑，与产品业务无关——**属于库，不属于产品**。

对照：admin 侧已经解决了同类问题（`server.NewWithConfig` 挂载 + `@azex/ledger-react` LedgerAdmin），holder 侧是空白。

## 2. 需求

### 用户故事

- 作为**消费方产品的终端用户**，我能看到我的余额（可用/冻结/待入账）和交易记录，用我能懂的语言。
- 作为**消费方产品的开发者**，我装上 ledger（Go 库）+ ledger-react 后，用户钱包是拿来即用的能力：不写投影层、不碰 journal 内部概念、不把全账本权限暴露进用户请求路径。

### 能力需求

| # | 需求 | 说明 |
|---|---|---|
| R1 | **holder-scoped 只读 HTTP 端面** | 余额（BalanceBreakdown：available/pending/locked/total）、交易流水（翻译后的 holder 视图，cursor 分页）、活跃 holds。只读，无任何写操作 |
| R1b | **双模式挂载** | 端面既随 ledgerd（service 模式）暴露，也以**独立可挂载子路由**供 library 消费方嵌入自己的 HTTP server——不含任何 admin 路由（见 §3.4）。armatrix 这类 library 消费方是首要目标，R1 缺了这条就不成立 |
| R2 | **holder-scoped 鉴权 principal** | 与 API key 平行的短时效 token，绑定单一 holder；由宿主后端用自己的 key 铸造。持有 token 只能读该 holder 的切片，泄漏 blast radius = 单用户只读 |
| R3 | **概念翻译在库侧完成** | 端面返回的是 holder-facing transaction view，不是 journal 原文（聚合语义见 §3.3）：方向（in/out，从该 holder 视角净额推导）、稳定 `kind` code + 默认展示标签 `kind_label`、金额、业务时间、摘要。内部字段（entry 借贷方、对手科目、内部 id）不出现 |
| R4 | **ledger-react wallet 组件 + hooks** | wallet provider + headless hooks + 组件（BalanceCard / Balances / TransactionList / WalletPanel），三 surface（headless / shadcn / heroui）对齐现有拆分；auth 供给用 `getToken` 回调统一两种拓扑（§3.6）；组件清单与 props 契约见 §3.8 |
| R5 | **display 配置随 preset 走** | InstallDefaultPresets / ExtendedPresets 的 classification / journal type 自带默认用户可读标签（可覆盖），消费方零配置可用。需要 schema expand（见 §3.5） |

### 非目标

- **写操作**：充值/消费/提现仍走宿主产品的业务流（支付、风控、幂等在产品侧）——钱包端面纯读。
- **admin 面**：已由现有 API + LedgerAdmin 覆盖。
- **多租户平台化**：token 绑定单 holder 即可，不做跨 holder 的组织/家庭账户聚合（YAGNI，未来真出现再议）。
- **currency 展示换算**（如 armatrix 的 USD↔CREDITS）：留产品侧——库不懂产品汇率语义，那是定价业务。

## 3. 设计概要

### 3.1 信任模型（custody 红线对齐）

浏览器**永不持有 API key**。两种消费拓扑，库都支持：

```
拓扑 A（推荐，token 下发）：
  浏览器 ── holder token ──→ 宿主后端挂载的 holder 子路由（§3.4）
  宿主后端：session 鉴权后 POST /api/v1/holder-tokens（用自己的 write/admin key）
           铸造 {holder, ttl} token 下发给前端

拓扑 B（BFF 代理）：
  浏览器 ── 产品 session ──→ 宿主 BFF ──（key + 服务端注入 holder）──→ holder 端面
  （现状 armatrix 模式的规范化；react client 不带 token、走同源 cookie，见 §3.6）
```

Token 性质与实现（review 拍板：**无状态 HMAC 签名**，不落表、不加运维面）：

- 短时效（默认 ~15min，可配上限）、只读、绑定单 holder；
- 带前缀 `lht_`（ledger holder token），与 API key 走同一 `Authorization: Bearer` header 但 middleware 按前缀分流成两种 principal——错误码、访问日志字段（key name vs holder）分开，审计不混；
- 撤销粒度 = 全局（换签名密钥即全体失效）+ TTL 自然过期；只读 15min 的 blast radius 下不值一张 token 表 + GC；
- 签名密钥由宿主注入（env / 配置），与 API_KEYS 同样在 boot 时校验，缺失/格式错即 fail fast；
- 格式：`lht_<base64url(payload)>.<base64url(HMAC-SHA256(payload))>`，payload = `{"holder":…,"iat":…,"exp":…}`——自描述、无状态校验，不引入 JWT 依赖（无 alg 协商面）；
- mint 契约：请求体 `{holder, ttl_seconds?}`（ttl 超上限报错），需要 write scope（write key 泄漏本可直接写账，mint 只读 token 是更小的能力）；访问日志记 mint 方 key name + 目标 holder。mint 端点是**宿主控制面**（key principal），不属于用户可达端面——用户面只有 `/holder/*` 三个只读端点。

### 3.2 端面草案

```
POST /api/v1/holder-tokens                       # write scope（宿主后端调用；service 模式与挂载模式都有）
GET  /api/v1/holder/balances?currency_uid=…      # holder token；BalanceBreakdown 列表（参数可选，缺省全币种）
GET  /api/v1/holder/transactions?cursor&limit    # holder token；翻译后流水，{list, next_cursor} cursor 分页（api-contract §6）
GET  /api/v1/holder/holds                        # holder token；活跃 reservations 摘要
```

所有响应走统一包络 `{code, message, data}`；holder 由 token 决定，**任何端点都不接受 holder 参数**。

transaction view item（wire，snake_case，金额字符串）：

```jsonc
{
  "uid": "…",                  // journal uid（幂等追溯锚，可展示）
  "kind": "deposit",           // 稳定 code（源自 journal type / classification 映射）——产品侧 i18n/覆盖的锚
  "kind_label": "充值",         // 库侧默认展示标签（§3.5 display 配置），产品可按 kind 覆盖
  "direction": "in",           // in | out（该 holder 该币种净额方向，见 §3.3）
  "amount": "100.5",           // 净额绝对值
  "currency_uid": "0197…",     // 币种 uid（uid-only 契约）
  "currency_code": "CREDITS",  // 展示用 code——用户面直接可用，不逼前端再查一次
  "occurred_at": "2026-07-08T02:00:00Z",  // = journal.effective_at（I-14 业务时间，非写入时间）
  "reversal_of_uid": "",       // 非空 = 这是一笔冲正（前端呈现为「退款/冲正」标记）
  "memo": "…"                  // = journal.metadata["memo"]（well-known key，宿主写入时即为用户可读文案；缺省空串）
}
```

balances item（`data` 为该形状的列表；直接投影 `core.BalanceBreakdown` + 币种 code）：

```jsonc
{
  "currency_uid": "0197…",
  "currency_code": "CREDITS",
  "total": "120.5",            // total = available + pending + locked（不变量，前端可断言）
  "available": "100.5",
  "pending": "0",
  "locked": "20"
}
```

holds item（活跃 reservations 的用户面摘要——只出金额与时间，不出 reservation 内部状态机；`core.Reservation` 无 metadata 字段，用户可读的用途文案本期不做，产品侧如需可按 uid 自行关联）：

```jsonc
{
  "uid": "…",                  // reservation uid（追溯锚）
  "amount": "20",              // 剩余锁定额 = reserved_amount − settled_amount
  "currency_uid": "0197…",
  "currency_code": "CREDITS",
  "created_at": "2026-07-08T02:00:00Z",
  "expires_at": "2026-07-08T03:00:00Z"
}
```

### 3.3 transaction view 聚合语义（核心投影规则）

**Grain = (journal, holder, currency) 净额聚合**。规则：

1. 对一个 journal 内属于该 holder 且 classification **带 balance_role**（available/pending/locked——与 BalanceBreakdown 同一「spendable money」口径）的 entries，按 currency 分组；组内每条 entry 先按其 classification 的 `normal_side` 折算为带符号的余额变动（entry 方向 = normal side 时为 `+`，反之为 `-`），`净额 = Σ 变动`。role-less 的 holder 侧记账账户（fee_expense 等）不参与——把它们算进来会让手续费在净额里对消归零、对用户隐身（实现期发现，26-07-08 修订）；
2. 每个非零净额组产出一行 view item：`direction = 净额正负`，`amount = |净额|`——同 holder 双边 entry（支付 + 手续费）自然合并为一行净支出；FX（同 holder 两币种）产出两行（一出一进），符合用户「卖了 X 得到 Y」的心智；
3. **净额为零的组不产出行**（holder 自有 classification 间互转，如 available→locked 的内部移动，用户面不可见——holds 端面已单独表达锁定状态）；
4. reversal journal 正常产出行（方向天然相反），并带 `reversal_of_uid` 供前端标记为冲正/退款；被冲正的原交易不改写（append-only 语义直通用户面）；
5. cursor 基于 journal id 的稳定排序（`ORDER BY id DESC`），投影为 SQL 查询（queries 侧新增），不在应用层拼装。**已知边界**：排序键是写入序（id），`occurred_at` 展示的是业务时间（effective_at）——回填（backdated）journal 会出现在列表顶部而时间戳靠前。cursor 稳定性优先于严格业务时间序；对用户流水这是可接受近似，产品有强业务时间序需求时再议（届时需 (effective_at, id) 复合 cursor）。

### 3.4 架构落点（含双模式挂载）

- **core/ 不动**：`BalanceBreakdown` 已是 holder-facing 域概念；流水翻译是查询投影（postgres queries 侧新增 holder 视图查询 + sqlc）。
- **server/**：
  - holder token 铸造/校验 middleware（与 API key middleware 平行的 principal，`lht_` 前缀分流）；
  - 3 个只读 handler；
  - **`server.HolderHandler(cfg, deps…) http.Handler`**：独立构造的 holder 子路由（含 mint 端点 + 3 个读端点，**不含任何 admin 路由**），library 消费方直接 `r.Mount("/api/v1", …)` 进自己的 chi server——这是 armatrix 路径；
  - ledgerd（service 模式）在现有路由上复用同一子路由——一份实现，两种暴露；
  - 横切面归属：挂载模式下 CORS / 限流由宿主 middleware 负责（子路由不自带）；ledgerd 模式沿用现有 server 配置。holder 端面是用户流量剖面（QPS 高于 admin 面），RUNBOOK 补一段容量注记（BalanceBreakdown 走 checkpoint+delta 读路径，成本已知）。
- **web/packages/ledger-react**：新增 wallet hooks + 组件，三 surface；schema 走现有 openapi.yaml codegen 链。
- **openapi.yaml**：新端面进契约，`codegen:check` 护栏照常生效。

### 3.5 schema 变更（expand，deployment.md 三步纪律）

R3/R5 的 display 配置需要落库：

- `classifications` / `journal_types` 各加 `display_label TEXT NOT NULL DEFAULT ''`（no-NULL 约束，空串 = 未配置，投影时回退 `name`）；
- kind 映射的机械规则（两级，review 拍板；wire 带稳定 `kind` code 后产品侧还有最后一层按 code 覆盖的能力，不需要二维配置）：
  - `kind` = `journal_type.code`（永远，稳定锚，不参与覆盖）；
  - `kind_label` 判定：该 (journal, holder, currency) 组的 entries **全部落在同一 classification 且其 `display_label` 非空** → 用 classification 的标签；否则用 `journal_type.display_label`；仍为空 → 回退 `journal_type.name`；
- presets 更新：InstallDefaultPresets / ExtendedPresets 写入默认标签（deposit→充值 等）；已安装的存量库靠 expand 迁移 DEFAULT '' + 投影回退 name，零破坏；
- 本次只有 expand 步，无 rename/drop，不触发 migrate/contract 后续 release。

### 3.6 前端 auth 供给与 token 刷新（R4 的 DX 关键）

15min TTL 意味着前端必然遇到过期，wallet client 的 auth 是**回调注入**而非静态配置：

```ts
createWalletClient({
  baseUrl,
  getToken?: () => Promise<string>,  // 拓扑 A：回调向宿主后端换 token（宿主自己缓存/续期）
                                     // 拓扑 B：省略——同源 cookie，BFF 注入 holder
})
```

- 请求带 token → 401（token 过期/失效）→ client 自动调一次 `getToken()` 重试；再失败则冒泡给 hooks 的 error 态（组件三态照常）；
- hooks/组件对两种拓扑零感知——拓扑差异被 client 配置吸收（composition root 纪律）。

### 3.7 命名（review 拍板：分层命名）

- **Go / HTTP 面 = `holder`**（域词，开发者受众）：`/api/v1/holder/*`、`server.HolderHandler`、`holder token`；
- **npm 组件面 = `wallet`**（用户概念词，产品受众）：子路径 `@azex/ledger-react/wallet`、`useWalletBalance` / `useWalletTransactions` / `useWalletHolds`、`<WalletBalanceCard/>` `<TransactionList/>`。

### 3.8 前端组件设计（R4 展开）

**入口拆分**（沿用三消费面模式，wallet 与 admin 面隔离，互不拖 bundle）：

```
@azex/ledger-react/wallet            # shadcn 皮肤组件 + WalletProvider（复用 .ledger-root tokens / appearance）
@azex/ledger-react/wallet/heroui     # HeroUI v3 皮肤（optional peer，host 拥有主题）
@azex/ledger-react/wallet/headless   # wallet client + hooks，零 UI
```

**Provider**：`<WalletProvider config={{ baseUrl, getToken?, queryClient?, appearance?, theme?, onError? }}>` ——独立于 admin 的 `LedgerProvider`（不同信任域、不同 client），但复用同一套 appearance/theme/portal 机制与 QueryClient 注入约定。

**headless hooks**（全部只读，React Query）：

| Hook | 返回 | 说明 |
|---|---|---|
| `useWalletBalance(currencyUid?)` | `BalanceBreakdown[]` | 缺省全币种；单币种传参过滤 |
| `useWalletTransactions(params?, limit?)` | infinite query | `{list, next_cursor}` 契约，`pages.flatMap` 消费，同现有分页 hooks |
| `useWalletHolds()` | 活跃 holds 列表 | 锁定明细 |

**组件清单**（每个组件必有 loading skeleton（与内容等高）/ error / empty 三态；金额走 financial-display 分档；`direction` 上色遵循 PnL 规则：in 绿 `+` 前缀、out 红 `-` 前缀）：

| 组件 | 职责 | 关键 props |
|---|---|---|
| `<WalletBalanceCard/>` | 单币种余额卡：total 大数 + available / pending / locked 分层行；locked 行可展开显示 holds 明细（内部消费 `useWalletHolds`） | `currencyUid`；`actions?: ReactNode`（宿主放「充值/提现」按钮的 slot——写操作在产品侧，见非目标） |
| `<WalletBalances/>` | 多币种：每币种一张 `WalletBalanceCard`，自动布局 | `actions?`（透传） |
| `<TransactionList/>` | 流水列表：kind 标签、direction 色、金额、occurred_at、memo、冲正标记；cursor 分页复用 `LoadMoreBar` | `kindLabels?: Record<string, string>`（按稳定 `kind` code 覆盖库侧默认 `kind_label`，产品 i18n 锚）；`renderItem?`（整行自定义的逃生舱） |
| `<WalletPanel/>` | 零装配组合件：`WalletBalances` + `TransactionList` 纵向组合，对标 admin 面的 `LedgerAdmin` 定位 | `actions?` / `kindLabels?`（透传） |

**双皮肤纪律**：与 admin 面相同——shadcn 与 heroui 版页面逻辑镜像，数据流全部走 `/wallet/headless` hooks，皮肤层只做渲染。

**用户表面纪律**（user-facing-surfaces.md 护栏）：组件渲染输出永不出现 journal / entry / debit / credit / classification / reservation 等内部词汇；错误态文案是「出了点问题，请重试」级别的脱敏文案（原始错误经 `onError` 给宿主日志）。包内测试断言内部词汇不出现在渲染结果。

**宿主接入示例（拓扑 A）**：

```tsx
<WalletProvider config={{ baseUrl: "/api/v1", getToken: fetchHolderToken }}>
  <WalletPanel actions={<TopUpButton />} kindLabels={{ deposit: "Top up" }} />
</WalletProvider>
```

## 4. 消费方迁移（以 armatrix 为例，验证需求完备性）

| armatrix 现状 | 迁移后 |
|---|---|
| `GET /api/credits/balance`（自建） | `server.HolderHandler` 挂进自己 server + 前端 `useWalletBalance`（或 BFF 拓扑直连） |
| `GET /api/credits/transactions`（自建） | `useWalletTransactions` + `<TransactionList/>`（heroui surface，与其 HeroUI v3 栈原生匹配） |
| 概念翻译散在 handler/adapter | 库侧 display 配置 + wire `kind` code，产品只覆盖标签文案 |
| 保留在产品侧的 | 业务定价（`/credits/pricing`）、充值支付流、CreditChecker 写路径 adapter、USD↔CREDITS 展示换算——本就是产品业务，不属于钱包读投影 |

验收：armatrix 钱包读路径的自建代码归零；同样的集成对 azex/dayzero 开箱可用。

## 5. 决策记录（v0.1 开放决策 → v0.2 review 收口）

| # | 决策 | 结论 | 依据 |
|---|---|---|---|
| 1 | token 实现 | **无状态 HMAC** | 只读 + 15min TTL，落表吊销粒度不值一张表 + GC；全局换 key 兜底 |
| 2 | 翻译配置粒度 | **journal type 默认 + classification 覆盖（两级）** | wire 带稳定 `kind` code 后产品侧还有最后覆盖层，二维配置 YAGNI；机械判定规则见 §3.5 |
| 3 | currency 展示换算 | **留产品侧** | 库不懂汇率语义；wire 同时给 currency_uid + currency_code 已够展示 |
| 4 | 命名 | **Go/HTTP=holder，npm=wallet** | 分层受众：域词对开发者，用户概念词对产品面（§3.7） |

## 6. Phase 拆解与验收

> **进度（26-07-08）**：P1 `c318954` / P2 `da8dd30` / P3 `6d66202` / P4 `ff4e6be` 已合入 main 并推送（各 phase 验收全过：投影规则集成测试、token 表驱动测试、codegen gate、双皮肤护栏断言、fullstack `/wallet` demo）。剩 P5（armatrix 迁移）在消费方仓库进行；`@azex/ledger-react` 0.4.0 已 bump 未发版（等 tag）。

按现有「Adding Features」工作流（SQL → sqlc → core/server → openapi → react）排序，每个 phase 独立可交付、独立过 CI：

| Phase | 动作 | 验收 |
|---|---|---|
| P1 数据层 | expand 迁移（`display_label` 两表）+ presets 默认标签 + holder 投影查询（transactions 聚合 / holds 摘要）+ sqlc | 迁移可重入；`make sqlc-diff` 绿；投影查询单测覆盖 §3.3 五条规则（双边合并 / FX 两行 / 净零不出行 / 冲正标记 / cursor 稳定） |
| P2 服务端 | holder token mint/校验 middleware（`lht_` 前缀分流）+ 3 个只读 handler + `server.HolderHandler` 子路由 | token 过期/伪造/跨 holder 访问被拒的表驱动测试；`HolderHandler` 挂进裸 chi server 的集成测试（不含 admin 路由断言）；ledgerd 路由复用同一实现 |
| P3 契约 | openapi.yaml 新端面 + `codegen:check` + wallet headless（client + `getToken` 刷新重试 + hooks） | codegen 无 diff；401→refresh→重试一次的 msw 测试；hooks 缓存 key 遵循 ledgerKeys 单源 |
| P4 组件 | wallet 双皮肤组件四件套 + `/wallet` `/wallet/heroui` `/wallet/headless` exports + 三态/极端数据 | vitest：内部词汇不出现在渲染输出（user-facing-surfaces 护栏断言）；双皮肤逻辑镜像；examples/fullstack 增加 wallet 页 demo（两拓扑至少演示 A） |
| P5 消费方验证 | armatrix 迁移：挂 `HolderHandler`、前端换 `useWalletBalance`/`<TransactionList/>` | armatrix 钱包读路径自建代码归零（credit.go 读投影删除）；其 e2e 通过 |

发版：P1-P4 合入后 ledger 打 minor tag + `@azex/ledger-react` minor 发版（新增 subpath，非 breaking）；P5 在消费方仓库进行，不阻塞本仓发版。

## 7. See Also

- `docs/INVARIANTS.md` — I-14（effective_at as-of 语义，`occurred_at` 的来源）
- `docs/plans/2026-07-03-api-contract-alignment-design.md` — v0.4 uid-only / wire 契约（本端面全部沿用）
- `docs/plans/2026-06-15-ledger-consumption-optimization-plan.md` — 库消费路径（`HolderHandler` 是同一「上游化」思路在 holder 面的延续）
- `~/.claude/rules/api-contract.md` — 包络 / snake_case / 金额字符串 / cursor 分页（§6）
- `~/.claude/rules/custody.md` — 浏览器凭证红线（holder token 的 blast radius 设计依据）
- `~/.claude/rules/user-facing-surfaces.md` — 概念翻译（R3）与组件渲染护栏（§3.8）的上位纪律
- `~/.claude/rules/deployment.md` — expand→migrate→contract（§3.5 只有 expand 步）
- armatrix `internal/handler/credit.go` — 问题证据与 P5 迁移对象

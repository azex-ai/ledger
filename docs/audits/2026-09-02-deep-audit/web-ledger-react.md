# 审计报告 — J · web-ledger-react

> 基线 commit `fd87cae`。范围：`web/packages/ledger-react/`（约 18.4k 行 src）三消费面
> （root shadcn skin / `./heroui` skin / `./headless`）+ `./wallet*` 四个入口，
> 以及 `web/`（dogfood app）与 `.github/workflows/ledger-react*.yml`。
> 所有 `file:line` 以 `fd87cae` 为准。实跑过：`typecheck`（绿）、`vitest run`（38 files / 182 tests 全绿）、
> `tsup build`（成功）、`npm audit`（prod 0 高危）。

## 摘要

这个包的**机械纪律很硬**（幂等 key 生命周期、mutation 失败反馈、skin parity、styles 全局泄漏
都有静态 gate 守着），但**门禁守的是"形状"，没有一条守"这条请求真的能成功吗"**。本轮最要紧的
三条都是同一个形态：**请求根本没发出去 / 发出去必被拒，而 UI 把它渲染成一个正常的空状态**——
`working-agreements.md §3`「未运行 ≠ 通过」在前端复发。其中 `BalancesPage` 的
「Balance Trend (30 days)」图表**在两个 skin 上都是永远不可能渲染的死功能**（请求缺服务端必填的
`currency_uid`，恒返 400，而错误被丢弃），`SnapshotsPage` 在不填 holder 时会告诉运维
「No snapshots found」而实际上一次请求都没发。

第二类问题是**上轮修复的"接线"没走完最后一公里**：M1 的 `errorText` 想surface 服务端错误详情，
但 `pkg/httpx/response.go:174` 只发 `DisplayMessage(code)` 这张静态码表，per-instance 详情（幂等
payload mismatch 的实际金额）永远不出服务端；`message.fields` 服务端从无写入点，前端的 `fields`
映射是死代码。M3 声称的 shadcn↔heroui「a11y/overflow parity」实际落成的是一条把 21 : 63 的
差距冻结下来的 ratchet。M5 的 types-conform 只钉了 21 个 wire type 里的 11 个，而它自己注释里
列的「spec 里根本没有」的实体，现在 spec 里已经有 5 个了——注释过期，gate 没跟上。

金额侧结论是正面的：**没有任何路径用 float 参与运算**（`decimal.ts` 全 BigInt），M1 的负号修复
在两个 skin 都真的接到渲染路径且有 pin。唯一的 float 触碰是 4 个图表点，但它们的 recharts
`<Tooltip>` 没有 `formatter`，运维在 tooltip 里读到的是一个丢过精度、没走分档的裸浮点数。

无 Critical。**Major 9 条，Minor 14 条。**

---

## 发现

### [Major] J-1 「Balance Trend (30 days)」图表在两个 skin 上都永远不可能渲染，且错误被丢弃

- **位置**：`web/packages/ledger-react/src/components/pages/BalancesPage.tsx:38-42,118`
  与 `src/heroui/pages/BalancesPage.tsx:52-56,140`
- **判定**：CONFIRMED
- **失效场景**：`snapParams` 只带 `{holder, start, end}`，**没有 `currency_uid`**。
  服务端 `server/handler_system.go:191-195` 对 `/api/v1/snapshots` 的
  `currency_uid` 是**硬必填**（空则 `400 currency_uid is required`）。
  `client.ts:75-86` 的 `qs()` 会把 undefined 键整个丢掉，所以这个请求**在任何输入下**都是 400。
  返回值只取 `data`（`const { data: snapData } = useSnapshots(snapParams)`），
  `isError` 从未被读，于是 `snapshots = []` → `chartArray = []` → `{chartArray.length > 0 && (...)}`
  整张卡片不渲染。运维看到的是"这个 holder 没有历史快照"，实际是"我们从没问过"。
- **最小复现**：在 Balances 页输入任一 holder → 上方余额表正常出数据 → 下方
  「Balance Trend (30 days)」卡片永不出现；DevTools Network 显示
  `GET /api/v1/snapshots?holder=1001&start=...&end=...` → 400。
- **为什么现有机制没拦住**：`test/pages/static-pages.test.tsx:10-16` 是这个页面**唯一**的测试，
  它的断言明写「no fetch」——只验了输入 holder **之前**的状态，搜索之后的路径零覆盖。
  即使补了测试也未必红：`test/pages/render-page.tsx:16-21` 的 `getOk` **完全忽略 query 参数**，
  MSW 会对缺必填参数的请求照样返 200（§1.4「测试做了真实服务端不会做的事」）。

### [Major] J-2 SnapshotsPage 在不填 holder 时把"请求没发"渲染成"没有数据"

- **位置**：`src/components/pages/SnapshotsPage.tsx:43-50,80-83`；
  `src/heroui/pages/SnapshotsPage.tsx:35-44,92-95`；gate 在 `src/hooks/use-system.ts:59`
- **判定**：CONFIRMED
- **失效场景**：`useSnapshots` 的 `enabled: params.holder !== undefined && params.holder !== 0`。
  运维只填 Currency + 日期区间点 Search → `handleSearch` 恒写入 4 个 key 的对象
  （值可为 undefined，但 `Object.keys().length === 4`）→ query 被 **disabled**（holder undefined）→
  React Query v5 下 disabled query 的 `isLoading` 与 `isError` 均为 false → 落到
  `snapshots.length === 0` 分支 → 渲染 **"No snapshots found"**。
  这是历史余额取证界面，"这个币种这段时间没有快照"是一个会被当结论用的断言。
- **最小复现**：Snapshots 页只填 Currency=`<uid>` + Start/End → Search → 文案
  "No snapshots found"，Network 面板**零请求**。
- **附带**：`Object.keys(query).length === 0` 这条 pre-search 判据在第一次 Search 之后
  永远为 false（4 个 key 恒存在），所以"全部留空点 Search"也显示"No snapshots found"。
  反向情形（只填 holder）会真的发请求并被服务端 400 掉，但 `ErrorState message="Failed to load snapshots"`
  不用 `errorText`，运维永远不会知道 currency + 日期是必填的。
- **为什么现有机制没拦住**：`test/pages/static-pages.test.tsx:19-27` 同样只断言 pre-search 文案。

### [Major] J-3 钱包余额卡在 holds 查询 loading / 失败时显示「Nothing is on hold right now.」

- **位置**：`src/wallet/components/balance-card.tsx:34-41,139,154,166,192`；
  `src/wallet/heroui/balance-card.tsx:33,127,142,154,180`
- **判定**：CONFIRMED
- **失效场景**：`useWalletHolds()` 的 `isLoading` / `isError` **在两个 skin 里都从未被读**
  （`grep -n "holds" | grep -i "error\|loading"` 在两个文件上都是空）。卡片同时显示
  `On hold: {formatAmount(balance.locked)}`（来自 `/holder/balances`，成功）和展开区的
  「Nothing is on hold right now.」（来自 `/holder/holds`，失败）。终端用户看到的是
  「你有 50 USDC 被锁定」+「你现在没有任何锁定」两句自相矛盾的话。
- **最小复现**：让 `/api/v1/holder/holds` 返 500（`/holder/balances` 正常，`locked > 0`）→
  余额卡展开 On hold → 显示「Nothing is on hold right now.」。
- **为什么现有机制没拦住**：这是 `./wallet` 面，`test/skin-parity.test.ts` 只扫
  `src/*/pages/`，`test/mutation-feedback.test.ts` 同理；`test/wallet/components.test.tsx`
  没有 holds 失败用例。这是**终端用户面**，比 M2/M3 修的 admin 假空态严重一档。

### [Major] J-4 `dist/styles.css` 把包自己的 `--font-sans` / `--font-mono` 写进宿主全局 `:root`，C2a 门禁按设计放行

- **位置**：`src/styles/index.css:107-109`（`@theme inline`）；门禁
  `test/styles.test.ts:39-40,134-138`（`GLOBAL_TOKEN_ALLOWLIST` 允许 `--font-*`）
- **判定**：CONFIRMED（读 `dist/styles.css` 实测）
- **失效场景**：编译产物在全局 `:root,:host` 里声明 58 个 token，其中
  `--font-sans: var(--font-geist-sans,ui-sans-serif, system-ui, sans-serif)` 与
  `--font-mono: var(--font-geist-mono,ui-monospace, monospace)` **不是 Tailwind 的默认值**，
  是本包自己的 Geist 链（对照：`dist/heroui.css` 同位置是 Tailwind 原版
  `ui-sans-serif, system-ui, sans-serif, "Apple..."`，说明这两条确实是 shadcn skin 特有的注入）。
  任何用 Tailwind 且没有定义 `--font-geist-sans` 的宿主，只要它的样式表先于
  `@azex/ledger-react/styles.css` 加载，其 `font-sans` / `font-mono` 工具类会解析到 system-ui，
  **整个宿主应用的品牌字体被换掉**。
- **为什么现有机制没拦住**：C2a 的门禁设计目标正是"business token 不许进全局"
  （`BUSINESS_TOKEN_RE`），但 `--font-sans` 落在 `GLOBAL_TOKEN_ALLOWLIST` 的 `font` 命名空间里，
  被当成"Tailwind 自己的 token"放行。README:208-215 的免责声明也只列了
  `--color-* / --spacing / --text-* / --radius-*`，没提字体，且明说这些泄漏
  "carry no host-affecting resets"——对 `--font-sans` 这条不成立。
  这是"门禁的 allowlist 洞恰好开在包唯一真正改写宿主值的那个 token 上"。

### [Major] J-5 四个图表把金额 `parseFloat` 后交给没有 `formatter` 的 recharts Tooltip，运维读到的是丢精度的裸浮点

- **位置**：`src/components/dashboard/balance-trend.tsx:24,65-75`；
  `src/heroui/pages/DashboardPage.tsx:154`；
  `src/components/pages/BalancesPage.tsx:47,129-136`；`src/heroui/pages/BalancesPage.tsx:63`
- **判定**：CONFIRMED
- **失效场景**：注释写的是 "chart display only — intentional lossy conversion"，
  但四处 `<Tooltip>` **都没有传 `formatter`**（`grep formatter` 在这四个文件上为空）。
  recharts 默认把 dataKey 的原值直接渲染。所以 hover 一根柱子时，运维读到的是
  `Number(parseFloat("<18 位小数字符串>"))` 的 JS 默认字符串——既没走
  `financial.md` 的分档（>=1000 一位小数 / >=1 四位…），也已经丢了 double 之外的精度。
  轴刻度同理。`financial.md`「金额…绝不 number」与 `typescript.md`「前端只展示、绝不 parseFloat」
  在这里被绕过：轴/tooltip 是"展示"，但展示的是浮点值而非 `formatAmount` 的输出。
- **最小复现**：System Balances 卡片对一个 `total_balance = "1000000000.123456789012345678"`
  的系统户 hover → tooltip 显示的数字与同一页表格里 `formatAmount` 给出的
  `1,000,000,000.1` 不是同一个串。
- **兜底**：BalancesPage 上方的余额表用 `formatAmount`，是权威值；所以定 Major 不是 Critical。

### [Major] J-6 `docs/openapi.yaml` 的 `/snapshots` query 参数名与 Go handler、TS client 三方不一致，且没有任何门禁覆盖 query 参数

- **位置**：`docs/openapi.yaml:1250-1262`（`holder` / `currency` / `from` / `to`）
  vs `server/handler_system.go:186,191,197-198`（`holder` / `currency_uid` / `start` / `end`）
  vs `src/client/client.ts:462-470`（`holder` / `currency_uid` / `start` / `end`）
- **判定**：CONFIRMED
- **失效场景**：TS 与 Go 一致，**spec 与两者都不一致**，且 spec 把 `from`/`to` 标成可选而
  handler 是必填。本 territory 的"openapi ↔ Go ↔ TS 三方一致"在这个端点上是断的。
  任何按 spec 写集成的第三方消费方会 100% 拿到 400。
- **为什么现有机制没拦住**：`codegen:check`（`.github/workflows/ledger-react.yml:38`）只比对
  `schema.ts` 与 spec 的 **components/schemas**；`openapi-typescript` 生成的 `paths` 类型
  没有被任何代码消费——`client.ts` 的 URL 与 query 是手拼的 `qs()`，编译期完全不碰生成类型。
  所以路径/参数层的漂移**没有任何机器校验**。

### [Major] J-7 `types-conform.ts` 只钉了 11 个 wire type，其注释所列的"spec 里没有对应 schema"已过期 5 项

- **位置**：`test/client/types-conform.ts:53-93`
- **判定**：CONFIRMED
- **失效场景**：文件末尾声明 `Classification / JournalType / EntryTemplate / Currency /
  HealthStatus / Snapshot / PreviewResult / HolderBalances / BalanceByCurrency /
  JournalWithEntries` 在 spec 里"根本没有 schema"。实测 `docs/openapi.yaml` 现在有
  `Classification`(2428) / `JournalType`(2467) / `Template`(2527) / `Currency`(2561) /
  `HealthStatus`(1976) / `TemplatePreviewResult`(2631)。也就是说这 6 个**现在可以钉而没有钉**，
  注释是陈述过期事实。`types.ts` 里这批类型（metadata 四个页面 + Dashboard 健康卡全部依赖）
  与后端契约的一致性，今天没有任何机器保障。
- **另一半**：现有 11 条断言是**单向**的（`const _x: HandType = {} as SchemaType`）。
  它能抓"手写类型比 spec 更严"，抓不到"手写类型比 spec 更松"（`settled_amount?` 之类），
  也抓不到 spec 新增字段。方向是对的，但报告里要写清它证明的命题比文件标题窄。
- **为什么现有机制没拦住**：这个文件本身就是 M5 的产物，它的 gap 写在自己的注释里、
  标了"escalated to team-lead"，然后随 spec 补齐而失效，没人回来复核。

### [Major] J-8 【上轮修复复核】M1 的 `errorText` 无法交付它声称的内容：服务端只发静态码表文案，`message.fields` 全链路是死的

- **位置**：`src/lib/error-message.ts:11-15`（客户端）；
  `pkg/httpx/response.go:169-176`（服务端，`Text: bizcode.DisplayMessage(ae.Code)`）；
  `pkg/bizcode/errors.go:148-172`（码表）；`pkg/httpx/response.go:147`（`Fields` 字段定义）
- **判定**：CONFIRMED
- **失效场景**：`fd87cae` 的 commit body 说 M1 是
  "surface server error text on every mutation … hiding actionable detail like
  **idempotency-key payload mismatches** or unbalanced journals"。实际链路：
  `postgres/reserver_store.go:362` 那条带具体金额的
  `idempotency key %q payload mismatch (recorded %s, got %s)` 被
  `httpx.Error` 用 `resolveError` 折成 `core.ErrConflict` → bizcode 10901 →
  wire 上的 `message.text` 恒为 **"Operation conflicts with current state"**，
  原始详情只进 `slog.Error`（`response.go:171`）。所以 55 处 `errorText(err, fallback)`
  在幂等冲突这一类上**必然拿到一句比本地 fallback 更泛的话**
  （"Operation conflicts with current state" vs "Failed to settle reservation"）。
- **`message.fields` 更彻底**：`client.ts:129` 读它、`types.ts:13` 声明它，
  但 `pkg/httpx` 全仓**没有任何一处给 `ErrorMessage.Fields` 赋值**
  （`grep -rn "Fields" pkg/httpx/ server/` 只有定义与一处无关的 `desc.Fields`）。
  前端也没有任何页面把 `err.apiError.fields` 映射到表单字段（`grep fields src/` 只有 client + types）。
  `api-contract.md §1` 的字段级校验错误这条契约，两端都只有形状没有实现。
- **修复没有被复制成同形缺陷**，但**修复本身没有接到真实路径**——这正是 §1.4 要问的第一个问题。

### [Major] J-9 `./headless` 面出金额而不出金额格式化器；`decimal.ts` 的自述 import 路径是错的

- **位置**：`src/headless.ts`（全文，无 `lib/utils` 导出）；`src/index.ts`（同）；
  `src/lib/utils/decimal.ts:5-7`
- **判定**：CONFIRMED（`grep -o "formatAmount\|parseUnits\|shortenAddress" dist/*.d.ts` 为空）
- **失效场景**：`formatAmount` / `formatSignedAmount` / `formatCompact` / `validateAmount` /
  `shortenAddress` / viem 的 `parseUnits|formatUnits|parseEther…` 全部**不在任何 package entry
  的导出里**——`dist/index.d.ts`、`dist/headless.d.ts`、`dist/wallet.d.ts`、`dist/heroui.d.ts`
  四个 `.d.ts` 里一个都搜不到。而 `./headless` 的存在理由就是"hosts that bring their own UI"，
  这些宿主拿到的是 18 位小数字符串，只能自己重新实现 `financial.md` 的分档表——
  正是 `display.ts` 存在要防止的漂移。
- **附带的错误文档**：`src/lib/utils/decimal.ts:5-7` 明写
  `Consumer code should prefer viem-style usage: import { parseUnits, formatUnits } from "@azex/ledger-react";`
  ——这行 import **编译不过**。
- **为什么现有机制没拦住**：`.github/workflows/ledger-react.yml:43-60` 的
  "Assert build artifacts" 只检查 `dist/index.js` 首行、`dist/styles.css` 非空、
  `dist/server.js` 无 `"use client"`；没有任何导出面清单断言。

---

### [Minor] J-10 `formatCompact` 是零调用点、零测试、docstring 举例错误的导出函数

- **位置**：`src/lib/utils/display.ts:124-154`
- **判定**：CONFIRMED（实跑探针，跑完已删，`git status` 干净）
- 三重问题：① `grep -rn formatCompact src` 除定义与 barrel 外**零调用点**，且如 J-9 所述
  它也到不了任何 package entry —— 纯死代码；② `test/lib/display.test.ts` 只测
  `formatAmount` / `formatSignedAmount`，M3 声称"the money formatter had zero tests"已修，
  但第三个金额格式化器仍是零测试；③ docstring `formatCompact("999") → "999"` 与实际不符——
  实测返回 `"999.0000"`（<1000 时回退 `formatAmount`）。另外
  `display.ts:144-145` 写着 "callers must clamp first"，而根本没有 caller 去 clamp。

### [Minor] J-11 `formatAmount("")` 返回 `"0.00"`——"缺失"与"零"在展示上不可区分

- **位置**：`src/lib/utils/display.ts:69-77`
- **判定**：CONFIRMED（实测 `formatAmount("") === "0.00"`，因为 `parseUnits("",18) === 0n`）
- 今天不可达（`server/handler_bookings.go:47` 的 `SettledAmount` 无 `omitempty`，
  decimal 零值序列化为 `"0"`；`Reservation.settled_amount?` 在
  `ReservationsPage.tsx:283` 有 `r.settled_amount && !isZeroAmount(...)` 守卫）。
  但任何将来变成可选的金额字段会静默显示成 `0.00`。`test/lib/display.test.ts` 没有空串用例。

### [Minor] J-12 【上轮修复复核】M3 的 "shadcn 与 heroui a11y/overflow parity" 实际落成的是一条把 21:63 差距冻结的 ratchet

- **位置**：`test/skin-parity.test.ts:154-178`（`CENSUS_BASELINE` = shadcn 21 / heroui 63）
- **判定**：CONFIRMED（逐页统计见附录 A）
- 全 15 个页面里 shadcn 的 `aria-label` 总计 3（`DepositsPage` 1、`TemplatesPage` 2），
  heroui 34；`min-w-0` shadcn 1（`JournalDetailPage`）、heroui 7。
  `fd87cae` commit body 写的是 "M3: shadcn skin overflow/a11y parity with heroui + a ratchet gate"，
  但 gate 自己记录的基线就是 3 倍差。具体后果：shadcn skin 的 15 个 `<table>` **没有一个有
  可访问名**（无 `aria-label`、无 `<caption>`），`src/components/pagination-bar.tsx:22`
  的分页容器没有 `aria-label`（heroui 版 `pagination-bar.tsx:26` 有）。
  gate 的注释诚实地写了 "Full shadcn↔heroui token equality is still a longer-term goal"，
  但 commit body 说的是 "parity"——这两句话不是一回事。

### [Minor] J-13 `useLedgerMutation` 的幂等 key 作用域是"hook 实例"而非"实体/payload"，今天只靠调用点恰好是 per-row 才安全

- **位置**：`src/hooks/use-ledger-mutation.ts:52-58,72-73`
- **判定**：CONFIRMED（服务端语义：`postgres/booking_store.go:356-358` 与
  `postgres/reserver_store.go:350-354` 的 receipt 都是**跨实体全局**唯一，
  同 key 换实体 → `core.ErrConflict`）
- key 存在 `useRef`，失败后**不清**，成功才清。若某个 hook 实例被一个列表页的多行共用，
  第一次对 A 的写在服务端成功但客户端超时之后，对 B 的点击会带着 A 的 key → 服务端报
  "idempotency key already used for a different booking" → 失败 → key 仍不清 → **死锁**
  （只有一次成功能清，而所有后续尝试都注定失败）。
  今天不可达：`ReservationsPage.tsx:36,90,145,179`、`DepositsPage.tsx:62,111,175`、
  `WithdrawalsPage.tsx:44,98,140,168,220,248` 的对话框全是 `{ id }` per-row 组件，
  每行一个 hook 实例。唯二 page-scoped 的是
  `use-deposit-reviews.ts:81,104` 的 approve/reject——它们安全**只是因为**
  `server/handler_deposit_reviews.go:100-117,124-152` 根本不读 Idempotency-Key。
  也就是说这条不变式没有任何东西在守，靠的是两个互不知情的巧合。
- `test/hooks/use-ledger-mutation.test.tsx:40-83` 全程 `mutate(undefined)`，
  从不改变 variables ——跨实体这一维完全没被测。

### [Minor] J-14 metadata 的 create/deactivate 全部发送服务端**不读**的 Idempotency-Key，而 hook 注释声称符合 api-contract §9

- **位置**：`src/hooks/use-metadata.ts:7-24`（注释）、`36-61,73-98,110-135,164-189`；
  服务端 `server/handler_metadata.go`（全文只有一处 `IdempotencyKey: "preview"`，
  create/deactivate 均不读）
- **判定**：CONFIRMED
- `server/middleware_idempotency.go:69-71` 会把 header 注入 body，但
  `httpx.Decode`（`pkg/httpx/response.go:179-186`）不开 `DisallowUnknownFields`，
  多出来的字段被静默丢弃。结果：`useCreateClassification` 等 8 个 mutation 的
  幂等承诺是**纯装饰**；超时重试会真的产生第二次写（靠 `code` 唯一约束兜底，不是靠幂等）。
  注释「same lifecycle as useLedgerMutation … api-contract.md §9」误导后来的维护者。

### [Minor] J-15 `useReverseJournal` 丢弃 `useLedgerMutation` 铸的 key，落回 `client.ts` 的 per-attempt `randomUUID()`——M4 原始形态在冲销路径上仍在

- **位置**：`src/hooks/use-journals.ts:48-55`；`src/client/client.ts:181-185,60-73,105-110`
- **判定**：CONFIRMED
- `client.reverseJournal(id, reason)` 的 body 只有 `{reason}`、也不传 header →
  `request()` 走 `idempotencyKeyFromBody` → **每次 HTTP attempt 一个新
  `crypto.randomUUID()`**，即 M4 描述的原始缺陷。
  无害只是因为 `server/handler_journals.go:337-360` 的 `handleReverseJournal`
  压根不读幂等键（重复冲销由 `journals.reversal_of` 的部分唯一索引挡）。
  但这是 M4 修复留下的最后一个未清扫的同形点，且 `useLedgerMutation` 在这里白铸了一个 key。

### [Minor] J-16 heroui 的 Reconciliation 用 sonner 的 **success** toast 播报"账不平"

- **位置**：`src/heroui/pages/ReconciliationPage.tsx:21-27,34-41`
- **判定**：CONFIRMED
- `toast.promise(..., { success: (result) => result.balanced ? "Ledger is balanced" :
  \`Unbalanced — gap: ${...}\` })`——`balanced === false` 是 promise resolve，
  走 sonner 的 success 分支（绿勾）。同一页 shadcn 侧
  （`src/components/pages/ReconciliationPage.tsx:54`）用的是
  `StatusBadge status="failed"`。卡片正文里 heroui 也有 `StatusChip failed`（:64），
  所以只是 toast 这一层的观感偏差，但对象是偿付能力结论。

### [Minor] J-17 唯二覆盖 Balances / Snapshots 的测试只断言"搜索之前"，且 MSW 忽略 query 参数

- **位置**：`test/pages/static-pages.test.tsx:9-27`；`test/pages/render-page.tsx:15-21`
- **判定**：CONFIRMED
- 这是 J-1 / J-2 能活下来的直接原因。`getOk(path, data)` 只按 path 匹配，
  query 全忽略，所以即使补上"输入 holder 后"的用例，缺必填参数依然是绿的。
  修 J-1/J-2 时需要同时让 MSW handler 校验必填 query（否则回归 pin 是空洞的）。

### [Minor] J-18 `enabled` 门在 headless 面是一个静默 no-op 陷阱

- **位置**：`src/hooks/use-system.ts:59`（`useSnapshots`）、
  `src/hooks/use-journals.ts:70`（`useEntries`）、`src/hooks/use-journals.ts:26`（`useJournal`）
- **判定**：CONFIRMED
- 三个 hook 都从 `./headless` 导出。消费方调 `useEntries({})` 或
  `useSnapshots({currency_uid: x})` 会得到一个永远 `isLoading===false / isError===false /
  data===undefined` 的对象，没有任何信号说明"我没跑"。签名上看不出 `holder` 是必填。
  这是 J-2 的可复用版本，会在每个宿主重演。（部分属 E territory，见「移交」。）

### [Minor] J-19 ReconciliationPage 的两处 inline `isError` 不走 `errorText`，文案是「Check the API logs.」

- **位置**：`src/components/pages/ReconciliationPage.tsx:45-50,101-106`；
  heroui 同文案在 `:25,:40`
- **判定**：CONFIRMED
- `fd87cae` 说 M1 是 "surface server error text on **every** mutation"，
  但这两条走的是 inline 渲染而不是 `.mutate` 的 `onError` 回调，
  被 `errorText` 的改造漏掉。（叠加 J-8，实际影响有限——服务端本来也只发码表文案。）
  另外「Check the API logs」把内部排障手段写进了操作面文案。

### [Minor] J-20 C1 的 mutation-feedback 门禁扫描面窄于它守护的不变式

- **位置**：`test/mutation-feedback.test.ts:13-20,63-78,110-113`
- **判定**：CONFIRMED
- ① 只扫 `src/components/pages/*.tsx` + `src/heroui/pages/*.tsx`，
  不扫 `src/wallet/**`（终端用户面）与 `src/components/dashboard/**`；
  ② 正则 `\.mutate\(` 不匹配 `.mutateAsync(`（heroui ReconciliationPage 的两处）；
  ③ 「既无 onSuccess 也无 onError」被显式豁免。
  今天扫描面外没有真实违例（两处 wallet `.mutate` 都带 onError），
  但一个新页面放在 `dashboard/` 或 `wallet/` 下就绕过门禁了。

### [Minor] J-21 钱包流水行自行从 `direction` 推导正负号并硬拼 `+`/`-`，与 `formatSignedAmount` 的契约相悖，且 TS 侧没有任何东西钉住"服务端发绝对值"

- **位置**：`src/wallet/components/transaction-list.tsx:80-91`；
  `src/wallet/heroui/transaction-list.tsx` 同形；契约在
  `src/lib/utils/display.ts:96-99`（"callers … must never re-derive or strip the sign themselves"）
- **判定**：CONFIRMED
- 今天正确，因为 `postgres/holder_store.go:144-145` 是
  `Direction: direction, Amount: net.Abs()`。但 TS 侧的 wallet 类型没有"绝对值"这条注释，
  也没有测试钉它。服务端哪天不再 `Abs()`，展示就变成 `--5.0000` ——
  M1 修的正是"符号被两处各管一半"这一类。

### [Minor] J-22 `DepositAddressCard` 的 `assets: string[]` 接受空数组，产出「Only send  on Ethereum」

- **位置**：`src/wallet/components/deposit-address-card.tsx:76-83,154-157`；heroui 同形
- **判定**：CONFIRMED
- M5（`fd87cae`）把 `network` / `assets` 改成必填以避免"发错链不可恢复"，
  但类型只保证"传了数组"，没保证非空。`assets.join(" or ")` 对 `[]` 得空串。
  这条警告正是防不可恢复资损的那一句。

### [Minor] J-23 dev 依赖存在 1 个 high + 2 个 low 通告；生产依赖 0

- **位置**：`web/package-lock.json`（实跑 `npm audit --omit=dev` → `found 0 vulnerabilities`；
  `npm audit` → browserslist high ×2 advisory、esbuild low、postcss-selector-parser low）
- **判定**：CONFIRMED
- 三者都只在构建期出现，不进 `dist`。记录不定级。

---

## 上轮修复复核（cea9e5d / f86de1e / fd87cae）

> 判据：① 修复是否接到真实路径；② 若把修复拆掉，回归 pin 会不会红；③ 有没有在别处复制出同形缺陷。
> 未找到 2026-08-26 web audit 的原始报告文件（`docs/audits/` 下只有
> `2026-08-25-financial-engineering/`，全仓 grep 也没有 web 报告），故按三条 commit body 逐条核。

| 条目 | 接到真实路径？ | pin 能证伪？ | 同形缺陷复制？ |
|---|---|---|---|
| **C1** 每个 `.mutate` 有 onError | ✅ 全 15×2 页面实测；`errorText` 覆盖除 wallet 外的 22 处 `onError` | ⚠️ 能，但扫描面窄（**J-20**） | 无违例，但门禁盲区在 `wallet/` 与 `dashboard/` |
| **C2** clipboard 三态 | ✅ `clipboard.ts` 返回 promise，两个 skin 都按 `ok` 分支 toast | ✅ `test/lib/clipboard.test.ts` + 两个 skin 的渲染级 pin | 无 |
| **M4** 幂等 key 每次 attempt 重铸 | ⚠️ **部分**：`useLedgerMutation` 与 booking/reservation 路径已修；`useReverseJournal` 仍落回 `client.ts` 的 per-attempt `randomUUID()`（**J-15**）；metadata 8 个 mutation 发的 key 服务端不读（**J-14**） | ⚠️ pin 只测同一 variables 的重试，跨实体维度零覆盖（**J-13**） | key 的作用域从"HTTP attempt"搬到了"hook 实例"，而正确粒度是"实体+payload"（**J-13**） |
| **M6** skin parity | ✅ 三条静态断言真的在跑；逐对实测 hook 集合一致（附录 A） | ⚠️ 只比 token 计数，比不了"两边同样地错"——**J-1 / J-2 就是两个 skin 对称地错**，门禁必然绿 | 无 |
| **M1** 负号 | ✅ `formatSignedAmount` 返回带号 `text`，两个 skin 的 drift 单元格都用 `drift.text`（`ReconciliationPage.tsx:137-141` / heroui `:121`） | ✅ `test/lib/display.test.ts:80-95` 直接断言 `"-3.2000"`，删掉符号即红 | ⚠️ 兄弟仍在：`transaction-list.tsx` 自行从 `direction` 推号（**J-21**） |
| **M2** classification 查询失败降级成假空态 | ✅ `use-classification-id.ts` 暴露 isLoading/isError，`useDeposits/useWithdrawals/useSweeps` 折叠（`use-deposits.ts:46-49` 等） | ✅ `test/hooks/use-deposits.test.tsx` 等三个 pin | ❌ **同形缺陷仍在别处**：`useSnapshots` 的 `enabled` 门（**J-2**）、`useWalletHolds` 的未检查（**J-3**）、`BalancesPage` 丢弃 `isError`（**J-1**）——M2 只修了 classification 这一个 gate，没有推广成"每个 gated/未消费的 query 都要折叠状态" |
| **M3** 金额格式化器无测试 | ✅ `test/lib/display.test.ts` 109 行，覆盖分档/符号/零/截断 | ✅ | ⚠️ 第三个格式化器 `formatCompact` 仍是零测试（**J-10**） |
| **M5**（f86de1e partial + fd87cae） 手写类型 ↔ 生成 schema | ⚠️ 11/21 钉住；注释所列"spec 无 schema"已过期 5 项（**J-7**）；query 参数层完全无覆盖（**J-6**） | ✅ 已钉的 11 条是真门（`tsc --noEmit` 会红） | 无 |
| **M7** 每个 payload 一个 key | ✅ `usePayloadIdempotencyKey` 接到两个 skin 的 settle-partial / settle / confirming / confirm | ✅ `test/hooks/use-idempotency-key.test.ts` + `test/pages/reservation-settle-partial.test.tsx` | 无（但见 J-13：非 payload-keyed 的那批仍是 hook 作用域） |
| **fd87cae C1** nav↔route | ✅ `test/nav-routes.test.ts` 真的 glob `web/src/app` | ✅ | 反向（app 有路由但 nav 无入口）不覆盖 |
| **fd87cae C2a** 全局 token | ⚠️ 门禁真的在跑，但 allowlist 的洞恰好开在包唯一改写宿主值的 token 上（**J-4**） | ⚠️ | heroui 侧无此问题（值是 Tailwind 原版），即两个 skin 不对称 |

**总评**：C1 / C2 / M1 / M3 / M7 是干净的修复（接到路径 + 有能证伪的 pin）。
M2 / M4 / M5 / M6 / C2a 是"修了点上、没修面上"：M2 只修 classification 一个 gate 而
同形的三处还在；M4 把 key 的错误粒度从 attempt 挪到了 hook 实例；M6 的门禁按构造无法发现
"两个 skin 对称地错"，而本轮三条 Major 全是这个形态。

---

## 移交

- **H（structure-and-contract）**：`docs/openapi.yaml:1250-1262` 的 `/snapshots` query 参数名与
  handler 不符（J-6）；spec 的 `paths` 类型无任何消费方，端点/参数层没有机器校验。
- **H / D**：`pkg/httpx/response.go:147` 的 `ErrorMessage.Fields` 全仓无写入点——
  `api-contract.md §1` 的 `message.fields` 契约只有形状没有实现（J-8 后半）。
- **D（threat-model）**：`httpx.Error` 用静态码表脱敏是对的，但 `bizcode.RegisterDisplayMessage`
  (`pkg/bizcode/errors.go:224`) 允许运行时改写码表——谁能调、宿主改写后会不会把内部串
  推到用户面，我没走完。
- **E（consumer-surface）**：`useEntries` / `useSnapshots` / `useJournal` 的 `enabled` 门
  在 headless 面是静默 no-op（J-18）；`./headless` 不出金额格式化器（J-9）是 facade 面的同类问题。
- **F（test-credibility）**：`test/pages/render-page.tsx` 的 `getOk` 忽略 query 参数，
  是"测试做了真实服务端不会做的事"的前端版本（J-17）。
- **I（operability）**：ReconciliationPage 的「Check the API logs.」把排障手段写进操作面文案（J-19）。

## 我没能验证的

1. **`web/`（dogfood Next app）本身只做了目录与 CI 层的检查**，没有逐文件读
   `web/src/app/api/v1/[...path]/route.ts` 与 `web/src/app/api/session/` ——
   API key 是否会经代理泄漏到浏览器、session cookie 的属性，我没核。
2. **没有实跑浏览器**。J-1 / J-2 / J-3 是读完前后端两侧调用链推断的（服务端必填校验、
   React Query v5 disabled query 的 `isLoading/isError` 语义），标 CONFIRMED 但没有
   在真实 Next dev server 上目视复现。
3. **`heroui` skin 只在结构层逐对 diff**（hook 集合、four-state token、金额格式化调用点、
   a11y 计数，见附录 A），没有逐行读完 15 个 heroui 页面的全部 JSX。
   heroui 独有的 `Table.ScrollContainer` / `isPending` render-prop 等交互细节可能还有个别差异。
4. **`recharts` 的 `<Tooltip>` 默认渲染格式**我按其默认行为推断（无 `formatter` 时输出 dataKey 原值），
   没有实跑截图确认具体串形。J-5 的定性（未走 `formatAmount`、值来自 `parseFloat`）是确定的，
   具体显示串形不是。
5. **`dist/heroui.css` 的全局 token 只做了字体维度对比**，另外 56 个 token 与 Tailwind
   默认值是否逐一相同没有全量比对。
6. **`@heroui/react` 组件自身的 a11y**（`Table.Content` / `Pagination` 内部实现）当作黑盒，
   没有审。
7. **`tsup.config.ts` 的 tree-shaking 只验证了 `./headless` 的传递外部依赖**
   （实测只有 react / react/jsx-runtime / @tanstack/react-query）；
   `./charts`、`./wallet/headless`、`./heroui/charts` 的传递依赖没有逐个走。
8. **`test/client/types-conform.ts` 的双向性**我按 TS 赋值兼容性规则推断，
   没有真的插入一处矛盾类型跑 `tsc` 确认它会红（契约 §2「拆掉再看」只在内存里推演）。

---

## 附录 A — 双 skin 逐对 diff

15 对页面（`src/components/pages/*.tsx` ↔ `src/heroui/pages/*.tsx`）+ 4 对 wallet 组件。

### A.1 hook 集合（`useXxx(` 调用点）

| 页面 | 差异 |
|---|---|
| DashboardPage | shadcn 委派给 `src/components/dashboard/{health-cards,recent-journals,balance-trend}.tsx`，heroui 内联。**数据侧一致**（`useHealth` / `useJournals` / `useSystemBalances` / `useUidCodeLookups` 两边都有），skin-parity gate 的 `SHADCN_SUBCOMPONENT_DIRS` 映射覆盖了这一点 |
| DepositReviewsPage | heroui 多 1 个 `useState(false)`（受控 modal 开关）；无行为差异 |
| WithdrawalsPage | heroui 多 5 个 `useState(false)`（5 个受控 modal）；shadcn 用非受控 `AlertDialog`。**动作集合与 status 分派完全一致**（`ReserveButton/ReserveConfirm`、`ReviewButtons`、`ProcessDialog/ProcessModal`、`ConfirmButton/ConfirmConfirm`、`FailDialog/FailModal`、`RetryButton/RetryConfirm`，`:352-361` ↔ `:503-512`） |
| 其余 12 对 | hook 集合完全一致 |

### A.2 金额格式化调用点（`formatAmount(`）

| 页面 | shadcn | heroui | 说明 |
|---|---|---|---|
| BalancesPage / SnapshotsPage / SweepMonitorPage / WithdrawalsPage | 1 | 1 | 一致 |
| DepositsPage / JournalsPage / ReservationsPage | 2 | 2 | 一致 |
| DepositReviewsPage | 3 | 3 | 一致 |
| JournalDetailPage | 5 | 5 | 一致 |
| **ReconciliationPage** | **4** | **6** | heroui 多的 2 处在 `toast.promise` 的 success 文案（`:24,:39`），渲染面一致。见 **J-16** |
| Classifications / Currencies / JournalTypes / Templates / Dashboard | 0/0 | 0/1 | heroui DashboardPage 内联了 shadcn 放在 `dashboard/recent-journals.tsx` 里的那处 |

`formatSignedAmount` 两侧各 1 处（drift 单元格），符号处理一致 —— M1 修复在两个 skin 都落地。

### A.3 四态（loading → error → empty → data）

| 页面 | isLoading | isError | empty | 结论 |
|---|---|---|---|---|
| 13 对列表页 | 2 : 2 | 2 : 2 | 1 : 1 | 结构对称 |
| JournalDetailPage | 2 : 2 | 2 : 2 | 0 : 0 | 详情页无 empty 态，合理 |
| ReconciliationPage | 0 : 0 | 2 : 0 | — | mutation 页；shadcn inline `isError`，heroui `toast.promise` error 分支。对称但都不用 `errorText`（**J-19**） |
| **SnapshotsPage** | 2 : 2 | 2 : 2 | 2 : 1 | **两侧同样地错**（**J-2**）——差的 1 是文案写法（三元 vs `hasSearched`），不是逻辑 |
| **BalancesPage（snapshots 那条 query）** | — | **0 : 0** | — | **两侧都丢弃 `isError`**（**J-1**） |
| **wallet balance-card（holds 那条 query）** | — | **0 : 0** | — | **两侧都不读 holds 状态**（**J-3**） |

### A.4 a11y / 溢出加固计数

| 页面 | aria(sc:hu) | truncate(sc:hu) | min-w-0(sc:hu) |
|---|---|---|---|
| BalancesPage | 0:2 | 2:2 | 0:0 |
| ClassificationsPage | 0:1 | 1:1 | 0:0 |
| CurrenciesPage | 0:1 | 1:1 | 0:0 |
| DashboardPage | 0:1 | 0:2 | 0:0 |
| DepositReviewsPage | 0:2 | 1:1 | 0:0 |
| DepositsPage | 1:6 | 1:2 | 0:0 |
| JournalDetailPage | 0:1 | 3:0 | 1:0 |
| JournalsPage | 0:1 | 2:2 | 0:0 |
| JournalTypesPage | 0:1 | 1:1 | 0:0 |
| ReconciliationPage | 0:1 | 0:0 | 0:0 |
| ReservationsPage | 0:3 | 1:1 | 0:0 |
| SnapshotsPage | 0:1 | 0:0 | 0:0 |
| SweepMonitorPage | 0:1 | 2:2 | 0:0 |
| TemplatesPage | 2:9 | 0:4 | 0:7 |
| WithdrawalsPage | 0:3 | 2:3 | 0:0 |
| **合计** | **3 : 34** | **17 : 22** | **1 : 7** |

见 **J-12**。heroui 的 aria 优势有一部分来自 HeroUI `<Table.Content>` 强制要求 `aria-label`，
但 `TemplatesPage`（9:2）与 `DepositsPage`（6:1）的差是真实的表单/按钮标注差距，
`TemplatesPage` 的 `min-w-0` 7:0 是真实的 truncate 失效风险差距。

### A.5 wallet 四对组件

| 组件 | 结论 |
|---|---|
| `deposit-address-card` | 逐行 diff（去空白）后只有组件库替换 + 语义色名（`text-primary`→`text-success`、`text-destructive`→`text-danger`）。逻辑镜像完好，M5 的 network/assets 必填 + 不可恢复警告两侧一致 |
| `balance-card` | 逻辑镜像完好——**包括同样的 holds 状态未检查缺陷（J-3）** |
| `transaction-list` | 镜像完好，**包括同样的 direction 推号写法（J-21）** |
| `wallet-panel` | 镜像完好 |

### A.6 全局样式产物

| | `dist/styles.css` | `dist/heroui.css` |
|---|---|---|
| 全局 `:root,:host` token 数 | 58 | 62 |
| `--font-sans` 值 | `var(--font-geist-sans,ui-sans-serif, system-ui, sans-serif)` ← **包自己的值（J-4）** | `ui-sans-serif, system-ui, sans-serif, "A…` ← Tailwind 原版 |
| scoped preflight | 有（`.ledger-root`） | 无（宿主拥有 HeroUI 主题） |
| `.ledger-root` 业务 token | 有 | 无 |

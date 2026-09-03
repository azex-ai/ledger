# 独立评审 · 消费方视角（第一次 import 这个库的后端工程师）

- **评审对象**：`/Users/aaron/projects/ledger` @ `c854c6e`（worktree `r3-consumer`，分支 `r3-consumer`）
- **日期**：2026-09-03（SGT）
- **视角约束**：只读消费方可见材料——`README.md` / `docs/COOKBOOK.md` / `docs/api.md` / `docs/frontend.md` / `docs/openapi.yaml` / `CHANGELOG.md` / `docs/BREAKING.md` / `docs/INVARIANTS.md` / `docs/RUNBOOK.md` / `examples/` / `go doc` 导出符号 / `docs/api-surface.txt`。**未读** `docs/audits/`、`docs/plans/`、任何 `_test.go`、任何 `.go` 实现源码（`server/handler_*.go` 等一律没看，403/503 的成因全部靠黑盒试探 + 日志推断）。
- **环境**：`postgres:17.2-alpine` 容器（`r3consumer-pg`，映射 15432），Go 1.26.6，消费方 module 建在 scratchpad，主仓零写入（本文件除外）。

---

## 摘要

全链路（充值 → 预留 → 结算 → 提现 → 对账）**跑通了**，账务本身没问题：双分录平衡、幂等三态、TOCTOU 预留、reversal、solvency、16 项 full reconcile 全部按文档表现。8 个 example 有 7 个一次跑通。`RunInTx` clone 的拒绝面（嵌套 / `AttestationService` / `EnableOnchain` / `Worker` / `RegisterChannel` / `Authorize` / `ConfirmPending` / 带闸 `Reserve`）**全部命中且错误文案精确到"该怎么改"**——这是我见过最扎实的一组防误用护栏。CHANGELOG `[Unreleased]` 里我逐条核过的 14 项行为声明，**13 项 MATCH**。

问题几乎全部集中在**文档与代码之间**，而不是代码内部。最要命的是：**README 的两个 "Quick Start" 代码块都不能编译**，而其中 worker 那个按最显然的方式改对之后，会**静默地一个后台任务都不启动**——CHANGELOG 自己点名了"按旧版 README Quick Start 接线的消费方"正是这次改动要打破的对象，但 README 没跟着改。

共 **27 条**：Critical 2 / Major 9 / Minor 16。

---

## 第一小时时间线（文档说 vs 实际发生）

| # | 我做了什么 | 文档说会发生 | 实际发生 |
|---|---|---|---|
| 1 | `make db` | README:259-261「`docker-compose.yml` here starts only PostgreSQL, for local development (`make db`)」 | **失败**：`required variable POSTGRES_PASSWORD is missing a value`。README / Makefile 都没提要先准备 `.env`；`.env.example` 存在但**全仓无任何文档引用它** |
| 2 | 自建容器，跑 README Tier 1：`ledger.Migrate(dbURL)` → `pgxpool.New(ctx, dbURL)` → `ledger.New(pool)` | README:83-90 只说「传给 `Migrate` 的连接要能 `CREATE ROLE`」 | 全部成功、**零警告**。但我顺手调了 API 表里列着的 `svc.AssertRuntimeRole(ctx)`，才发现：`connected as "ledger", expected "ledger_app" -- the ACL-enforced invariants (I-22, I-42, and the append-only guards) ... on this connection they are not in force`。→ **F-C1** |
| 3 | 翻 `examples/*/main.go` 想确认自己是不是接错了 | RUNBOOK:817「Every `examples/*/main.go` reads them」（两个凭证） | 7 个 example 全部读 `DATABASE_URL` + `MIGRATE_DATABASE_URL` 两个。**只有 README 的 Quick Start 用一个 URL**，而那是新人唯一会照抄的地方 |
| 4 | 照抄 README:198-201 起 worker | 「Background worker (rollup, expiry, reconcile, snapshots, partition management…)」 | **编译失败**：`svc.Worker returns 2 values`。→ **F-C2a** |
| 5 | 改成 `worker, _ := svc.Worker(...)`；`go worker.Run(ctx)` 照抄 | 同上 | **一个 job 都没起**，进程里毫无痕迹。手动接住 `Run` 的返回值才看到：`refusing to start with the default silent logger`。README:217 明说 logger 是 opt-in，README:200 又把 `Run` 的 error 丢进 goroutine。→ **F-C2b** |
| 6 | 装 preset，走充值：`SetLifecycleIfEmpty` + `CreateBooking` + `Transition` + `ExecuteTemplate("deposit_confirm")` | README:139-192 | 全绿。balance=100，breakdown `{Available:100 Pending:0 Locked:0 Total:100}` |
| 7 | COOKBOOK Recipe 4：`Reserve(50)` → `RunInTx{Settle(32) + 记账}` | COOKBOOK:219-283 | 全绿。`HeldAmount` 50 → 0，余额 100 → 68 |
| 8 | 提现 + 对账 + solvency | README / COOKBOOK | 全绿。`Balanced:true Gap:0`，`Liability:48 Custodial:48 Solvent:true` |
| 9 | 挂 `server/`，照抄 README:229-242 | 「returns the full ledger API as an `http.Handler`」 | **编译失败**：`srv.Handler undefined`。`*server.Server` 自己实现了 `ServeHTTP`，正确写法是 `http.ListenAndServe(":8080", srv)`。→ **F-C2c** |
| 10 | 起 `examples/fullstack/backend`，按 `docs/api.md` §1 打 `POST /bookings {"classification_code":"deposit"}` | api.md:226-268 给了这个请求的完整示例，状态码列 `201/400/401/404/422/429/503` | **HTTP 500 / code 19999「An unexpected error occurred」**。服务端日志：`classification "deposit" has no lifecycle`。而 api.md 的错误表说 500 **可重试** → 客户端会永远重试一个永久失败的请求。→ **F-M1** |
| 11 | 按 api.md §4 打 `POST /journals`（用它自己示例里的 `cls-custodial`） | api.md:485-508，状态码列里**没有 403** | **403 / 10150「You don't have permission」**。openapi.yaml 里 `POST /journals` **有** 403。→ **F-M7** |
| 12 | 注册内置 `evm` channel adapter，打 `POST /webhooks/evm` | README:552「`POST /api/v1/webhooks/stripe` will now route through your adapter」；api.md:377「the server routes a sighting-capable adapter to the deposit-ingestion path **automatically**」 | **503 / 18102「This feature is not enabled on this server」**，每一次都是。内置 `evm` 实现了 `ParseSighting`，于是被路由去 deposit-ingestion，而那条路要求宿主另外调 `srv.SetDepositIngester(...)`——两份文档都没写。换一个**不**实现 `ParseSighting` 的自写 adapter（README:528-552 那个形状），立刻走通 legacy 路径。→ **F-M6** |

> 第一小时的净结果：**照抄 README 的两个 Quick Start，得到的是两个不编译的代码块 + 一个静默不工作的后台 worker + 一个所有 ACL 不变量都不生效的运行时连接。** 账务逻辑本身一次都没错。

---

## 默认关着的保护 · 清单与信号

| 保护 | 默认 | 我是怎么知道的 | 文档说了吗 | 判定 |
|---|---|---|---|---|
| 每笔 journal 签名（`WithAttestor`） | 关 | worker 启动 `WARN`，逐字说清后果 + 该调哪个函数 | README:967-977 说了 | **信号极好** ✅ |
| 外部 anchor | 关 | worker 启动 `WARN`；`StartupReport.AttestationAnchor` / `AttestationAnchorType` 两个字段可编程读 | README:988-1030 说了 | **信号极好** ✅ |
| `AuthVerifier`（提现闸） | 关 | worker 启动 `WARN`，点名 `RequireVerifiedBalance` 会拒绝一切 | 说了 | **信号极好** ✅ |
| inbound webhook 重放缓存 | 关 | `server.NewFromDeps` **构造时**就 `WARN`，文案给出 `srv.SetWebhookNonceRecorder(svc.WebhookNonceRecorder())` | README:236-241 / :1080 / api.md:410-419 都说了 | 好 ✅（只是 README 说"第一次 callback 才 warn"，实际是构造时——比文档更强） |
| API key 认证 | 关（`APIKeys` 空时） | 构造时 `WARN`「every endpoint is open to any caller」 | README:1083 说了 | 好 ✅ |
| CORS 通配 | 开（dev） | 构造时 `WARN` | 说了 | 好 ✅ |
| `RequireVerifiedBalance` | 关（逐调用） | `ReserveInput` 字段 godoc 极详尽 | 说了 | 好 ✅ |
| 开发者 credit preset | 关 | `POST /dev/credits` → 503/18102；需要 `ENV=dev` + `DEV_CREDIT_ENABLED` + 显式装 preset | 说了 | 好 ✅ |
| `SetFullReconciler`（全量对账） | **开**（已自动接线） | `StartupReport.FullReconcile=true` + 启动日志 `job=full_reconcile` | README:211-213 说它**仍需显式调用、是 deliberately opt-in** | **文档反了** → F-M5 |
| **`ledger_app` 运行时角色 / ACL 不变量（I-22 / I-42 / append-only guard）** | **关**（连接是 superuser 时） | **只有主动调 `svc.AssertRuntimeRole(ctx)`**。`ledger.New` 不警告，worker startup report 不警告，`StartupReport.Warnings` 里没有它 | README 只在 §Prerequisite 提"迁移需要 CREATE ROLE"，**没说运行时要换成 `ledger_app`**；Quick Start 用同一个 URL；没有 example 调 `AssertRuntimeRole` | **无信号** → **F-C1** |
| `Deps.ReadyProbe` / `SetReady` | 关 | `/system/ready` 永远 503 | api.md:1068-1077 说得很清楚（还自陈了历史） | 好 ✅ / 但 **README:1081 仍是被 api.md 点名作废的那句话** → F-M9 |
| `srv.SetDepositIngester` | 关 | webhook 全部 503/18102，wire 文案不说是哪个 feature | **两份文档都没提** | **无信号** → F-M6 |
| `presets.InstallPendingBundle` | 名义上关 | —— | README:716-719 承诺"未装前两个 accessor 都返回 `core.ErrNotFound`" | **闸根本不存在**（见 F-M2） |

---

## 发现

### Critical

#### F-C1 · README Quick Start 让运行时连接 = 迁移连接，所有 ACL 不变量静默失效，且无任何信号
`README.md:111-113`（`ledger.Migrate(dbURL)` / `pgxpool.New(ctx, dbURL)` / `ledger.New(pool)` 同一个 `dbURL`）+ `README.md:83-90`（Prerequisite 只讲迁移需要 `CREATE ROLE`）。

复现：按 README 逐字做，然后调 API 表里列着的 `svc.AssertRuntimeRole(ctx)`：

```
postgres: role check: connected as "ledger", expected "ledger_app" -- the ACL-enforced
invariants (I-22, I-42, and the append-only guards) constrain "ledger_app" and nothing
else, so on this connection they are not in force: invalid input
```

关键是**信号缺口**，不是配置本身：我用同一条 superuser 连接注入了真 logger 起 worker，`ledger.New` 没警告，`worker: starting` 那行没提，`StartupReport.Warnings` 三条警告分别是 Attestor / anchor / AuthVerifier——**唯独这一条没有**。也就是说这个库对"缺 attestor"喊得震天响，对"append-only 触发器压根管不到你这条连接"一声不吭。而 RUNBOOK:840-841 自己写着「`ledger_app` never owning anything is what makes I-22 true」。

`docs/RUNBOOK.md:817` 声称「Every `examples/*/main.go` reads them」，实测 7 个 example 确实都读两个 URL——**唯一用单 URL 的就是 README 的 Quick Start**，也就是新人唯一会照抄的地方。`.env.example:6` 的 `DATABASE_URL` 同样指向 superuser，且整份 `.env.example` 里没有 `MIGRATE_DATABASE_URL`。

建议（任选其一，最好都做）：① Quick Start 改成两个 URL，和 example 一致；② `ledger.New` 在角色不是 `ledger_app` 时通过 logger 发一条 `WARN`（和另外三条并列），或把它加进 `StartupReport.Warnings`。

**判定：CONFIRMED**

---

#### F-C2 · README 的两个 Quick Start 代码块都不编译；worker 那个"修好"之后静默启动零个后台任务

三处，同一根因（README 没跟上已发布的破坏性变更）：

**(a) `README.md:199`** — `worker := svc.Worker(service.DefaultWorkerConfig())`
```
assignment mismatch: 1 variable but svc.Worker returns 2 values
```
`docs/api-surface.txt:1203` 记的是 `ledger.Service.Worker = method (cfg service.WorkerConfig) (*service.Worker, error)`，CHANGELOG `[Unreleased]` 明写「`worker := svc.Worker(cfg)` → `worker, err := svc.Worker(cfg)`」。`README.md:755` 的 API 表也仍写返回 `*service.Worker`。`docs/COOKBOOK.md:363` 同样的写法。

**(b) `README.md:200`** — `go worker.Run(ctx)`。改成 `worker, _ := svc.Worker(...)` 之后照抄这行，结果是：

```
(a) Worker() err = <nil>
(a) Run() err    = service: worker: refusing to start with the default silent logger: ...
```

rollup / expiry / reconcile / snapshot / partition **一个都没起**，而 error 被 `go` 语句吞掉。README:217 明说「Observability (logger / metrics / tracing) is opt-in」，Tier 2 的 `svc, _ := ledger.New(pool)` 也确实没注 logger——**README 的两句话互相打架，合起来的结果就是静默停摆**。这正是 `Worker.Run` 自己的 godoc 要防的东西（"a worker booted silently cannot be told apart from one that never booted at all"），也正是 CHANGELOG 点名的对象：*"a consumer wired per the old README Quick Start (no `WithLogger`) previously ran with zero output ... it now fails fast at `Run` instead"*。fail-fast 的机制是对的；README 把它 fail 掉的那个信号丢了。

**(c) `README.md:242`** — `http.ListenAndServe(":8080", srv.Handler())`
```
srv.Handler undefined (type *server.Server has no field or method Handler)
```
`docs/api-surface.txt:1706-1716` 里 `server.Server` 没有 `Handler`，它直接实现 `ServeHTTP`。正确写法 `http.ListenAndServe(":8080", srv)`。

建议：README 的每个 Quick Start 代码块加进可编译的 doc-example 门（现有 `readme_runnable_test.go` 显然没覆盖到这三处），并把 `go worker.Run(ctx)` 改成 CHANGELOG/godoc 里那个接住 error 的形状。

**判定：CONFIRMED（三处均实测）**

---

### Major

#### F-M3 · 带闸 `Reserve` 的可用额与所有消费方可见的余额读数不一致，且没有任何 API 能读到闸看到的那个数
`docs/COOKBOOK.md:219-283`（Recipe 4：`Settle` 放进 `RunInTx`）与 `docs/COOKBOOK.md:296-310`（Recipe 5 第 2 条：提现用 `RequireVerifiedBalance: true`）单独看都对，**合起来产生一个消费方无法诊断的状态**。

实测（余额 1000，`WithAttestor` 已配）：
```
ungated Reserve(200)                        -> ok
Settle(50) INSIDE RunInTx                   -> ok        （Recipe 4 教的写法）
GATED Reserve(900)                          -> insufficient balance: available 800
--- 同一时刻消费方能读到的东西 ---
GetBalanceBreakdown  -> {Available:1000 Pending:0 Locked:0 Total:1000}
HeldAmount           -> 0
```

三个读数三个答案：breakdown 说 1000，`HeldAmount` 说占用 0，闸内部算的是 800。原因写在 `core.ReserveInput.RequireVerifiedBalance` 的字段 godoc 里（RunInTx 内写的 discharge claim 签不了名 → 回落到"整笔 `ReservedAmount` 一直算到 `ExpiresAt`"），**但 COOKBOOK 建议用这个 flag 的那一段完全没提这层交互**，也没提"要把 `Settle`/`Release` 挪到顶层 Service 才能立刻放回"。默认 `ExpiresIn` 15 分钟，意味着每笔计量扣费都会把整个预算额度对提现闸冻结 15 分钟，且没有 error、没有 log、UI 上显示的可用余额是 1000。

建议：① COOKBOOK Recipe 5 第 2 条补一段交叉引用 Recipe 4；② 暴露一个"闸视角可用额"的只读方法（哪怕只是 `HeldAmount` 加一个 gated 变体），否则消费方无法向用户解释"为什么显示 1000 却提不出 900"。

**判定：CONFIRMED**

---

#### F-M2 · README 承诺的 pending-bundle 闸不存在；只装 `InstallDefaultPresets` 就能走完 `AddPending → ConfirmPending` 并产出可花余额
`README.md:716-719`：

> Requires `presets.InstallPendingBundle(ctx, ...)` — ... install it explicitly before using either accessor (**the calls below fail with `core.ErrNotFound` until you do**).

`README.md:361-364` 进一步把它定性为「opt-in on purpose (the former adds a whole extra deposit path)」。

实测（全新库，只 `InstallExtendedPresets`，**没有**调 `InstallPendingBundle`）：
```
AddPending      -> ok=true err=<nil>
breakdown       -> {Available:0 Pending:500 Locked:0 Total:500}
ConfirmPending  -> err=<nil>
breakdown       -> {Available:500 Pending:0 Locked:0 Total:500}
solvency        -> {Liability:500 Custodial:500 Solvent:true Margin:0}
```

两次调用、零 preset 安装，产出 500 可花余额，并且 solvency 认为它有 500 托管资产撑着（`AddPending` 记的是 DR custodial / CR pending，那笔托管收据是被**断言**出来的）。对比 `dev_credit` —— 同样"凭空造余额"，库要求 `ENV=dev` + `DEV_CREDIT_ENABLED` + 显式装 preset **三重**门。这条路一重都没有，而 README 声称有。

（根因看得见：`README.md:345` 自己列了 `DepositBundle()` 就带 `deposit_pending` / `deposit_confirm_pending` / `deposit_release_pending` 三个 journal type，所以 pending 路径在装 default preset 时就活了。要么 README 改口径，要么把这条路径真的挡起来。）

**判定：CONFIRMED**

---

#### F-M1 · `POST /api/v1/bookings` 对出厂的 `deposit` 分类返回 HTTP 500，且 HTTP 面没有任何办法挂上缺的 lifecycle
`docs/api.md:226-268` 用的正是 `{"classification_code": "deposit"}`，`docs/api.md:270` 列的状态码是 `201/400/401/404/422/429/503`，`docs/openapi.yaml` 里 `POST /bookings` 只声明 `201` 和 `422`。

实测（`examples/fullstack/backend` 出厂状态，装了 default preset）：
```
POST /bookings -> HTTP 500 code=19999 {"text":"An unexpected error occurred"}
服务端日志       -> classification "deposit" has no lifecycle
```

三重问题：
1. 这是**调用方可修的配置问题**，却报成不可归因的 internal error；
2. `docs/api.md:107` 的错误表把 500 归为 **Retryable: Yes**，遵守该契约的客户端会对一个永久失败的请求无限重试；
3. Go 侧的解法是 `svc.Classifications().SetLifecycleIfEmpty(...)`（README:160-171 有教），**HTTP 面没有对应端点**——`POST /classifications` 能带 `lifecycle` 建**新**分类，但改不了出厂的 `deposit`/`withdraw`。所以一个纯 HTTP 消费方按 api.md §1 走，创建 booking 这件事在出厂配置下做不到。

而 `examples/fullstack` 正是 README:250 指定的 "complete assembly"，它装了 preset、起了 worker、也没调 `SetLifecycleIfEmpty`。

**判定：CONFIRMED**

---

#### F-M4 · `docs/BREAKING.md` 缺三条它自己的规则要求必须有的条目
`docs/BREAKING.md:1-14` 开宗明义：「Every change that can break a consumer at build time or at run time gets an entry here, in the same commit as the change」，第 1 类明确包含「a removed or **re-signed** symbol」，并声称 `TestAPISurface_BreakingChangesAreDocumented` 会在改过的符号没被本文件点名时失败。

`docs/api-surface.txt` 已经记录了新签名：
- `:1203` `ledger.Service.Worker = method (cfg service.WorkerConfig) (*service.Worker, error)`
- `:2062` `service.Worker.Subscribe = method (handler func(context.Context, core.Event) error) error`

`CHANGELOG.md` `[Unreleased]` 的 "Go module — Breaking" 段落把这三项都列了：`Worker` 改返回值、`Subscribe` 改返回值、`Run` 拒绝 `NopLogger`（运行时破坏）。

`grep '^### ' docs/BREAKING.md` 的 30 个标题里，**一条都没有**。`grep -n 'Worker\|Subscribe\|NopLogger\|AllowSilent' docs/BREAKING.md` 只命中 4 处无关正文。

也就是说：`api-surface.txt` 的快照被重新生成了，但它 header 里写的那条纪律（"if the diff REMOVES or CHANGES a symbol ... add a docs/BREAKING.md entry in the same commit"）没执行；而按 BREAKING.md 的描述，本该有个门拦住这件事。**门要么没覆盖 struct 方法，要么被绕过了。** 这直接影响 F-C2——BREAKING.md 是消费方升级时唯一会逐条读的清单。

**判定：CONFIRMED**

---

#### F-M5 · README 说全量对账套件仍需显式开启；实际已自动接线并在跑
`README.md:208-215`：

> Two jobs still need an explicit call ... and the fleet-wide **full reconciliation suite** (`worker.SetFullReconciler(svc.FullReconciler(cfg))`, deliberately opt-in — it is a heavier scan than the lightweight accounting-equation check that always runs).

实测 `StartupReport`：`FullReconcile:true`，启动日志 `worker: started job=full_reconcile interval=1h0m0s`——**没调过任何 `SetFullReconciler`**。`(*ledger.Service).Worker` 的 godoc 和 CHANGELOG 都写明它现在自动接线。方向反了的文档比缺文档更贵：消费方会以为一个重扫描是关着的，实际它每小时在跑（README 自己强调这是"heavier scan"）。

**判定：CONFIRMED**

---

#### F-M6 · 注册内置 `evm` adapter + 挂 server 不够：所有 callback 返回 503「This feature is not enabled on this server」，直到宿主另调 `srv.SetDepositIngester(...)`
`README.md:528-552`（「`POST /api/v1/webhooks/stripe` will now route through your adapter」）与 `docs/api.md:371-377`（「the server routes a sighting-capable adapter to the deposit-ingestion path **automatically**」）都没提这个前置调用。

实测（`svc.RegisterChannel(onchain.New(secret))` + `server.NewFromDeps(..., Channels: svc.Channels(), ...)`，**没调** `SetDepositIngester`）：
```
built-in evm adapter implements ParseSighting: true
callback #1 -> HTTP 503 {"code":18102,"message":{"text":"This feature is not enabled on this server"}}
callback #2 -> HTTP 503 同上
-- 换成不实现 ParseSighting 的自写 adapter --
callback #3 -> HTTP 404 {"code":10201,...}   ← 走到了 ingestion（booking 不存在而已）
```

也就是：**用库自带的那个 adapter，webhook 完全不通；自己手写一个反而通。** wire 上的 18102 文案不说是"哪个 feature"，服务端日志也只有 `feature not enabled`——消费方拿不到任何指向 `SetDepositIngester` 的线索。这是任务里说的"必须先调 A 才能 B，但签名不体现"的标准形态：`SetDepositIngester` 是 `*server.Server` 上的一个可选 setter，构造函数不要它，编译器不管它。

（对比：`Deps.ReadyProbe` 把同一类问题做成了构造期字段，`docs/api.md:1096-1099` 还专门列了 "Two other lifecycle methods are equally easy to miss"——`SetHolderSurface` 和 `StartRateLimiterGC`。`SetDepositIngester` 属于同一族，却不在那份名单里。）

**判定：CONFIRMED**

---

#### F-M7 · `docs/api.md` §4 `POST /journals` 的示例请求在默认配置下被 403 拒绝，且该节状态码表没有 403
`docs/api.md:485-497` 的示例 entries 用 `cls-custodial`（`is_system`），`docs/api.md:508` 状态码列为 `201/400/401/422/429/503`。

实测：403 / 10150。服务端日志给出了很好的解释（`classification ... is system-managed ... or set AllowSystemClassificationPost after review`），`docs/openapi.yaml` 里 `POST /journals` 也**已经**声明了 403，`README.md:1082` 也讲了这个 guard——**只有 api.md 这一节没跟上**。同一份文档在 `POST /journals/template` 那节（:512-551）把这件事解释得非常好，还专门写了"an earlier revision of this document showed `deposit_confirm`, which the endpoint now answers with 403"——同样的清理没做到隔壁那节。

**判定：CONFIRMED**

---

#### F-M8 · 7 条 openapi 路径在 `docs/api.md` 里完全没有章节，其中包含账户冻结/关闭这种资金面控制
`README.md:948` 把 api.md 定位成「Long-form HTTP API reference with examples」，api.md 自身有 §1–§15 的完整编号结构。机器比对 `docs/openapi.yaml` 的 59 条路径与 api.md 全文（宽松匹配、参数名归一）后仍缺：

```
/accounts/{holder}/policies
/accounts/{holder}/policy          ← PUT，冻结/关闭账户 + balance-floor 覆盖
/deposits/{uid}/review/reject      （approve 只在 api.md:52 的一句权限说明里出现过）
/holder-tokens                     ← 整个 holder token 面
/holder/balances
/holder/deposit-address
/holders/{holder}/deposit-address
```

`PUT /accounts/{holder}/policy` 我实测可用（200），它能冻结一个 holder——这类端点没有 long-form 文档，消费方只能从 openapi schema 反推语义。

**判定：CONFIRMED**

---

#### F-M9 · `README.md:1081` 仍在讲一句 `docs/api.md` 自己点名"nothing implemented"的话
`README.md:1081`：

> **Health vs. readiness**: `/api/v1/system/health` returns 503 on DB failure; `/api/v1/system/ready` returns 503 until migrations + worker have booted.

`docs/api.md:1068-1077` 已修正并自陈历史：

> The library observes neither migrations nor the worker ... (E-M11 — this section used to say the probe turned green "once migrations + worker have booted", which nothing implemented; a deployment that wired neither got a permanent 503).

实测：不接 `ReadyProbe` / 不调 `SetReady`，`/system/ready` 恒为 `503 / 18101`。README 那句被修掉的话原封不动留在原地。

**判定：CONFIRMED**

---

#### F-M10 · `README.md:1001` / `docs/RUNBOOK.md:1229-1238` 说 `anchors/r2` 不能 `go get`；实测能
README:1001：「**Not yet independently `go get`-able** ... not `go get github.com/azex-ai/ledger/anchors/r2@<tag>` -- that does not yet resolve」。RUNBOOK:1233-1238 给出的理由是「Go ignores a dependency's own `replace` directives, so `go get ...@<tag>` from an external module does **not** resolve as-is」。

实测（全新 module，`GOWORK=off`）：
```
go get github.com/azex-ai/ledger/anchors/r2@latest
  -> go: added github.com/azex-ai/ledger/anchors/r2 v0.0.0-20260902015108-fd87cae8b89b
go build ./...   -> 成功
go run .         -> r2.Config    （真的 import 到了）
```
真正会失败的只有 `go mod tidy`，而且**不是文档说的那个原因**：
```
github.com/azex-ai/ledger/anchors/r2.test imports github.com/azex-ai/ledger/anchortest:
  module github.com/azex-ai/ledger@latest found (v0.6.0), but does not contain package .../anchortest
```
——是已发布的 root tag `v0.6.0` 里还没有 `anchortest`，跟 `replace` 无关。

影响：`anchors/r2` 是 README 反复强调的**唯一生产级 anchor**（「an anchor the attacker can also rewrite makes the rest decorative」）。文档劝退消费方走 `go get`，让他们去搞 parent-directory `go.work`，或者干脆推迟接真 anchor——而实际上 `go get` + `go build` 是通的。这条声明的方向错了，代价落在整条防篡改链的最外环。

**判定：CONFIRMED**

---

### Minor

| # | 位置 | 内容 | 判定 |
|---|---|---|---|
| F-m1 | `docs/api.md:283-291` | `POST /bookings/{uid}/transition` 的请求示例**没有** `idempotency_key`，该节末尾只写「Auth: required.」而没写「Idempotency: required」（同页 `POST /bookings` 两个都写了）。openapi 的 `TransitionInput` 把它标成 required。逐字照抄示例 → `400 / 10001` | CONFIRMED |
| F-m2 | `docs/COOKBOOK.md:363` | `w := svc.Worker(service.WorkerConfig{}) // sensible defaults incl. ExpirationInterval` —— 不编译（同 F-C2a），而且这一段（Recipe 6）的全部论点就是"worker 必须真的在跑，否则钱永久锁死"。注释说的 "sensible defaults" 倒是对的（`Worker` 会用 `DefaultWorkerConfig` 填零值字段），但它下一行 `go w.Run(ctx)` 同样吞掉 error | CONFIRMED |
| F-m3 | `.env.example:16` | `API_KEYS=dev-key-1,dev-key-2` → `server.LoadConfig()` 直接失败：`malformed API_KEYS entry (want name:scope:secret, got 1 fields)`（dev 和 production 都失败） | CONFIRMED |
| F-m4 | `.env.example:14-16` 注释 | 「Comma-separated list of allowed bearer-token API keys for **mutating endpoints (POST/PUT/PATCH/DELETE)**. Required in production; **without keys, all writes pass through unauthenticated**.」——三处不符：① 认证覆盖**每个**端点含 GET（README:1083）；② production 下没有 key 是**拒绝启动**（`API_KEYS is required when ENV="production"`），不是"放行"；③ 格式错（见 F-m3） | CONFIRMED |
| F-m5 | `.env.example` 整体 | 没有 `MIGRATE_DATABASE_URL`（7 个 example 全都读它），`DATABASE_URL` 指向 superuser（与 RUNBOOK §9 的 `ledger_app` 指导相反）；且这个文件**在 README / Makefile / 任何 docs 里都没有被引用过**，而 `make db` 恰恰依赖它提供的 `POSTGRES_PASSWORD` | CONFIRMED |
| F-m6 | `Makefile:48-49` + `README.md:259-261` | `make db` 开箱即失败（`POSTGRES_PASSWORD must be set`），README 说它就是本地起库的入口，没给这一步 | CONFIRMED |
| F-m7 | `CHANGELOG.md` `[Unreleased]` | 自相矛盾：一处写「the suite runs **sixteen** checks rather than fifteen」，另一处（`Worker` 自动接线那条）写「now gets the full **15-check** reconciliation suite」。实测 16：`[global_dr_cr_equality checkpoint_balance orphan_entries accounting_equation settlement_netting non_negative_balances role_less_liability untagged_holder_kind orphan_reservations idempotency_uniqueness stale_rollup_queue journal_dr_cr system_rollup_integrity snapshot_integrity unauthorized_journals period_close_violations]` | CONFIRMED |
| F-m8 | `examples/tx-compose/main.go:135` 附近 | 按 RUNBOOK 推荐的凭证划分（app = `ledger_app`）跑这个 example 直接死：`create demo_orders: ERROR: permission denied for schema public`。它在启动时对 app 凭证做 DDL；换 superuser 才通过。header 注释（:14-17）说「creates a tiny `demo_orders` table on startup」，没说这要 DDL 权限 | CONFIRMED |
| F-m9 | `anchordev` / README §Anchoring | 把同一个 anchor 目录用在一个被重建过的库上 → `VerifyLedger` 报 **TAMPERED**（`seq 1: DB root_hash does not match the externally anchored head`），与真实攻击**不可区分**。`examples/tamper-evident:135` 用 `os.MkdirTemp` 绕开了，所以 example 不会踩；文档没有任何提示。本地开发 reset 数据库是高频动作 | CONFIRMED |
| F-m10 | `core` store 命名 | `ClassificationStore.GetByCode` / `JournalTypeStore.GetJournalTypeByCode` / `TemplateStore.GetTemplate(code)` / `CurrencyStore.GetCurrency(uid)` —— 四种风格；而且**没有按 code 查 currency 的方法**，但 COOKBOOK 每个 recipe 都在用 `usdtUID` / `creditsUID`，消费方只能 `ListCurrencies` 全量拉回来过滤 | CONFIRMED |
| F-m11 | `README.md:145-201` | Tier 2 代码块用了 `presets.DepositLifecycle`(:169) 和 `service.DefaultWorkerConfig()`(:199)，但 README 里唯一的 import 块在 Tier 1(:101-109)，两个包都不在里面 | CONFIRMED |
| F-m12 | `README.md:238-239` 与 `:1080` | 都说「The **first unprotected callback** logs a warning」/「the first callback logs a warning saying so」。实际在 `server.NewFromDeps` **构造时**就打了（行为比文档更强，但描述不准；靠"第一次 callback"来发现的人会以为没打） | CONFIRMED |
| F-m13 | `POST /journals/deposit-tolerance` | 漏传 openapi 标为 required 的 `tolerance` → `fields: {"tolerance": "is not a valid decimal amount"}`。缺字段报成"格式非法"，与 `POST /holder-tokens` 的 `{"holder": "is required"}` 不一致 | CONFIRMED |
| F-m14 | 列表端点 `limit` | `docs/api.md:177` 说「`limit` defaults to `50` and is capped at `200`」。`?limit=999` / `?limit=0` / `?limit=abc` 全部 200 且静默回落到默认，没有 400。超界静默容忍尚可，`limit=abc` 静默吞掉是 fail-open | CONFIRMED |
| F-m15 | `README.md:203-215` 与 `svc.Worker` godoc | README 说「`worker.Subscribe(fn)` ... works with no extra wiring call」，但 `Subscribe` 现在返回 `error`（CHANGELOG 有，BREAKING.md 无——见 F-M4），README 的描述里没有体现要检查它 | CONFIRMED |
| F-m16 | `examples/fullstack/README.md:32` | 「see `backend/main.go`, **~230 lines** including seed data」；实际 345 行 | CONFIRMED |

---

## 值得记一笔的好东西（不是缺陷）

评审纪律要求只报缺陷，但下面几条是我实测到的、明显被人认真设计过的地方，写下来是为了避免整改时误伤：

1. **`RunInTx` clone 的拒绝面**。我逐个试了 10 个方法，每一个的错误文案都精确说明了"为什么这里不行 + 应该在哪里调"。例如 `Worker`：*"the Worker would be stitched from stores bound to a transaction RunInTx discards ... call Worker on the top-level Service"*。元数据 store 的写入（classification / journal type / currency / template）全部正确绑定事务，回滚后一条不留。
2. **worker 启动的三条 `WARN`**（Attestor / anchor / AuthVerifier）逐字说清"关着会失去什么 + 调哪个函数打开"，且同时进 `StartupReport.Warnings` 供程序读。这是这份代码库对"degraded 不能长得像 full"这条纪律执行得最好的地方——也正因为如此，F-C1 里那一条**没有**警告才格外刺眼。
3. **`docs/api.md` §2 Webhooks 与 §4 `POST /journals/template`**：把三种 403 的结构性理由、为什么 `status:"confirmed"` 被直接拒、以及"这份文档早先版本给的例子现在会 403"都写清楚了。这是我读过写得最好的 API 文档之一。
4. **`ReserveInput.RequireVerifiedBalance` 的字段 godoc** 把签名 / 不签名两条 hold 规则、为什么不能信状态机、以及"只有签名和时间流逝逃得过 DB 凭证"讲透了。F-M3 的问题不是这段文档，而是 COOKBOOK 建议用它的地方没交叉引用它。
5. **CHANGELOG `[Unreleased]` 的行为声明基本可信**：我逐条实测了 14 项（`Deactivate*` 返 `ErrNotFound`、停用币种被拒且是 `ErrInvalidInput`、`fee_charge` 4 legs、`equity` 变 debit-normal、`checkout_settlement_net` 拒 `gross_amount`、`ProtectedTemplateCodes()` 5 条含 `dev_credit`、`SolvencyCheck` 无匹配分类时 `ErrInvalidInput`、`ConfirmPending` 在 `RunInTx` 内被拒、`RegisterChannel` 在 clone 上报错、`reverse` 拒绝调用方 key、booking `expires_at` 为 `null` 不是 `""`、`GET /journals` newest-first、npm 版本线 0.3.0 → 0.5.1 → 0.6.0），**13 项 MATCH，1 项是 15/16 的自相矛盾（F-m7）**。
6. **README 关于 `replace` 不可用的那段负面声明是准确的**（:72-79），我复现出了逐字相同的 `invalid version: unknown revision 000000000000`。
7. **全仓文档内链零断裂**（README / api.md / COOKBOOK / RUNBOOK / INVARIANTS / frontend / BREAKING / CHANGELOG / CAPACITY / DR / fullstack README 交叉比对，按 GitHub slug 规则）。`openapi.yaml` 确为 59 paths / 100 schemas，与 README:947 一致。
8. **HTTP 包络 100% 合规**：我打的 40+ 个请求（含全部错误路径），响应体键集恒为 `{code, message, data}`，成功 `message:null`，失败 `data:null` 且 `message.text` 已脱敏（内部原文只在服务端日志里，靠 request_id 关联）。幂等三态（原样重放 / 换 payload → `409 / 10901`）行为与 `docs/api.md:149-173` 完全一致。

---

## 我没能验证的

- **`anchors/r2` 的真实行为**：没有 R2 账号 / Object-Lock bucket，只验到 `go get` + `go build` + `r2.Config` 可引用（F-M10）。`anchortest.RunConformance` 也没跑。
- **`chains/evm`**：e2e 测试带 `//go:build e2e`，需要真链，未触及。
- **并发 / TOCTOU / 多副本**：`Reserve` 的 advisory lock 序列化、`SELECT FOR UPDATE`、leader election、`SKIP LOCKED` rollup 队列——全部只做了单进程顺序调用，没做并发压测。
- **前端**：只查了 npm registry 元数据（`@azex/ledger-react@0.6.0`，`peerDependenciesMeta` 正确把 `@heroui/react` 标为 optional，与 README:265-272 一致）。没有 `npm install`、没有起 Next.js、没有开浏览器，`docs/frontend.md` 的 15 个 page component / RSC prefetch / 两套皮肤一致性均**未验证**。
- **`docs/INVARIANTS.md`**：只间接验证了我在链路里踩到的那几条（I-3 幂等、I-22/I-42 的 ACL 前提、I-32/I-49/I-65 的 gated reserve 行为、I-38 的 system-classification 闸）。7019 行里绝大多数没核。
- **`docs/RUNBOOK.md` 的处置流程**：reconciliation failure / solvency alert / rollup backlog / emergency stop / PITR 恢复演练，一条都没实操。
- **限流与 holder token 面**：`TRUSTED_PROXY_CIDRS`、per-IP token bucket、`StartRateLimiterGC` 未接时的桶泄漏、`/holder/*` 的 token 签发与鉴权行为，只做了单次冒烟。
- **`examples/fullstack` 的前端两个 flavor**（`web/` :3090 与 `web-heroui/` :3091）未启动。
- **`POST /webhooks/{channel}` 的三种 403**（channel mismatch / 非 deposit booking / `status:"confirmed"`）：被 F-M6 的 503 挡在前面，没能走到。
- **迁移的 `CREATE ROLE` 前置条件在非 superuser 路径上的行为**（RUNBOOK:840-900 描述的 `SET ROLE ledger_owner` / `ADMIN OPTION` / "refusing to run: N other session(s)" 分支）——我全程用 superuser 迁移。

---

## 附：环境与复现

```bash
docker run -d --name r3consumer-pg -e POSTGRES_DB=ledger -e POSTGRES_USER=ledger \
  -e POSTGRES_PASSWORD=ledger -p 15432:5432 postgres:17.2-alpine
docker exec r3consumer-pg psql -U ledger -d ledger -c "ALTER ROLE ledger_app WITH PASSWORD 'app';"

# 消费方 module（scratchpad，非主仓）
# go.work 用绝对路径 use 了 r3-consumer worktree 的 5 个 module + ./consumer
MIGRATE_DATABASE_URL='postgres://ledger:ledger@localhost:15432/<db>?sslmode=disable' \
DATABASE_URL='postgres://ledger_app:app@localhost:15432/<db>?sslmode=disable' \
go run ./<probe>
```

Example 执行结果：`embed` / `billing` / `credits-topup` / `event-subscribe` / `crypto-deposit` / `tamper-evident` 以 `ledger_app` 跑通；`tx-compose` 需 superuser（F-m8）；`fullstack/backend` 需 `HTTP_PORT` 覆盖（默认 8090 与本机其他服务冲突，非缺陷）。

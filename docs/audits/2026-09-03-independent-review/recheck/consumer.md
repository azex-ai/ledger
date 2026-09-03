# R3 复核 · 消费方视角

- **复核对象**：main `5a11533`（代码等于 `0e7512a`），worktree `/Users/aaron/projects/_worktrees/ledger/r3-consumer`，分支 `r3-recheck-consumer`
- **基线报告**：`docs/audits/2026-09-03-independent-review/consumer.md`（评审 `c854c6e`）
- **日期**：2026-09-04（SGT）
- **纪律**：只读，不修任何文件（本报告除外）。所有实验在本 worktree + scratchpad 消费方 module 里做，主仓零代码改动。
- **环境**：`postgres:17.2-alpine`（容器 `r3consumer-pg`，端口 15432），Go 1.26.6，`ledger_app` 已设密码，迁移用 superuser。

---

## 一句话结论

**Critical 复发 = 0。** 2 条 Critical（含 3 个子项）全部闭合并且是**机制性**闭合（README 24 个 Go 块进了编译门禁 + `StartupReport.RuntimeRoleWarning` 是新的运行时信号），不是改一句话了事。10 条 Major 中 **6 条完全闭合、4 条部分闭合**（残余均为文档/契约面，无行为面复发）。16 条 Minor 未纳入 W5-readme 的独占域（契约 §1 只写 `consumer C1 C2 M1–M10`），其中 3 条被顺带闭合，11 条逐字未动，2 条部分。另记 **3 条新问题**（1 条是 F-M8 的同类复发，1 条是清洁安装首次跑批的 ERROR 噪声，1 条是门禁覆盖面的观察）。

---

## Critical

| 原编号 | 原结论 | 复核方法 | 复核结论 | 残余 |
|---|---|---|---|---|
| **F-C1** | README Quick Start 让运行时连接 = 迁移连接（superuser），I-22 / I-42 / append-only guard 静默失效，`ledger.New` 与 worker startup report **均无任何信号** | ① 读 README:80-103 新 Prerequisite；② 用 superuser 与 `ledger_app` 两条连接各跑一次 `ledger.New(WithLogger)` + `AssertRuntimeRole` + `Worker().Run` + `StartupReport()` | **已闭合** | 无 |
| **F-C2a** | `worker := svc.Worker(...)` 不编译（返回两个值） | ① `go test . -run TestREADME -v`；② 把 README:221 块逐字抠出来自己加 preamble 编译 | **已闭合** | 无 |
| **F-C2b** | 修好编译后 `go worker.Run(ctx)` 把 error 丢进 goroutine → 默认 NopLogger 下**零个后台 job 启动且无痕迹** | 用默认 `ledger.New(pool)`（无 logger）实跑 `Worker()` + `Run()` | **已闭合** | 无 |
| **F-C2c** | `srv.Handler()` 方法不存在 | 读 README:289；`docs/api-surface.txt` 的 `server.Server` 方法集；README 编译门禁 | **已闭合** | 无 |

### F-C1 证据

README:80-103 把 Prerequisite 重写成两条连接，并逐字写出了我原报告里的那个失效语义（"not violated, not logged, identical behavior until something goes wrong in a way the invariants said could not happen"）。Tier 1 代码块现在自己就调 `AssertRuntimeRole` 并解释了为什么是 warn 而不是 exit。

关键是**新增了运行时信号**（原报告的核心诉求）。实测：

```
######## SUPERUSER connection (the old README shape)
  AssertRuntimeRole -> postgres: role check: connected as "ledger", expected "ledger_app" -- ...
  WARN  worker: postgres: role check: connected as "ledger", expected "ledger_app" -- ...
  StartupReport.RuntimeRoleWarning = "postgres: role check: connected as \"ledger\", ..."

######## ledger_app connection (the new README shape)
  AssertRuntimeRole -> <nil>
  StartupReport.RuntimeRoleWarning = ""
```

角色正确 → 字段为空、无 WARN；角色错误 → WARN 与另外两条（anchor / AuthVerifier）并排出现，且 `StartupReport.RuntimeRoleWarning` 可编程读。原报告说的「这个库对缺 attestor 喊得震天响、对 ACL 不生效一声不吭」已经不成立。

### F-C2 证据

`go test . -run TestREADME -count=1 -v` 全绿，`TestREADMEGoBlocksCompileUnlessMarkedSnippet` 有 16 个 subtest 通过（覆盖面见 N-3）。我另外把三个头部块逐字抠出来自己配 preamble 编译：

| 块 | 结果 |
|---|---|
| README:115（Tier 1 Hello Ledger，含 `AssertRuntimeRole`） | `go build` OK |
| README:168（Tier 2 presets + booking lifecycle） | `go build` OK |
| README:221（worker，双返回值 + goroutine 内 `log.Fatal(err)`） | `go build` OK |

运行时：默认 NopLogger 下 `Worker()` 返回 `err = <nil>`、`Run()` 返回非 nil（`refusing to start with the default silent logger: ...`）。README 现在的写法会 `log.Fatal` 它 —— 静默停摆不再可能。

`srv.Handler()` 已改为 `http.ListenAndServe(":8080", srv)`，并在行尾注明「`*server.Server` implements http.Handler directly -- there is no separate .Handler() method」。

### 两个 `readme-gate: snippet` 豁免是否站得住

| 位置 | 内容 | 判定 |
|---|---|---|
| README:443（豁免 :440 块） | `presets.DepositLifecycle` / `presets.WithdrawalLifecycle` 两行裸 selector + 注释 | **站得住**。裸 selector 在语句位置是 `evaluated but not used`，任何 preamble 都救不了。理由写在豁免注释里，与事实一致 |
| README:1102（豁免 :1093 块） | `TestMyAnchorConformance` 模板，调用 `newTestBucket` / `newMyAnchor` —— 两个让读者自己去写的函数 | **站得住**。这是给消费方的测试配方，其中两个标识符按定义不存在于本库；补桩会把「你要自己实现」这层意思抹掉。且注释指明 `anchors/r2` 自己的测试是这段配方的可执行版本 |

两条豁免都写了具体理由而不是一句 "snippet"，符合"豁免必须自证"的要求。

---

## Major

| 原编号 | 原结论 | 复核方法 | 复核结论 | 残余 |
|---|---|---|---|---|
| **F-M1** | `POST /bookings` 对出厂 `deposit` 返回 500/19999；api.md 错误表把 500 标为可重试；HTTP 面无法挂 lifecycle | 起 `examples/fullstack/backend`（:18091），实打 | **部分闭合** | 3 项，见下 |
| **F-M2** | README 承诺的 pending-bundle 闸不存在 | 读 README:777-793 / :410-416；只装 `InstallExtendedPresets` 实跑 `AddPending` + `ConfirmPending` | **已闭合** | 无 |
| **F-M3** | 闸视角可用额与所有消费方可见读数分歧，COOKBOOK 两条 recipe 合起来产生陷阱 | 读 COOKBOOK diff；实跑「RunInTx 内 Settle」与「顶层 Settle」两条路径，用 `insufficient balance` 的错误文本读出闸算的可用额 | **部分闭合** | 无 API 读闸视角（文档已明写承认） |
| **F-M4** | BREAKING.md 缺 `Worker` / `Subscribe` / `Run` 三条 | `grep '^### ' docs/BREAKING.md` | **已闭合** | 无 |
| **F-M5** | README 说全量对账套件仍需显式 `SetFullReconciler` | 读 README:242 | **已闭合** | 无 |
| **F-M6** | 注册内置 evm adapter + 挂 server 不够，callback 全 503/18102 | 读 README:277-288 / :607-614、api.md §2；进程内起 server 实打 webhook | **部分闭合** | api.md §2 未同步 |
| **F-M7** | api.md §4 `POST /journals` 自己的示例被 403，状态表无 403 | 读 api.md:486-523 | **已闭合** | 无 |
| **F-M8** | 7 条 openapi 路径无 api.md 章节 | 机器比对 openapi 全部路径 vs api.md 全文 | **已闭合（原 7 条）** | 新增 2 条又缺（→ N-1） |
| **F-M9** | README:1081 的 ready-probe 陈述已被 api.md 自己作废 | 读 README:1160 | **已闭合** | 无 |
| **F-M10** | 文档说 `anchors/r2` 不能 `go get`，实测能 | 在两个全新 module 里分别实测 `go get` / `build` / `run` / `mod tidy`，默认 proxy 与 `proxy.golang.org` 各一遍 | **部分闭合** | 2 项，见下 |

### F-M1 — 部分闭合（残余 3 项）

行为已修好，而且修得比我建议的更好：

```
POST /bookings {"classification_code":"deposit", ...}
HTTP=400
{"code":10001,"message":{"text":"Please check your input and try again",
 "fields":{"classification_code":"classification \"deposit\" has no lifecycle attached, so no booking
  can be created against it -- call ClassificationStore.SetLifecycleIfEmpty(ctx, uid, lifecycle) first
  (README \"Add a custom lifecycle\", or presets.DepositLifecycle / presets.WithdrawalLifecycle for the
  out-of-the-box deposit/withdraw classifications)"}}}
```

400（按 api.md 自己的错误表**不可重试**）+ 字段级定位 + 逐字可执行的修法。`classification_code: "nope"` 仍正确地是 404。走 `core.ErrNoLifecycle` 哨兵而不是字符串匹配，`docs/api-surface.txt:508` 有登记。

残余：

1. **`docs/openapi.yaml` 的 `POST /bookings` 仍只声明 `201` 和 `422`** —— 没有 `400`。这正是我原报告 F-M1 第 2 点（"neither api.md nor openapi declares 500"）里 openapi 那一半。codegen 出来的客户端 / 契约测试仍然不知道这个状态码存在。
2. **`docs/api.md:274`** 的状态码行虽然本来就列了 `400`，但 §1 全节没有一句提到"分类没有 lifecycle"这个最常见的 400 成因，也没有 `message.fields` 的示例。
3. **错误文案给的是 Go 方法名，而 HTTP 面没有对应端点。** 实测 `PUT|PATCH|POST /classifications/{uid}/lifecycle` 与 `PUT /classifications/{uid}` 全部 404；openapi 里 classification 相关路径只有 `/classifications` 和 `/classifications/{uid}/deactivate`。所以纯 HTTP 消费方拿到这条完美的错误信息之后，**仍然无法照做** —— 只能改去建一个自带 lifecycle 的新分类。原报告这一点未闭合。

### F-M3 — 部分闭合（残余：无闸视角读接口，已被文档承认）

COOKBOOK Recipe 5 新增了整整两段（`docs/COOKBOOK.md` diff +34 行）把这个交互讲透，包括我原报告的核心那句被逐字承认：

> `GetBalanceBreakdown` and `HeldAmount` show the ordinary, ungated view — spendable, `0` outstanding — the whole time; **there is currently no accessor that reads the amount a `RequireVerifiedBalance`-gated `Reserve` would see**, so the two views can disagree with no error, no log, and nothing to diff against.

并给了两条缓解。我把两条路径都实跑了（余额 1000，Reserve 200，Settle 50，已配 `WithAttestor`）：

| 路径 | `GetBalanceBreakdown.Available` | `HeldAmount` | 闸算的可用额 |
|---|---|---|---|
| A：`Settle` 在 `RunInTx` 内（Recipe 4 教的写法） | 1000 | 0 | **800** |
| B：`Settle` 在顶层 `Service`（COOKBOOK 新给的缓解） | 1000 | 0 | **1000** |

—— 分歧照旧存在（设计如此，`ReserveInput` godoc 有完整推导），而 **COOKBOOK 新写的缓解经实测确实有效**：顶层 settle 的 discharge claim 能签名，闸立刻放回。文档从"没提"变成"讲清 + 给出经验证的出路"。残余就是那个自认的缺口：仍无接口能读到 800 这个数，UI 上显示 1000 而提现失败的支持工单仍然无法自证。

### F-M6 — 部分闭合（README 闭合，api.md §2 未同步）

README 两处补得非常到位：Quick Start 的 HTTP 装配块里直接有 `srv.SetDepositIngester(svc.Onchain())` 并附 8 行注释说明"不调就是每个 callback 都 503/18102"；`Add a custom channel adapter` 节末尾也补了"注册 adapter 本身不够"。

行为未变（本来就是设计）：进程内实测，注册内置 `onchain.New(secret)` + `NewFromDeps` 但不调 `SetDepositIngester` →

```
webhook -> HTTP 503 {"code":18102,"message":{"text":"This feature is not enabled on this server"},"data":null}
```

残余：`docs/api.md` §2「Webhooks」这一节**没跟上**。

- api.md:369-371 原句「…that the server routes to **automatically** whenever the resolved channel adapter implements it」原封不动 —— 这句正是当初误导我的那句。
- api.md:415 的状态码行只有一个裸 `503`，没有注明"sighting-capable adapter 在 `SetDepositIngester` 未接线时无条件 503/18102"。
- §17 开头那句"Every endpoint **in this section** answers 503/18102 until that add-on is wired"覆盖不到 `POST /webhooks/{channel}` —— 它在 §2。

### F-M10 — 部分闭合（残余 2 项，均为新测得）

README:1076 与 RUNBOOK:1410-1421 都重写了，并且明说是"2026-09-03 consumer review F-M10, re-measured"。新口径：`go get` / `go build` 可用，唯一失败的是 `go mod tidy`，原因是 tagged root 里没有 `anchortest`，**不是** `replace`。

我在两个全新 module 里重测，得到两点与新口径不符：

**残余 1 — 结论依赖 GOPROXY，文档未提。** 本机 `go env GOPROXY` = `https://goproxy.cn,direct`（常见镜像）。在该默认下：

```
$ go get github.com/azex-ai/ledger/anchors/r2@latest
go: module github.com/azex-ai/ledger@latest found (v0.6.0), but does not contain
    package github.com/azex-ai/ledger/anchors/r2
```

README 括号里的另一种写法（"from inside a module that already requires the root"）我也单独试了 —— 先 `go get github.com/azex-ai/ledger@latest` 成功、再 get 子模块，**同样失败，同样的错误**。只有显式 `GOPROXY=https://proxy.golang.org` 时才会合成伪版本并成功（`go run` 打印出 `r2.Config`）。也就是说新口径在上游 proxy 下成立、在镜像下不成立，而这层依赖两份文档都没写。

**残余 2 — `go mod tidy` 实际有两个错误，其中一个正是文档说"不是这个原因"的那个。** 在 `GOPROXY=https://proxy.golang.org` 下：

```
github.com/azex-ai/ledger/anchors/r2/internal/miniotest:
  ...@v0.0.0-00010101000000-000000000000: invalid version: unknown revision 000000000000
go: example.com/r2c imports
  github.com/azex-ai/ledger/anchors/r2 tested by
  github.com/azex-ai/ledger/anchors/r2.test imports
  github.com/azex-ai/ledger/anchortest: module github.com/azex-ai/ledger@latest found (v0.6.0),
    but does not contain package github.com/azex-ai/ledger/anchortest
```

第二条是文档写的 `anchortest` 原因 ✓；第一条是 `internal/miniotest` 的相对 `replace` 解析失败 —— 即 README:1076 明确否定的「not for the `replace` reason an earlier revision of this line gave」。旧口径把 replace 当成**唯一**原因是错的，新口径把它说成**不是**原因也是错的：两个原因同时存在。

---

## Minor（W5-readme 独占域只覆盖 `consumer C1 C2 M1–M10`，Minor 未纳入契约）

| 原编号 | 原结论 | 复核结论 | 证据 |
|---|---|---|---|
| F-m1 | api.md `POST /bookings/{uid}/transition` 示例缺 required 的 `idempotency_key`，且该节没写 "Idempotency: required" | **未处置** | api.md:284-292 示例原样；:320 仍只有 "Auth: required."。实打该示例 → `HTTP=400 code=10001` |
| F-m2 | COOKBOOK:363 worker 片段不编译 + 吞 error | **已闭合** | 改为双返回值 + goroutine 内 `log.Fatal`，并加了 9 行注释解释为什么 |
| F-m3 | `.env.example` 的 `API_KEYS=dev-key-1,dev-key-2` 让 `LoadConfig()` 直接失败 | **未处置** | 文件逐字未改 |
| F-m4 | `.env.example` 注释三处与实际不符（作用范围 / production 行为 / 格式） | **未处置** | 同上 |
| F-m5 | `.env.example` 无 `MIGRATE_DATABASE_URL`、`DATABASE_URL` 指向 superuser、且该文件全仓无人引用 | **未处置** | `grep -rn env.example README.md docs/ Makefile examples/` 仍为空。⚠️ 这条现在与已闭合的 F-C1 直接冲突：README 现在正确地要求两条连接，`.env.example` 仍只给一条且是 superuser |
| F-m6 | `make db` 开箱失败（`POSTGRES_PASSWORD must be set`） | **未处置** | 实跑仍同样失败。Makefile 本轮有改动（`test` target 加了 Docker 探针），`db` 未动 |
| F-m7 | CHANGELOG 自相矛盾（15 vs 16 checks） | **部分** | :217 已改为 "sixteen ... (seventeen as of Wave 5's ...)"；**:340 仍写 "15-check"**。实测 `RunFullReconciliation` 返回 **17** 项（新增 `reversal_chain_integrity`） |
| F-m8 | `examples/tx-compose` 按 RUNBOOK 推荐的 `ledger_app` 跑会死在 `CREATE TABLE` | **未处置** | 头部注释未加凭证说明 |
| F-m9 | 复用 `anchordev` 目录跨重建的库 → `VerifyLedger` 报 TAMPERED，与真实攻击不可区分 | **未处置（可复现）** | 同目录、DB 重建后第二次运行：`status=TAMPERED reasons=[seq 1: DB root_hash does not match the externally anchored head]` |
| F-m10 | store getter 命名四种风格；无按 code 查 currency | **未处置** | `go doc ./core` 的四个接口签名未变 |
| F-m11 | README Tier 2 用了 `presets.` / `service.` 但无 import 块 | **已闭合（机制性）** | 编译门禁使这类问题不再可能出现 |
| F-m12 | README 说 webhook 重放警告在"第一次 callback"时打 | **已闭合** | README:274 与 :1159 都改为 "at construction (not on the first callback)" |
| F-m13 | 缺 required 字段 `tolerance` 报成 "is not a valid decimal amount" | **未处置** | 实打仍是同一文案 |
| F-m14 | `?limit=abc` / `limit=999` 静默回落，无 400 | **未处置** | 实打均 `200` |
| F-m15 | README 描述 `Subscribe(fn)` 时未体现它现在返回 error | **部分** | BREAKING.md:299 已补条目；README:243 的描述仍未提要检查返回值 |
| F-m16 | `examples/fullstack/README.md` 说 "~230 lines"，实为 345 | **未处置** | 仍是 `~230 lines`；`wc -l` = 345 |

---

## 新问题（本轮新记录，不在原报告内）

### N-1 · openapi 又长出 2 条 api.md 没有的路径（F-M8 同类复发）— Minor

openapi 从 59 条涨到 61 条（README:1022 的 "61 paths, 102 schemas" 已同步更新，准确）。机器比对后，仍不在 `docs/api.md` 的路径：

```
/deposits/dead-letters
/deposits/dead-letters/{uid}/replay
```

原报告的 7 条已全部补齐（新增 §16 Accounts / §17 Crypto Deposit / §18 Holder Wallet Surface），但同一轮新加的这两条又没进 api.md。`/deposits/dead-letters/{uid}/replay` 是写端点（会重新驱动一次 `IngestDeposit`），属于资金面动作。

### N-2 · 全新安装的第一次 `RunAttestBatch` 记 ERROR，且方法返回 nil — Minor

清洁库、空 anchor 目录，第一次批量存证：

```
ERROR service: attestation: recording the anchor observation failed
  [seq 0 error postgres: record anchor observation: ERROR: null value in column "observed_head"
   of relation "anchor_observations" violates not-null constraint (SQLSTATE 23502)]
round 1: attested=4 seq=1 err=<nil>
round 2: attested=2 seq=2 err=<nil>
round 3: attested=2 seq=3 err=<nil>
```

只在第 1 轮出现（`seq 0` = 空 anchor 的 `Head` 返回空字节，插 `observed_head` 撞 NOT NULL），第 2/3 轮干净，`VerifyLedger` 最终 `VERIFIED`。效果上大概率无害（seq 0 本就没什么可观测），但两点值得看：

1. 它以 **ERROR** 级别出现在每一次全新安装的启动路径上 —— 而这条日志通道正是承载真实 anchor 篡改信号的那条（`anchor_observations` 是 `VerifyReport.AnchorSeq < LastObservedAnchorSeq` 这个"回滚检测"的数据来源，其 godoc 说这个形态"no benign mechanism produces"）。让运维在第一天就学会忽略它，代价在后面。
2. `RunAttestBatch` 返回 `err = <nil>` —— 只检查 error 的调用方看不到。

无法判断是否为 W5 引入：`anchor_observations` 来自更早的 migration，而我在第一轮从未给 `WithAttestor` 的 Service 注过 logger，所以第一轮不可能看见。判定 **PLAUSIBLE，非回归证据不足**。

### N-3 · README 编译门禁的覆盖数与"24 个块必须编译"的表述差 6 块 — 观察，非缺陷

`docs/plans/2026-09-03-wave5-contract.md` §1 写的是「README 全部 24 个 Go 块**必须编译**」。实测：README 有 24 个 ` ```go ` 块，2 个带 `<!-- readme-gate: snippet -->`，`TestREADMEGoBlocksCompileUnlessMarkedSnippet` 产生 **16** 个 subtest（22 − 16 = 6）。

未产生 subtest 的 6 个（按块起始行）：**115**（Tier 1）、**168**（Tier 2）、517、530、539、558。

我把其中最要紧的 115 / 168 / 221 三块逐字抠出来自己配 preamble 编译，**全部通过**，所以这不是"有块不编译"的缺陷。同一文件里还有 `TestREADMECustomTemplateRuns`（0.92s，跑真库），推测 530/539 由它以"执行"而非"编译"的方式覆盖。但两个 Quick Start 块正是 F-C1/F-C2 的原发位置，它们**是否**被门禁本身覆盖，值得维护方确认一次 —— 如果只是被别的测试间接覆盖，那 F-C2 的机制性保障就比契约表述弱一档。（我按纪律未读 `readme_runnable_test.go`，所以只能给到观察，给不出判定。）

---

## 复核方法附录

```bash
git checkout --detach 5a11533 && git checkout -b r3-recheck-consumer
docker run -d --name r3consumer-pg -e POSTGRES_DB=ledger -e POSTGRES_USER=ledger \
  -e POSTGRES_PASSWORD=ledger -p 15432:5432 postgres:17.2-alpine
docker exec r3consumer-pg psql -U ledger -d ledger -c "ALTER ROLE ledger_app WITH PASSWORD 'app';"

go test . -run TestREADME -count=1 -v          # README 门禁，全绿
# scratchpad 消费方 module（go.work use 本 worktree 的 5 个 module）
MIGRATE_DATABASE_URL='postgres://ledger:ledger@localhost:15432/<db>?sslmode=disable' \
DATABASE_URL='postgres://ledger_app:app@localhost:15432/<db>?sslmode=disable' go run ./<probe>
# HTTP：HTTP_PORT=18091 go run ./examples/fullstack/backend
```

探针：`run`（角色/worker 信号）、`p1`（pending bundle / 闸视角双路径 / 对账项数）、`p2`（webhook 503 / anchor 陈旧目录 / 存证轮次）、`t1` `t2` `t3`（README 块逐字编译）、`/tmp/r2get2` `/tmp/r2get3`（`go get anchors/r2`，两种 GOPROXY）。

**未验证**（同第一轮，本次未扩展）：`anchors/r2` 对真实 Object-Lock bucket 的行为、`chains/evm` e2e、并发/多副本、前端 `@azex/ledger-react` 的运行时、`docs/INVARIANTS.md` 中未被本链路触及的绝大多数条目、RUNBOOK 各处置流程、限流与 holder token 面的深度行为。

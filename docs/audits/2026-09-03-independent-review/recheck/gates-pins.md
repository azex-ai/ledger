# 第三轮独立复审 · 复核（recheck）· 门禁与 pin

- 复核员：`r3-gates-pins`（同一人，原报告 `docs/audits/2026-09-03-independent-review/gates-pins.md` 的作者）
- 日期：2026-09-04（SGT）
- 复核对象：main `5a11533`（代码等于 `0e7512a`）
- 实验位置：worktree `/Users/aaron/projects/_worktrees/ledger/r3-gates-pins`，分支 `r3-recheck-gates-pins`（从 `5a11533` detach 后开出）。只读代码，不修任何被评审对象；每次 mutation 只改一处，跑完立即还原并打印 `git status --porcelain`。
- 判据：**零 Critical / 高 复发**。

**基线**：`go test ./... -count=1` 全绿（2 分 29 秒，`postgres` 145s）。`web` 下 `npm ci` + `npm run -w @azex/ledger-react build` + `npx vitest run --root packages/ledger-react` = 41 files / 225 tests 全绿。

---

## 0. 结论

**复发数 = 0。** F-1…F-9 与 P-1…P-6 十五条，全部按原样重做同一 mutation：

- **11 条已修且经证伪**（F-1 F-2 F-3 F-4 F-5 F-6 F-7 F-8 F-9 P-1 P-2 P-3 P-6）—— 原来的 mutation 现在直接红在门禁本身，多数还红在新增的专用 pin 上。
- **2 条按拍板保留**（P-4 `docs/frontend.md` 无一致性门禁、属 W5-readme 独占域；P-5 `anchortest.Check` 的未探维度属设计工作）—— 状态与 W5 章节自述一致，不是复发。
- 无一条回到「拆机制而全绿」的状态。

对 W5 处置章节的**对抗式抽查 6 条**（I-28 / I-10 / I-53 / I-6 / P-2 / I-70 的 14 入口）全部**核实成立**，未发现夸大。两处 lead 集成改动（`appendOnlyGuardFloor` 25→23、`dbOnlyRegisterSize` 13→15）**均判定合理，未放松门禁**（理由见 §3）。

**新增 4 条（本轮复核发现，非复发）**，其中一条要紧：

| # | 严重度 | 一句话 |
|---|---|---|
| **R-4** | **中（与被判 CRITICAL 的 I-70 同一 bug class、同一可达面）** | `core.Allocate` / `core.Round` / `core.ConvertAt` / `core.Delta` / `core.EncodeAmount` 五个**导出**的 money helper 对 `1E999999999` **3 秒不返回**（无界展开），I-70 的 `ValidateAmountMagnitude` 没有覆盖它们。更要紧的是 CI 那三个 30 秒 fuzz target 之一 `FuzzAllocate` **结构上不可能发现这一类**：它用 `decimal.NewFromBigInt(big.NewInt(x), -exp)` 且 `exp∈[0,18]` 造数，指数永远是负的。W5「30 秒预算是有效的」这个结论只对 `FuzzJournalValidate`（喂字符串）成立，不能跨三个 target 推广。 |
| **R-3** | 中低 | F-8 只修了 `go-verify.yml`。`go-release.yml` 的 `verify-anchors-r2` / `verify-chains-evm` 两个 job **仍无 govulncheck**，而 `TestGoVerifyRunsRealCommandsForEveryModule` 只读 `go-verify.yml`，`TestReleaseWorkflowUsesSameVerifyAsGoVerify` 只比根 `verify` job 的 `uses:` —— 所以打 `anchors/r2/v*` tag 发版时那个 module 不做漏洞扫描，且没人会注意到。 |
| **R-2** | 低 | `appendOnlyGuardFloor` 25→23 迁走的两个守卫里，`entry_attestations_no_delete` 这一个**无 pin**：把它整个不创建，`go test ./...` 全绿。它已离开 `ledger_block_mutation` 的 census（`TestAppendOnlyGuards_*` 不再覆盖），而替代 pin `TestPoisonedAttestationTailHasAWayBack` 只驱动 `ledger_attestations`。 |
| **R-1** | — | （非缺陷，正向核实）替代守卫函数 `ledger_attestation_chain_block_delete()` 本身**是被 pin 的**：改成恒 `RETURN OLD` → `TestPoisonedAttestationTailHasAWayBack` + `examples/tamper-evident` 红。 |

---

## 1. F-1…F-9 逐条复核

| 原编号 | 原 mutation（本轮原样重做） | 现在哪个测试红 | 结论 |
|---|---|---|---|
| **F-1** | `001_baseline.up.sql` 的 `CREATE TABLE journals` 里 `idempotency_key TEXT UNIQUE NOT NULL` → `TEXT NOT NULL`（只动 journals 一张表），跑 `go test ./... -count=1` | `postgres.TestJournalIdempotencyKey_RejectsDirectSQLDuplicate`（新增，直击 pin）、`postgres.TestEveryIdempotencyKeyColumnHasATotalUniqueIndex`（新增，schema 面全量推广）、`core.TestDbOnlyMechanismsExistWhereRegistered`（register 的 objects 改成**按声明解析**而非全文 substring，所以删一处就红——原来要删到 0 处才红） | **已修**（原：全套零失败） |
| **F-2** | `ledger_block_mutation()` 函数体 → `BEGIN RETURN NEW; END`（函数名保留，catalogue 自检照旧过），跑全套 | `postgres.TestAppendOnlyGuards_EveryTriggerRefusesItsMutation`（新增）+ 原有 3 个（`TestAuditTrailRowsStayImmutable` / `TestCheckpointIntegrity_CheckpointRebuilds_IsAppendOnly` / `TestPeriodClosesGuard_NoUpdateNoDelete`） | **已修**（原：全套只红 3 个，`journal_entries`/`journals` 的 append-only 无 pin） |
| **F-3** | ① 把 I-53 的 Enforced-by 改成只引 `001_baseline.up.sql` + `chain_cursors`；② 往 `unresolvableEnforcedCitations` 加一条 `"I-53"`；③ Pinned-by 换成无关的 `postgres.TestRoleAttributes` | `core.TestDbOnlyMechanismRegisterOnlyShrinks`（新增，`assert.Len(register, dbOnlyRegisterSize)`） | **已修**（原：`./core` 全绿） |
| **F-4** | ① Go：`postgres/rollup_adapter.go` 植入 `map[core.NormalSide]map[core.EntryType]int` 查表版 `r3DriftSignedAmount`；② SQL：`balance_trends.sql` 加一条 `FILTER (WHERE entry_type=…)` + `(entry_type='debit')::int`、不含 `CASE`/`END` 的查询 | `TestSignAuthorityGate_GoHasNoUnclassifiedNormalSideBranch`（regexp 新增 `map[core.NormalSide]`、map-key 位置、`strings.\w+(…"debit")`）+ `TestSignAuthorityGate_SQLHasNoUnclassifiedEntryTypeArithmetic`（span 集合新增 aggregate-FILTER 与 boolean-cast 两种形状），报错点名到 `R3DriftSignedBalance` 和 `rollup_adapter.go:47` | **已修**（原：三个 gate 全绿） |
| **F-5** | `service/rollup.go` 的 `s.metrics.StuckRollups(stuck)` 包进 `err == nil && false` 的不可达分支，跑全套 | `observability.TestEveryMetricsMethodHasAProductionCallSite`（新增了常量-false 可达性判断）+ `service.TestRollup_ReportsStuckAndPendingSeparately` / `TestRollup_ReportsQueueDepthOnAnEmptyTick`（新增行为 pin） | **已修**（原：全套含 postgres 全绿） |
| **F-6** | `TemplatesPage.tsx` 里 8 个 `aria-label="…"` → `aria-label=""`，**先 `npm run build`** 再跑 vitest | `test/skin-parity.test.ts` 2 个（per-page floor + `SHADCN_MIN_RATIO_OF_HEROUI`），census 从 47 掉到 41，报错 `expected 41 to be greater than or equal to 46.62` | **已修**（原：225/225 全绿；现 token 判据要求非空值，注释里明写 `aria-label=""` / `aria-label={""}` 不计） |
| **F-7** | `core.JournalInput` 加 `EventRef int64 \`json:"event_ref"\`` | `core.TestEveryInt64OnACoreTypeIsClassified`（新增，类型侧判据，正是原报告建议的形状）；`TestAPISurface_MatchesSnapshot` 仍红（原来唯一的报警） | **已修**（原：core/server/service 全绿，只有快照红） |
| **F-8** | 删掉 `go-verify.yml` 里 `govulncheck (anchors/r2)` 那一步 | `core.TestGoVerifyRunsRealCommandsForEveryModule`：`has no step that actually runs [govulncheck] for workspace module "anchors/r2"`（矩阵新增 `vulncheck` 位，且用 `consumerReachableModule()` 把两个 `internal/` fixture module 正确排除） | **已修**（原：矩阵无 vulncheck 位，缺口不可见）。残余见 **R-3** |
| **F-9** | 读 `internal/postgrestest/postgrestest.go` 的 Docker 不可达路径 | 代码路径已改：`t.Skip("Cannot connect to the Docker daemon")` 整段删除，改为 `require.NoErrorf(t, sharedServer.err, "…This is a failure, not a skip: a suite that did not execute is not a suite that passed…")`；`Makefile` 的 `test` target 前置 `docker info` 探针并给出 `DATABASE_URL` / `make test-short` 两条显式出路 | **已修**（原：Docker 停机时本地 `make test` 打印 `ok`） |

---

## 2. P-1…P-6 逐条复核

| 原编号 | 原 mutation / 检查 | 现在哪个测试红 | 结论 |
|---|---|---|---|
| **P-1** | `holderTokenAuth` 保留函数名、内部改成 `next.ServeHTTP(w, r)` 直通 | `server.TestHolderRoutes_RejectEveryRequestWithoutAValidToken`（新增，**行为** pin）+ 原有 3 个 handler 测试 | **已修**（原：门禁本身绿，只由 3 个不相干测试兜住） |
| **P-2** | 把 prometheus collector `journals_posted_total` 改名为 `journals_written_total` | `TestREADMEMetricNamesExistInThePrometheusAdapter`（新增，根包）+ `observability.TestDocMetricNamesExistInRegistry` + `TestPrometheusMetrics_EndToEnd` | **已修**（原：README 只有「(N methods)」计数门禁，指标**名字**无门禁） |
| **P-3** | 未做 mutation（原为静态怀疑：spec 的 25 个 `enum` 不从 Go 常量派生） | 新增 `server.TestOpenAPIEnumsAreDerivedFromGoConstants`（7 个词表按 **Go 类型**登记，覆盖具名 component 与内联副本；三条规则含「不得是词表的真子集/真超集」） | **已修**（读代码核实，判据设计正确：按 Go 类型而非 component 登记，正好堵住「内联副本掉队」） |
| **P-4** | 读 `ledger-react.yml` 的 path filter | — | **按拍板保留**。path filter 依旧（有意），`docs/frontend.md` 仍无 `readme_docs_consistency_test.go` 那样的一致性门禁。W5 章节已认定为缺口并归 W5-readme 独占域。**不是复发** |
| **P-5** | 读 `anchortest.Check` 的完备性 | — | **按拍板部分保留**。W5 已把「Check 能抓它探的维度」证到（删 head-regression 阶段 → 2 个 `TestCheck_Catches*` 红；R2 `Publish` 去 create-only → `TestAnchor_Conformance` 红）。我点名的两个未探维度（并发 `Publish` 同 seq、网络分区下 `Head` 返回旧值而非报错）仍未探，属新增 conformance 阶段。**不是复发** |
| **P-6** | 读 `TestVerifyReservationDischargeAuth` 的 6 个裸 `require.Error` | 现每个否定用例断言**是哪个检查**拒的，`verifier rejects` 追加 `require.ErrorIs`，并新增第七个用例把 I-45 在 discharge 面的形态（未知 key 必须以 `core.ErrUnknownAuthKey` 到达）钉住 | **已修** |

---

## 3. 两处 lead 集成改动的合理性

### `appendOnlyGuardFloor` 25 → 23 —— **合理，未放松**

- 事实核实：migration 029 用 `ledger_attestation_chain_block_delete()` 替换了 `ledger_attestations` / `entry_attestations` 上原本执行 `ledger_block_mutation()` 的两个 DELETE 守卫。新函数是「同样的拒绝 + 一道 owner-only 门」（`ledger_attestation_discard_is_authorized()` → `ledger_discard_attestations_from(seq, reason)`，要求非空 reason，且 seq 必须已存在）。
- 因为 `readBlockedMutations` 按 `p.proname = 'ledger_block_mutation'` 取 census，这两个 (table, event) 对**确实**离开了那张表 —— 所以 23 是**诚实的下调**，不是掩盖损失。文件里就地写明了理由与替代 pin，符合它自己「lowering this number is the honest edit」的治理措辞。
- **正向核实（R-1）**：把 `ledger_attestation_chain_block_delete()` 改成恒 `RETURN OLD` → `postgres.TestPoisonedAttestationTailHasAWayBack` 红 + `examples/tamper-evident.TestAuthorizePostAuthorizedKeepsRunInTxVerifiable` 红。替代机制有真 pin。
- **残余（R-2，新）**：单独把 `entry_attestations_no_delete` 这一个触发器不创建（`ledger_attestations` 的那个保留）→ `go test ./... -count=1` **全绿**。替代 pin 只驱动 `ledger_attestations`；`service/attest_verify_test.go` 反而是 `ALTER TABLE journal_entries DISABLE TRIGGER` 之类为了植入篡改而**禁用**它 —— 禁用不是断言。建议把 `TestPoisonedAttestationTailHasAWayBack` 的「a raw owner DELETE is still refused」子测试对 `entry_attestations` 也跑一遍（两行），或给这个函数建一个和 `TestAppendOnlyGuards_*` 同形的 catalogue census。

### `dbOnlyRegisterSize` 13 → 15 —— **合理，未放松**

- 逐条读了新增的两项：
  - **I-66**：机制是 029 的 4 个 `*_insert_guard` 触发器 + 两道 owner-only 门（`ledger_template_line_repair_is_authorized` / `ledger_discard_attestations_from`）。纯 DB 对象，无导出 Go 入口。
  - **I-68**：机制是 030 的 `ALTER FUNCTION … SET search_path`（proconfig 条目）、两个约束触发器、以及 `REVOKE TEMPORARY ON DATABASE … FROM PUBLIC`（数据库级 ACL）。同样无 Go 面。
- 两项都是 register 的**正当**用法（「mechanism with no Go face」），且新的 `objects` 是**带 kind 的 `dbObject`**（`kindFunction` / `kindTrigger` / `kindPrivilege` …），由 `objectIsDeclared` 按 `CREATE (CONSTRAINT )?TRIGGER <name>` 之类的**声明形状**匹配，未知 kind 返回 `known=false` 从而响亮失败 —— 比原来的全文 substring 严格得多（这也是 F-1 现在能红在 register 上的原因）。
- 同时 `TestDbOnlyMechanismRegisterOnlyShrinks` 把条数锁成常量，增长必须体现在 diff 里。**净效果是收紧，不是放松**：注册进来的两条各自另有专用行为门禁（I-66 → `postgres/insert_path_guard_test.go`，我用 R-1 直接证伪过其中一支；I-68 → `postgres/guard_function_search_path_test.go`）。

---

## 4. 对 W5 处置章节的对抗式抽查（6 条）

| 抽查 | W5 的声称 | 我做的 | 结果 |
|---|---|---|---|
| **I-28**（章节称「唯一真正空洞的一条，已补」） | 停掉 `VerifyLedger` 里 `a.Seq == anchorSeq && !bytes.Equal(a.RootHash, anchorHead)` 那一行 → 原来全绿；新增 `TestVerifyLedger_AnchorHeadDisagreeingAtTheAnchoredSeqIsTampered` | 同一 mutation，跑 `go test ./... -count=1` | **成立**：恰好红那一个新增 pin，无别的噪声。声称精确 |
| **I-10**（章节称「机制在、pin 名单错，已修」） | 拆 `linkJournalToEventAndBooking` → 原列三个 pin 全绿；真正红的是 `TestPostJournal_EventUID_RejectsUnrelatedJournal` / `TestTxComposition_RunInTx_BookingEventJournalLinkage` + migration 027 与 onchain 全生命周期 | 同一 mutation，跑 `./postgres ./service .` | **成立**：postgres 红 5 个（含它点名的两个 + 三个 migration 027），service 红 11 个（onchain 全生命周期）。与自述一致 |
| **I-53** | `confirmationDepth` 恒 0 → 红 | 同一 mutation | **成立**：`TestOnchain_Watch_NeverScansPastConfirmationDepth` 红（另带 4 个 watcher 测试） |
| **I-6** | `NUMERIC(30,18)` → `(20,8)` → 红 | 同一 mutation（`journal_entries.amount`） | **成立**：`TestSchema_NumericColumnsAreExactly30_18` + `TestSchema_MoneyNamedColumnsAreNumeric30_18` 红 |
| **P-2** | 改名 `rollups_pending` 立刻红 | 改名 `journals_posted_total` | **成立**（见 §2） |
| **I-70 的 14 个入口** | 14 个 caller-supplied 金额入口 × 4 个病态值，各带 100ms 时限 | 数了基线 subtest = **14**；再把 `ValidateAmountMagnitude` 改成恒 `return nil` | **成立**：14 个中 **12 个**红。剩下 2 个（`core.AccountPolicyInput.Validate`、`core.EntryTemplate.Render`）在守卫拿掉后仍在预算内拒绝，即它们另有独立拒绝路径 —— 不是漏洞，但「每个入口都靠这个守卫」的读法要打折：**12 个依赖它，2 个不依赖** |

时限断言的设计我单独核了一遍：`mustReject` 用 goroutine + `select` + 100ms `time.After`，并且**同时**断言 `errors.Is(err, core.ErrInvalidInput)`。这正是「返回错误不是命题、快速返回才是」的正确形状 —— 一个先展开再拒绝的实现会满足裸 `require.Error` 而仍是 DoS，这里抓得住。

---

## 5. 新发现

### R-4 · 同一 bug class 在五个导出 money helper 上仍然存在，而 `FuzzAllocate` 结构上找不到它 —— CONFIRMED（中）

**做法**：不动仓库。在 `/tmp` 建一个独立 module，`replace github.com/azex-ai/ledger => <worktree>`，`GOWORK=off go run`，每个调用放进 goroutine + 3 秒 `select` 超时。

```
core.Allocate(total=1E999999999)                       >3s     <-- DID NOT RETURN
core.Allocate(weight=1E999999999)                      >3s     <-- DID NOT RETURN
core.Allocate(total=10E777777070)                      >3s     <-- DID NOT RETURN
core.Round(1E999999999, 2, HalfUp)                     >3s     <-- DID NOT RETURN
core.ConvertAt(1E999999999, 1, 2, HalfUp)              >3s     <-- DID NOT RETURN
core.Delta(debit, 1E999999999, 0)                      >3s     <-- DID NOT RETURN
core.EncodeAmount(1E999999999)                         >3s     <-- DID NOT RETURN
core.SignedAmount(debit, debit, 1E999999999)               0s  returned
CONTROL core.ReserveInput.Validate                         0s  returned  (exponent 999999999 … invalid input)
```

（`10E777777070` 正是 `core/testdata/fuzz/FuzzJournalValidate/7ec58597750a4f04` 里那个输入。`-1E999999999` 在 `Allocate` 的 weight 位置被「weight 不得为负」提前拒掉，total 位置一样挂。）

三点值得分开说：

1. **`core.EncodeAmount` 有一个名字承诺了这一类、实际没覆盖的 pin。** `TestEncodeAmount_RejectsOutOfRangeMagnitude` 用的是 `170141183460469231731.687303715884105728`（约 2^127，**指数有界**），被 `bigIntToFixedTwosComplement` 在廉价路径上拒掉。真正的病态输入走的是 `shift > 0` 分支里的 `new(big.Int).Exp(big.NewInt(10), big.NewInt(18+999999999), nil)` —— 无界。这是原报告 F-5/F-6 同款的「守形状不守语义」，只是长在一个 pin 的**名字**上。

2. **`FuzzAllocate` 无法发现它，不是预算问题而是输入空间问题。** `core/money_test.go:207`：
   ```go
   f.Fuzz(func(t *testing.T, totalUnits, w1, w2, w3, w4 int64, exp int32) {
       if exp < 0 || exp > 18 { t.Skip() }
       total := decimal.NewFromBigInt(big.NewInt(totalUnits), -exp)
   ```
   指数恒为 `-exp ∈ [-18, 0]`，正指数永远造不出来。所以 W5 章节「30 秒预算被证明有效」的结论只对 `FuzzJournalValidate`（它 fuzz 的是会被 `decimal.NewFromString` 解析的字符串）成立。**建议**：给 `FuzzAllocate` 加一路 `f.Fuzz(func(t, totalStr string, ...))`（或直接把 `exp` 的范围放开并用 `NewFromBigInt(coef, exp)` 造正指数），否则这个 target 对整个 magnitude 类是盲的。

3. **可达面要说准确，不要夸大。** 这五个函数**不是 HTTP 可达**：`server/amount.go` 的 `parseWireAmount` 直接拒绝任何含 `e`/`E` 的 wire 金额（而一个不用科学计数法的十亿位数字是十亿字节的 body）。它们的可达面是**库模式**：`core.Allocate` / `core.Round` / `core.ConvertAt` 都是带完整 doc、`docs/COOKBOOK.md` 教消费者调用的导出 API，消费者自己 parse 出来的 `decimal.Decimal` 直接传进去就会挂。这与 I-70 被判 CRITICAL 时的可达面**是同一类**（`JournalInput.Validate` 也是库入口，HTTP 侧同样被 `parseWireAmount` 挡着），所以我按「同 class、同可达面、未覆盖」记中级。
   - 仓内唯一的内部调用点 `postgres/reversal_fraction_store.go:363` 是安全的：它的 `scaledTotal` / `weights` 来自已落库的 `NUMERIC(30,18)` 行，不可能是病态值。

### R-3 · `go-release.yml` 的两个 submodule verify job 仍无 govulncheck —— CONFIRMED（中低）

`go-verify.yml` 的 `vulncheck` job 现在有三步（root + `chains/evm` + `anchors/r2`），F-8 修得对。但 `go-release.yml` 的 `verify-anchors-r2` / `verify-chains-evm` 是**独立手写**的 job（工作流注释自己说明 `go-verify.yml` 是 root-module-shaped、不适用于它们），步骤集为 `go vet` / `golangci-lint` / `go build` / `go test`，**无 govulncheck**：

```
$ grep -n 'govulncheck' .github/workflows/go-release.yml
13:# behind -- missing lint, sqlc-diff, govulncheck, and both submodules
```
（唯一命中是注释。）

覆盖它的门禁也不存在：`TestGoVerifyRunsRealCommandsForEveryModule` 只 `loadWorkflow(t, goVerifyPath)`；`TestReleaseWorkflowUsesSameVerifyAsGoVerify` 只比根 `verify` job 的 `uses:` 是否同一个文件；`TestGoWorkModulesAllCoveredByGoVerify` 会读 go-release.yml，但只查 `working-directory:` 出现过，不查命令内容。净效果：`anchors/r2/v*` / `chains/evm/v*` tag 触发的发版路径不做漏洞扫描 —— 正是 F-M6 当初要治的「两个 caller 结构性分叉」，只是这次分叉在 submodule 那一侧。

**建议**：两个 job 各加一步 `govulncheck ./...`（带 `working-directory`），并把 `TestGoVerifyRunsRealCommandsForEveryModule` 的矩阵改成对 go-verify.yml **和** go-release.yml 的 submodule job 都成立（或给后两个 job 单独一条断言）。

### R-2 · `entry_attestations_no_delete` 无 pin —— CONFIRMED（低）

见 §3。mutation：029 里只保留 `ledger_attestations` 的那条 `CREATE TRIGGER`，不重建 `entry_attestations` 的 → `go test ./... -count=1` **零失败**。

严重度定低：`ledger_app` 对 `entry_attestations` 无 `DELETE` 权限（least-privilege 那侧另有门禁），所以要触发需要 owner 级凭证；但这个触发器的**全部意义**就是「连 owner 也只能走那道带 reason 的门」，而这一点在 `entry_attestations` 上无人断言。删掉它，per-entry leaf hash（I-27「每条 entry 恰好被覆盖一次」/ I-29）就能被无痕删除。

### R-1 · 替代守卫本身有 pin —— 正向核实，非缺陷

见 §3。

---

## 6. 我这一轮没能验证的

1. **branch protection** —— 仍无法从代码验证。本报告所有「测试会红」只等价于「workflow 会失败」，不等价于「合不进 main」。与原报告、W5 章节结论一致，需 Aaron 在 GitHub 侧确认。
2. **`chains/evm` 的 `-tags e2e`** —— 本机无 anvil (foundry)，未跑。I-20 的复核我采信 W5 自述（它声称在 `chains/evm` 模块内跑过），未独立重做。
3. **`anchors/r2`（MinIO testcontainer）** —— 本轮只跑了根 module 的 `./...`，未跑 `make test-submodules`。I-56 / I-48 在 R2 侧的结果同样采信 W5 自述。
4. **30 秒真 fuzz** —— 三个 target 我都没有实跑 30 秒（`FuzzAllocate` 的盲区是**读输入编码**得出的结论，不是跑出来的；`FuzzJournalValidate` / `FuzzLifecycleValidate` 未跑）。
5. **`web/` 宿主应用** —— 只跑了 `packages/ledger-react` 的 vitest，未跑宿主 `npm run build` / `npm run lint`（W5 自述已跑，exit 0）。
6. **24 条不变式里我没抽查的 18 条** —— 本轮只对抗式抽查了 6 条（I-28 / I-10 / I-53 / I-6 + P-2 + I-70 入口表）。抽到的 6 条全部与自述一致、无夸大，据此对余下 18 条给**中等置信**，但那不是独立验证。

---

## 7. 方法论交代

- **同一处 mutation 只改一处**；每次结束打印 `git status --porcelain`，全程为空。R-4 的探针没有改仓库任何文件（外部 module + `replace` + `GOWORK=off`）。
- **改 `.sql` 查询文件对运行时无效**（运行时走 `postgres/sqlcgen/*.sql.go` 的副本），migration 类改源文件有效 —— 这条在原报告里已交代，本轮所有 mutation 遵循同一规则。
- **F-6 必须先 `npm run build` 再跑 vitest**，否则红的是 `readDistFile` 的 staleness 而不是被测属性（原报告踩过一次，本轮先建后跑）。
- 我这一轮**没有**因为「上一轮是我写的」而对 W5 的处置放宽：每条都重做了同一 mutation，抽查的 6 条都是我自己重跑的，两处集成改动是逐条读迁移 SQL + 找替代 pin + 再 mutation 验证得出的结论。

---

## 8. 最终 worktree 状态

```
$ git -C /Users/aaron/projects/_worktrees/ledger/r3-gates-pins status --porcelain
```

（除本报告文件外为空；`web/node_modules` 与 `web/packages/ledger-react/dist` 是被 `.gitignore` 覆盖的构建产物。）

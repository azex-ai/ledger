# 独立复审（第三轮）· 门禁与 pin 的可证伪性审计

- 审计员：`r3-gates-pins`（独立、只读主仓；实验全部在 worktree `/Users/aaron/projects/_worktrees/ledger/r3-gates-pins`，分支 `r3-gates-pins`，基线 commit `c854c6e`）
- 日期：2026-09-03（SGT）
- 方法：对仓内每一个自称「门禁 / gate / pin」的测试做 **mutation**——把它声称能抓的漂移真的做出来，跑测试，记红/绿。每次 mutation 只改一处、跑完立即还原（`cp` 备份 + `git checkout --`）。
- 范围内：Go 全部 5 个 workspace module 的测试、`docs/INVARIANTS.md` I-1…I-65、`.github/workflows/*.yml`、`Makefile`、`web/packages/ledger-react` 的 vitest 套件。
- 范围外（按指令未读）：`docs/audits/`、`docs/plans/2026-09-02-*`、`docs/plans/2026-09-03-*`。

**基线确认**：worktree 下 `go test ./... -count=1` 全绿（2 分 08 秒，`postgres` 124s，Docker/testcontainers 可用）；`web` 下 `npm ci` + `npm run -w @azex/ledger-react build` + `npx vitest run --root packages/ledger-react` = 41 files / 225 tests 全绿。所有下述「绿」都是相对这个真实跑起来的基线说的，不是「跳过」。

---

## 1. 摘要

这套门禁的整体质量**高于**我见过的绝大多数仓库：`core/invariants_pins_test.go`（1390 行）已经把「pin 名字存在」「pin 落在正确的包」「pin 的函数体真的引用了 Enforced-by 的符号」「Enforced-by 引用的符号真的存在」四层都机器化了，`core/ci_workflow_gates_test.go` 甚至把「CI step 的 `run:` 真的调了那条命令」都断言了。W3 那轮对抗评审留下的痕迹到处可见（M-1/M-2/M-3/M-4/M-10/m-4/m-5 的修补都能在代码里读到）。

我做了 **31 次 mutation**（含 3 次控制组），其中 **21 次证明 pin 是真的**（拆机制 → 红），**7 次找到真实缺口**（拆机制 / 造漂移 → 全绿）。

按严重度排序的 7 条 CONFIRMED 缺口：

| # | 严重度 | 一句话 |
|---|---|---|
| **F-1** | **高（离钱最近）** | 把 `journals.idempotency_key` 的 `UNIQUE` 去掉，**整个 `go test ./...` 全绿**。I-3（也是 `financial.md` 的硬红线）在账本最核心的那张表上完全没有 pin。 |
| **F-2** | 高 | `ledger_block_mutation()` 改成 `BEGIN RETURN NEW; END`（25 个 `*_no_update` / `*_no_delete` 触发器全部失效），全套只红 3 个测试，**`journal_entries` / `journals` 的 append-only（I-2 / I-25 的 DB 层背书）无任何 pin**；而 `TestIdempotencyReceiptTablesAreAppendOnly` 是空洞的——它断言的是 GRANT，不是触发器。 |
| **F-3** | 高（治理） | `unresolvableEnforcedCitations`（db-only 机制豁免名单）**可以静默增长**。两行文档改动 + 一条名单项，就能把任意一条不变式的 pin 完全解绑，`core` 全绿。姊妹名单 `citationStyleGapInvariants` 有 `TestCitationStyleGapListStaysClosed` 锁着，这条没有——而它自己的错误文案声称它「equally closed to silent growth」。 |
| **F-4** | 中 | I-50「符号约定只有一份实现，且由机器检查」：Go 侧用 **map 查表**、SQL 侧用 **`FILTER (WHERE entry_type=…)` + 布尔转 int**（不写 `CASE…END`）各植入一份完整的第二实现，`TestSignAuthorityGate_*` 三个全绿（同批控制组用经典写法立刻红）。 |
| **F-5** | 中 | I-61 的 emission coverage：把真实发射点包进 `if false { … }`，AST 门禁照旧算它「有 production call site」；census 那半只要某个 mock 声明过同名方法就算「有 behaviour pin」。实测 `StuckRollups` 的发射被彻底关掉，**全套（含 postgres）全绿**。 |
| **F-6** | 中 | web `skin-parity` 的 hardening census 数的是 `aria-label=` 这个**属性名**。把 `TemplatesPage.tsx` 里 8 个 `aria-label` 的值全清成 `""`，`npm run build` 之后 41 个 test file / 225 个 test **全绿**（仓内无 jsx-a11y lint）。 |
| **F-7** | 中低 | I-18「uid-only 外部身份」的 banned-key 是从 schema 的 BIGSERIAL 列名派生的。给 `core.JournalInput` 加一个 `EventRef int64 \`json:"event_ref"\`` 装内部 id，`core` / `server` / `service` 全绿；唯一报警的是 `TestAPISurface_MatchesSnapshot`（快照差异，改法是重生成快照）。 |

另外两条 CI 层缺口（非 mutation，读配置得出）：

- **F-8**：`govulncheck` 只在**根 module** 跑。`anchors/r2`（AWS S3 SDK）和 `chains/evm`（go-ethereum）——第三方面积最大的两个 module——有 vet/lint/build/test 但**没有漏洞扫描**；`go-release.yml` 里它们各自的 verify job 也没有。而专门防这种「根有、submodule 没有」漂移的 `TestGoVerifyRunsRealCommandsForEveryModule`，矩阵里只有 `{vet, build, test, lint}`，**不含 govulncheck**，所以没人会注意到。
- **F-9**：`internal/postgrestest` 在 Docker 不可用时 `t.Skip`。CI 用 service container + `DATABASE_URL` 绕过了这一点，所以 CI 侧没问题；但**本地 `make test` 在没起 Docker 时会以「全绿」收场**，`postgres` 包 100+ 个集成测试一个都没跑，输出里看不出区别。`Makefile` 的 `test` target 已经为 `-count=1` 写了长注释治理同一类问题，这一类（未运行读作通过，`working-agreements.md` §3）还没治。

---

## 2. Gate × mutation 表

「结果」列：**红** = 门禁抓到（pin 有效）；**绿** = 门禁没抓到（缺口）；**控制** = 用来证明门禁确实在跑的正向对照。

| # | 门禁 / pin | 我造的漂移 | 结果 |
|---|---|---|---|
| M1 | `core.FuzzJournalValidate` + `TestJournalInvariant_*`（I-1） | `core.JournalInput.Validate` 的 per-currency 平衡判断改成 `if false && …` | **红**（4 个 pin 中 3 个红） |
| M2 | `postgres.TestJournalBalanceTrigger_RejectsDirectSQLImbalance`（I-24） | `check_journal_currency_balance()` 函数体首行插 `RETURN NULL;`（**函数名保留**） | **红**（该 pin 红；`TestMoneyConservation_Network` / `TestUnbalancedJournalsFleetScan_*` / I-12 的 4 个 pin 全绿——它们盯的是别的半边） |
| M3 | `TestSign_*` / `TestDelta_*` / `TestSignedAmount_*` / `TestEntryDirection_*`（I-43） | `core.Sign` 对未知 `normal_side` 由报错改为默认 `+1` | **红** |
| M4 | `TestSignAuthorityGate_GoHasNoUnclassifiedNormalSideBranch` + `…SQLHasNoUnclassifiedEntryTypeArithmetic`（I-50） | ① Go：在 `postgres/rollup_adapter.go` 植入 `map[core.NormalSide]map[core.EntryType]int` 查表版 `r3DriftSignedAmount`；② SQL：在 `balance_trends.sql` 加一条用 `FILTER (WHERE je.entry_type='debit')` + `(je.entry_type='debit')::int` 算符号、**不含 `CASE`/`END`** 的查询 | **绿（F-4）** |
| M5 | 同上（控制组） | ① `if side == core.NormalSideDebit`；② 经典 `CASE WHEN entry_type='debit' THEN amount ELSE -amount END` | **控制：红** |
| M6 | `TestReserverStore_Reserve_Concurrent_RejectsOverCommit` 等（I-4） | `reserveWithQueries` 里删掉 `acquireBalanceLocks` | **红**（`TestReserverStore_Reserve_Concurrent` 本身仍绿） |
| M7 | `TestReserve_AvailableBasisExcludesPendingLockedAndRoleless` / `TestReserve_PendingOnlyBalanceNotReservable`（I-11） | 未加闸路径的 available base 改成所有 role 之和（含 pending/locked/roleless） | **红** |
| M8 | `TestReserve_RequireVerifiedBalance_RechecksUnderLock`（I-49） | 加闸路径去掉锁内 entries 复算，只用 `*verifiedAvailableBase` | **红** |
| M9 | `TestReserve_RequireVerifiedBalance_RejectsInflatedCheckpoint`（I-49） | 加闸路径退回 checkpoint 支撑的 `sumBalancesByRoleWithQueries` | **红** |
| M10 | `TestReserve_SignedDischarge_*`（I-65） | `verifiedDischarges` 的 receipt 分支 `core.VerifyReservationDischargeAuth(...)` 换成 `error(nil)` | **红** |
| M11 | 同上 | receipt + settlement-leg **两处**验签都换成 `error(nil)` | **红**（多红 `RejectsTamperedAmount`） |
| M12 | `TestLedgerStore_PostJournal_PeriodClosed_Rejected`（I-15） | `effectiveAt.Before(activeClose.CloseBefore)` 判断改成 `if false && …` | **红** |
| M13 | `TestClosePeriod_WaitsForInFlightBackdatedJournal`（I-59） | 写路径去掉 `acquirePeriodReadBarrier`（共享半） | **红**（`TestClosePeriod_RejectsAfterBarrier` 仍绿） |
| M14 | 同上 | `ClosePeriod` 去掉 `acquirePeriodCloseBarrier`（排他半） | **红**（doc 声称「Removing either half makes it red」——成立，但只由 1 个 pin 承担） |
| M15 | `TestWorker_JobPanic_DoesNotCrashProcess`（I-60） | `Worker.safeRun` 的 `recover()` 换成 `interface{}(nil)` | **红**（进程真 panic） |
| M16 | `TestConfirmPending_VerifiedBaseCapsEntriesForgedInTheWindow`（I-64） | `ConfirmPending` 去掉 `bal = decimal.Min(*verifiedPendingBase, bal)` | **红** |
| M17 | I-64 全部 pin | `ConfirmPending` 的 `bal.LessThan(required)` 闸整体停用 | **红**（6 个 pin 全红） |
| M18 | `FuzzLifecycleValidate` / `TestLifecycle_*`（I-8） | `core.Lifecycle.Validate` 立即 `return nil` | **红** |
| M19 | `TestSystemAccountHolder_RoundTrip`（I-9） | `SystemAccountHolder` 改成恒等（不取负） | **红**（`TestIsUserAccount` 仍绿） |
| M20 | `TestRouteAuthorization_EveryRouteIsGuarded` 的 holder 分支 | `holderTokenAuth` 保留函数名，内部改成 `next.ServeHTTP(w, r)` 直通 | 门禁本身**绿**，但 `TestLedgerdHolderSurface` / `TestHolderDepositAddress*` **红** → 净结果红。仍值得记：scoped 分支是行为探测（`deriveRouteRequirement` 真跑 middleware），holder 分支只按**函数名**匹配 |
| M21 | `TestEveryMetricsMethodHasAProductionCallSite` + `…HasABehaviourPin`（I-61） | `service/rollup.go` 的 `s.metrics.StuckRollups(stuck)` 包进 `… && false` 的不可达分支 | **绿（F-5）**：`./...` 全套（含 postgres 124s）全绿 |
| M22 | 同上 | `AnchorLagSeqs` / `AttestationBatchResult` 同样处理 | **红**（`TestAttestation_HealthyRunReportsZeroLag` / `…AnchorPublishFailureEmitsMetric`）——说明缺口是 per-metric 的，不是门禁整体 |
| M23 | `TestInvariantsPinsReferenceEnforcedSymbols` + `TestDbOnlyMechanismsExistWhereRegistered` | 把 I-53 的 Enforced-by 改成只引一个文件路径 + 一个 snake_case 对象（`chain_cursors`），往 `unresolvableEnforcedCitations` 加一条 I-53，再把 Pinned-by 换成完全无关的 `postgres.TestRoleAttributes` | **绿（F-3）** |
| M24 | 同上（控制组） | 只把 I-53 的 Pinned-by 换成 `postgres.TestRoleAttributes`，Enforced-by 不动 | **控制：红**（`I-53's pin TestRoleAttributes never references any of its Enforced by symbols (RunWatchOnce)`） |
| M25 | `TestPostJournal_SignsWithConfiguredAttestor` 等（I-26） | `attestJournal` 首行 `if s.attestor == nil` 改 `if true`（永不签名） | **红**（5 个） |
| M26 | `TestVerifiedBalance_*`（I-32） | `VerifiedBalance` 整段 live 验签跳过 | **红**（8 个） |
| M27 | `TestVerifiedBalance_CachedAuthorizedVerdictDoesNotSkipTheLiveCheck`（I-33） | 缓存 `Authorized` 裁决短路 live check（复原 T4 的 bug） | **红**（2 个） |
| M28 | `TestPrecision_*`（I-16） | `checkAmountPrecision` 恒返回 nil | **红**（6 个） |
| M29 | `TestCanonical*_MicrosecondPrecisionOnly`（I-46） | `canonicalTimestamp` 不再 `Truncate(time.Microsecond)` | **红**（core 3 个 + postgres 1 个） |
| M30 | `TestNewLocalAttestor_RejectsUnknownKeyID` / `TestFullReconciliation_UnauthorizedJournals_FlagsUnknownKeyAsDistinctFromForgery`（I-45） | `authdev` 把未知 keyID 包成 `ErrUnauthorizedJournal` 而不是 `ErrUnknownAuthKey` | **红**（authdev 2 个 + service 1 个；`TestService_KeyRotation_*` 仍绿） |
| M31 | `TestIdempotencyReceiptTablesAreAppendOnly` / `TestBookingsAndEventsGuards` / `TestReversalChainIntegrity` 等（I-2 / I-25） | `ledger_block_mutation()` 函数体改 `BEGIN RETURN NEW; END`（**名字保留**，所以 001_baseline 的 catalogue 自检与 register 的 objects 检查都过） | **绿（F-2）**：`./...` 全套只红 3 个——`TestAuditTrailRowsStayImmutable`、`TestCheckpointIntegrity_CheckpointRebuilds_IsAppendOnly`、`TestPeriodClosesGuard_NoUpdateNoDelete` |
| M32 | `TestNoInternalIDFieldsInCoreTypes` / `TestContract_NoInternalIDKeysInJSON` / `TestNoInternalIDsInCoreInterfaceSignatures`（I-18） | `core.JournalInput` 加 `EventRef int64 \`json:"event_ref"\`` | **绿（F-7）**（只有 `TestAPISurface_MatchesSnapshot` 红） |
| M33 | `TestService_RunInTx_NestedCallIsRejected` / `TestService_VerifyLedger_NotRunOnTxBoundClone`（I-40） | `RunInTxWithOptions` 与 `VerifyLedger` 的 `s.tx != nil` guard 各改 `if false` | **红** |
| M34 | `TestChainCursorStore_SetCursor_IsMonotonic`（I-52） | `postgres/sqlcgen/chain_cursors.sql.go` 里去掉 `WHERE chain_cursors.last_scanned_block < EXCLUDED.last_scanned_block` | **红**（注：只改 `sql/queries/*.sql` 无效——运行时走 sqlcgen 的副本；`sqlc diff` job 会抓源/生成物分叉） |
| M35 | `TestLedgerRoCannotReadWebhookSecret` / `TestGrantCoverage_*`（I-36） | 删掉 `REVOKE SELECT ON public.webhook_subscribers FROM ledger_ro` | **红** |
| M36 | `TestConfigTableChangesAudited` / `TestAccountPolicyEnforcementKnobChangeIsAudited`（I-58） | `ledger_log_config_table_change()` 改名（触发器挂不上） | **红**（`TestPartialGuardTablesAreAudited` / `TestReconcileScanCursorChangesAudited` 仍绿；`TestDbOnlyMechanismsExistWhereRegistered` **绿**——它只查名字在文件里出现过） |
| M37 | `TestObjectOwnership_*` / `TestPartitionFunctions_OwnedByLedgerOwner`（I-57 / I-35） | `ledger_resweep_ownership()` 改名 | **红**（5 个） |
| M38 | `TestJournalEntries_DuplicateIDAcrossPartitions_Rejected` / `TestGrantCoverage_*`（I-42） | migration 008 的列级 `GRANT INSERT (…)` 换回表级 `GRANT INSERT`（`id` 列重新可写） | **红** |
| M39 | `TestLedgerStore_AccountPolicy_*`（I-17） | 写路径去掉 `enforceAccountPolicies` 调用 | **红**（7 个） |
| M40 | `TestBalanceRead_AgreesWithEntriesOnlyRecompute` / `TestLedgerStore_GetBalance_MultipleJournals`（I-5） | `getBalanceWithQueries` 直接返回 checkpoint，不加 delta | **红** |
| M41 | `TestPostJournal_ReversalOfUID_*`（I-51） | `postJournalWithQueries` 去掉 `validateReversalOfInput` | **红**（3 个） |
| M42 | `TestCheckpointIntegrity_RecomputeBalance_IgnoresCheckpointTampering`（I-23） | `RecomputeBalance` 改为先读 `balance_checkpoints` | **红**（2 个；`TestSolvencyCheck_IgnoresTamperedCheckpoints` / `TestVerifiedBalance*` 仍绿——它们各有自己的 entries-only 查询） |
| M43 | `TestPartitions_*`（I-13） | 不创建 `journal_entries_default` catch-all 分区 | **红**（4 个 + `TestDbOnlyMechanismsExistWhereRegistered`） |
| M44 | `TestLedgerStore_PostJournal_Idempotent` / `TestIdempotency_ConcurrentSameKey` / `TestSchema_*`（I-3） | `journals.idempotency_key TEXT UNIQUE NOT NULL` → `TEXT NOT NULL` | **绿（F-1）**：`go test ./... -count=1` 全套零失败 |
| M45 | 同上，扩到 5 处 | 001_baseline 里全部 5 处 `idempotency_key TEXT UNIQUE NOT NULL` 都去 UNIQUE | 部分红：只有 reservation settlement/receipt 相关的 10 个测试红 + `TestDbOnlyMechanismsExistWhereRegistered`（**要等最后一处消失才红**——它是全文件 substring 搜索） |
| W1 | web `skin-parity` hardening census（+ 全部 41 个 test file） | `TemplatesPage.tsx` 里 8 个 `aria-label="…"` 全改成 `aria-label=""`，**先 `npm run build`** 再跑 | **绿（F-6）**：225/225 通过 |
| W2 | 同上（无 build） | 同样改动但不重建 dist | **红**，但红的原因是 `readDistFile` 的 **staleness**（`styles.test.ts` + `build-artifacts.test.ts` 6 个），不是 a11y——即 m-4 那个新鲜度闸有效，a11y 闸不存在 |

---

## 3. I-1…I-65 逐条「pin 能否证伪」

「拆机制」= 我具体拆掉了什么。「结论」：**红** = 真证伪；**绿** = 空洞；**未验证** = 本轮没做该条的 mutation（原因见第 5 节）。

| I | 主题（缩写） | 拆机制 | Pin 结果 | 结论 |
|---|---|---|---|---|
| I-1 | per-currency 平衡 | ① `JournalInput.Validate` 平衡判断停用；② `check_journal_currency_balance()` 变 no-op | ① 3/4 pin 红；② 由 I-24 的 pin 红 | **红** |
| I-2 | append-only / 只用 reversal 纠正 | `ledger_block_mutation()` 变 no-op（`journal_entries_no_update/_no_delete`、`journals_no_delete` 全失效） | 8 个 pin 全绿 | **绿**（见 F-2；应用层 reversal 守恒那半未单独 mutation） |
| I-3 | 每笔动钱都幂等 | `journals.idempotency_key` 去 `UNIQUE` | 15 个 pin 全绿；`./...` 全套全绿 | **绿**（见 F-1） |
| I-4 | reserve/settle TOCTOU 安全 | `reserveWithQueries` 去 `acquireBalanceLocks` | `…RejectsOverCommit` + 2 个 I-49 pin 红 | **红** |
| I-5 | 实时余额 = checkpoint + delta | `getBalanceWithQueries` 只返 checkpoint | 2/5 pin 红 | **红** |
| I-6 | `NUMERIC(30,18)` | 未拆（纯 schema 列类型） | — | 未验证 |
| I-7 | 默认 NOT NULL | 未拆 | — | 未验证 |
| I-8 | Lifecycle FSM well-formed | `Lifecycle.Validate` 立即 return nil | 4/4 pin 红 | **红** |
| I-9 | 系统 holder = 用户 holder 取负 | `SystemAccountHolder` 改恒等 | `…RoundTrip` 红 | **红** |
| I-10 | event 与 journal 同一事务 | 未拆 | — | 未验证 |
| I-11 | reservation ≤ available | available base 含 pending/locked/roleless | 2/7 pin 红 | **红** |
| I-12 | 全系统守恒 | `check_journal_currency_balance()` 变 no-op | **4 个 pin 全绿**（红的是 I-24 的 pin） | **分不清**：I-12 自称由「I-1 + I-2 + 那个延迟约束触发器」背书，但它自己的 4 个 pin 对触发器失效无感；实际证伪落在 I-24 | 
| I-13 | 分区覆盖完整 | 不创建 `journal_entries_default` | 4/6 pin 红 | **红** |
| I-14 | effective date 一致 | 未拆 | — | 未验证 |
| I-15 | 期末关账是硬写屏障 | 关账拒绝判断停用 | 2/6 pin 红 | **红** |
| I-16 | 金额精度受币种 exponent 约束 | `checkAmountPrecision` 恒 nil | 6 个 pin 红 | **红** |
| I-17 | 账户策略强制 | 写路径去掉 `enforceAccountPolicies` | 7 个 pin 红 | **红** |
| I-18 | uid-only 外部身份 | `core.JournalInput` 加 `EventRef int64` / `json:"event_ref"` | 8 个 pin 全绿（只有 API 快照红） | **绿**（见 F-7） |
| I-19 | sweep booking 不落 journal | 未拆 | — | 未验证 |
| I-20 | 充值幂等键跨 reorg 稳定 | 未拆 | — | 未验证 |
| I-21 | review 挂起充值、零账本影响 | 未拆 | — | 未验证 |
| I-22 | `ledger_app` 无 DDL | 间接：① `REVOKE SELECT`（I-36）；② 列级 GRANT（I-42）；③ 所有权（I-57） | 三处均红（`TestGrantCoverage_*` / `TestLedgerAppIsLeastPrivilege` / `TestRoleAttributes` 参与） | **红（间接）** |
| I-23 | checkpoint 等可精确重算、检测不自修 | `RecomputeBalance` 改读 checkpoint | 2/8 pin 红 | **红** |
| I-24 | 每笔 journal 的平衡由 DB 层保证 | `check_journal_currency_balance()` 变 no-op（名字保留） | `TestJournalBalanceTrigger_RejectsDirectSQLImbalance` 红 | **红** |
| I-25 | 非 journal 的余额计算表不可越权改 | `ledger_block_mutation()` 变 no-op | 14 个 pin 中仅 3 个红；`TestIdempotencyReceiptTablesAreAppendOnly` 断言的是 GRANT | **部分绿**（见 F-2） |
| I-26 | 带签名的 journal 必须签名有效 | `attestJournal` 永不签名 | 5 个 pin 红 | **红** |
| I-27 | attestation 链完整 | 未拆（`AttestationBatchResult` 的发射 mutation 红，见 M22，但那是 I-61 的面） | — | 未验证 |
| I-28 | anchor head 与链一致 | 未拆 | — | 未验证 |
| I-29 | Merkle root 绑进签名链 | 未拆 | — | 未验证 |
| I-30 | inclusion proof 可靠 | 未拆 | — | 未验证 |
| I-31 | reversal / template batch 也签名 | 未拆 | — | 未验证 |
| I-32 | 提现时 verified balance fail-closed | `VerifiedBalance` 跳过 live 验签 | 8 个 pin 红 | **红** |
| I-33 | 缓存裁决只在失败方向可信 | 缓存 `Authorized` 短路 live check | 2 个 pin 红 | **红** |
| I-34 | 充值复核需 capability；二源不可达则升级 | 未拆 | — | 未验证 |
| I-35 | 分区维护不需要服务凭证持 DDL | 间接：`ledger_resweep_ownership()` 改名 | `TestPartitionFunctions_OwnedByLedgerOwner` 等红 | **红（间接）** |
| I-36 | 只读角色看不到写路径密钥 | 删 `REVOKE SELECT … FROM ledger_ro` | 2 个 pin 红 | **红** |
| I-37 | 偿付负债只数 role-bearing 用户侧 | 未拆（`RecomputeBalance` mutation 下 `TestSolvencyCheck_IgnoresTamperedCheckpoints` 仍绿，但那不是它的机制） | — | 未验证 |
| I-38 | HTTP 契约机器校验 + system leg 规则 | 未拆机制；已读全部 22 个 openapi 门禁（双向、含类型/format/required/嵌套/版本），设计上很扎实 | — | 未验证（见第 5 节 §「openapi enum」） |
| I-39 | advisory lock 结构性安全 | 间接：I-4 / I-59 的锁 mutation 红 | — | **红（间接）** |
| I-40 | tx-bound clone 不静默降级/逃逸 | `RunInTx` 嵌套 guard + `VerifyLedger` guard 各停用 | 2 个 pin 红 | **红** |
| I-41 | 对账/链上信号在歧义处 fail closed | 未拆 | — | 未验证 |
| I-42 | `journal_entries.id` 只来自序列 | 列级 GRANT 换回表级 | 2 个 pin 红 | **红** |
| I-43 | normal_side 只解释一次、未知值拒绝 | `core.Sign` 未知值默认 `+1` | 4 个 pin 红 | **红** |
| I-44 | holder 交易视图的 `kind` 词表 | 未拆 | — | 未验证 |
| I-45 | 区分「我没这把钥匙」与「签名无效」 | `authdev` 未知 keyID 包成 `ErrUnauthorizedJournal` | 3 个 pin 红 | **红** |
| I-46 | digest 只依赖微秒可存的瞬间 | `canonicalTimestamp` 不再 truncate | 4 个 pin 红 | **红** |
| I-47 | `Migrate()` 在整个 cluster 串行 | 未拆 | — | 未验证 |
| I-48 | 每个 `core.Anchor` 实现可证一致 | 未拆（conformance suite 自身完备性见第 5 节） | — | 未验证 |
| I-49 | 加闸下 Reserve 只按 entries 算 | ① 去锁内复算；② 退回 checkpoint role sums | ① 1 个 pin 红；② 1 个 pin 红 | **红** |
| I-50 | 符号约定只有一份实现且机器检查 | Go map 查表 + SQL `FILTER`/布尔转 int 第二实现 | 3 个 gate pin 全绿（控制组红） | **绿**（见 F-4） |
| I-51 | caller 提供的 link 是被验证的声明 | 去 `validateReversalOfInput` | 3 个 pin 红 | **红** |
| I-52 | 前扫游标不越过摄入 | sqlcgen 里去掉单调 WHERE | 1 个 pin 红 | **红** |
| I-53 | 前扫停在 reorg 可变的 tip 之后 | 未拆机制（但用它演示了 F-3 的解绑路径） | — | 未验证（机制）；**meta 层绿** |
| I-54 | 无 logger 也能观测 | 未拆 | — | 未验证 |
| I-55 | 记住 anchor 之前说过什么 | 未拆 | — | 未验证 |
| I-56 | anchor head 不回退，且机器校验每个实现 | 未拆 | — | 未验证 |
| I-57 | 所有对象归 `ledger_owner` | `ledger_resweep_ownership()` 改名 | 5 个 pin 红 | **红** |
| I-58 | guard 放过的改动被记录且不可改 | audit 触发器函数改名 | 2/6 pin 红 | **红** |
| I-59 | 关账与在飞写串行，且落后关账线可观测 | ① 去共享屏障；② 去排他屏障 | 均 1/3 pin 红（`TestClosePeriod_WaitsForInFlightBackdatedJournal`） | **红**（但只挂在 1 个 pin 上） |
| I-60 | panic 的 job / handler 不杀进程 | `safeRun` 不 recover | 2 个 pin 红（进程真 panic） | **红** |
| I-61 | `core.Metrics` 每个方法有 production call site | 真实发射点包进 `if false`（call site 保留） | `StuckRollups`：`./...` 全套全绿 | **绿**（见 F-5） |
| I-62 | 未配置 token 的充值走复核 | 未拆 | — | 未验证 |
| I-63 | 深 reorg 异常持久记录 | 未拆 | — | 未验证 |
| I-64 | 充值确认可铸的量以「验证过的 entries」为上界 | ① 去 `min(V,E)`；② 整个闸停用 | ① 1 个 pin 红；② 6 个 pin 红 | **红** |
| I-65 | discharge 声明只在签名有效时抵扣 | 验签换 `error(nil)`（单处 / 双处） | 1→2 个 pin 红 | **红** |

统计：**红 30 条**（含 3 条间接）· **绿 5 条**（I-2 / I-3 / I-18 / I-25 / I-50 / I-61 中 I-25 记「部分绿」）· **分不清 1 条**（I-12）· **未验证 24 条**。

---

## 4. 发现明细

### F-1 · `journals.idempotency_key` 的 UNIQUE 没有任何 pin —— CONFIRMED（高）

**做法**：`postgres/sql/migrations/001_baseline.up.sql` 的 `CREATE TABLE journals` 里 `idempotency_key TEXT UNIQUE NOT NULL` → `idempotency_key TEXT NOT NULL`（一行，只动 journals 一张表）。

**结果**：`go test ./... -count=1` **零失败**（含 `postgres` 全部集成测试 124s）。I-3 列了 15 个 pin，一个都没红。`TestLedgerStore_PostJournal_Idempotent` / `TestIdempotency_ConcurrentSameKey` 也没红。

**为什么绿**：Go 侧在插入前先 `acquireIdempotencyLock(hashtextextended(key))` 串行化，再 `GetJournalByIdempotencyKey` 预检 —— 单进程测试里 DB 约束永远走不到。I-3 的 Enforced-by 第一条就是「`UNIQUE` constraint on `journals.idempotency_key`」，而 `unresolvableEnforcedCitations["I-3"]` 里登记的 objects 是 `uq_bookings_idempotency` / `uq_ingest_dead_letters_idempotency_key` / `idempotency_key TEXT UNIQUE NOT NULL` —— **后者是全文件 substring 搜索**，5 处里删掉 1 处仍然命中（实测要删到 0 处才红）。

**这条约束保护的是谁**：不是走 Go 层的正常调用（那被 advisory lock 挡住了），而是 ① 多进程/多副本竞态下 advisory lock 失效或代码路径绕过的情况；② 漏出的 `ledger_app` 凭证直接 `INSERT INTO journals`（它确实有 `INSERT` 权限）；③ PITR 恢复后重放。这三种正是 `financial.md`「每次写操作必须有 idempotency_key（UNIQUE index）」要防的。

**建议的 pin 形状**（不是让我改，是给作者）：一个直接 SQL 的双插入测试，`assert.ErrorContains(err, "23505")` —— 和 `TestJournalBalanceTrigger_RejectsDirectSQLImbalance`（I-24 的 DB 层 pin）同一个形状。同时把 register 的 `objects` 从「文件里出现过这个串」改成「出现次数 ≥ N」或者改用具体约束名。

### F-2 · `ledger_block_mutation()` 背后 25 个 append-only 触发器，22 个无 pin —— CONFIRMED（高）

**做法**：`001_baseline.up.sql` 里该函数体 `RAISE EXCEPTION …` → `RETURN NEW;`（**函数名不动**，所以 001_baseline 第 1440–1530 行那个「每张表都要挂一个执行 `ledger_block_mutation()` 的 BEFORE UPDATE 触发器」的 catalogue 自检、以及 migration 006 的同款自检，全部照旧通过 —— 它们查的是 `tgfoid = 'public.ledger_block_mutation()'::regprocedure`，名字对就行）。

**结果**：`./...` 全套只红 3 个：`TestAuditTrailRowsStayImmutable`、`TestCheckpointIntegrity_CheckpointRebuilds_IsAppendOnly`、`TestPeriodClosesGuard_NoUpdateNoDelete`。

**没红的重点**：`journal_entries_no_update`、`journal_entries_no_delete`、`journals_no_delete` —— 也就是 I-2「Append-only journals」和 I-25 在**账本本体**上的 DB 层背书 —— **没有任何测试断言它们会拒绝**。仓里有 8 处测试为了植入篡改而 `ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_no_update`（`postgres/attested_auth_pin_test.go`、`service/attest_verify_test.go`、`service/attest_verify_merkle_test.go` …），所以大家都知道它存在 —— 但「先禁用再篡改」不是断言。

**附带的空洞断言**：`postgres/roles_test.go:641` 的 `TestIdempotencyReceiptTablesAreAppendOnly`。它的四个子测试都是 `appPool.Exec(ctx, "UPDATE reservation_operation_receipts SET amount = 999999 …")` + `assertPermissionDenied(t, err)`。`ledger_app` 本来就没有这些表的 `UPDATE` 权限，语句在 ACL 层就被拒了，**根本到不了触发器**。所以这个测试的名字（append-only）与它实测的东西（least-privilege）不是一回事；migration 006 加的 `reservation_operation_receipts_no_update/_no_delete` 等 12 个触发器一个都没被它验证。`TestReconcileScanCursorChangesAudited` 就在同一个文件下方几十行，那一个是真的对（它先确认写能成功，再断言审计行内容），所以这不是文件级的习惯问题，是这一个测试的问题。

**建议**：这类触发器的 pin 必须用一个**有 UPDATE 权限的凭证**（`pool`，即 owner）去写，并断言 `check_violation`；用 `ledger_app` 写只能证明 GRANT。

### F-3 · db-only 豁免名单可以静默增长，一条项就能解绑一条不变式 —— CONFIRMED（高，治理）

`core/invariants_pins_test.go` 有两个豁免名单：

- `citationStyleGapInvariants`（现为空）→ 有 `TestCitationStyleGapListStaysClosed` 断言它**必须为空**。C-2 那轮就是因为「治理只写在注释里」被评审者加两条进去照旧全绿，才补上这把锁。
- `unresolvableEnforcedCitations`（现 14 条）→ **没有任何测试限制它的内容或条数**。`TestDbOnlyMechanismsExistWhereRegistered` 只验「登记的 migration 存在，且文件里出现过登记的 object 名字」。

**做法**（2 处编辑）：
1. `docs/INVARIANTS.md` 里把 I-53 的 Enforced-by 从 `` `service.Onchain.RunWatchOnce` `` 改成只引 `` `postgres/sql/migrations/001_baseline.up.sql` `` + `` `chain_cursors` ``（一个 ≥6 字符含下划线的 snake_case 名，`auditEnforcedCitations` 会当作 DB 对象在 SQL+Go 语料里查到 → `resolved > 0`，所以 `TestInvariantsEnforcedCitationsResolve` 不红；同时 `enforcedLeafNames` 拿不到任何导出 Go 符号 → `leaves == 0`）。
2. 往 `unresolvableEnforcedCitations` 加 `"I-53": {migration: "…001_baseline.up.sql", objects: []string{"chain_cursors"}, …}`。
3. 顺手把 I-53 的 Pinned-by 换成完全无关的 `postgres.TestRoleAttributes`。

**结果**：`go test ./core/ -count=1` 全绿。控制组（只做第 3 步）立刻红，报 `I-53's pin TestRoleAttributes never references any of its Enforced by symbols (RunWatchOnce)` —— 所以差别完全来自那条名单项。

顺带：`TestInvariantsPinsReferenceEnforcedSymbols` 末尾的 fail-closed 哨兵是 `require.Greater(checked, len(unresolvableEnforcedCitations))`。65 条 section 对 15 条登记，这个下界离得太远，起不到限制作用 —— 名单要长到 32 条才会触发。

**建议**：给 `unresolvableEnforcedCitations` 加一把和 `TestCitationStyleGapListStaysClosed` 同款的锁 —— 至少断言 `len(...)` 等于一个硬编码的期望值（14），并要求 `objects` 里的每个名字在登记的 migration 文件里出现的**次数**也登记。名单本身要么冻结，要么只许减。（现在这个 map 的错误文案已经在说它「is checked, reported, and equally closed to silent growth」，但那句话在代码里没有对应物 —— 这正是 C-2 修的那个形状，在姊妹名单上原样复发。）

### F-4 · I-50 的「唯一符号权威」门禁能被两种自然写法绕过 —— CONFIRMED（中）

`normalSideBranchRE` 认的是：`== / != core.NormalSide*`、`case core.NormalSide*`、`switch … normalSide`、`== / != "debit"|"credit"`、`case "debit"|"credit"`。

**Go 侧绕过**（植入 `postgres/rollup_adapter.go`，一个非豁免文件，`go vet` 通过、门禁全绿）：

```go
var r3DriftSignTable = map[core.NormalSide]map[core.EntryType]int{
    core.NormalSideDebit:  {core.EntryTypeDebit: 1, core.EntryTypeCredit: -1},
    core.NormalSideCredit: {core.EntryTypeDebit: -1, core.EntryTypeCredit: 1},
}

func r3DriftSignedAmount(side core.NormalSide, et core.EntryType, amount decimal.Decimal) decimal.Decimal {
    if r3DriftSignTable[side][et] < 0 { return amount.Neg() }
    return amount
}
```

常量出现在 **map key** 位置（后面跟 `:` 而不是 `==`），三条 pattern 都不命中。用查表代替分支实现符号是很常见的写法，不是刻意躲检测。另一个同类绕过：`strings.EqualFold(string(side), "debit")`（字面量前面没有 `==`）。

**SQL 侧绕过**（加进 `sql/queries/balance_trends.sql`）：`bareEntryTypeSpans` 只在 `case … end` 的 span 里找 `entry_type` + `amount`，所以任何**不写 CASE** 的符号算法都看不见：

```sql
SELECT COALESCE(
         SUM(je.amount) FILTER (WHERE je.entry_type = 'debit'  AND c.normal_side = 'debit')
       - SUM(je.amount) FILTER (WHERE je.entry_type = 'credit' AND c.normal_side = 'debit')
       + SUM(je.amount * (1 - 2 * (je.entry_type = 'debit')::int)) FILTER (WHERE c.normal_side = 'credit')
       , 0)::numeric AS balance
FROM journal_entries je JOIN classifications c ON c.id = je.classification_id
WHERE je.account_holder = sqlc.arg(holder)::bigint;
```

`FILTER (WHERE …)` 聚合和 `(bool)::int` 算术都是标准 PG 写法。**同批控制组**用经典 `CASE WHEN entry_type='debit' THEN amount ELSE -amount END` 立刻红，证明门禁在跑。

历史上让这个门禁诞生的那个 bug（`balance_trends.sql` 把 500 充值报成 500 流出）恰恰就是「测 `entry_type` 而不 join `classifications`」—— 用 `FILTER` 重写同一个错误，门禁看不到。

**建议**：把 SQL 侧的判据从「`case … end` 里同时出现 entry_type 和 amount」放宽成「任何 `-- name:` block 里同时出现 `entry_type` 与 `amount`，且该 block 不含 `ledger_signed_amount`/`ledger_signed_delta`」——现有 7 条豁免正好就是这类需要按名分类的，机制不变。Go 侧至少再加 `core.NormalSide` 作为 **map key type** 和 `strings.EqualFold(...,"debit"|"credit")` 两个 pattern。

### F-5 · I-61 的两个 metrics 门禁都是「名字出现」而非「真的发射」 —— CONFIRMED（中）

- `TestEveryMetricsMethodHasAProductionCallSite`：M-10 已经把「注释里出现算命中」修成「必须是 `*ast.CallExpr` 且 receiver 形如 metrics」。但 AST 不判**可达性**：把 `s.metrics.StuckRollups(stuck)` 留在 `if … && false { … }` 里，call site 照旧存在。
- `TestEveryMetricsMethodHasABehaviourPin`：`metricsNamesInTests` 收集**所有 `_test.go` 里的每个 `*ast.Ident` / `*ast.SelectorExpr` / `*ast.FuncDecl` 名字**。任何测试为了实现 `core.Metrics` 而声明的空方法（`func (m *recordingMetrics) StuckRollups(int64) {}`，`service/rollup_test.go:679`）就足以让该方法算「有 behaviour pin」。

**实测**：`service/rollup.go:171` 的 `if stuck, err := s.queue.CountStuckRollups(ctx); err == nil {` 改成 `… err == nil && false {`。`go test ./... -count=1` **全绿**（含 postgres 124s）—— 这个信号可以彻底消失而全套无感。

作为对照，我对 `AnchorLagSeqs` / `AttestationBatchResult` 做同样处理是**红**的（`TestAttestation_HealthyRunReportsZeroLag` / `…AnchorPublishFailureEmitsMetric`），说明缺口是 per-metric 的：`untestedAlertMetrics` 只登记了 2 个（`SweepUnattributed` / `PendingEvents`），但按「唯一的测试出现处是 mock 方法声明」筛，候选还有 `StuckRollups`（已实证）以及 `AnchorLagSeqs` / `AnchorPublishResult` / `AttestationBatchResult` / `JobTickCompleted` / `JobTickSkippedLocked` / `SweepAddressUnreadable`（这几个我逐一核过至少 3 个是有真断言的，筛法会误报，见第 5 节）。

**建议**：census 的判据从「名字作为任意 identifier 出现」收紧到「名字作为 `SelectorExpr.Sel` 出现在一个 `_test.go` 的**断言表达式**里」，或者干脆改成「某个 recorder 字段被 `assert`/`require` 读过」。coverage 那半可以加一条便宜的可达性近似：call site 所在的最内层 `if` 条件不得是常量 `false`。

### F-6 · web hardening census 数的是属性名，不是属性有值 —— CONFIRMED（中）

`skin-parity.test.ts` 的 `HARDENING_TOKENS = ["aria-label=", "truncate", "min-w-0"]`，`pageCensus` 用正则数出现次数，per-page floor 只许升不许降。M-14 已经把「数裸词 `aria-label`（会命中散文）」修成「数属性 `aria-label=`」。

**实测**：`src/components/pages/TemplatesPage.tsx`（floor = 14，J-12 专门加固过的那一页）里 8 个 `aria-label="…"` 全改成 `aria-label=""`。`npm run -w @azex/ledger-react build` 之后 `npx vitest run` = **41 files / 225 tests 全绿**。仓内没有 `jsx-a11y`（`web/eslint.config.mjs` 无 a11y 规则，符合 `nextjs.md`「Next 16 默认不引 ESLint」的口径），所以 `npm run lint` 也不管。

同理可绕的两种更自然的漂移：把 `truncate` 从长地址单元格移到一个装饰性 `<span>`；把 `min-w-0` 从 flex 子项移到无所谓的容器 —— 计数不变，防溢出/可读性没了。

顺带一条**正面结论**：不重建 dist 时 6 个测试会红，红的原因是 `readDistFile` 的 staleness 检查（m-4 那个修补），它确实有效 —— 也就是说这一轮 web 门禁里唯一真正 fail-closed 的东西是新鲜度，不是内容。

**建议**：token 判据加值非空（`/aria-label="[^"]+"/`）。若要更进一步，用 `@testing-library` 的 `getByRole(..., { name })` 对每个 page 的表格/输入做一次真实可访问名断言 —— 这一步才是从「census」升级为「pin」。

### F-7 · I-18 的 banned-key 是从列名派生的，换个名字就穿过 —— CONFIRMED（中低）

**做法**：给 `core.JournalInput` 加一个字段（就在 `EventUID` 上面）：

```go
EventRef int64 `json:"event_ref"`
```

**结果**：`./core` / `./server` / `./service` 全绿；只有根包的 `TestAPISurface_MatchesSnapshot` 红。

`internal/idschema.BannedKeys` 从 migration 里的 BIGSERIAL/IDENTITY 列名派生（`id`、`journal_id`、`classification_id` …），`event_ref` 不在其中。I-18 的措辞是无条件的（「core types and interfaces speak uids exclusively」），门禁实际是「不得使用 schema 里那些列名」。`TestNoInternalIDsInCoreInterfaceSignatures` 也没抓到（它看的是接口签名，不是 struct 字段）。

严重度定中低，因为 ① 唯一报警的 `TestAPISurface_MatchesSnapshot` 确实是 CI 里会红的（`fetch-depth: 0` 那条注释说明 breaking gate 是当真的）；但 ② 它的修法是「重新生成快照」，一个赶时间的人会直接照做，而快照 diff 里 `EventRef int64` 看起来无害。

**建议**：在派生的 banned key 之外，追加一条类型侧判据 —— `core` 包导出类型的 `int64` 字段，除白名单（`AccountHolder`、`Exponent`、金额相关等）之外一律要求登记理由。这和现有 `internalIDAllowlist` 的形状一致。

### F-8 · `govulncheck` 不覆盖两个 submodule，而 CI 门禁的矩阵里没有它 —— CONFIRMED（中）

`go-verify.yml` 的 `vulncheck` job 只有一步：根目录 `govulncheck ./...`。Go 不跨 module 边界，所以 `anchors/r2`（`aws-sdk-go-v2` S3）与 `chains/evm`（`go-ethereum`）—— 恰好是第三方依赖面最大的两个 —— 完全没有漏洞扫描。`go-release.yml` 里 `verify-anchors-r2` / `verify-chains-evm` 各自只有 vet/lint/build/test，也没有。

`core/ci_workflow_gates_test.go` 的 `TestGoVerifyRunsRealCommandsForEveryModule` 是专门为了防「根有、submodule 漏」这种漂移建的（F-m8 / M-1），但它的 coverage 结构体是 `struct{ vet, build, test, lint bool }` —— **没有 vulncheck 位**。所以这个缺口既存在，也没人会注意到。

CLAUDE.md 的 Gotchas 里已经很细致地讨论了「testcontainers 会出现在 consumer 的 dependency graph 里但 `govulncheck ./...` 因为不可达而正确排除」—— 那段推理只对根 module 成立。

**建议**：`go-verify.yml` 的 vulncheck job 加两步 `working-directory: anchors/r2` / `chains/evm`；同时给上面那个 coverage 矩阵加一个 `vulncheck` 位（判据：`runInvokes(step.Run, "govulncheck")`）。

### F-9 · Docker 不在时本地 `make test` 以「全绿」收场 —— CONFIRMED（中低，仅本地）

`internal/postgrestest/postgrestest.go` 的 `baseConnection`：

```go
if sharedServer.err != nil && strings.Contains(sharedServer.err.Error(), "Cannot connect to the Docker daemon") {
    t.Skip("Docker daemon not running, skipping integration test")
}
```

CI 里 `DATABASE_URL` 指向 service container，走不到这条，所以 **CI 侧没问题**。但本地 `make test` 在没起 Docker 时会把 `postgres` 包 100+ 个集成测试（也就是本报告里绝大多数「红」的来源）全部 skip 并打印 `ok`。这正是 `working-agreements.md` §3「未运行 ≠ 通过」和 `Makefile` 里为 `-count=1` 写的那段治理注释的同一个形状 —— 只是这一类还没治。

**建议**：`make test` 前置一个 `docker info >/dev/null` 检查（或让 `postgrestest` 在既没有 `DATABASE_URL`、又不是 `-short`、且 Docker 不可用时 `t.Fatal` 而不是 `t.Skip`）。

### PLAUSIBLE（有理由怀疑，本轮未做成 mutation）

| # | 门禁 | 怀疑 |
|---|---|---|
| P-1 | `TestRouteAuthorization_EveryRouteIsGuarded` 的 holder 分支 | scoped 分支是**行为探测**（`deriveRouteRequirement` 真跑 middleware 链、按 scope 逐级探），holder 分支只 `chainContains(chain, "holderTokenAuth")` **按函数名**匹配。我把 `holderTokenAuth` 改成 `next.ServeHTTP(w, r)` 直通后门禁本身仍绿（净结果由 `TestLedgerdHolderSurface` 等三个测试救回）。C-1 那轮修的正是「有授权检查 ≠ 有对的那个」，holder 分支还留在修之前的形状。 |
| P-2 | `TestREADMEMetricsMethodCountMatchesInterface` / `TestREADMEOpenAPICountsMatchSpec` | 只比**数量**。改名、一增一减、语义换掉都不动数字。文件自己的注释承认「a few genuinely can't be phrased without a number」，属已披露；但 README 里那些**名字**没有任何门禁。 |
| P-3 | openapi 契约门禁（22 个） | 双向、含 type/format/required/嵌套/version，非常扎实。我没找到 **enum 值**的推导：spec 里 `status`/`kind` 的 `enum:` 列表不是从 Go 常量集派生的，所以 Go 侧新增一个状态值不会让门禁红（I-44 的 `TestHolderTransactionsKindIsExplicitVocabulary` 覆盖了 holder kind 那一个词表，其余没查）。 |
| P-4 | `ledger-react.yml` 的 path filter | 只在 `web/**` / `docs/openapi.yaml` / 该 workflow 自身变化时触发。所以 web 全套（含 F-6 的 census、skin parity、dist 门禁）对纯 Go / 纯 docs 的提交不跑。这大概是有意的，但 `docs/frontend.md` 的漂移因此没有门禁（`README` 侧有 `readme_docs_consistency_test.go`，`docs/frontend.md` 没有对应物）。另：`Makefile` 的 `openapi-check` 注释已自承 CI 的 codegen:check「是报告而非保证」，取决于仓库的 branch protection —— 我无法从代码里验证那项配置。 |
| P-5 | `anchortest` conformance（I-48 / I-56） | `TestCheck_Catches*` 系列证明 `anchortest.Check` 能抓 5 类坏实现，这是很好的自证。没证的是**完备性** —— 一个在 `Check` 没探的维度上违约的 anchor（例如并发 `Publish` 同 seq、或 `Head` 在网络分区下返回旧值而非报错）照旧「provably conformant」。`TestCheck_OutOfBandPhaseIsSkippedNotPassedWithoutTheHook` 说明作者已经意识到「skip ≠ pass」这一层。 |
| P-6 | 只有 `require.Error`（不辨错误种类）的 pin | 我用 AST 近似筛了「离钱最近、testify 断言只有 `NoError`/`Error`」的测试函数，得到 26 个。**逐个核过后大部分是合理的**（`TestVerifyReservationDischargeAuth` 是一张 6 行负例表，只是用 `require.Error` 而不是 `require.ErrorIs`；`TestConfirmPending_ConcurrentSameKey_NeverInsufficientBalance` 的断言本身就是「不该报错」）。真正值得改的是：I-45 明确要求区分 `ErrUnknownAuthKey` 与签名无效，而 discharge 侧（I-65）的 `TestVerifyReservationDischargeAuth` 6 个负例全用 `require.Error`，换成 `require.ErrorIs` 才能防「所有失败原因坍缩成一种」。 |

### 互斥 / 自相矛盾的测试

**没找到**真正互斥的断言对。我具体查过两组看起来矛盾的：

- `TestReserverStore_Settle_RefusesExpiredReservation` vs `TestReserverStore_FinalizeSettlement_AllowedAfterExpiry` —— I-49 的文本明确说明这是有意的（过期后 settle 拒、finalize 放，因为 `expires_at` 是唯一不可伪造的 discharge 依据）。
- I-53 的「WATCHER 路径上 `pending`/`confirming` 不再发生」vs I-20 的 `TestOnchain_IngestDeposit_FullLifecycle` —— 后者驱动的是 webhook 路径，I-53 自己的 section 也写明了这个区分。

---

## 5. 我没能验证的（盲区）

**盲区条数：24 条不变式 + 6 类门禁面。**

### 未做 mutation 的 24 条不变式

I-6、I-7、I-10、I-14、I-19、I-20、I-21、I-27、I-28、I-29、I-30、I-31、I-34、I-37、I-38、I-41、I-44、I-47、I-48、I-53（机制面）、I-54、I-55、I-56、I-62、I-63。

原因分三类：
1. **attestation / anchor 家族（I-27…I-31、I-48、I-55、I-56）**：机制横跨 `core`（Merkle/digest）+ `service`（batch/anchor）+ `anchors/r2`（外部对象存储 + MinIO testcontainer），每条要拆的点不止一处，且相互纠缠 —— 单点 mutation 很容易做成「改了个不相干的地方」，我在 I-40 上已经踩过一次（第一次改中了 `DBTX()` 而不是 guard，报告里的 M33 是修正后的重跑）。宁可留白也不给假结论。
2. **onchain 家族（I-19…I-21、I-34、I-41、I-62、I-63）**：机制主要在 `service/onchain.go` 的启动校验与 stage 推进，pin 靠大量 stub 装配；同上。
3. **纯 schema 类（I-6、I-7、I-10、I-14、I-44、I-47、I-54）**：可拆但优先级排在离钱更近的后面，时间用完了。

### 未验证的门禁面

1. **branch protection**：`Makefile` 自己在 `openapi-check` 上写明「CI 的 codegen:check 是报告还是阻塞，取决于仓库的 GitHub branch protection 设置，从代码里无法验证」。我同样无法验证 —— 也就是说本报告里所有「CI 会红」都只等价于「workflow 会失败」，不等价于「合不进 main」。
2. **`web/` 宿主应用（dogfood）**：`ledger-react.yml` 最后两步 `npm run build` + `npm run lint` 跑的是 `web/` 那个 Next 应用，我只跑了 `packages/ledger-react` 的 vitest，没跑宿主构建与 lint。
3. **`chains/evm` 的 e2e（`-tags e2e`）**：需要 PATH 上有 anvil (foundry)，本机没有；这套测试自己会 self-skip，CI 靠 `foundry-toolchain@v1` 把 skip 变成真跑。我因此没能验证 e2e 那 5 类断言（CREATE2 派生 → 部署 → Transfer 解码 → sweep 签名广播 → Scanner 复查）的可证伪性。这一层本身就是 F-M5 修的「从未被编译过」的那批文件。
4. **`anchors/r2`**：需要 MinIO testcontainer；本轮 `go test ./...` 只覆盖根 module，没跑 `make test-submodules`。所以 I-56 的 `TestAnchor_ObjectLockRefusesDeletingAPublishedVersion` 等我没有实测基线。
5. **fuzz 门禁的实效**：CI 各跑 30s（`FuzzJournalValidate` / `FuzzLifecycleValidate` / `FuzzAllocate`）。我只跑了 seed corpus（`go test` 默认行为），没做 30s 真 fuzz，所以无法判断 30s 对这三个目标是否足以覆盖有意义的状态空间。
6. **`core.Metrics` 的 behaviour-pin 普查**：我的 AST 近似筛法（「唯一非注释出现处是 mock 方法声明」）会误报 —— 例如 `JobTickCompleted` 被筛进候选，但它其实通过 recorder 的 `completed` **字段**被真实断言（`service/worker_metrics_test.go:175`）。我只逐一实测确认了 `StuckRollups`（全绿）与 `AnchorLagSeqs`/`AttestationBatchResult`（红）。剩下 4 个候选（`AnchorPublishResult`、`JobTickSkippedLocked`、`SweepAddressUnreadable`、`JobTickCompleted`）**未逐一实测**，不应按 F-5 的结论对待。

### 方法论上要交代的两件事

- **改 `.sql` 查询文件对运行时无效**：运行时走 `postgres/sqlcgen/*.sql.go` 里的字符串副本。我在 I-52 上先得到一次假绿（改 `sql/queries/chain_cursors.sql` 后 `TestChainCursorStore_SetCursor_IsMonotonic` 仍绿），改 sqlcgen 后立刻红。**报告里凡是标「红/绿」的 SQL 查询类 mutation 都已改在 sqlcgen 上**；migration 类（`sql/migrations/*.sql`，走 embed FS）改源文件即有效。任何复核我这份报告的人请注意这一点。
- **一次 mutation 一处**：驱动脚本对同一文件的多处编辑做过一次错误处理（备份被第二次编辑覆盖），导致 `postgres/reserver_discharge_auth.go` 曾残留在已改状态；已当场 `git checkout --` 还原并修正脚本（改用按路径去重的备份表）。此后每次 mutation 结束都打印 `git status --porcelain`，全部为空。

---

## 6. 最终 worktree 状态

```
$ git -C /Users/aaron/projects/_worktrees/ledger/r3-gates-pins status --porcelain
```

（输出为空 —— worktree 干净，无残留 mutation。`web/node_modules`、`web/packages/ledger-react/dist` 为构建产物，已被 `.gitignore` 覆盖，故不出现在上述输出中。）

主仓 `/Users/aaron/projects/ledger` 本轮唯一写入的文件是本报告：
`docs/audits/2026-09-03-independent-review/gates-pins.md`。

---

# W5 处置（`w5-gates`，2026-09-03/04 SGT）

> 分支 `w5-gates`，基线 = main `02041d7`。本节由整改 worker 追加，是**本报告唯一被改动的部分**。
> 纪律：**每一条都先按复审员写的 mutation 跑一次红，改完再跑一次绿**。凡是我判「不成立」的，下面写明判据，不是省略。

## 1. F-1…F-9 逐条处置

| # | 处置 | 复现的 mutation | 改后 |
|---|---|---|---|
| **F-1** | 已修 | `journals.idempotency_key` 去 `UNIQUE` → 全套零失败 | 两个直击 pin + 一条通用门禁，见下 |
| **F-2** | 已修 | `ledger_block_mutation()` 改 `RETURN NEW` → 只红 3 个 | 同一 mutation 现在红 **25** 个 |
| **F-3** | 已修 | 加一条豁免项 + 两行文档改动 → `core` 全绿 | 尺寸锁 + 声明解析，两半都红 |
| **F-4** | 已修 | Go map 查表 / SQL `FILTER`+布尔转 int → 三个门禁全绿 | 两种写法都红 |
| **F-5** | 已修 | `StuckRollups` 发射包进 `&& false` → 全套（含 postgres）全绿 | 覆盖门禁与 census 都红，并补了真 pin |
| **F-6** | 已修 | 8 个 `aria-label` 清空 + 重建 → 225/225 全绿 | 同一 mutation 现在红 |
| **F-7** | 已修 | `EventRef int64` 加进 `core.JournalInput` → core/server/service 全绿 | 立刻红 |
| **F-8** | 已修 | （非 mutation，读配置）两个 submodule 无 govulncheck | 补扫描 + 矩阵加 vulncheck 位；删掉步骤则红 |
| **F-9** | 已修 | （环境）Docker 停机时 `make test` 报 `ok` | 改 fail-closed，两半各有 pin |

### F-1 的推广：「约束声明 × 直击 pin」表

`core.TestDatabaseSideClaimsHaveADirectSQLPin`：读 `docs/INVARIANTS.md` 每一条 **Enforced by**，凡声称 Postgres 侧机制（constraint / index / trigger / grant / partition）的，要求该 section 的 pin 里**至少有一条自己发 SQL**（写入、目录读取或权限探测都算；走本库写路径的不算）。

22 条声称 DB 机制，其中 **5 条没有直击 pin**。全部在机制侧修掉，豁免表 ship 时为空且锁死：

| I | 声称的机制 | 原状 | 处置 |
|---|---|---|---|
| I-1 | `check_journal_currency_balance()` 触发器 + `chk_journal_balance` CHECK | 4 个 pin 全走 `Validate`，DB 层背书无 pin | 引用已存在的 `TestJournalBalanceTrigger_RejectsDirectSQLImbalance`（此前只挂在 I-24 名下）+ 新增 `TestJournalTotalsCheck_RejectsDirectSQLImbalance` |
| I-3 | 五张表的 `idempotency_key UNIQUE` | 15 个 pin 全走 advisory lock | 新增 `TestJournalIdempotencyKey_RejectsDirectSQLDuplicate`（双插入断言 23505）+ `TestEveryIdempotencyKeyColumnHasATotalUniqueIndex`（目录派生） |
| I-11 | `classifications.balance_role` CHECK | pin 全走 `CreateClassification`（Go 侧先校验） | 新增 `TestBalanceRoleCheck_RejectsDirectSQLUnknownRole` |
| I-12 | 同 I-1 的延迟触发器 | 4 个 pin 对触发器失效无感（复审员标「分不清」） | 引用同一个触发器 pin —— **这就是那一行的答案** |
| I-16 | `currencies.exponent CHECK (0..18)` | pin 全走 `CreateCurrency` | 新增 `TestCurrencyExponentCheck_RejectsDirectSQLOutOfRange` |

顺带发现两条**失效的迁移引用**：I-11 引 migration `032`、I-16 引 `027_currency_exponent.up.sql`，baseline 压平后两者都不存在（027 现在是 `unlink_event_journal`）。已改为真正声明该约束的文件。

三个新 CHECK pin 一律用 **INSERT** 而非 UPDATE —— 写这些 pin 时发现配置表上有 `ledger_*_guard` 触发器，UPDATE 会先被它拒（"may only change is_active"），断言就会因为错误的原因通过。

### F-3 的两点重判

1. **对象校验改解析声明**（已做）：`objects` 从 `[]string` 改成 `[]dbObject{kind, name, detail}`，按 kind 找声明形状（`CREATE TRIGGER <name>` / `CREATE [OR REPLACE] FUNCTION <name>` / `CREATE [UNIQUE] INDEX <name>` / `CONSTRAINT <name>` / `CREATE ROLE <name>` / 列级 unique·nullable·类型（**限定在该表的 `CREATE TABLE` 语句内**）/ 分区表 / 分区 / 语句内的 REVOKE）。未知 kind **直接失败**，不静默通过。删掉 `journals` 那一处 UNIQUE 立即红 —— 旧的全文件 substring 要删到 5 处全没才红。
2. **「登记项对应约束若已有直击 pin 则必须出表」—— 判定不成立**，已跟 team-lead 确认。`unresolvableEnforcedCitations` 的判据是「Enforced by 里没有任何可解析的导出 Go 符号」，和「有没有直击 SQL 的 pin」是两个正交维度：按那条规则 I-3 现在有了直击 pin 就得出表，但它的 Enforced by 仍然只有约束名，出表会让 `TestInvariantsPinsReferenceEnforcedSymbols` 立刻红。改为等效的两条：**尺寸快照锁**（`dbOnlyRegisterSize = 13`，只许改小）+ 让 `TestDatabaseSideClaimsHaveADirectSQLPin` **同样适用于已登记 section**（登记不豁免直击 pin 要求）。
3. 顺带修掉报告点名的松下界：`require.Greater(checked, len(register))` 在 13 条登记下要等名单涨到 32 才会响。改成对文档的真实 floor（今天 52 条被检查，floor 取 50）。

### F-5 的两点收紧（以及一条附带的行为 bug）

- **覆盖门禁**加静态可达性：常量 false 的 `if`、常量 true 的 `else`、永不执行的 `for`，其中的 call site 不算。完全可达性是数据流问题、不是这个门禁的职责；常量 false 是「调试时把信号关掉忘了打开」的形状，也是唯一一种「发射不可达但对读者和语法扫描完全正常」的写法。
- **census** 从「名字作为任意 identifier 出现过」收紧为「作为 selector 出现，或作为**函数体非空**的方法声明出现」。空方法体是每个宽接口 mock 都必须为每个方法写的样板；按旧规则这个 census 永远报不出东西。收紧后跑全接口，**恰好只报出 `StuckRollups`** —— 正是复审员实测的那一个。这是判据校准得当的证据。
- 补 `service.TestRollup_ReportsStuckAndPendingSeparately`。
- **写这条 pin 时发现一个真实行为 bug**：`ProcessBatch` 在 `len(items) == 0` 时提前 return，**在两个 gauge 发射之前**。而 `StuckRollups` 数的正是「耗尽重试、不会被 dequeue」的条目 —— 队列**全部**卡死时恰好一条都 dequeue 不到，两个 gauge 一次都不发；gauge 停写不等于归零，仪表盘会一直显示最后一个健康值。已按 team-lead 授权修（gauge 先发再 return），并加 pin。

### F-9 的边界

`postgrestest` 现在在容器起不来时 `t.Fatal`，只保留 `-short` 这一条调用方显式说出口的跳过；`make test` 前置 `docker info` 探测，并在 `DATABASE_URL` 已设时不触发。两半都由 `core/test_infra_gates_test.go` 钉住 —— 它是源码级断言，因为「Docker 不在」这个条件没法从一个需要 Docker 才能跑的进程里造出来。

## 2. PLAUSIBLE 段逐条

| # | 结论 | 处置 |
|---|---|---|
| **P-1** | **成立** | 已修。复审员的 mutation（`holderTokenAuth` 换成直通）此前只由三个不相干的 handler 测试兜住；新增 `server.TestHolderRoutes_RejectEveryRequestWithoutAValidToken`，对 holder 前缀下每条路由跑三次（无凭证 / 别的密钥签的 token / 有效 token），前两次必须 401 且不到达 store，第三次必须不是 401。同一 mutation 现在直接红在门禁本身。**正向 control 当场救了这个 pin 一次**：第一版把 `/api/v1` 前缀拼重了，每个请求都 404，两条否定断言全部因为完全错误的原因通过。 |
| **P-2** | **成立** | 已修。README 印给读者去建仪表盘的 `ledger_*` 指标名此前无门禁；新增 `TestREADMEMetricNamesExistInThePrometheusAdapter`，从 adapter 的 namespace 常量 + 各 collector 的 `Name` 派生。单向：反向会要求 README 列全 41 个，而它明说了去读 `core/metrics.go`。改名 `rollups_pending` 立刻红。 |
| **P-3** | **成立** | 已修。spec 里 25 个 `enum` 无一从 Go 常量派生。新增 `server.TestOpenAPIEnumsAreDerivedFromGoConstants`：7 个词表（EntryType / NormalSide / HolderRole / BalanceRole / AccountPolicyStatus / ReservationStatus / HolderTxKind / DepositToleranceOutcome）按 **Go 类型**而非 OpenAPI component 登记 —— 因为多数词表在 spec 里既有具名 component 又有内联副本（ReservationStatus 是 component + query 参数 + 响应字段），只派生具名那份等于放任内联副本掉队。三条规则：① 每个词表必须在 spec 里**逐字出现**；② 任何 enum 不得是某个词表的**真子集/真超集**（这正是「某一份副本停止更新」的形状；单值 enum 豁免，那是响应体里的字面量）；③ 其余每个 enum 登记不派生的理由，且名单不得比 spec 活得久。有一个 enum 是「词表减去一个值」（holder **transaction** 的 kind 永远不为 `""`，读路径把 `HolderTxKindNone` 解析成 `HolderTxKindOther`），这条也是派生的。加/减一个 `BalanceRole` 常量都红。 |
| **P-4** | **成立，未修，记录在案** | `ledger-react.yml` 的 path filter 确实让 web 全套对纯 Go / 纯 docs 提交不跑。这是有意的（省 CI 时间且 web 产物不受 Go 改动影响）。`docs/frontend.md` 没有 `readme_docs_consistency_test.go` 那样的对应物 —— **确认是缺口**，但它属于 W5-readme 的独占域（`docs/` 文档一致性门禁），未在本分支动。 |
| **P-5** | **成立，部分处置** | `anchortest.Check` 的**完备性**确实无法自证。本轮把它验到了能验的程度：删掉 head-regression 阶段 → `TestCheck_CatchesHeadRegression` / `TestCheck_CatchesOutOfBandHeadRegression` 红（见下 I-48）；`anchors/r2` 的 `Publish` 去掉 create-only 检查 → `TestAnchor_Conformance` + `TestAnchor_PublishIsCreateOnlyPerSeq` 红（I-56）。也就是说「Check 能抓它探的维度」是真的。复审员点名的两个**未探维度**（并发 `Publish` 同 seq、网络分区下 `Head` 返回旧值而非报错）仍未探 —— 属新增 conformance 阶段，是设计工作不是门禁工作，**留给拍板**。 |
| **P-6** | **成立** | 已修。`TestVerifyReservationDischargeAuth` 六个否定用例是六个裸 `require.Error`，分不清「六个检查都在工作」和「只有第一个在工作」。每个用例改为断言**是哪一个检查拒的**；`verifier rejects` 追加 `require.ErrorIs` 要求 verifier 自己的错误活着传到调用方；新增第七个用例直接钉 I-45 在 discharge 面的形态（不认识的 key 必须以 `core.ErrUnknownAuthKey` 到达）。把其中两个分支坍缩成统一的 "unauthorized" → 三个 subtest 红，此前六个全过。 |

## 3. 互斥 / 自相矛盾的测试

复审员的结论**复核成立**，没有新增发现。具体核了它点名的两组：

- `TestReserverStore_Settle_RefusesExpiredReservation` vs `TestReserverStore_FinalizeSettlement_AllowedAfterExpiry` —— 两者驱动的是**不同操作**（`Settle` / `FinalizeSettlement`），I-49 的正文写明这是有意的：过期后 `expires_at` 是唯一不可伪造的 discharge 依据。不互斥。
- I-53 的 WATCHER 路径 vs I-20 的 webhook 路径 —— 两条 section 各自写明了路径区分。不互斥。

## 4. 24 条未 mutation 的不变式 —— 逐条结果

每条拆掉它 **Enforced by** 点名的那个机制，跑该 section **Pinned by 所在包**（这一点很重要，见下方方法论）。

| I | 拆的机制 | 结果 |
|---|---|---|
| I-6 | `journal_entries.amount` `NUMERIC(30,18)` → `(20,8)` | **红**（`TestSchema_NumericColumnsAreExactly30_18`） |
| I-7 | `journals.total_debit` 去 `NOT NULL` | **红**（`TestSchema_NullableColumnsExactlyMatchI7Exceptions`） |
| I-10 | 不再调 `linkJournalToEventAndBooking` | **红，但由未登记的 pin 承担** → 见下 |
| I-14 | entry 的 `EffectiveAt` 写 `time.Now()` 而非 journal 解析出的值 | **红**（`..._EffectiveAt_Backdated`） |
| I-19 | 让 sweep 路径真的 `PostJournal` | **红** |
| I-20 | `TxLogSeq` 改用区块级 log index | **红**（在 `chains/evm` 模块内跑） |
| I-21 | `routeToReview` 同时 post `deposit_confirm` journal | **红**（`TestRouteToReview_EmitsDepositReviewRequired`） |
| I-27 | `entry_attestations` 去掉 `PRIMARY KEY (entry_id)` | **红**（6+） |
| **I-28** | `VerifyLedger` 停掉 `a.Seq == anchorSeq` 的 head 比对 | **绿 —— 空洞** → 已补 pin，见下 |
| I-29 | `AttestationRootHashV2` 不再把 `merkle_root` 绑进签名 | **红**（含 `TestVerifyLedger_TamperedMerkleRootAlone`） |
| I-30 | `largestPowerOfTwoLessThan` → 朴素折半 | **红**（golden vectors + 奇数叶不复制） |
| I-31 | 冲销路径永不签名 | **红** |
| I-34 | `requireCapability` 直通 | **红**（含 `TestDepositReview_SelfMintSelfApprove_MI2`） |
| I-37 | 偿付负债的 role 过滤改成 `IS NOT NULL` | **红**（4+） |
| I-38 | 注册一条 spec 未声明的路由 | **红** |
| I-41 | `scanned == 0 && resumedLap` 判断停用 | **红**（2 个） |
| I-44 | `HolderTxKind.IsValid()` 恒 true | **红** |
| I-47 | `Migrate` 去掉 cluster advisory lock | **红** |
| I-48 | `anchortest.Check` 跳过 head-regression 阶段 | **红**（2 个） |
| I-53 | `confirmationDepth` 恒 0（扫到链头） | **红** |
| I-54 | `Worker.Run` 不再拒绝 nop logger | **红**（根包 `TestServiceWorker_RefusesToRunUnderTheDefaultSilentLogger`） |
| I-55 | 不再 `RecordAnchorObservation` | **红**（`TestVerifyLedger_AnchorRollbackToAnOlderSeqIsTampered`） |
| I-56 | R2 `Publish` 允许同 seq 覆盖不同 head | **红**（`TestAnchor_Conformance` + `TestAnchor_PublishIsCreateOnlyPerSeq`） |
| I-62 | 未配置 ceiling 不再是启动错误 | **红** |
| I-63 | 不再调 `RecordReorg` | **红**（`TestOnchain_ReorgRecheck_AnomalyOutlivesTheRecheckWindow`） |

统计：**23 红 · 1 空洞（I-28，已修）· 1 引用错位（I-10，已修）**。

### I-28 —— 唯一真正空洞的一条（已修）

section 的主张是「最新的外部 anchor head 与数据库的 attestation 链一致」，机制是 `VerifyLedger` 里**唯一一行**把 `seq == anchorSeq` 的 DB 行与 anchor 已发布的 head 相比。把它停掉，`go test ./...` **每个包都绿**。它列的 11 个 pin 全是这行**周围**的情况（anchor 为空、anchor 落后、anchor 回滚、anchor 领先于 DB），各由不同分支裁决，head 比对没了它们照旧过。

这恰恰是锚定的全部意义：`VerifyLedger` 里其他每一个检查读的都是「拥有数据库的攻击者能改写的数据」，只有这一行是跟数据库之外的东西比。

新增 `service.TestVerifyLedger_AnchorHeadDisagreeingAtTheAnchoredSeqIsTampered`：数据库保持真实且自洽，改的是**anchor 说了什么**，于是 DB 侧每个检查都过，findings 只能来自被测的这行比对。（改 DB 行会同时触发上一分支的 root_hash 自洽检查 —— 那样报出 TAMPERED 什么也证明不了，这正是这个机制看起来「有覆盖」的原因。）control 断言的是**那一条 reason 不存在**而不是整体 VERIFIED，因为 fixture 的 journal 是刻意伪造的、自带一条 unsigned-journal finding，比状态会让断言在无关 finding 上通过。

### I-10 —— 机制在、pin 名单错（已修）

拆掉 `linkJournalToEventAndBooking` 后，它列的三个 pin 全绿；真正红的是 `TestPostJournal_EventUID_RejectsUnrelatedJournal`、`TestTxComposition_RunInTx_BookingEventJournalLinkage`、onchain 充值全生命周期测试和 migration 027 的 unlink 测试。已把前两个写进 Pinned by（后两个经由 service 到达该机制，pin-vs-mechanism 门禁按设计无法把它们钉到这条上，正文写明了）。

## 5. 六类未验证门禁面

| # | 门禁面 | 结果 |
|---|---|---|
| 1 | **branch protection** | **仍无法从代码验证**，结论与复审员一致：本报告所有「CI 会红」只等价于「workflow 会失败」，不等价于「合不进 main」。属仓库设置，需 Aaron 在 GitHub 侧确认。 |
| 2 | **`web/` 宿主应用（dogfood）** | **已跑**：`npm run build` exit 0（15 条路由全部产出）、`npm run lint` exit 0。`ledger-react.yml` 确实有这两步（第 65-66 行）。 |
| 3 | **`chains/evm` e2e（`-tags e2e`，需 anvil）** | **已跑且已证伪**。本机有 foundry，`TestE2E_WatchThenSweep` 真跑 2.25s（不是 self-skip）。两次 mutation 都红：① Transfer 金额解码改成常量 1 → 红；② CREATE2 域字节 `0xff` → `0xfe` → 红（且 `core` 的 golden vector 也红）。这层是有效的。 |
| 4 | **`anchors/r2`（需 MinIO testcontainer）** | **已跑且已证伪**。见 I-56：`Publish` 去掉 create-only 检查 → `TestAnchor_Conformance` + `TestAnchor_PublishIsCreateOnlyPerSeq` 红。 |
| 5 | **fuzz 30s 的实效** | **已跑，并且找到了一个 Critical** → 见下节。`FuzzLifecycleValidate`（544 万次执行）与 `FuzzAllocate`（480 万次）30s 内无发现；`FuzzJournalValidate` **30s 内失败**。 |
| 6 | **`core.Metrics` 的 behaviour-pin 普查** | **已用机器判据答完**。F-5 收紧后的 census 跑全接口，恰好只报出 `StuckRollups`。复审员留的 4 个候选（`AnchorPublishResult`、`JobTickSkippedLocked`、`SweepAddressUnreadable`、`JobTickCompleted`）在收紧后的判据下**均判为有 pin** —— 它们都由带非空函数体的 recorder 方法捕获（例如 `JobTickCompleted` 走 recorder 的 `completed` 字段，`service/worker_metrics_test.go:175` 有真断言）。复审员怀疑「筛法会误报」，成立。 |

## 6. 门禁面 #5 的产物：`FuzzJournalValidate` 30 秒内找到 money-path DoS（**CRITICAL，待拍板**）

**已修**（team-lead 2026-09-04 裁决 Critical 成立并授权在本分支修 —— 见本节末「处置」）。证据链（端到端实证，不是推断）：

失败输入：`amount = "10E777777070"`。最小复现用 `"1E999999999"`。

| 步骤 | 结果 |
|---|---|
| `decimal.NewFromString("1E999999999")` | **成功**，微秒级（shopspring 惰性存 coefficient=1 / exponent=999999999） |
| `core.JournalInput.Validate()` | **通过**，微秒级（只调 `IsPositive()`） |
| `postgres.checkAmountPrecision` | **通过**，微秒级（`amount.Equal(amount.Truncate(18))` 对该 exponent 为 true） |
| `d.Add(d)` / `d.Equal(d)` | 微秒级 |
| `d.String()`（上 wire / 绑 SQL 参数） | **不返回** |
| `LedgerStore.PostJournal(...)` | **90 秒不返回**，栈停在 `math/big.karatsubaSqr` —— 真的在展开 10^999999999 的十进制数字 |

**可达面（修正我上报时的说法）**：我最初报的是「任何持 write scope 的 key 发 `POST /api/v1/journals` 就能挂死进程」。修的时候回查了每一个 untrusted 金额解析点，**这句话是过度陈述**，据实更正：

- `server/amount.go` 的 `parseWireAmount` 会拒绝含 `e`/`E` 的字符串（api-contract §4 的 wire 格式规则），而**全部 14 个 REST handler 的金额都走它** —— 所以 REST 面对这个形式是**偶然被挡住的**。
- `channel/onchain/evm.go:124,156` 的 `EVMAdapter.ParseSighting` **直接对原始 body 调 `decimal.NewFromString`，不经过 `parseWireAmount`** —— 入站 webhook → `DepositSighting` → `IngestDeposit` 是**真正远程可达**的那条路径。
- **library 模式全面暴露**，前面什么都没有（而 library 模式是本库的主消费形态）。

「REST 面被挡住」不能当缓解：挡它的是一条 wire 格式规则而不是量级上界，离「不成立」只差一次放宽、或一个自己解析金额的新 handler。这也正是 I-66 把门放在 `core` 而不是 handler 的理由 —— 和 `core/limits.go` 为 Metadata/Source 写的理由同一条。

`core/limits.go` 已经为 Metadata/Source 立了「防病态上界」，并在它自己的注释里写明理由是「HTTP body cap 只覆盖两种消费模式中的一种，所以要放在 core 一次性检查」。金额没有对应的上界 —— **同一个形状的缺口，同一个文件里缺的那一条**。

建议修法（小而定向，判据本身常数时间）：`NUMERIC(30,18)` 本来就只能存 12 位整数位，所以 `len(coefficient.String()) + exponent > 12` 的金额根本不可存 —— 在 `Validate` 的 per-entry 循环**最前面**拒（必须在任何算术之前：不同 exponent 相加会触发 rescale，同样爆），`Reserve` / `AddPending` / `CreateBooking` / `Transition` 的金额入口同理。

### 处置（team-lead 裁决后）

`core.ValidateAmountMagnitude`（`core/limits.go`，与 `validateFreeformFields` 同形同文件）+ **I-66**。判据由 `NUMERIC(30,18)` 自己的宽度派生，不是拍的：整数位 > 12 或小数位 > 18 一律 `core.ErrInvalidInput`。常数时间，只读 `Exponent()` 和 coefficient 的位数 —— 检查本身绝不能变成它要防的那次展开。

先做兄弟扫描（core 里 59 个 `decimal.Decimal` 字段，筛出 13 个调用方提供的入口），**不是只修 `PostJournal`**：`JournalInput`（`EntryTemplate.Render` → `ExecuteTemplate` 由此继承）、`ReserveInput`、`SettleInput`、`SettlePartialInput`、`AddPendingInput`、`ConfirmPendingInput`、`CancelPendingInput`、`CreateBookingInput`、`TransitionInput`、`AccountPolicyInput`、`DepositSighting`（webhook 那条）、`SweepPolicy`、`TokenConfig`。在 `JournalInput.Validate` 里放在 per-entry 循环**最前面**，早于 `totalsByCurrency()` —— 不同 exponent 相加会 rescale，和渲染一样会炸。

pin：`core.TestEveryAmountEntryPointRejectsAPathologicalAmount`，14 个入口 × 4 个病态值，**每条带 100ms 时间上界**。时间上界才是断言本身：一个「先展开再拒绝」的检查能满足 `require.Error`，而它就是那个 DoS。接受侧的 control 当场抓到一个真 bug —— 第一版把负 coefficient 的 `-` 号数成了一位，`-999999999999`（合法的透支下限）被拒。

失败输入现已作为种子入库（`core/testdata/fuzz/FuzzJournalValidate/7ec58597750a4f04`），每次普通 `go test` 都重放，不只在 fuzz 时。修后复跑 CI 的同一个 30s 预算：528 万次执行，PASS。

实测：`PostJournal` 从「90 秒不返回」变成 **58µs 返回 `ErrInvalidInput`**。

⚠️ **编号偏差待你确认**：你指定 I-70，但 66–69 是兄弟分支的号段，在我这条分支上还不存在 —— 写 I-70 会让 `TestInvariantsDocIsOrderedAndGapless` 直接红（"I-70 follows I-65"），违反 Done 的全绿要求。我用了 **I-66**（本分支绿）。合并时二选一：先合我这条则 66 本来就对、兄弟们顺延；后合则按 gate 的提示改成当时空出的号。

## 7. 方法论：两条必须交代的事（复审员自己交代过两条，这是新的两条）

1. **mutation 必须跑「pin 所在的包」，不是「机制所在的包」。** I-54 的机制在 `service/worker.go`，我第一次只跑 `./service/` 得到「绿」；它的 pin 全在**根包**（doc 里按惯例裸引用），跑根包立刻红。这个错误会把一条有效的不变式误判成空洞 —— 上面那张表里每一行的包集合都是从该 section 的 Pinned by 里机器抽出来的。
2. **`_ = fn` 不是调用，加一个 `var` 不是加一条模板。** I-21 我第一次写的 mutation 是 `_ = o.postDepositConfirmedJournal`（只是引用），I-19 第一次是加了个字符串常量 —— 两次都「绿」，两次都是**无效 mutation**，不是空洞 pin。改成真的调用 / 真的 post journal 之后两条都红。同理 I-28 / I-63 我第一次写的正则没匹配上真实代码（`row.Seq` vs `a.Seq`、`!= nil` vs `== nil`），也各得到一次假绿。**凡是判「空洞」的，必须先证明 mutation 真的改变了行为。** 上面表里唯一的空洞（I-28）是在 mutation 确认生效、且跑遍全部五个 module 之后才下的结论。

# 审计报告 — onchain-money-path（链上资金路径）

> Territory G · 第二轮深度审计 · 基线 commit `fd87cae` · 只读
> 范围：`chains/evm/`（独立 module）→ `channel/onchain/` → `service/onchain.go` →
> `postgres/deposit_address*` / `registration_rescan*` → journal；以及反向的 sweep 签名与广播。
> 格式遵循 `docs/plans/2026-08-25-financial-audit-contract.md` §3。

## 摘要

上一轮的 3 条 Major + 1 条 Minor **全部真修了，而且修在真实路径上**（scanner 的 per-address
fail-closed 接进了 `sweepTick` 并有指标；`priorFeeFloor` 真的去链上读 `TransactionByHash`；
`GasPrice` 改用与实付同一个 `feeCapBasis`；mi2/mi5 都落地了）。所以这一轮的发现全部是新的，
且**重心从「归集出不去」移到了「充值进不来」**——上一轮把 sweeper 读透了，没读 watcher。

最要紧的两条都在**前向扫链**这一段，而这一段是整条资金路径上**测试覆盖最薄的地方**：
① `scanChainOnce` 在 `IngestDeposit` 失败后**照样推进游标**（`service/onchain.go:1246-1253`），
一次 DB 抖动 = 一笔真实充值永久消失，没有死信、没有指标、只有一行日志；
② `TxLogSeq` 的取值**取决于本次查询传了哪些地址**（`chains/evm/reader.go:153-154`），
而 watcher 传全量地址、registration rescan 只传一个地址——同一笔 tx 在两条路径上派生出**不同的幂等键**，
按先后顺序不同，结果要么是**同一笔充值记两次账**，要么是**另一笔充值进死信永不入账**。
I-20 声称 `txlog_seq` 是「tx 内确定」的，实现并不满足这个声称，而 I-20 的三条 pin
**没有一条经过 `chains/evm.Reader.FetchDeposits`**——那个函数一个单测都没有。

结构性观察：这条资金路径上**每一个「不可逆的自动决策」都建立在单次、无重试、无法定人数的 RPC 观测上**
（浅 reorg 直接打终态 `failed`、深 reorg 的 auto-reverse 直接冲销），
而**这些分支没有任何一条被测试执行过**（`handleReorg` / `processRegistrationRescan` 零测试，
gas-bump 重播分支被 `WithMaxSweepBumps(0)` 绕开）。上一轮说的
「机制存在，但没接到真实路径」在这里换了个形态：**机制接上了，但只在最顺利的那条支路上被执行过**。

---

## 发现

### [Critical] 前向扫链在 `IngestDeposit` 失败后仍然推进游标 —— 真实充值永久丢失，无死信、无指标

- **位置**：`service/onchain.go:1246-1253`
- **判定**：CONFIRMED
- **失效场景**：

  ```go
  for _, s := range sightings {
      if _, err := o.IngestDeposit(ctx, s); err != nil {
          o.log().Error("service: onchain: watcher: ingest failed", ...)   // 只记日志
      }
  }
  if err := o.deps.Cursors.SetCursor(ctx, chainID, to); err != nil { ... }  // 无条件推进
  ```

  `IngestDeposit` 有多条**非 ErrConflict** 的失败出口，每一条都会让这笔充值被静默丢弃：
  - `Registry.GetByAddress` / `Currencies.ListCurrencies` / `Booker.CreateBooking` 的任何 DB 瞬时错误
  - `o.currencies.resolve` 找不到 `tokenCfg.CurrencyCode` → `core.ErrNotFound`
  - `checkAmountPrecision`（`postgres/booking_store.go:124`）：token decimals 高于 currency exponent
    时（例如 18 位 token 记入 exponent=6 的币种），任何带尾数的转账都会被
    `core.ErrPrecisionExceeded` 拒绝
  - `advanceConfirmation` 里 `Transition(confirming)` 失败

  只有 `core.ErrConflict` 走死信（`service/onchain.go:686-690`）；**其余全部只有一行 log**。
  游标推进后，前向扫描永不回头（`from = cursor.LastScannedBlock + 1`，`service/onchain.go:1204`），
  recheck 循环只遍历**已存在的 booking**，registration rescan 只对**新注册地址**入队 ——
  **系统里没有任何一条路径会再看到那个区块**。用户的钱在链上，账本里不存在。

  对比同一文件里的 `processRegistrationRescan`（`service/onchain.go:592-597`）：
  ingest 失败 → `return err` → **不推进** `AdvanceRegistrationRescan`，走 retry。
  **同一个逻辑动作，两条路径失败语义相反**——这正是上一轮 scanner 那条 Major 的同形复发，
  只是这次在 service 层。

- **最小复现**：
  1. 配一个 `CreditTokens` 条目，其 `CurrencyCode` 对应的 `currencies.exponent` 小于 token 的 `Decimals`
     （例：DAI decimals=18，账本币种 exponent=6）
  2. 向注册地址转 `1.0000001` DAI
  3. watcher tick → `FetchDeposits` 返回 sighting → `CreateBooking` 因精度被拒 →
     日志 `watcher: ingest failed` → `SetCursor(to)` 成功
  4. 之后永远不会再有任何一次尝试。`ingest_dead_letters` 里也没有这一笔（不是 ErrConflict）

- **为什么现有机制没拦住**：
  - `scanChainOnce` **没有专属测试**；`fakeChainReader.FetchDeposits` 恒返回 `(nil, nil)`
    （`service/onchain_integration_test.go:107-109`），所以**整个 pull 摄入路径从来没有产生过一次 sighting**。
    删掉 `for ... IngestDeposit` 整个循环，测试全绿。
  - `core.Metrics` 里没有任何「ingest 失败」计数器（`core/metrics.go:96-131` 的 onchain 六个指标里没有）；
    `ChainCursorLag` 反而是**健康的**（游标一直在动），告警面上看不出任何异常。
  - `working-agreements.md` §3「未运行 ≠ 通过」在这里的形态是：**「没入账」和「入账了」在游标上没有区别**。

---

### [Critical] `TxLogSeq` 取决于查询传入的地址集合 —— watcher 与 registration rescan 对同一笔 tx 派生不同幂等键

- **位置**：`chains/evm/reader.go:142-155`（`seqByTx` 的赋值）
  / `service/onchain.go:588`（rescan 只传单地址）/ `service/onchain.go:1237-1242`（watcher 传全量）
  / `core/onchain.go:85-91` 与 `docs/INVARIANTS.md:904` 的 I-20 声称
- **判定**：CONFIRMED
- **失效场景**：

  `FetchDeposits` 里 `seq` 是**本次返回的日志集合内**、按 (block, logIndex) 排序后、
  同一 `TxHash` 内的序号：

  ```go
  seq := seqByTx[lg.TxHash]
  seqByTx[lg.TxHash] = seq + 1
  ```

  而返回集合已经被 `Topics[2] = shard`（= 传入的 `addresses`）过滤过。于是：

  | 调用方 | 传入 addresses | 同一笔 tx 里 B 的 Transfer 得到的 seq |
  |---|---|---|
  | `scanChainOnce`（watcher） | 全部注册地址 | 取决于同 tx 里排在它前面的其它命中数 |
  | `processRegistrationRescan` | **只有 `job.Address` 一个** | 恒为 `0` |

  幂等键 `deposit-{chain}-{tx}-{seq}`（`service/onchain.go:697-699`）因此**对同一笔链上转账不唯一**。

  具体两种结局（设 tx `0xT` 内有两条 Transfer：→addrA 100，→addrB 200，A、B 均已注册）：

  - **rescan 先到 → 双重入账（凭空多记 200）**
    1. rescan(B) 扫到 `0xT` → 只见 B 的 log → seq=0 → key `deposit-1-0xt-0` → booking(B, 200) → 确认 → **B 记 200**
    2. watcher 稍后扫到 `0xT` → 见 A@seq0、B@seq1
       - A：key `deposit-1-0xt-0` 已存在但 holder/amount 不符 → `ensureBookingMatchesInput` 返回
         `ErrConflict`（`postgres/idempotency_match.go:92-95`）→ 死信 → **A 的 100 永不入账**
       - B：key `deposit-1-0xt-1` 是新键 → 又建一个 booking(B, 200) → 确认 → **B 再记 200**
    3. 净结果：B 收到 200 却记了 400，A 收到 100 记了 0
  - **watcher 先到（常见顺序）→ 合法充值进死信**
    watcher 建 `-0`(A) / `-1`(B)；rescan(B) 派生 `-0` 与 A 冲突 → 死信 → B 需人工介入

  触发条件是「**一笔 tx 内向两个已注册地址转账**」，这是 disperse / multisend / 交易所批量提现的
  常规形态；而**充值地址是可公开推导的**（`salt = holder`，工厂地址公开，
  `core.DeriveDepositAddress`），设计文档 §5-2 自己也承认这一点，所以这不只是运气问题，
  而是可被人为构造的。

  同一根因还有第二个面：`reader.go:147` 与 `:151` 的两处 `continue`（非白名单 token、
  日志解码失败）**发生在 `seq` 递增之前**，所以「某条 log 这次解码成功、下次失败」
  也会让后续 log 的 seq 整体位移。

  第三个面：webhook 路径的 `txlog_seq` 由**外部扫描器**给出
  （`channel/onchain/evm.go:107`），它无从知道「我方注册地址集合」是什么，
  只能按自己的定义（通常是 tx 内 Transfer 全序）编号 —— 与 watcher 必然不一致。
  两条摄入路径同时启用时，同一笔转账双记账。

- **最小复现**：
  1. 注册 holder A → 地址 addrA
  2. 用一个 multisend 合约在一笔 tx 内向 addrA 转 100 USDT、向 addrB（holder B 的推导地址，尚未注册）转 200
  3. 等 watcher 游标越过该区块（此时只记了 A 的 `-0`）
  4. 注册 holder B → 入队 rescan(B) 从 `ScanStartBlock` 起
  5. rescan 扫到该区块 → seq=0 → 与 A 的键冲突 → B 进死信（若第 3 步的 watcher 落后于 rescan，
     则变成 B 双记）

- **为什么现有机制没拦住**：
  - **I-20 的声称与实现不符**。invariant 原文写的是「among the logs in that transaction that credit
    one of our registered addresses」——「our registered addresses」是个**会变的集合**，
    而 I-20 的结论「`txlog_seq` for a given transfer is stable」只在集合不变时成立。
  - **I-20 的三条 pin 没有一条经过实现**：
    `postgres.TestDepositBooking_IdempotencyKey_StableAcrossBlockNumberChurn`（store 层）、
    `service.TestOnchain_IngestDeposit_FullLifecycle`（手喂 sighting）、
    `onchain.TestEVMAdapter_ParseSighting`（webhook 解析）。
    **`chains/evm.Reader.FetchDeposits` 没有任何单测**（`chains/evm/` 下没有 `reader_test.go`），
    唯一执行它的是 `e2e_test.go`，而那个文件带 `//go:build e2e`，CI 从不跑（见下方 Major）。
    换句话说：I-20 点名「both ingestion paths must derive this from the transaction's internal
    transfer ordering」，而 watcher 这一侧的派生逻辑**零门禁**。

---

### [Major] 浅 reorg 判定基于单次 `TxIncluded=false` 直接打终态 `failed`，且幂等键让重挖后的同一笔永远无法再入账

- **位置**：`service/onchain.go:1321-1342`（判定）/ `presets/deposit.go:17`（`failed` 是终态）
  / `service/onchain.go:739-741`（终态 no-op）/ `chains/evm/reader.go:174-186`（`TxIncluded`）
- **判定**：CONFIRMED
- **失效场景**：

  ```go
  if int32(confirmations) < cfg.Confirmations {
      included, err := o.deps.Reader.TxIncluded(ctx, chainID, txHash)
      if err != nil { ...; return }          // 错误 → 安全返回
      if !included {                          // 一次 false → 立刻终态
          o.deps.Booker.Transition(..., ToStatus: "failed", ...)
      }
      return
  }
  ```

  `TxIncluded` 的实现是「`TransactionReceipt` 返回 `ethereum.NotFound` → `false`」。
  **节点落后于链尖、负载均衡后端之间视图不一致、收据被裁剪**，都会返回 `NotFound` 而非 error。
  这条分支正好只在**确认数不足**（即 tx 刚上链 1~N 个块）时才走，而这恰恰是节点间视图最容易分叉的窗口。

  一次误判的后果是**不可逆的**：
  - `DepositLifecycle.Terminal = {confirmed, failed, expired}`，`failed` 没有任何出边
  - 重挖后 watcher/webhook 再观测到同一笔 → `CreateBooking` 幂等命中 → 返回那个 **failed** booking →
    `advanceConfirmation` 的 `case "confirmed", "failed", "expired", "review": return booking, nil`
    → **静默 no-op**
  - `recheckPendingDeposits` 只列 `pending`/`confirming`，永不回访

  即：**幂等键在 reorg 重放时从保护变成了障碍**（这正是本轮契约要问的那一条）。
  没有死信、没有指标（`core/metrics.go` 里没有「浅 reorg 打 failed」的计数器）、
  没有 RUNBOOK 条目（`docs/RUNBOOK.md` §12 只讲深 reorg）。

  设计一致性问题：**深** reorg（已入账后回滚）默认 `ReorgPolicyManual`，理由写在
  `core/onchain.go:377-379`——「A false positive (RPC blip, lagging node) auto-debits the user」。
  **浅** reorg 用的是**同一类证据**（单次 `TxIncluded`），却是**自动且不可逆**的。
  同一份风险认知在两条相邻分支上得出相反结论。

- **最小复现**：
  1. 链阈值 `Confirmations = 12`，一笔充值在块 100 被观测，进入 `confirming`
  2. 第一次 recheck tick 时 RPC 负载均衡把请求打到一个落后 3 个块的后端 → 收据未找到 → `NotFound`
  3. booking 直接 `failed`（终态）
  4. 后续任何一次重新观测都被幂等键解析回这个 failed booking 并静默返回

- **为什么现有机制没拦住**：
  - **这条分支没有任何测试执行过**。唯一驱动 recheck 的
    `TestOnchain_RecheckPendingDeposits_HonorsRealBlockNumber`
    在开头就 `h.reader.setIncluded(chainID, txHash, true)`
    并注释「tx is genuinely still on-chain throughout」（`service/onchain_integration_test.go:625`）。
    删掉整个 `if !included { ... }` 块，测试全绿。
  - 设计文档 §6 line 84 确实写了「阈值前 tx 消失（浅 reorg）→ `Transition(failed)`」，
    所以这是**设计被忠实实现**——问题在于设计没有区分「tx 真的消失了」与「这个节点这一刻看不见它」。

---

### [Major] watcher 扫到链尖（`to = latest`），没有确认深度缓冲 —— reorg 换掉的块里的新充值永远看不到

- **位置**：`service/onchain.go:1213-1224`、`:1252`
- **判定**：CONFIRMED
- **失效场景**：

  ```go
  latest, _ := o.deps.Reader.LatestBlock(ctx, chainID)
  to := latest                       // 直接扫到未确认的链尖
  if to-from+1 > o.maxBlocksPerScan { to = from + o.maxBlocksPerScan - 1 }
  ...
  o.deps.Cursors.SetCursor(ctx, chainID, to)
  ```

  `cfg.Confirmations` 只用于 `advanceConfirmation` / `recheckOneDeposit` 的**入账**判定
  （grep 全仓：`Confirmations` 在扫描范围计算里零命中），扫描本身**没有任何回退深度**。

  reorg 时：块 `H` 被替换成 `H'`。游标已经过了 `H`，永不回头。
  - 原来在 `H` 里、`H'` 里也有的 tx：已经建了 booking，recheck 会处理（虽然见上一条的误判风险）
  - **只存在于 `H'` 里的 tx**（reorg 后被重新排序进来、或原本在 mempool 里被 `H'` 打包的新转账）：
    **从来没有被扫描过，也永远不会被扫描** → 用户的充值不存在于账本

  registration rescan 有同一问题（`service/onchain.go:581-597`，`to` 上限就是 `latest`），
  但它至少只跑一次；watcher 是常驻的。

- **最小复现**：
  1. watcher tick 时 `LatestBlock` 返回 1000，扫 `[900,1000]`，`SetCursor(1000)`
  2. 链发生 2 块深度的 reorg：块 999/1000 被 999'/1000' 替换
  3. 一笔向注册地址的转账只出现在 1000'（例如它在原 1000 里因 gas 排序落选）
  4. 下个 tick 从 1001 开始 → 这笔充值永远不会被 `FetchDeposits` 命中

- **为什么现有机制没拦住**：`scanChainOnce` 无专属测试（见 Critical #1）；
  `fakeChainReader.FetchDeposits` 恒空，所以扫描区间的选择从来没有被断言过。
  `docs/RUNBOOK.md` 与 `docs/INVARIANTS.md` 都没有关于「扫描回退深度」的任何声称。

---

### [Major] `SweepPolicy.GasCeiling` 的字段文档单位是 wei，实现比较的是 gwei —— 按字段文档配置的消费方，闸门被抬高 10^9 倍

- **位置**：`core/onchain.go:334`（`// GasCeiling is the max gas price (wei) ...`）
  vs `chains/evm/sweeper.go:29-30`（`weiGweiDecimals = 9`）、`:93-107`（`GasPrice` 返回 gwei）
  、`core/interfaces.go:584`（`// GasPrice returns the current gas price (gwei)`）
- **判定**：CONFIRMED
- **失效场景**：

  消费方配置 `SweepPolicy` 时读的是**该字段自己的文档注释**（`core/onchain.go:334`），
  它说 wei。于是写 `GasCeiling: decimal.RequireFromString("50000000000")`（= 50 gwei，按 wei 计）。
  实际比较（`service/onchain.go:1459`）：

  ```go
  gasPrice := o.deps.Sweeper.GasPrice(...)      // normalizeAmount(feeCapBasis, 9) → gwei，例如 "80"
  if gasPrice.GreaterThan(policy.GasCeiling) { skip }   // 80 > 5e10 恒为 false
  ```

  → **gas 闸门永不触发**，任何 gas 价格下都会广播归集，包括极端尖峰。
  这不只是文档瑕疵：`GasCeiling` 是这条路径上**唯一**限制花多少 gas 的闸，
  上一轮那条 Minor 修好了「校验量 ≠ 实付量」，却留下了「校验量的单位 ≠ 文档承诺的单位」。

  `SweepPolicy.Validate()`（`core/onchain.go:345-365`）只检查非负，没有任何量级合理性检查，
  所以 10^9 倍的配置错误在启动时完全静默。

- **最小复现**：读 `core/onchain.go:334` 配置 `GasCeiling` = 50e9，跑 `RunSweepOnce`，
  在 `GasPrice` 返回任何值（gwei 量级）时 `gasPrice.GreaterThan(policy.GasCeiling)` 恒 false。

- **为什么现有机制没拦住**：所有 sweep 测试都用 `GasCeiling: decimal.NewFromInt(100)`
  加 `h.sweeper.gasPrice` 的手工值（`service/onchain_integration_test.go:686-693` 等），
  两侧同单位，测不出单位错配；`chains/evm/sweeper_test.go` 只测 `quoteFee`，
  **没有一条断言 `GasPrice` 的返回单位**。

---

### [Major] gas-bump 的 stuck 计时器从不重置 —— 到点之后每个 sweep tick 都 bump 一次，重试预算按 `Interval` 而非 `sweepStuckAfter` 消耗

- **位置**：`service/onchain.go:1826-1829`（判定）/ `:1862-1867`（bump 后不 transition）
  / `service/onchain.go:280-283`（`WithSweepStuckAfter` 的文档承诺）
- **判定**：CONFIRMED
- **失效场景**：

  ```go
  if time.Since(b.UpdatedAt) < o.sweepStuckAfter { return nil }
  ...
  newTxHash, _ := o.deps.Sweeper.BatchSweep(..., txHash)
  o.trackSweepTx(b.UID, newTxHash)
  o.bumpSweep(b.UID)                 // 没有任何 Transition → bookings.updated_at 不变
  ```

  `b.UpdatedAt` 只在 `Booker.Transition` 时更新，而 gas-bump **不做 transition**
  （注释 `service/onchain.go:1794-1799` 明确说 ChannelRef 不能改）。
  所以一旦 `time.Since(b.UpdatedAt)` 跨过 `sweepStuckAfter`，它**永远**跨过 ——
  之后**每一个 sweep tick 都会 bump 一次**。

  默认 `sweepStuckAfter = 5m`、`maxSweepBumps = 5`。若消费方把 `policy.Interval` 配成 1 分钟：
  - 预期（按 `WithSweepStuckAfter` 的措辞「how long a sent booking waits ... before a gas-bump
    retry is attempted」）：第 5、10、15、20、25 分钟各一次 → 约 30 分钟的 ride-out
  - 实际：第 5、6、7、8、9 分钟连着 5 次 → **第 9 分钟就打 `failed`**

  每次 bump 都是在链上真实费用之上再 +12.5%（`chains/evm/sweeper.go:294-300` 的 `bumpFee`
  以 `priorFeeFloor` 读回的链上实付为基准），所以这不只是提前放弃，还是
  **在 gas 尖峰期以 1.125^n 的速度往上抬价**——而 gas 尖峰正是归集卡住的典型原因。
  `recheckSweepSent` 里的 `GasCeiling` 检查（`:1849`）比较的是**市价**，
  拦不住 `prior * 1.125` 这一支（这是上一轮 Minor 的残留面）。

- **最小复现**：`sweepStuckAfter=5m`、`maxSweepBumps=5`、`policy.Interval=1m`，
  让一笔归集卡在 mempool：第 5..9 分钟连续 5 次 `BatchSweep` 重播，第 10 分钟 booking → `failed`。

- **为什么现有机制没拦住**：**gas-bump 分支没有任何测试执行过**。
  两个 revival 测试都用 `service.WithMaxSweepBumps(0)`
  （`service/onchain_integration_test.go:744,827`），让 `bumps >= maxSweepBumps` 在第一次
  就成立，**直接跳到 failed，从不进入 bump 分支**；
  `TestOnchain_Sweep_NonceReuseAndNoJournal` 用默认 5 分钟 `sweepStuckAfter`，
  `time.Since(UpdatedAt)` 是秒级，**在第一个 if 就返回**（该测试自己断言
  `BatchSweep must be called exactly once`）。
  `RUNBOOK.md` §15 的「Residual limitation」只披露了「重启会重置 bump 计数」，
  没有提到「同一进程内计时器不重置」这条独立的、更容易触发的路径。

---

### [Major] 广播成功但 `Transition(sent)` 失败 → tx hash 无处可寻，nonce 被占，该链归集停摆

- **位置**：`service/onchain.go:1769-1783`
- **判定**：CONFIRMED
- **失效场景**：

  ```go
  txHash, err := o.deps.Sweeper.BatchSweep(ctx, chainID, token, targets, nonce, "")   // 已广播上链
  if err != nil { return ... }
  if _, err := o.deps.Booker.Transition(ctx, ...ToStatus:"sent", ChannelRef: txHash...); err != nil {
      return fmt.Errorf("sweep: transition sent: %w", err)                            // hash 就此丢失
  }
  ```

  广播**先于**持久化。DB 在这两行之间不可用（连接池耗尽、failover、进程被 kill）时：
  - 交易已经在 mempool / 已上链，钱可能已经到国库
  - booking 停在 `pending`，`ChannelRef` 为空，`o.sweepTx` 内存映射也没写
  - 下个 tick：`findInFlightSweep` 找到这个 `pending` booking → `advanceSweep` 的 `case "pending"`
    → 再次 `BatchSweep(..., nonce, "")`（`priorTxHash` 传空！）
    - 若上一笔已上链：节点回 `nonce too low` → 每个 tick 都失败 → **该链归集永久停摆**
      （`findInFlightSweep` 一直返回这个 pending booking，新的归集永远建不出来）
    - 若上一笔仍 pending：`priorTxHash=""` → `priorFeeFloor` 退回内存 `lastFee`；
      进程若重启过则内存也空 → 按市价出价 → 正是上一轮那条 Major 描述的 underpriced 失败模式
      （修复只覆盖了 `recheckSweepSent` 那条调用链，**这条 `pending` 重播路径没有 priorTxHash 可传**）

  全仓没有任何按 nonce 反查签名者已发交易的能力（grep `TransactionByHash` 只出现在
  `chains/evm/sweeper.go` 的 `priorFeeFloor`，不接受 nonce 查询），所以系统无法自愈。

- **最小复现**：在 `BatchSweep` 返回后、`Transition` 之前杀掉进程（或让该 Transition 的 DB 写失败），
  重启后观察 sweep booking 停在 `pending` 且 `channel_ref` 为空，
  之后每个 tick 都在同一 nonce 上重播。

- **为什么现有机制没拦住**：无测试覆盖「广播成功 + 持久化失败」；
  `fakeSweeper.BatchSweep` 永不失败、`Transition` 也永不失败。
  `RUNBOOK.md` §15 的处置流程假设 booking 处于 `sent` 且 `channel_ref` 有值。

---

### [Major] token config 缺失时 `tc` 零值静默关掉 AutoCreditCeiling 与对账两道闸

- **位置**：`service/onchain.go:759-771`
- **判定**：CONFIRMED
- **失效场景**：

  ```go
  tc := cfg.CreditTokens[booking.Metadata["token"]]     // 找不到 → 零值 TokenConfig
  reason, err := o.reviewGate(ctx, booking, tc)
  ```

  `reviewGate`（`:834-840`）对零值 `tc` 的行为是：
  `tc.AutoCreditCeiling.IsPositive()` 为 false → 跳过上限闸；
  `tc.ReconcileCeiling.IsPositive()` 为 false → 跳过对账闸 → **返回 `""`，直接入账任意金额**。
  注释把这写成 pre-M3 行为，但它与 `validateAutoCreditCeilings`
  （`service/onchain.go:428-437`）的立论直接冲突 —— 那个校验存在的全部理由就是
  「零值不等于 unbounded，静默默认正是 M3 要关掉的信任模型」。

  可达路径：token 从 `CreditTokens` 下架（延迟上币、换合约地址、配置回滚）之后，
  **已经在 `confirming` 的历史 booking 仍会被 `recheckOneDeposit` → `advanceConfirmation` 驱动到
  `confirmed`**（`IngestDeposit` 的 allowlist 早退只挡新 sighting，`service/onchain.go:619-623`），
  此时两道闸都消失。启动校验只看**当前** `CreditTokens` 里的条目，管不到「曾经在、现在不在」的那些。

- **最小复现**：
  1. 配 `CreditTokens[T] = {AutoCreditCeiling: 300}`，摄入一笔 10000 的充值 → `confirming`
  2. 重启前把 T 从 `CreditTokens` 移除（`validateAutoCreditCeilings` 通过，因为 T 已不在 map 里）
  3. recheck tick 达到确认阈值 → `tc` 零值 → `reviewGate` 返回 `""` → **10000 直接入账，不进 review**

- **为什么现有机制没拦住**：`TestOnchain_Run_RejectsUnconfiguredAutoCreditCeiling` 等四条测试
  只覆盖**启动时 map 里有条目但字段没配**，没有一条覆盖**运行时 map 里查不到**。
  I-34 的措辞也只覆盖 review 的解析权限与第二源不可达，没有覆盖这个零值出口。

---

### [Major] token decimals 只来自配置，从不与链上 `decimals()` 交叉校验

- **位置**：`chains/evm/reader.go:145,163`（`tokenCfg.Decimals` → `normalizeAmount`）
  / `chains/evm/scanner.go:71-84`（`tokenDecimals` 同样只读配置）/ `core/onchain.go:198-200`
- **判定**：CONFIRMED
- **失效场景**：

  `normalizeAmount(raw, decimals) = decimal.NewFromBigInt(raw, -decimals)`。
  `decimals` 100% 来自 `core.TokenConfig.Decimals`，由消费方在 composition root 手填。
  全仓 grep `"decimals"` 的 ABI 调用：**零命中** —— 从不向 token 合约求证。

  配错一位（USDT 写成 18 而不是 6）→ 每笔充值按 `10^-12` 记账，用户实际充 1000 USDT 记成
  0.000000000001；反向配错（18 位 token 写成 6）→ 记成 10^12 倍，**凭空铸币**，
  且 `AutoCreditCeiling` 是**按记账后的金额**比较的（`reviewGate:835`），
  所以偏大方向会被 review 闸拦下，**偏小方向（少记）不会有任何信号**。

  同一个 `Decimals` 还驱动 sweep 侧的余额判定（`Scanner.tokenDecimals`），
  配小了会让所有地址的余额看起来低于 `MinThreshold` → 归集静默停止。

  `core.ChainConfig` / `core.TokenConfig` **没有 `Validate()`**，
  连 `Decimals >= 0` 都不检查（负值会让 `NewFromBigInt(raw, +n)` 放大金额）。

- **判定说明**：「配置错了会算错钱」本身是消费方责任；这条报为 Major 的理由是
  **系统里存在一个廉价的、纯只读的交叉校验（启动时对每个 CreditTokens 条目调一次 `decimals()`）
  却没有做**，而这个值直接决定入账数量级，且错误方向之一完全无声。

- **为什么现有机制没拦住**：`TestScanner_TokenDecimals`（`chains/evm/scanner_test.go:16`）
  只断言「从配置里查得到」，正是在为这个未校验的来源背书。

---

### [Major] manual 策略下的深 reorg 告警会在 `reorgRecheckWindow` 之后自行静默，系统里不留任何持久痕迹

- **位置**：`service/onchain.go:1390-1391`（窗口早退）/ `:1404-1411`（manual 只记日志+计数器）
- **判定**：CONFIRMED
- **失效场景**：

  ```go
  if latest-blockNumber > o.reorgRecheckWindow { return }   // 默认 500 块
  ...
  if o.reorgPolicy != core.ReorgPolicyAutoReverse { return } // manual：只 Warn + 计数器
  ```

  `manual`（默认）下的处置是：`log().Warn` + `DepositReorgDetected(chainID)` 计数器，
  **不写 booking metadata、不写事件、不写死信**。booking 仍是 `confirmed`，journal 仍在，余额仍在。

  告警会在每个 `reorgRecheckInterval`（默认 5m）重复，直到 `latest - blockNumber > 500`。
  在 2 秒出块的链上，**500 块 ≈ 17 分钟**——比一个 on-call 的响应时间还短。
  窗口一过，`recheckOneConfirmedDeposit` 直接早退，告警停止，
  而系统里**唯一还留着这件事发生过的证据是 Prometheus counter 的历史值和日志保留期**。
  `RUNBOOK.md` §12 要求 on-call「用第二个源核实后再决定是否冲销」——
  等他核实完，能告诉他「是哪一笔」的信号已经没了。

- **为什么现有机制没拦住**：**深 reorg 路径零测试**——
  grep `service/*_test.go` 的 `[Rr]eorg` 只命中一处注释（`service/onchain_integration_test.go:1318`）。
  `recheckConfirmedDeposits` / `recheckOneConfirmedDeposit` / `handleReorg`
  （含 `ReorgPolicyAutoReverse` 那条**会自动扣用户余额**的分支）**没有任何测试执行过**。

---

### [Major] `chains/evm` 的真链语义只由一个 CI 从不执行的 e2e 测试覆盖

- **位置**：`chains/evm/e2e_test.go:1`（`//go:build e2e`）/ `.github/workflows/ci.yml:68-71`
- **判定**：CONFIRMED（已实跑核实）
- **失效场景**：

  CI 的 `Test chains/evm` 步骤是 `go test -race -timeout 5m -count=1 ./...`，
  **没有 `-tags e2e`**，lint / vet 步骤同样没有。于是 `TestE2E_WatchThenSweep`
  ——唯一验证以下事实的测试——从不在任何自动化里跑：
  - `core.DeriveDepositAddress` 与真实 `DepositFactory` 部署出的地址一致
  - `packBatchSweep` 的 ABI 与真实合约的 `batchSweep(uint256[],address)` 一致
  - `Reader.FetchDeposits` 能从真实 `eth_getLogs` 结果里正确解出 sighting（含 `TxLogSeq`）
  - `Sweeper.BatchSweep` 的签名 → 广播 → 上链链路成立

  上一轮报告自己写过「`DepositFactory` 合约本身没读……链上强制假设未验证」；
  一年后这个假设仍然只由一个不跑的测试守着。
  `.github/workflows/ci.yml:65-67` 的注释说「Without these steps chains/evm and anchors/r2 ship with
  tests that CI never runs, which is the shape this repo keeps paying for」——
  **同一个形态在同一个文件里还剩一半没修**。

  我实跑了 `go vet -tags e2e ./...`（`chains/evm` 目录，exit 0），确认代码仍可编译，
  即它不是坏掉的死代码，只是没人跑。

- **为什么现有机制没拦住**：没有任何门禁断言「带 build tag 的测试文件必须在某处被执行」。

---

### [Minor] reader 里两处静默 `continue`，畸形日志与非白名单 token 的丢弃没有任何信号

- **位置**：`chains/evm/reader.go:147`、`:151`
- **判定**：CONFIRMED
- 说明：`decodeTransferLog` 失败（topics 数不对、data 长度不对）→ `continue`，
  没有日志、没有计数器。`working-agreements.md` §3：「降级必须落痕」。
  这两处 `continue` 还发生在 `seq` 递增之前，因此同时是上面那条 Critical 的一个额外抖动源。

### [Minor] `chain_cursors` 的单调性声称在 service/ 层并不存在，watcher 循环也没有单飞锁

- **位置**：`postgres/sql/queries/chain_cursors.sql:5-9`（「that is an orchestration-layer invariant
  (service/)」）vs `service/onchain.go:1926-1935`（watcher 循环无 `NewLockedJob`，
  对比 `:1946-1957` 的 sweep 有）
- **判定**：CONFIRMED
- 说明：多副本部署下每个副本独立跑 `scanChainOnce`，`SetCursor` 写的是各自节点看到的 `latest`，
  可以倒退。倒退本身在这里是良性的（重扫走幂等），但 **SQL 注释断言的那条「由 service/ 保证」的
  不变式在代码里不存在**——文档承诺了一个没人实现的保证。
  另有 N 倍 RPC / DB 成本与 N 路幂等争用（并发正确性交 territory B）。

### [Minor] `probeMulticall` 的探测结果永久缓存

- **位置**：`chains/evm/scanner.go:86-101`
- **判定**：CONFIRMED
- 说明：`multicallKnown[chainID]` 一旦写入永不失效。若探测时节点在特定区块上返回空 code
  （状态未同步、归档节点故障），该链会**在整个进程生命周期内**退化到 N 次单查路径。
  只影响性能与 RPC 配额，不影响正确性（m-10 之后两条路径语义已一致）。

### [Minor] `TxIncluded` 用 `err == ethereum.NotFound` 而非 `errors.Is`

- **位置**：`chains/evm/reader.go:180`
- **判定**：CONFIRMED
- 说明：当前 `ethclient` 直接返回该哨兵值所以成立；一旦上游包装（换 transport、加中间件），
  比较失败会让「未找到」落进 error 分支。就本仓而言这个方向反而是**更安全**的
  （error → 不打 failed），所以只是 Minor；但它与 Critical #3 的判定强耦合，值得一并收口。

### [Minor] legacy `ParseCallback` webhook 路径可把 deposit booking 推到 `confirmed` 而不出 journal

- **位置**：`server/handler_webhooks.go:104-121`（`ToStatus: core.Status(payload.Status)`）
- **判定**：PLAUSIBLE（前置条件：适配器**不**实现 `ParseSighting`）
- 说明：本仓唯一的 onchain 适配器 `channel/onchain.EVMAdapter` 实现了 `ParseSighting`，
  所以会被 `handler_webhooks.go:79` 路由走，这条路径**用本仓代码无法到达**。
  但消费方自带的 channel adapter 若只实现 `channel.Adapter`，一个 `status:"confirmed"`
  的回调就能把 `confirming` 的 deposit 推到终态 `confirmed`——**不出 journal**，
  之后 `advanceConfirmation` 对终态 no-op，这笔充值永远不会被记账。
  没有任何 invariant 声称「`confirmed` 的 deposit booking 必须有 journal」。

### [Minor] `time.Since(b.UpdatedAt)` 混用进程本地时钟与 DB 时钟

- **位置**：`service/onchain.go:1826`
- **判定**：CONFIRMED
- 说明：`UpdatedAt` 由 Postgres `now()` 写入，`time.Since` 用本地 wall clock。
  容器时钟偏移几分钟就会让 stuck 判定提前或延后同样的量。与上面那条 Major 叠加。

---

## 上轮修复复核

上一轮（`docs/audits/2026-08-25-financial-engineering/onchain-money-path.md`）3 条：
2 Major + 1 Minor（不是 7 条，7 是加上 `operability.md` 里同批的那几条）。逐条核：

| 上轮发现 | 现状 | 是否接到真实路径 | pin 能否证伪 |
|---|---|---|---|
| **Major 1** 归集重试状态只在内存，重启后卡单无法替换 | **已修**：`chains/evm/sweeper.go:236-275` 新增 `priorFeeFloor`，优先 `TransactionByHash` 读链上实付 fee；`core.Sweeper.BatchSweep` 签名加 `priorTxHash` | ✔ 服务侧 `recheckSweepSent:1862` 确实传了 `txHash`（内存映射优先、退回持久 `ChannelRef`） | ⚠️ **只覆盖一半**，见下 |
| **Major 2** scanner 两条路径失败语义相反（multicall fail-open / 并发 fail-closed） | **已修**：`ScanBalances` 改为返回 `(balances, unreadable, err)`，两条路径都做 per-address fail-closed（`scanner.go:168-183`、`:241-256`）；`decodeERC20BalanceOf` 也不再把畸形返回当零 | ✔ `sweepTick:1503-1515` 真的消费 `unreadable`：Warn + `SweepAddressUnreadable` 指标；`observability/prometheus.go:264-268` 有对应 counter | ✔ `TestMulticallResultsToBalances_FailsClosedPerAddress`、`TestDecodeERC20BalanceOf_FailsClosedOnMalformedReturn`、`TestOnchain_Sweep_UnreadableAddressDoesNotBlockReadableAddresses` 三条，删掉 `unreadable` 分支会红 |
| **Minor 3** GasCeiling 校验量 ≠ 实付量 | **已修**：`GasPrice` 改用 `feeCapBasis(baseFee, tip) = 2*baseFee+tip`，与 `quoteFee` 共用同一函数（`sweeper.go:112-116`） | ✔ | ⚠️ 无测试断言 `GasPrice` 的返回值/单位；**且留下了新的单位矛盾**（见 Major「GasCeiling 文档说 wei」）与**重试路径不受 ceiling 约束**的残留面（见 Major「stuck 计时器不重置」） |

**Major 1 修复的 pin 缺口（新发现，本身按 Major 计）**：

- `chains/evm/sweeper_test.go` 的三条测试（`TestSweeper_QuoteFee_*`）用 `quoteFeeClient` 假件
  **直接调 `quoteFee`**，覆盖的是纯函数那一段。
- **服务侧那一半没有任何 pin**：`service/onchain_integration_test.go:161-167` 的
  `fakeBatchSweepCall` **记录了 `priorTxHash` 字段，但全文件没有一处断言它**
  （grep `priorTxHash`：只有 struct 定义、参数、append 三处）。
- 更根本的是：**gas-bump 分支从未被任何测试执行**。两条 revival 测试用
  `service.WithMaxSweepBumps(0)`（`:744`、`:827`）让 `bumps >= maxSweepBumps` 在第一次判定就成立，
  直接跳到 `failed`；`TestOnchain_Sweep_NonceReuseAndNoJournal` 用默认 `sweepStuckAfter=5m`
  而 `UpdatedAt` 是秒级，**在第一个 `if` 就 return**，并且自己断言
  `BatchSweep must be called exactly once`。
- **可证伪推演**：把 `recheckSweepSent:1862` 的 `txHash` 改成 `""`
  （即完全退回上一轮那个 bug），`go test ./service/... ./chains/evm/...` **全绿**。
- 一次覆盖率实跑（`go test ./service/ -run TestOnchain -coverpkg=.../service`）给出
  `recheckSweepSent 47.1%`、`handleReorg 0.0%`、`processRegistrationRescan 0.0%`。
  ⚠️ 该次运行在 600s 处被 `go test` 的包级超时打断（testcontainers 逐测试起容器），
  所以这些数字只是**下界**；`handleReorg` / `processRegistrationRescan` 的 0% 已由 grep
  独立确认（无任何测试引用）。

**`9268f17`「eleven Minor」中落在 `chains/evm` 的部分**：本轮核到的相关收口有
`normalizeTokenKey` 的统一小写、`decodeTransferLog` 的非 panic 化、`ClientSet` 的
「部分配置直接拒绝」——三条都成立且在真实路径上。

**nonce prune 失败 500（已修）在别处有没有兄弟**：有一个同形态但方向相反的。
那条的形态是「非致命错误升级成整请求失败」；本 territory 的同形态是
**「致命错误降级成一行日志」**——`scanChainOnce:1246-1250` 的 ingest 失败（本报告 Critical #1）。
两者都是「错误的严重度判断」，只是这次判轻了，而判轻的一侧代价是丢钱。

**上一轮「我没能验证的」三条的当前状态**：
- `DepositFactory` 合约仍未读（不在本仓），「归集只能到国库」的链上强制假设**仍未验证**，
  且唯一会接触真实合约字节码的 e2e 测试 CI 不跑（见 Major）。
- `scanner.go` / `logs.go` 的 reorg 处理：本轮读完了，产出 3 条（Critical #2、Major 浅 reorg、
  Major 扫到链尖）。
- multicall.go：上轮已验，本轮确认修复成立。

---

## 移交

- **→ audit-concurrency (B)**：watcher 循环无单飞锁（`service/onchain.go:1926-1935`），
  多副本同时对同一 sighting 调 `CreateBooking` 的幂等争用是否安全（advisory lock 键空间）。
- **→ audit-concurrency (B)**：`runRegistrationRescansOnce` 的 lease（`registrationRescanTimeout`）
  与每个 job 的 `context.WithTimeout` **取同一个值**（`service/onchain.go:543,554`），
  租约到期与处理超时同时发生的边界；`attempts` 从不在成功后重置，导致
  `time.Second << min(attempts,6)` 的退避随扫描进度单调增长。
- **→ audit-threat (D)**：sweeper 私钥经 `chains/evm/signer.go:36` 的 `crypto.HexToECDSA`
  从 hex 字符串加载，与 `DATABASE_URL` 是否同一 secrets bundle（`custody.md` scoped 委托，
  上一轮同一条移交至今未见结论）。
- **→ audit-threat (D)**：webhook HMAC 用 `hmac.Equal` 比较 **hex 字符串**
  （`channel/onchain/evm.go:88-92`），大小写不同的合法签名会被拒（可用性面，非安全面）。
- **→ audit-financial (A)**：`normalizeAmount` 之后的 `checkAmountPrecision`
  （`postgres/booking_store.go:124`）会把「token decimals > currency exponent」的合法充值
  直接拒绝——这属于精度策略，但它的失败**在本 territory 变成了丢钱**（Critical #1）。
- **→ audit-operability (I)**：`core.Metrics` 缺三个信号——watcher ingest 失败、
  浅 reorg 打 failed、sweep 卡单时长/pending nonce 龄；
  `RUNBOOK.md` §12 的深 reorg 处置流程与 `reorgRecheckWindow=500` 的告警自动静默相互矛盾。
- **→ audit-test-credibility (F)**：`fakeChainReader.FetchDeposits` 恒返回 `(nil, nil)`
  （`service/onchain_integration_test.go:107-109`），使整个 pull 摄入路径不可被测试触达；
  `fakeBatchSweepCall.priorTxHash` 记录而不断言。

---

## 我没能验证的

- **`DepositFactory.sol` 本身**（不在本仓）。`BatchSweep` 的目标地址重派生校验
  （`chains/evm/sweeper.go:164-173`）在**本地**成立，但「工厂只会把资金送到国库、
  sweeper key 无法改目的地」这个整个 scoped-委托风险模型赖以成立的链上强制**仍未验证**。
  与上一轮同一条，状态未变。
- **anvil e2e 未实跑**。我只跑了 `go vet -tags e2e`（编译通过），没有起 anvil 跑
  `TestE2E_WatchThenSweep`，所以「它现在还是绿的」我不知道——只知道 CI 不跑它。
- **`service/` 全量测试未跑完**：`go test ./service/ -run TestOnchain` 在 600s 处被包级超时打断
  （每个测试起一个 Postgres 容器）。上面引用的覆盖率数字是**下界**，
  凡以覆盖率为唯一依据的结论我都另外用 grep 独立确认过（`handleReorg`、
  `processRegistrationRescan`、gas-bump 分支）。
- **Critical #2 的双重入账变体没有实跑复现**。两种结局（双记 / 死信）都是从
  `reader.go` 的 seq 赋值 + `service/onchain.go:588` 的单地址查询 + `idempotency_match.go:92`
  的冲突判定**读**出来的，逻辑链完整但没有构造真实的两地址同 tx 场景跑一遍。
  先后顺序哪个更常见（决定是「双记」还是「死信」）我也没有实测数据。
- **RPC provider 在超限时返回空数组而非报错**这一行为我没有对具体 provider 求证。
  若成立，它会与 Critical #1 叠加成「静默跳过整个区块区间」；若不成立（provider 一律报错），
  那条路径是 fail-closed 的。标 PLAUSIBLE，未验。
- **`postgres` / `server` 层的 deposit 查询端点**没有逐个读（不属本 territory），
  所以「除 webhook 与 watcher 外还有没有第三个入口能建 deposit booking」我只做了
  `grep DepositClassificationCode` 级别的确认，没有逐端点核。

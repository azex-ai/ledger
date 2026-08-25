# 审计报告 — onchain money path（链上资金路径）

> Territory H，Team Lead 自审。契约 §2 的六格没有一格明确拥有 `chains/evm`（独立 Go module，
> 1079 行，经手真钱：CREATE2 派生、扫链、**归集签名与广播**）。territory A 管账本内的钱，
> D 管安全边界，但**「归集交易本身会不会卡死、会不会超付」没人管**。补上。

## 摘要

两条 Major、一条 Minor。`chains/evm/sweeper.go` 本身写得很好 —— 签名前重新派生并逐个校验目标地址、
fee bump 向上取整、`EstimateGas` 的错误不被静默替换。问题不在这份代码里，而在**它依赖的
两份状态都只活在进程内存里**：gas-bump 计数与上次使用的 fee。

后果是：**归集卡单 + 进程重启 = 该链的归集管线永久堵死**，且系统在结构上失去了自我修复的能力
（它不知道卡住那笔的 fee 是多少，也从不去链上读）。而 EVM 的 nonce 顺序性意味着一笔卡单
挡住其后全部归集 —— 钱会持续到账到充值地址，但**再也归集不出来**。

另有一条 Minor：gas ceiling 校验的量与实际支付的量不是同一个。

## 发现

### [Major] 归集的重试状态只在内存，重启后卡单无法被替换，堵死整条链的归集

- **位置**：`service/onchain.go:329`（`sweepBump` 内存 map）/ `chains/evm/sweeper.go:45`
  （`lastFee` 内存 map）/ `service/onchain.go:1733-1765`（重试路径）
- **判定**：CONFIRMED
- **失效场景**：

  归集卡单的重试机制是「同 nonce 重发、fee 至少上浮 12.5%」，这是正确的 EVM 做法。
  它依赖两份状态：

  | 状态 | 位置 | 持久化 |
  |---|---|---|
  | 已 bump 次数（上限 5） | `o.sweepBump[bookingUID]`，内存 map | ❌ 无（schema 里 grep 不到任何 sweep bump 列） |
  | 上次使用的 fee | `s.lastFee[chainID][nonce]`，内存 map | ❌ 无 |

  **进程重启后**：
  1. bump 计数归零 → 「超过最大重试、放弃并转卡单状态」这个终止条件**跨重启永远到不了**，
     若进程周期性重启，同一笔会被无限 bump
  2. `lastFee` 丢失 → `quoteFee` 找不到 prior，返回**当前市价** `2*baseFee + tip`，**不做 bump**
  3. 若卡单是在 gas 尖峰时发出的（**而这正是归集会卡住的典型原因**，两个条件高度相关而非独立），
     新 fee 会**低于**链上那笔 pending tx → 节点以 `replacement transaction underpriced` 拒绝
  4. 该 nonce 被永久占住。`NextNonce` 用 `PendingNonceAt`，会正确返回 nonce+1，
     所以后续归集能被签出去 —— 但**EVM 的 nonce 必须顺序执行**，nonce+1 在 nonce 落块前
     不可能被打包。**一笔卡单堵住其后全部归集。**

  **系统在结构上无法自愈**：全仓 grep `TransactionByHash` / `GasFeeCap()` / `GasTipCap()`
  在 `service/` 与 `chains/` 下**零命中** —— 代码从不去链上读那笔 pending tx 的实际 fee，
  所以重启后它没有任何途径重建出一个足够高的替换价。

  资金后果：充值仍会继续到账到各个 CREATE2 地址（那条路不经过 sweeper），
  但**钱归集不到国库**。是资金滞留，不是资金丢失 —— 但滞留时长取决于有没有人发现。

- **最小复现**：
  1. gas 尖峰期触发一次归集，让它卡在 mempool
  2. 重启 ledgerd
  3. 等重试循环触发 → `sweep: gas-bump rebroadcast: ... underpriced`
  4. 该链后续所有归集签出后无法上链

- **为什么现有机制没拦住**：
  - **没有告警**。`ledger_sweep_unattributed_total` 等六个链上指标零告警零 RUNBOOK
    （见 `operability.md` 同名发现），而卡死状态本身**连指标都没有** ——
    没有「归集卡了多久」「pending nonce 龄」这类可观测量
  - `docs/RUNBOOK.md` 里 grep 不到任何 sweep 相关条目，on-call 手上没有处置流程
  - 测试跑在内存内、单进程、无重启，**重启这个维度不在任何测试的假设空间里**

- **可选修法**（交 Aaron 拍板，不在本次范围）：把 bump 次数与上次 fee 落进 `bookings.metadata`
  或专门的列；或在重试前用 `TransactionByHash` 读回 pending tx 的实际 fee 作为 bump 基准
  （后者更稳，因为它以链上事实为准而不是以本地记忆为准）。

### [Major] 余额扫描的两条路径失败语义相反，走哪条取决于链上碰巧有没有 Multicall3

- **位置**：`chains/evm/scanner.go:145`（multicall 路径）vs `chains/evm/scanner.go:180,189`
  （并发回退路径）
- **判定**：CONFIRMED
- **失效场景**：

  「读这批地址的余额」这一个逻辑操作有两份实现，scanner 在运行时探测规范地址
  `0xcA11...CA11` 上有没有代码来决定用哪份。**两份的失败语义是相反的**：

  | 路径 | 单个地址读失败时 | 语义 |
  |---|---|---|
  | `scanViaMulticall` | `balances[addr] = decimal.Zero` 然后 continue | **fail-open** |
  | `scanConcurrently` | `return fmt.Errorf(...)`，整轮扫描中止 | **fail-closed** |

  multicall 路径的注释写明了这是有意的：
  `// untrusted RPC/target: treat unreadable balance as zero, not a hard failure`。
  但对归集而言，**「读不出来」和「真的是零」的后果完全相同：这个地址不被归集**，
  而且**没有错误、没有指标、没有日志区分这两种情况**。

  这意味着：
  - 在**有** Multicall3 的链上（几乎所有主流 EVM 链），一次 RPC 抖动 / 限流 /
    某个 token 合约对特定地址 revert，都会让那个地址的存款**静默地不被归集**
  - 在**没有** Multicall3 的链上，同样的失败会让整轮扫描响亮失败并被重试
  - **同一份部署在不同链上行为相反，而这个差异不在任何配置里** ——
    它取决于一次运行时探测

  若失败是瞬时的，下一轮扫描会捡回来；若是持久的（token 合约对该地址持续 revert、
  节点持续故障），**资金永久不可见且无任何信号**。

  `scanConcurrently` 内部还自相矛盾：RPC 错误 → 返回 error（fail-closed），
  但 `len(out) != 32` 的畸形返回 → `raw = big.NewInt(0)`（fail-open，`scanner.go:194`）。

- **最小复现**：在一条有 Multicall3 的链上，让某个 target 的 `balanceOf` 调用失败
  （token 合约 revert 或节点对该 call 返回错误）→ `aggregate3` 的该项 `Success=false`
  → 余额记为 0 → 该地址不进 sweep targets → 无任何输出表明发生过这件事。

- **为什么现有机制没拦住**：
  - 这是**跨路径的语义不一致**，任何单路径测试都看不见它 —— 要发现它必须同时读两份实现
    并比较，而没有测试同时驱动这两条路径并断言它们等价
  - 契约 §1.4 的形态：两个测试各自都对，但**没有测试问「这两条路径在失败时表现一致吗」**
  - 「未运行 ≠ 通过」在金额读取上的形态是「**读不到 ≠ 是零**」

- **可选修法**（交 Aaron 拍板）：两条路径都改为把「读不到」表达为第三种状态
  （而非零），由调用方决定跳过还是重试；至少要为不可读计数打一个指标，
  让静默变成可见。

### [Minor] gas ceiling 校验的量与实际支付的量不是同一个

- **位置**：`service/onchain.go:1422` / `:1752`（校验）vs `chains/evm/sweeper.go:203-207`（实付）
- **判定**：CONFIRMED
- **失效场景**：
  - 校验的是 `Sweeper.GasPrice()` = `SuggestGasPrice()`（在 EIP-1559 链上通常约等于 `baseFee + tip`）
  - 实际交易用的是 `feeCap = 2*baseFee + tip`，注释称「a conventional headroom so the tx stays
    includable for a couple of blocks of base fee movement」——**理由正当**，但它意味着
    `GasCeiling` 并不约束实付上限，实付可达约定值的两倍
  - 重试路径叠加：`max(市价, 上次 × 1.125)`，其中 `上次 × 1.125` 与市价无关，逐次累乘。
    5 次上限下最坏约 1.8 倍，**有界**，所以是 Minor 不是 Major
- **为什么没拦住**：`GasCeiling` 的字段注释写的是「max gas price (wei) the sweep job will pay」
  （`core/onchain.go:321`）—— **文档承诺的是上限，实现给的是软阈值**。二者不一致，
  且没有任何测试断言实付 ≤ ceiling。

## 移交

- **→ audit-threat**：sweeper 的私钥经 `crypto.HexToECDSA` 从 hex 字符串加载
  （`chains/evm/signer.go:36`）。这把 key 与 `DATABASE_URL` 是否处于同一 secrets bundle？
  `custody.md` 的红线在这里适用（它是「后端持有、链上强制范围」的 scoped 委托，
  与 HL agent wallet 同类，但需确认 factory 合约真的把它限制在「只能归集到国库」）。
- **→ audit-financial**：`normalizeAmount(price, weiGweiDecimals)` 的 wei→gwei 转换，
  以及 token decimals 的处理路径，属于金额精度范畴。

## 我没能验证的

- **`DepositFactory` 合约本身没读**（不在本仓库）。「归集只能到国库、sweeper key 无法提走
  资金」这个链上强制假设**未验证** —— 而整个 sweeper key 的风险模型建立在它之上。
- **scanner.go / logs.go 的 reorg 处理未细读**（209 + 72 行）。`onchain_reorg_recheck`
  循环存在，但「多深的 reorg 会被发现、发现后账本怎么补偿」我没走完。
- ~~multicall.go 未读~~ —— **已验，确认成立**，见上面第二条 Major。

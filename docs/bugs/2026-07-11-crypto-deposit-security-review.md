# Security Review — feat/crypto-deposit (crypto deposit + sweep)

> reviewer-sec · scope `git diff main...HEAD` · **read-only, no code changed**
> money path: 伪造入账 = 凭空铸钱。逐条核实 design §5 威胁模型。

## 🔴 CRITICAL

### C1 — 确认阈值（reorg 防线）被完全架空：`block_number` 两条 ingestion 路径都从不填充
- **位置**：`chains/evm/reader.go:160-169`（watcher 构造 sighting **漏设 BlockNumber**）；`channel/onchain/evm.go:79-116`（webhook `sightingPayload` 无 `block_number` 字段，ParseSighting 也不设）。消费点 `service/onchain.go:487`（metadata 恒写 `block_number=0`）→ `service/onchain.go:740` `confirmations := latest - 0 + 1`。
- **攻击场景**：watcher/hybrid 模式，任何 deposit 首次入账后进 `confirming`，recheck 循环（默认 20s）用 block_number=0 算出 confirmations = latest+1 ≫ 阈值 → 直接 `Transition(confirmed)` + 过账 journal，**无视链上实际确认深度**；且 `if confirmations < cfg.Confirmations` 为 false 会**跳过 shallow-reorg 的 TxIncluded 检查**。攻击者：充值 → ~20s 内 0–1 确认入账 → 提现 → reorg 掉充值 tx = 凭空得款。设计 §3「确认阈值是 reorg 防线」被降到 ~20s 窗口。
- **测试为何漏掉**：`service/onchain_integration_test.go:280/334/386` 手工构造 `BlockNumber:100/200/1`，真正的两个生产者从不填 → 测试全绿、生产恒 0。
- **修法**：`FetchDeposits` 写入 `lg.BlockNumber`；`sightingPayload` 增 `block_number` 并要求 webhook 提供；`DepositSighting.Validate()` 增 `BlockNumber > 0`。

## 🟠 MAJOR

### M2 — webhook 入账桥 `bookingToResponse(nil)` 空指针 panic
`server/handler_webhooks.go:140-145` + `handler_bookings.go:72`（立即解引用 `op.UID`）。IngestDeposit 对「未注册地址/非白名单 token/未配置链」按契约返回 `(nil,nil)`（正常「无需处理」），桥接层未处理 → panic（chi Recoverer 兜住返 500）→ 外部 scanner 视失败**永久重试风暴**；「deposit 先到、EnsureDepositAddress 后调」竞态必触发。修法：booking==nil 返 200 no-op。

### M3 — 大额人工复核/双源对账「留口」实现中不存在
IngestDeposit→advanceConfirmation 单次 confirmed sighting 即过账，**无金额阈值门、无人工复核队列、无双 provider 对账**。设计 §5-1 把「大额进人工复核」列为最大信任边界（RPC/webhook = trusted oracle）**唯一**补偿控制，未落地。`EVM_WEBHOOK_SECRET` 泄漏或 RPC 说谎 = 无上限铸币（webhook amount 全由攻击者控制、无链上二次校验）。

### M4 — 注册回扫全量 getLogs 无分块 + goroutine 无并发上限
`service/onchain.go:408-423` `rescanAddressOnChain` 用 `FetchDeposits(0, latest)` 一次性全历史 getLogs（不遵守 maxBlocksPerScan）。主网级链 provider 拒/超时 → 失败仅 Warn（:401-403）→ 设计 §2/§5-2b 靠它堵的「注册前充值丢失」缝隙**静默敞开**。`launchRegistrationRescan` 每次 spawn 无上限 goroutine → 批量 onboarding = RPC 放大 DoS。修法：分块 + 并发信号量 + 失败 metric/告警。

### M5 — sweep 永久卡死：terminal `failed` + 未上链 nonce
`service/onchain.go:1036-1075` + `:930-940`。sweep booking 超 maxSweepBumps 转 `failed` 但 nonce 从未上链 → `PendingNonceAt` 持续返回该 nonce → 下 tick 重建同 key：eligible 变了撞 `ErrConflict`；没变则幂等返 failed booking、`advanceSweep` 对 failed `return nil` 静默空转。该 (chain,token) 归集**永久停摆**，需人工。设计 §4 声称有 retry 边但编排未重新驱动 failed booking。

## 🟡 MINOR
- mi1 `decodeTransferLog`(logs.go:54-65) 未断言 to/from topic 高 12 字节为 0（白名单内无害，扩白名单需补）。
- mi2 dead-letter `reason` 存 `err.Error()`(onchain.go:492)，可能夹带内部 SQL/driver 文本进 on-call 表面。
- mi3 注册回扫失败只 Warn 无 metric（安全相关「缝隙未堵」对监控不可见）。

## ✅ 已核实无问题（逐条对威胁模型）
- **威胁6 幂等三态修复（重点审）**：`postgres/idempotency_match.go` 的 `maps.Equal(jsonToStringMetadata(existing), input)` 是语义相等，比原 byte 比较**更严格更正确**（原会误报 conflict）；classification/holder/currency/channel/amount/expires_at 仍逐字段精确比较。**未放松双花防线**（same key + 不同 amount 仍 ErrConflict）。依赖「booking metadata 恒为 flat string map」不变量，成立。
- **威胁2 webhook→sweep / classification 注入**：IngestDeposit 硬编码 deposit classification；legacy 路径校验 `ClassificationUID==deposit`；sighting payload 无 classification/booking_uid 字段。结构性封死。
- **HMAC**：`hmac.Equal` 等长 hex 常量时间比较；±5min 时效窗 + nonce 重放缓存（sha256(channel+ts+sig+body) + ON CONFLICT）。健全。
- **威胁3 checksum 归一化**：EnsureAddress/GetByAddress 均过 `core.ChecksumAddress`；IngestDeposit 先 checksum `s.To` 再查；watcher topic 大小写无关。无绕过。
- **威胁3 holder 校验/鉴权**：`DeriveDepositAddress` 拒 holder≤0；DB `CHECK(account_holder>0)`；端点在 ScopeWrite/Read API-key 鉴权组内（routes.go:57/104）。
- **威胁4 sweeper key**：LocalSigner 私钥仅内存、日志/错误/metadata 均不含 key，仅暴露公钥 EOA。BatchSweep 对每 target `DeriveDepositAddress` 强制对拍（大小写敏感、mismatch 中止整批）不可跳过。
- **威胁7 SQL 注入**：039/040 全 sqlc 参数化，无拼串；webhook body `io.LimitReader` 限 1MB。
- **威胁8 dead-letter 灌爆**：`uq_ingest_dead_letters_idempotency_key` + ON CONFLICT DO NOTHING，有界。
- **威胁5 nonce 幂等 happy-path**：gas-bump 复用同 nonce → 同 booking；findInFlightSweep 单飞检测（terminal-failed 边界见 M5）。

## 建议
C1 + M2 合入前**必修**（都在默认 watcher 路径，C1 直接是 money-path）。M3 是信任模型核心补偿控制缺失，拉 backend-engineer + go-financial 拍板本期是否补。

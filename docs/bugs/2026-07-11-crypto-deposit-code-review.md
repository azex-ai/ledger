Code review: feat/crypto-deposit (git diff main...HEAD, ~7.4k lines). 方法: codegraph 结构+blast radius / 通读 diff / producer↔consumer 交叉追踪; go build+vet 主模块绿. 结论: 1 Critical + 2 Major, 不建议直接合并. 问题集中在 DepositSighting.BlockNumber.

=== CRITICAL ===
[C1] 确认阈值(reorg 防线)完全失效: BlockNumber 从未被任何 producer 写入.
- watcher chains/evm/reader.go:160-169 构造 sighting 没赋 BlockNumber (156 行算了 lg.BlockNumber 只用于 confirmations,没拷进 struct) -> 恒 0
- webhook channel/onchain/evm.go:79-88 的 sightingPayload 没有 block_number 字段 -> 恒 0
- 消费端 service/onchain.go:487 写进 booking metadata "0", recheck loop recheckOneDeposit(onchain.go:740) 读回算 confirmations=latest-0+1(几千万) -> 永远>=阈值 -> 每笔存款下个 recheck tick(20s)被提前 confirmed, 无视真实确认深度. 首进 recheck 的正是刚到/确认数<阈值的新存款(绝大多数).
- 连带 recheckOneConfirmedDeposit(onchain.go:813) latest-0>reorgRecheckWindow 恒真 -> confirmed 存款永不做深 reorg 复核, manual ReorgPolicy 安全网也死.
- 设计 §3 "确认阈值是 reorg 防线" 完全绕过: 存款->即时到账->reorg 掉.
- 测试没抓到: e2e_test.go:142-147 断言各字段独漏 BlockNumber; service 集成测试用 fakeChainReader(FetchDeposits 返回 nil)+手动塞 BlockNumber+直接调 IngestDeposit 递增 Confirmations, 绕开 recheck loop 真实路径.
- 修法: 两 producer 都填 BlockNumber(reader 填 int64(lg.BlockNumber); webhook payload 加字段). 只修一半,见 M1.

=== MAJOR ===
[M1] block_number 进了幂等 payload, 真实 reorg 下破坏 I-20.
- service/onchain.go:487 把 block_number 放进 CreateBooking Metadata, postgres/idempotency_match.go:109 用 maps.Equal 纳入三态幂等比较.
- 设计 §3 明令"块高等易变字段走 Transition metadata,不进 booking payload". Confirmations 正确排除了(onchain.go:476-481 有注释),但 block_number 同样易变却被放进去.
- 后果: reorg 把 tx 重打包进不同区块 -> 第二次观测 block_number 变 -> 同 key 不同 payload -> ErrConflict -> dead-letter(而非幂等 no-op). 正是 I-20 声称保证的场景.
- I-20 pinning 测试假绿: postgres/invariants_test.go:461 makeInput(blockLevelLogIndexAtObservationTime) 拿到参数 _=... 直接丢弃, 两次观测硬编码 block_number:"100"(474 行), 从没变过 -> 把 log_index(没进 payload,对) 和 block_number(进了,是 bug)搞混.
- 修法(一并解 C1+M1): block_number 移出 CreateBooking payload, 改存 confirming transition 的 event metadata(recheck 从那读), 排除出 booking 幂等比较.

[M2] sweep 单飞锁粒度太细, 同链多 token 抢同一 EOA nonce.
- 锁按 (chain,token) 命名 service/onchain.go:1260 "sweep:%d:%s", 但 signer nonce 是 per-(chain,EOA): chains/evm/sweeper.go:75 NextNonce=PendingNonceAt(signerAddress).
- 同链 USDT/USDC/native 各自独立 policy+锁+findInFlightSweep 并发跑; 两 sweepTick 几乎同时 NextNonce -> 同一 pending nonce -> 两笔 tx 同 nonce -> 互相顶替 -> 被顶替 booking 卡 sent, gas-bump nonce too low -> 耗尽 maxBumps -> failed 需人工. 资金不丢, 但一 token 归集卡死.
- 设计 §4 是"全局单飞", 实现放宽成 per-(chain,token) 且注释称"不相关 policy 不该串行", 但同链不同 token 共享 EOA nonce 序列,并非不相关.
- 修法: 锁粒度收到 per-chain(或 per-EOA), 同链所有 token sweep 串行.

=== MINOR ===
[m1] server/handler_onchain.go:30-39 DepositAddressProvider 要 GetDepositAddress(holder), 但发货的 service.Onchain 没这方法, AddressRegistry port 也无 by-holder 查询(仅 GetByAddress/ListAddresses) -> GET /holders/{holder}/deposit-address 路由发了但库内无法接线; main.go:218 示例 srv.SetDepositAddressProvider(onchainSvc) 取消注释编译不过. opt-in 降级 FeatureNotEnabled,不碰钱路,但功能不完整.
[m2] service/onchain.go:630,687 cfg:=o.chains[chainID] 取了又 _=cfg // reserved for future -- golang.md 明禁的"先造再丢",多余 map 读,删掉即可.
[m3] recheckSweepSent(onchain.go:1089-1146) gas-bump 时不推进 booking.UpdatedAt(保持 sent), 首次卡住后后续 bump 按 sweep Interval 而非 sweepStuckAfter 触发, 可能更快耗尽 maxBumps. 受 maxBumps 兜底,影响小.

=== NITPICK ===
[n1] I-20 pinning 测试名 ..._StableAcrossLogIndexChurn 误导(见 M1), 应真正 churn block_number(会暴露 M1)或改名说明只证明同 payload 重放.

=== 查过无发现 ===
- core 依赖红线: go-ethereum 不在根 go.mod,仅 chains/evm; core 只 import decimal + x/crypto/sha3. OK
- DB 事务内无 RPC: TxComposer.RunInTx 回调只做 Transition+ExecuteTemplate. OK
- 先记后发: sweepTick 先 CreateBooking 再 BatchSweep; nonce 持久化 metadata, findInFlightSweep 复用同 booking/nonce (跨 token 除外=M2). OK
- RunInTx 原子性+EventUID 互链+panic 回滚 (ledger.go:269/onchainTxComposer). OK
- goroutine 退出路径: launchRegistrationRescan 独立 ctx timeout+Done; runLoop 有 ctx.Done; errgroup. OK
- TxLogSeq tx-local reorg 稳定: 按 (block,index) 排序后 seqByTx 计 tx 内序号,不用块级 log_index. OK
- 地址分片/decimals 归一/malformed log 不 panic (reader/logs/scanner). OK
- webhook classification 限定: legacy 路径查 deposit class+channel match; sighting 路径只创建 deposit booking/不收 booking_uid (设计 §5-5 满足). OK
- idempotency_match.go 语义化 metadata 比较修复: maps.Equal 覆盖 key 顺序/空白, 保留三态. OK
- SetLifecycleIfEmpty 回填: WHERE lifecycle='{}'::jsonb expand-safe+幂等. OK
- migration 039: IF NOT EXISTS, uid 从头 NOT NULL, 地址索引 case-sensitive 与 checksum 路径一致(service 查询前 ChecksumAddress, store 不 lower). OK
- I-19/I-20 的 6 个 Pinned-by 测试函数都真实存在(I-20 覆盖有 M1/n1 语义缺口). OK
- envelope/bizcode/uid-only: depositAddressResponse 去 Factory/InitHash, uid+RFC3339, 标准 httpx 包络. OK

未改任何代码(review only). C1/M1/M2 请拍板: 建议 C1+M1 合并一次修(block_number 移出 payload+改存 transition metadata+两 producer 填值), M2 单独收锁粒度.

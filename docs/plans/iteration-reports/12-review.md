# 第 12 轮独立域评审

结论：**PASS（代码无 blocker）**。有 1 条不阻塞代码集成的 Minor 文档校正，见下文；无需代码修复波次。

范围：`767e2088db7606be42038411e5764215c9da381b..d22e40d36369a692b0d676c93b1dfddd6ad937b0` 的 9 个文件。评审在独占分支 `codex/ledger20-t12-review` 完成，读取全部 diff、Capture/Reader 测试和直接依赖的事务、锁、settlement、模板渲染、journal 幂等实现及数据库维度不可变约束。未修改业务源码，未提交。本报告占 T2 域 reviewer 一席，主控另做 Codex 第二意见。

输入 → 输出 → 交接 → 验证 → 记忆：Capture 冻结契约及已明确接受的 metadata/mode 裁决、实现 diff → 本报告 → root 修正文档和集成 → 独立静态核验与 diff 检查、实现者真实 PG 证据 → 项目内注释/报告已记录无 savepoint、unsigned discharge 和宿主锁并集边界。

## 代码核验

| 维度 | 结论与证据 |
|---|---|
| 输入边界 | `capture.go:48` 复用 SettleInput.Validate，金额 magnitude/正数、必要 key 在打开事务前检查；`:54` 拒绝保留 metadata。map 复制后追加绑定，不修改调用者数据。模板渲染进一步检查 metadata 限制和 amount 参数，发生在 settlement 前。 |
| Reader 与 clone | `Service.ReservationReader()` 使用已有 reserverStore；`withTx` 将该 store 的 db/q/ledger 绑定同一事务。新 GetReservation 使用无 FOR UPDATE 查询，不增加过早行锁；数据库 guard 禁止更改 reservation 的 holder/currency，所以非锁定预读不把可变状态当授权依据。未扩张已有 Reserver/QueryProvider 接口。 |
| 锁序 | `capture.go:113` 调用现有 LockForTemplates，对 charge/settle keys 去重排序，再对模板实际涉及的 holder/currency 对排序，随后才到 settlement reservation 行锁。Render 的 holder 只能是正用户及其负系统对手方，currency 固定来自 reservation。正常模板、同 key 并发、不同 key partial 与充值之间没有新增反向取锁路径。宿主组合额外操作仍须预取并集，这是现有契约，不由 Capture 自动推断。 |
| 原子性 | settlement → ExecuteTemplate → 读取实际 entries → 净额检查全部位于同一 RunInTx。顶层任何错误回滚；tx clone 直接加入原事务，依赖调用者返回错误且不提供 savepoint，与冻结契约一致。检查错误模板发生在写入之后，但提交之前；报告未隐瞒调用者吞错的边界。 |
| 状态与并发额度 | Settle/SettlePartial 在行锁下重新核验状态、真实过期时间、币种精度与预留额度；partial 累加与 receipt/leg 同事务。先前预读的 Status/SettledAmount 不参与授权，因此等待锁期间完成的 settlement/release 不会被旧快照覆盖。 |
| 幂等与变参 | settlement receipt/leg 比对 reservation 与金额，journal 同 key 比对实际 entries、ActorID、Source 和 metadata。保留 metadata 绑定 reservation、mode、template code，同分录模板别名也不能误当重放。模式切换可提前 ErrInvalidTransition 是已接受裁决。相同增量在 finalize 后仍先匹配旧 leg，不再次扣款。 |
| 金额与分类 | `capture.go:155` 拒绝其他 holder/currency；`:162` 要求用户侧仅 available；`:165` 使用 classification.NormalSide 的 SignedAmount，最后严格等于 `-Amount`。空 journal、反方向、重复扣费、用户 memo/pending/locked 均不能被当成成功 Capture。系统科目仍由模板配置，保持账本原有语义。 |
| 外部调用与签名 | 所有操作走 tx clone；PostJournal 与 settlement 的 tx 路径不调用 attestor，沿用 unsigned 语义。README/注释明确 verified-balance gate 的保守 hold 与原币账本限制，没有声称事务可以回滚外部执行。 |

做得好的地方：通过读取持久化 journal 的实际 entries 验证扣款含义，避免仅按模板名字相信方向；新增 Reader 保持事务可见性，同时不破坏现有宿主接口实现。

## Minor 文档校正（非代码 blocker）

`docs/plans/iteration-reports/12.md:11` 的“settlement … 复核 … 余额”应准确写为“剩余预留额度”。`Settle` 没有重新验证当前可用余额；raw journal 绕过 hold 的既有限制仍适用。

同文件 `:13` 的“Partial Capture 0.3 则 available / held 都剩 0.7”混淆 available 科目的账面余额与扣除 hold 后的可用额度。此场景实际为：available 科目账面余额 0.7、held 0.7、可再次预留的 available 为 0；Finalize 后才释放余下 hold。已有 Reserve 实现 `postgres/reserver_store.go:580` 明确使用 `availableBase.Sub(heldDecimal)`。建议只修正该报告的术语，代码与测试断言无需更改。

处置：主控已接受并交原作者仅修正 `12.md`，由主控 scoped 核对文档闭环；本席不重复评审源码。

## 证据与验证范围

本席独立执行 `git diff --check 767e208..d22e40d36369a692b0d676c93b1dfddd6ad937b0`：PASS。没有需要另跑 PostgreSQL 才能裁定的新代码疑点，因此未申请或占用 PG slot，未重复实现者套件。

运行证据沿用 `12.md`：`go test -race -p 1 . ./postgres -run '^Test(Capture_|ReservationReader_)' -count=1 -timeout 5m` PASS，root 7.956s、postgres 1.700s；覆盖 full/partial/replay、并发相同 key、并发额度、充值、unsafe template 回滚、失效 reservation、clone 可见性和锁持有。文档/API/clone surface 检查及 go vet 亦为实现者证据，不表述为本席重新运行。测试断言读取真实余额、hold、journal 数量、receipt/leg 和 reservation 状态，未以仅返回 nil 作为资金路径验收。

本报告不代替全分支最终验收。无本席启动的数据库、容器或运行中的测试进程。

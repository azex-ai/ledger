# 中间版本 CI 失败诊断

本记录区分真实测试隔离缺陷与未证实的 fuzz 算法问题。独立 reviewer 只读 GitHub Actions 日志和 artifacts，root核对源码调用链；没有为诊断重跑 PG 或修改源码。

- [38036575199](https://github.com/azex-ai/ledger/actions/runs/38036575199/job/114168130961)：`TestMigrations_FullDownChainAndReapply` 删除 `ledger_owner` 失败，另一数据库 `ledger_test_10421_88` 仍有109个依赖对象。server log在08:09:08.832 UTC。
- [38038603752](https://github.com/azex-ai/ledger/actions/runs/38038603752/job/114174180601)：service/reconcile_full_integration_test.go:421 调用 SetupDB 时，baseline migration 报 `role "ledger_owner" does not exist`。server log在08:44:50.567 UTC。
- 两者对应 fixture/migration 源码相同。`go test ./...` 跨package并行且共享 DATABASE_URL；SetupRawDB只分数据库，full roundtrip直接调用golang-migrate Up/Down/Up；001 down的DROP OWNED只处理当前库，DROP ROLE影响cluster。生产Migrate维护库锁仅覆盖迁移过程，不能消除其他存活测试库的角色依赖；仅补锁不足以修复。Task20将破坏性roundtrip放入独立cluster fixture，不修改生产迁移、不全局串行或跳过测试。日志没有成功DDL逐语句记录，不能给出第二次失败对应DROP ROLE的精确时间。
- [38038642783](https://github.com/azex-ai/ledger/actions/runs/38038642783/job/114174294459)：FuzzAllocate以4 workers执行804934次，在30.09s仅报`context deadline exceeded`，无panic、性质断言或crasher路径；前两fuzz和root suite通过。三次run均Go1.26.9 linux/amd64，三个fuzz-corpus均只有各自HEAD已提交的两个Journal/Lifecycle seeds，逐字节一致，无Allocate新反例。未证实CPU/内存不足或具体Go runtime问题，不据此修改金额算法或盲增timeout；最终真实CI仍必须验收。

原始日志/种子暂存在本任务`.local/ledger20/ci-diagnosis`；可持久引用以以上GitHub run为准。

## 后续 CI 检查

- [38042226697](https://github.com/azex-ai/ledger/actions/runs/38042226697)，HEAD `30372f03`：service 的 `TestFullReconciliation_UnauthorizedJournals_PassesWhenAllSignedJournalsAreValid` 初始化再次报 `ledger_owner` 不存在，属于上述隔离问题。
- [38042627464](https://github.com/azex-ai/ledger/actions/runs/38042627464)，HEAD `06ea1fa0`：`examples/signed-capture.TestRun` 在同事件重放时报 `signed-capture:charge:job-1:completed` payload mismatch；其他root/postgres/service套件通过。不同于角色隔离失败，不能混为同一环境问题。
- 静态证据：示例 `main.go` 使用原始 `time.Now().UTC()`，`capture.go` 将 RecordedAt 原样作为 explicit EffectiveAt；`postgres/idempotency_match.go:28` 用原始 `time.Equal` 比较落库时间。现有 `core.canonicalTimestamp` 与 I-46 已记录 pgx/TIMESTAMPTZ 的微秒 floor 语义，签名规范化没有覆盖这处重放比较。固定纳秒输入的反事实验证与修复归 Task20；此处尚不宣称修复已通过。

后两项原始日志为本任务 `.local/ledger20/ci-38042226697.log` / `ci-38042627464.log`；日志中的 healthcheck `role root does not exist` 不等同 test assertion 失败，以上只提实际失败测试。

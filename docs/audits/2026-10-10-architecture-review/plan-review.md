# 20 轮计划 T1 评审

- 评审日期：2026-10-10
- Reviewer：library_boundary（独立规划评审席）
- 初审提交：`c4cf77f`
- 修订复核提交：`8d187cc2d7856394bc22457f6e7eab669b9ba8ca`
- 范围：`docs/plans/2026-10-10-ledger-20.md` 与 `docs/audits/2026-10-10-architecture-review/README.md`；重点为 Phase 0 的 Capture、holder、cache 契约及 20 轮任务边界。
- 结论：**PASS。修订后的规划无未关闭 blocking finding，可进入实施。** 此结论仅覆盖规划与审计文档，不代表实现或最终验收已经通过。

## 初审阻断点与关闭证据

### P-1：默认缓存隔离不能承诺检测隐藏的 cookie 身份切换——已关闭

初审计划第 17 行要求无显式 scope 时也隔离或清理身份切换后的缓存，但同一个 backend URL、稳定的 fetch、未提供 apiKey 的 BFF cookie 会话切换无法由 SDK 自动观察。若以此要求分派 Task 8，实施者可能只检测配置变化，仍留下同 URL 换账号的缓存共享路径。

`8d187cc` 的冻结契约明确：隐藏 cookie 身份变化由宿主更换非秘密 scope，或重挂拥有新 QueryClient 的 provider；默认行为只保证可观察的实例/config 隔离。SSR 和浏览器必须采用相同的显式 scope。Task 8 同步记录宿主责任，原有 provider、hydration、跨 backend/session、mutation invalidation 的验证要求仍保留。

关闭依据：不可观察输入的责任已放在能够观察它的宿主，默认隔离与显式 SSR scope 的保证范围清楚，且凭证不得进入 query key。无需为这次修订引入新的认证层。

### P-2：Capture 读取 reservation 的接口与允许修改范围缺失——已关闭

初审 Capture 契约要求只凭 ReservationUID 读取 holder/currency，但现有 `core.Reserver` 不提供读取方法，`ReservationQuerier` 只有列表接口；Task 12 的 Touches 仅允许 `capture.go` 与 `capture_test.go`，未覆盖必要的读取适配。直接在门面绕过边界调用 sqlc，或先锁 reservation 再取得账本全量锁，都不是已冻结契约所能合理推出的实现。

`8d187cc` 补充独立的 `core.ReservationReader.GetReservation` 接口、`Service.ReservationReader()` accessor 和 PostgreSQL adapter；复用现有 GetReservationByUID 与转换逻辑，无新增 SQL 或 migration，不扩张现有 Reserver/QueryProvider。Task 12 的 Touches 已覆盖新接口、adapter、adapter 测试与 `ledger.go`。

同一修订明确顺序：非锁定预读仅取得 holder/currency，随后取得全量 advisory locks，最后由 settlement 在行锁内重新验证实际状态、期限和金额。这样既有可实现的读取边界，也没有把预读当成状态校验或反转既有锁顺序。

关闭依据：接口、实施范围和锁顺序均已裁决；重试、竞争、余额/冻结数量与失败回滚仍由 Task 12 的实现及 T2 验证承担。

## 已接受的契约与范围

1. **holder：通过。** 保持 HTTP/Go int64 wire 契约，同时使 React numeric SDK 在发送前和响应解析后拒绝不安全整数，并让两套 UI 使用 strict parser，是明确且可实施的过渡边界。它解决静默取整问题，但不宣称 React 已支持完整 int64 ID；未来 string wire 迁移应独立版本化。Task 6 与 Task 7 的先后依赖合理。
2. **Capture：通过。** 原子组合、加入调用者事务、错误向上传播、无 savepoint、稳定派生幂等键、保留 metadata 字段及 available 经济效果检查均已说明。unsigned 门面和既有 signed 流程的保证范围也已区分。实施时仍须验证这些条件，不应把文档冻结视为验证完成。
3. **cache：通过。** backend 与非秘密身份共同决定作用域；hooks、prefetch、hydration、invalidation 需要一致。修订后宿主的隐藏身份切换责任明确，足以实施 Task 8。
4. **20 轮范围：通过。** 现有正确性问题先形成独立任务，后续门面、真实消费示例、可选模块消费和文档分别交付，最终统一验收。无 rate 持久化、无新表或 migration、无真实资金操作、无自动发布 tag，避免本轮演变为新的交易执行平台。Task 7/9/14 的实际行为依赖与最终整合依赖合理。
5. **审计 README：通过。** 将已复现问题、静态风险、既有优点和未来边界分别说明，没有把 USD valuation 或外部 swap 原子性误当作账本已有能力。测试与依赖审计的限制也没有被隐藏。

初审两点只影响 Task 8 与 Task 12/14 的接口落实，不要求暂停独立的 Task 2、Task 5 或 Task 16。修订还移除了纯 core 精度测试的不必要 PostgreSQL 互斥，并明确波次、实际并发限制和每任务报告路径；这些调整与既定范围一致。

## 验证与局限

本席通过 `git show` 阅读已提交文档，并仅对初审缺口涉及的现有配置与读取接口做了窄范围核对；收到修订后只复核对应 diff，没有重新审计全库或执行实现测试。

可重复的主要读取命令（在独立 review-library worktree 中运行）：

```sh
git show c4cf77f:docs/plans/2026-10-10-ledger-20.md
git show c4cf77f:docs/audits/2026-10-10-architecture-review/README.md
git show 8d187cc -- docs/plans/2026-10-10-ledger-20.md docs/audits/2026-10-10-architecture-review/README.md
```

主审提供的 `plan_check.py` 返回 0 与 bus 调度信息作为调度背景；本席未独立重跑该检查。不修改业务源码、不提交旧 probe、不执行合并或推送。各实现任务原有 T1/T2 与最终验收要求继续有效。

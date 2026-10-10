# 20 轮进度

计数以验收且已集成为准。每轮报告见同目录 iteration-reports；BASE / commit / 测试 / 评审结论逐项记录。

| 轮次 | 交付 | 状态 | BASE / 分支 / commit | 验证与评审 |
|---|---|---|---|---|
| 1 | 审计基线与契约冻结 | 已集成 | BASE d00fdeb / 8d187cc → 97e3336 | plan_check 0；T1 PASS（plan-review.md）；make test 全 root race 通过 |
| 2 | 冻结策略按币种净额判断 | 已集成 | ae0c1b7 → 7a0f596 | 独立域/root Codex PASS；make test 全 root race 通过 |
| 3 | 金额 helper 目标精度边界 | 评审通过，待集成 | 2239814 / codex/ledger20-t03 | core race/vet/fuzz；独立域及root PASS |
| 4 | 输入校验早于 tracing 展开 | 评审通过，待集成 | cf03361 / codex/ledger20-t04 | PG race/vet；独立域/root PASS |
| 5 | 预留请求对齐生成契约 | 评审通过，集成中 | d376f03 / codex/ledger20-t05 | T1 PASS；35 client tests/typecheck/build/codegen |
| 6 | SDK holder 安全整数边界 | 评审通过，待集成 | 8f52e08 / codex/ledger20-t06 | build/typecheck/346 tests；独立域及root PASS |
| 7 | 两种 skin 的 holder 输入校验 | 待实施 | — | — |
| 8 | 管理端缓存按实例与身份隔离 | 待实施 | — | — |
| 9 | 其余 SDK mutation 请求契约消漂移 | 待实施 | — | — |
| 10 | 科目配置经济效果验收样例 | 待实施 | — | — |
| 11 | Exchange 严格消费可用余额 | 待实施 | — | — |
| 12 | 原子 Capture 门面 | 实施中 | BASE 767e208 / codex/ledger20-t12 | 待验收 |
| 13 | 签名资金流程可运行组合示例 | 待实施 | — | — |
| 14 | credits 消费接入原子 Capture | 待实施 | — | — |
| 15 | USD 估值读模型示例 | 评审通过，待集成 | 3d69a74 / codex/ledger20-t15 | T1 PASS；race/vet/run，覆盖率 94.8% |
| 16 | 市场报价与执行扩展 ADR | 评审通过，待集成 | 8f4c9e5 / codex/ledger20-t16 | T1 PASS；文档/来源核实 |
| 17 | 可选 Go modules 独立消费验证 | 待实施 | — | — |
| 18 | web 生产依赖 advisory 修复 | 实施中 | BASE 4316088 / codex/ledger20-t18 | 官方来源核验中 |
| 19 | 库接入与兼容迁移文档 | 待实施 | — | — |
| 20 | 最终集成验收与交付 | 待实施 | — | — |

## 执行记录

- 第一轮整合 `97e33362abd436c072114ab863079ac65a78fad1` 已推送计划分支。`make test` 实际运行 `go test -race -timeout 15m -count=1 ./...` 全部通过；root 101.431s、postgres 277.257s、service 140.870s。独立 Go 子 modules 与 React 不在这次 root 命令覆盖内。
- 使用本次专用临时 PG17 实例，fixture 每测试独立数据库。
- bus #30..49 对应轮次 1..20。缺少独立 OS PID 的会话内 worker 在 submitted/reviewed 时保守保留 writer lease；整合使用冻结 commit 的独立 delivery worktree，通过 wt integrate 后才标 bus integrated，再删除原任务 worktree。不会提前把未合并任务记为 integrated，也不修改 lease 或关闭 guard。
- 所有实现者只提交各自任务分支，主控负责评审、串行整合与最终推送。

- 第二轮整合 `7a0f5961759afdf829bddcc6959dc00605c75ea4` 已推送；全 root race 通过（root 133.449s、postgres 287.375s、service 136.863s）。

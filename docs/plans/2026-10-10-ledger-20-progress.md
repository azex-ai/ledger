# 20 轮进度

计数以验收且已集成为准。每轮报告见同目录 iteration-reports；BASE / commit / 测试 / 评审结论逐项记录。

| 轮次 | 交付 | 状态 | BASE / 分支 / commit | 验证与评审 |
|---|---|---|---|---|
| 1 | 审计基线与契约冻结 | 已集成 | BASE d00fdeb / 8d187cc → 97e3336 | plan_check 0；T1 PASS（plan-review.md）；make test 全 root race 通过 |
| 2 | 冻结策略按币种净额判断 | 已集成 | ae0c1b7 → 7a0f596 | 独立域/root Codex PASS；make test 全 root race 通过 |
| 3 | 金额 helper 目标精度边界 | 评审通过，待集成 | 2239814 / codex/ledger20-t03 | core race/vet/fuzz；独立域及root PASS |
| 4 | 输入校验早于 tracing 展开 | 评审通过，待集成 | cf03361 / codex/ledger20-t04 | PG race/vet；独立域/root PASS |
| 5 | 预留请求对齐生成契约 | 已集成 | d376f03 → 1b01b97 | T1 PASS；client/typecheck/build/codegen；完整root race |
| 6 | SDK holder 安全整数边界 | 已集成 | 8f52e08 → 4df363d | frontend通过；独立域/root PASS；完整root race |
| 7 | 两种 skin 的 holder 输入校验 | 待实施 | — | — |
| 8 | 管理端缓存按实例与身份隔离 | 实施中 | BASE 4df363d / codex/ledger20-t08 | 待验收 |
| 9 | 其余 SDK mutation 请求契约消漂移 | 待实施 | — | — |
| 10 | 科目配置经济效果验收样例 | 待实施 | — | — |
| 11 | Exchange 严格消费可用余额 | 独立评审中 | 6e62cef / codex/ledger20-t11 | 12真实RED、Exchange race/vet通过；root PASS |
| 12 | 原子 Capture 门面 | 评审通过，集成中 | d22e40d +241905e / codex/ledger20-t12 | PG/API/vet通过；独立域/root PASS；Minor文档已修 |
| 13 | 签名资金流程可运行组合示例 | 待实施 | — | — |
| 14 | credits 消费接入原子 Capture | 待实施 | — | — |
| 15 | USD 估值读模型示例 | 评审通过，待集成 | 3d69a74 / codex/ledger20-t15 | T1 PASS；race/vet/run，覆盖率 94.8% |
| 16 | 市场报价与执行扩展 ADR | 评审通过，待集成 | 8f4c9e5 / codex/ledger20-t16 | T1 PASS；文档/来源核实 |
| 17 | 可选 Go modules 独立消费验证 | 实施中 | BASE 9b09eb5 / codex/ledger20-t17 | 待验收 |
| 18 | web 生产依赖 advisory 修复 | 评审通过，待集成 | 158135e / codex/ledger20-t18 | T1 PASS；prod audit0，SDK/Next build/typecheck/257 tests |
| 19 | 库接入与兼容迁移文档 | 待实施 | — | — |
| 20 | 最终集成验收与交付 | 待实施 | — | — |

## 执行记录

- 第一轮整合 `97e33362abd436c072114ab863079ac65a78fad1` 已推送计划分支。`make test` 实际运行 `go test -race -timeout 15m -count=1 ./...` 全部通过；root 101.431s、postgres 277.257s、service 140.870s。独立 Go 子 modules 与 React 不在这次 root 命令覆盖内。
- 使用本次专用临时 PG17 实例，fixture 每测试独立数据库。
- bus #30..49 对应轮次 1..20。缺少独立 OS PID 的会话内 worker 在 submitted/reviewed 时保守保留 writer lease；整合使用冻结 commit 的独立 delivery worktree，通过 wt integrate 后才标 bus integrated，再删除原任务 worktree。不会提前把未合并任务记为 integrated，也不修改 lease 或关闭 guard。
- 所有实现者只提交各自任务分支，主控负责评审、串行整合与最终推送。

- 第二轮整合 `7a0f5961759afdf829bddcc6959dc00605c75ea4` 已推送；全 root race 通过（root 133.449s、postgres 287.375s、service 136.863s）。

- 第五轮整合 `1b01b974` 已推送；完整 root race PASS（root 133.825s、postgres 288.004s、service 183.686s）。本轮 SDK 的 build/typecheck/client/codegen 证据单独记录，root Go gate 不替代 frontend 验证。

- 第六轮整合 `4df363d8` 已推送；完整 root race PASS（root 112.806s、postgres 283.516s、service 133.680s）。Task5/6 的client.ts自动合并无冲突；最终frontend回归仍会覆盖组合基线。

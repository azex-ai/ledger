# 20 轮进度

计数以验收且已集成为准。每轮报告见同目录 iteration-reports；BASE / commit / 测试 / 评审结论逐项记录。

| 轮次 | 交付 | 状态 | BASE / 分支 / commit | 验证与评审 |
|---|---|---|---|---|
| 1 | 审计基线与契约冻结 | 已集成 | BASE d00fdeb / 8d187cc → 97e3336 | plan_check 0；T1 PASS（plan-review.md）；make test 全 root race 通过 |
| 2 | 冻结策略按币种净额判断 | 已集成 | ae0c1b7 → 7a0f596 | 独立域/root Codex PASS；make test 全 root race 通过 |
| 3 | 金额 helper 目标精度边界 | 已集成 | 2239814 → 83060d6 | core race/vet/fuzz；独立域及root PASS；完整root race |
| 4 | 输入校验早于 tracing 展开 | 已集成 | cf03361 → 0c9417e | PG race/vet；独立域/root PASS；完整root race |
| 5 | 预留请求对齐生成契约 | 已集成 | d376f03 → 1b01b97 | T1 PASS；client/typecheck/build/codegen；完整root race |
| 6 | SDK holder 安全整数边界 | 已集成 | 8f52e08 → 4df363d | frontend通过；独立域/root PASS；完整root race |
| 7 | 两种 skin 的 holder 输入校验 | 已集成 | 0c3138d → 8f889dbe |T1 PASS；381 tests/build/types/codegen；完整root race |
| 8 | 管理端缓存按实例与身份隔离 | 已集成 | 1f02131 → 43b5ff4 | SDK/host tests；独立域/root PASS；完整root race |
| 9 | 其余 SDK mutation 请求契约消漂移 | 已集成 | a9a1298 → 30372f03 | T1 PASS；388 tests/build/types/codegen；完整root race |
| 10 | 科目配置经济效果验收样例 | 已集成 | 46ac7b6 → 2ff8ceeb | PG race/vet；独立域/root PASS；完整root race |
| 11 | Exchange 严格消费可用余额 | 已集成 | 6e62cef → c353622 | PG race/vet；独立域/root PASS；完整root race |
| 12 | 原子 Capture 门面 | 已集成 | 241905e → 21f5ba5 | 独立域/root PASS；完整root race；Minor文档已修 |
| 13 | 签名资金流程可运行组合示例 | 已集成 | 1a585d0 → 06ea1fa0 | PG race 12场景/vet；独立域/root PASS；完整root race |
| 14 | credits 消费接入原子 Capture | 已集成 | a29d202 → 4eed87c5 |PG race/vet；独立域/root PASS；完整root race |
| 15 | USD 估值读模型示例 | 已集成 | 3d69a74 → 98d21ee8 |T1 PASS；race/vet/run，覆盖率 94.8%；完整root race |
| 16 | 市场报价与执行扩展 ADR | 已集成 | 8f4c9e5 → c8db20f8 | T1 PASS；文档/来源核实；完整root race |
| 17 | 可选 Go modules 独立消费验证 | 已集成 | ed37bc4 → 46bc9ccc | T1 PASS；三host及失败传播/CI检查通过；完整root race |
| 18 | web 生产依赖 advisory 修复 | 已集成 | 158135e → 50b52c6 | prod audit0；T1 PASS；完整root race |
| 19 | 库接入与兼容迁移文档 | 评审通过，待集成 | 7dab106 / codex/ledger20-t19 | T1 PASS；Go/TS文档片段编译、API/codegen/link检查 |
| 20 | 最终集成验收与交付 | 待实施 | — | — |

## 执行记录

- 第一轮整合 `97e33362abd436c072114ab863079ac65a78fad1` 已推送计划分支。`make test` 实际运行 `go test -race -timeout 15m -count=1 ./...` 全部通过；root 101.431s、postgres 277.257s、service 140.870s。独立 Go 子 modules 与 React 不在这次 root 命令覆盖内。
- 使用本次专用临时 PG17 实例，fixture 每测试独立数据库。
- bus #30..49 对应轮次 1..20。缺少独立 OS PID 的会话内 worker 在 submitted/reviewed 时保守保留 writer lease；整合使用冻结 commit 的独立 delivery worktree，通过 wt integrate 后才标 bus integrated，再删除原任务 worktree。不会提前把未合并任务记为 integrated，也不修改 lease 或关闭 guard。
- 所有实现者只提交各自任务分支，主控负责评审、串行整合与最终推送。

- 第二轮整合 `7a0f5961759afdf829bddcc6959dc00605c75ea4` 已推送；全 root race 通过（root 133.449s、postgres 287.375s、service 136.863s）。

- 第五轮整合 `1b01b974` 已推送；完整 root race PASS（root 133.825s、postgres 288.004s、service 183.686s）。本轮 SDK 的 build/typecheck/client/codegen 证据单独记录，root Go gate 不替代 frontend 验证。

- 第六轮整合 `4df363d8` 已推送；完整 root race PASS（root 112.806s、postgres 283.516s、service 133.680s）。Task5/6 的client.ts自动合并无冲突；最终frontend回归仍会覆盖组合基线。

- 第十二轮整合 `21f5ba58` 已推送；完整 root race PASS（root 140.544s、postgres 290.942s、service 145.674s），覆盖Capture和ReservationReader。

- 第三轮整合 `83060d62` 已推送；完整 root race PASS（root 134.777s、postgres 300.078s、service 147.368s），目标 exponent 回归与 Capture 已在同一基线运行。

- 第四轮整合 `0c9417e3` 已推送；完整 root race PASS（root 115.155s、postgres 286.666s、service 189.708s）。

- 第十一轮整合 `c3536221` 已推送；完整 root race PASS（root 124.382s、postgres 303.172s、service 126.353s），Exchange 与 Capture 在同一基线通过。

- 第十八轮整合 `50b52c66` 已推送；完整 root race PASS（root151.064s、postgres296.455s、service149.060s）。生产依赖 audit0，开发工具链残留仍见 dependencies.md。

- 第八轮整合 `43b5ff4a` 已推送；完整 root race PASS（root141.844s、postgres303.981s、service157.987s），cache host tests 已接入 frontend CI。

- 第 7 轮整合 `8f889dbe` 已推送；完整 root race PASS（root 173.277s、postgres 296.913s、service 146.017s）。

- 第 14 轮整合 `4eed87c5` 已推送；完整 root race PASS（root 167.400s、postgres 302.620s、service 159.064s）。

- 第 15 轮整合 `98d21ee8` 已推送；完整 root race PASS（root 139.199s、postgres 298.847s、service 182.947s）。

- 第 16 轮整合 `c8db20f8` 已推送；完整 root race PASS（root 135.754s、postgres 300.013s、service 131.866s）。

- 第 17 轮整合 `46bc9ccc` 已推送；完整 root race PASS（root 149.900s、postgres 297.472s、service 165.747s）。

- 第 9 轮整合 `30372f03` 已推送；完整 root race PASS（root128.174s、postgres299.292s、service152.435s）。Task19硬依赖已解除并开始文档汇合。

- 第 13 轮整合 `06ea1fa0` 已推送；完整 root race PASS（root131.990s、postgres303.096s、service162.721s）。签名示例与既有Capture、Exchange在同一基线验证。

- 第 10 轮整合 `2ff8ceeb` 已推送；完整 root race PASS（root178.427s、postgres311.475s、service149.930s）；配置示例与签名示例同时通过。

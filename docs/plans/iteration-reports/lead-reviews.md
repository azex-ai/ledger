# 主控复核记录

## Task 2 — Codex 第二意见：PASS

范围 `97e3336..ae0c1b7`。主控逐行阅读 frozen 汇总改动、完整新增回归及 pending 测试修改，并核对 GetEffectiveAccountPolicy SQL 固定 holder 匹配；policy ID 已唯一绑定 holder，无需额外跨 holder 汇总键。新的 key 将 currency 维度加入 frozen netting，classification 不参与，合法同币 pending 确认保留。closed/min_balance 两分支与查询优先级没有改变。

测试明确拒绝 USD 扣减+PTS 入账、保留用户/对手方四个余额与四类持久行数，解冻后同 key 成功；不是把 bug 复现认证为正确。金额与分录方向沿用既有 core.SignedAmount。独立域评审 02-review.md 的真实 PG race 检查通过，无 blocker。主控未重复运行其相同窄测试；串行集成由 wt 运行完整 Go race gate。已明示的 holder-wide policy 更新跨币种并发锁边界不因本次修复而变化。

## Task 5 — T1：PASS

范围 `97e3336..d376f03`。主控读取全部 client/type-test/request-test diff 与报告。方法参数直接引用生成 ReserveInput，修复错误时长键并增加验证余额选项；序列化测试检查真实请求、金额字符串、幂等头、省略字段与 0/false；编译检查捕捉缺失可选字段和旧字段。build/typecheck/client 35 tests/codegen 已有实际通过记录。

旧 JS 或带额外字段的变量仍可能绕过 TypeScript excess-property check；本轮承诺是修复公开 TS 输入契约，不是运行时未知字段过滤器。消费迁移说明留 Task 19。额外静态 gate 误报和既有 index-key 告警在 05.md 如实记录；此次 diff 不修改这些文件，不把额外扫描标为绿。

## Task 16 — T1：PASS

范围 `97e3336..8f4c9e5`。主控阅读 ADR 全文和报告，核实当前 API 与建议宿主字段分别标识。包括状态/失败矩阵、两类实例、真实数量入账、防重复外部执行、已提交恢复、未知状态资金占用、跨库事务限制，以及 USD valuation 缺价语义。固定换算 codec 不承载市场成交回执；外部成交失败/未知与 DB 入账失败的处理区分正确。

官方来源已核对，链接与测试名称检查有证据。后续 adapter 测试明确未实现、未运行。本轮为设计交付，不宣称支持 live swap，不新增金融执行或依赖。

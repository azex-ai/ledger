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

## Task 3 — Codex 第二意见：PASS

范围 `ab3a5d1..2239814`。逐行检查 exponent guard、调用次序与新旧测试：三个入口的目标整数比较先于 decimal 算术，MinInt32 不取负；Round/ConvertAt 保留负精度与未知 rounding mode 的原有回退，Allocate 保留非负约束。36 位正负 1/3 分配测试断言具体余数和守恒。旧版只安全执行 ±37 复现；极值只在修复后运行。未新增导出符号。负精度 RoundUp 可能产生超出存储范围的结果属于既有 helper 语义，入账仍由存储校验约束，报告已明示。等待独立域席位结果后集成。

## Task 15 — T1：PASS

范围 `ab3a5d1..3d69a74`。阅读全文、代码和测试，检查 Currency UID 身份、持仓精度、价格时效和 exact decimal 乘积。量价均先 magnitude 校验，合法乘积保持 36 位后求和；中间值与合计继续受 core 上限约束。缺价/过期/未来价格均不混入 subtotal，只有完整覆盖输出 total；JSON 不把未知值编码成零。README 明示非负持仓、重复 UID 拒绝、fixture 价格、无兑换权。已有 race/vet/go run 证据满足 T1，未重复同一测试。

## Task 6 — Codex 第二意见：PASS

范围 `ab3a5d1..8f52e08`。主控完整读取新增校验器、client 调用处、89 项新增测试、全部公开 client 方法及手写 DTO holder 字段。16 类请求在 async request 内 fetch 前拒绝；24 类响应在成功 envelope 解码后、方法拆 list 前拒绝，unsafe later-row 使整个响应失败。raw JSON 两个相邻 int64 值的测试避免先由 JS stringify 合并证据。保留负 system/0 的服务端规则，metadata/actor/amount-map 不误识别为 holder。当前公开 DTO holder 位置均有 selector；未来新 DTO 需同步扩充，限制已写入报告。

Numeric wire 不变，不声称完整 int64。响应拒绝可能发生在服务端写入之后，BREAKING 提醒保留幂等键，不宣称自动回滚。所有 unsafe numeric 参数测试直接使用 Promise rejects，未出现同步 throw。Preview fixture 补合法 holder 的授权修改不弱化 totals 断言。独立域席位仍在执行，取得结论后才整合。

## Task 4 — Codex 第二意见：PASS

范围 `de583aa..cf03361`。主控完整读取两入口 diff、新 PG/exporter 回归、04.md，并核对 ReserveInput/CreateBookingInput.Validate 第一项即为 magnitude 校验，错误本身不展开金额。合法属性仍经 StartSpan 唯一过滤入口，避免直接 SetAttributes 绕过 PolicyMinimal。失败也创建并结束 span，记录错误；只将校验耗时移到 span 之前。

测试对 Full/Minimal 与 Reserve/Booking 组合验证有限非法数值、七类持久行数不变、无持仓占用以及合法金额回归；没有用旧版巨大指数施压。原边界后续转换都位于校验之后，无迁移/签名/事务变更。实现者真实 PG race 39.781s 与 vet PASS；独立席位静态检查已无 blocker，窄 PG 验证在排队，完成后集成。

Task4 域评审收尾：04-review.md 独立静态 PASS，无未决点。沿用已有真实 PG 证据，取消无新增问题的重复窄测试；本席未声称运行 PG。最终集成门禁会覆盖新回归。

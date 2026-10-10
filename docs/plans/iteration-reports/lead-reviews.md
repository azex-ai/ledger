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

## Task 12 — Codex 第二意见：PASS

范围 `767e208..d22e40d`。主控完整阅读 Capture / Reader、新增全部回归、导出变更和12.md，并查验 LockForTemplates、Settle/SettlePartial 既有实现。无锁读取绑定 holder/currency；模板锁及两枚幂等键先于 settlement 行锁；行锁内复验 live 状态/期限/金额。settlement 只改变 hold，实际 journal 随后在同事务内生成并检查目标 available 净减。顶层失败统一 rollback；clone 无 savepoint 的传播责任明确。

幂等 metadata 复制并绑定 reservation/mode/template，重放相同增量及最终已完成后的重放有真实 PG 证据；改变 mode 的安全错误区别已冻结。普通路径 unsigned、未修改 reservation.JournalUID、复杂 memo 模板须显式组合均有说明。Reader 不扩张现有接口，复用 WithDB 后的查询与 converter。并发同 key + deposit、六次0.2仅五次成功、过期/释放/坏方向/超额及外层 rollback 的测试断言最终余额和持久化效果。未发现 blocker；独立域席位进行中。

## Task 18 — T1：PASS

范围 `4316088..158135e`。读取全部 manifest diff、解析 lockfile 比较每个版本变更，并读完整 dependencies.md /18.md。40 个版本变化限定在 Next /对应 ESLint、sharp平台与libvips、source-map-js；不存在无关 major 或 force/override。选择官方16.3.8安全补丁，有维护者 release/advisory 和 registry 兼容性证据。干净安装后 production audit0、SDK/Next build/typecheck/257 tests 和良性原生sharp smoke实际通过。

完整audit仍20个开发依赖包条目（含critical），逐项列依赖来源；没有把“dev”当成无风险或完整供应链无漏洞，也没误称baseline静态扫描全绿。该补丁按已冻结Task18生产范围通过，开发工具链后续工作和最终集成基线验证仍应明确保留。

Task12 scoped 文档闭环：独立域席位代码 PASS，提出 1 条 Minor 术语校正。作者 `241905e` 仅改报告两行；主控核对 diff 与余额公式后确认：available 分类账面余额与 GetBalanceBreakdown.available（减 held）明确区分，settlement 复核的是预留额度。无源码变更，未重复测试，T2 通过。

## Task 11 — Codex 第二意见：PASS

范围 `f13d0ae..6e62cef`。主控完整读取75行生产diff、两个新增测试文件和11.md。available 净额按原 SignedAmount/NormalSide 求和，其他用户分类独立净额检查，无法用pending/locked或不同pending分类抵消；同维度真实净零正例保留。holder/currency 已固定后按classification建map足以识别完整维度。系统对手方限制与当前Render一致，classificationRoles签名保留，可与Capture直接组合。

既有预锁、Reserve、Settle、双journal执行和replay路径没有变化，只加强提交前实际entries检查。12个PG错误模板各自仍双分录平衡，旧版全部错误接受；新断言要求完整回滚且同key后续可完成正确兑换，不把仅“journal平衡”认证为经济结果正确。两种normal_side、多个available分类、净零重放均有对照。实现PG race13.195s/vet PASS，域席位进行中；无需重复同套PG。

## Task 17 — T1：PASS

范围 `9b09eb5..ed37bc4`。主控完整阅读脚本、三个实际consumer源码、Makefile/CI diff、CONSUMING与17.md。每个host仓外新go.mod且GOWORK=off，显式candidate replacements，不借依赖模块replace；readonly build/run和生产import图检查真实执行。root不吸入可选SDK，所有host生产imports排除testfixtures/Docker。trap只清本次mktemp目录，带空格路径、失败码42传播、拒绝仓内TMPDIR有一次性验证证据。

EVM/R2函数用于编译接口接线，未声称已连接RPC/对象存储。R2 miniotest显式replace仅使tidy解析现有测试依赖，生产imports与go.mod/go.sum元数据区别准确；尚未发布的零伪版本不会被本gate掩饰成远程可用。保留原root三种换算检查，CI仍复用既有make目标。无新增依赖和业务API。
## Task 14 · root Codex 第二意见（a29d202）

已核对 BASE c5d0f1e 到 a29d202 的全部示例 diff、新回归和报告。正数消费委托 Capture；零金额 final Release 与 partial 拒绝保持；stream Finalize 没有变成第二次扣款。业务 quote map 克隆后传递，保留字段由 Capture 添加，原 settle/charge suffix 不变。调用者事务须传播错误、原始 journal 可绕过 hold、unsigned discharge 的限制在 README 明确。

v3 fixture namespace 只用于新例库，setup/scenario 首行只读 guard 拒绝任一旧 demo journal/reservation；既有 v3 重跑允许。测试核对拒绝后六表、双币余额与 hold 不变，没有将重命名旧业务事件描述为迁移。schema bootstrap 仍先执行及禁止并发两版本的范围明确。账面 available classification 与扣 hold 后 spendable 数值已逐项核对。作者 PG race 12.979s；本席未重复相同测试。根本行为与冻结契约一致，PASS，独立域席仍需结论。

## Task 13 · root Codex 第二意见（1a585d0）

已核对 Authorize/attestJournal/PostAuthorized 原实现及示例全部生产源码、测试、README。签名和验证均在 RunInTx 前，内部先取模板/settlement 全锁再入账和结算；所有错误传播且仅提交成功返回 receipt。真实 pg_stat_activity 检查远端边界，settlement 超额测试让可入账 charge 与 receipt 全回滚。稳定 EffectiveAt 与双 key 重放不重新签名、不增加账务行。

signed replay 三项 auth 材料全空时按已裁决方式验证相关余额历史；部分缺损或新坏签名仍拒绝。VerifiedBalance 不冒充当前状态检查或精确单笔验签；其他 unsigned/坏签名历史导致保守拒绝的用例与文档齐全。模板、reservation 关联和跨操作不可变 event 由宿主负责，未把 private helper 冒充任意输入 API。unsigned discharge 原 hold 至 expiry 的门槛 80−60=20 有真实 PG 断言；无 distributed atomicity 或草稿恢复承诺。作者目标 race 6.183s；本席未重复相同测试。PASS，独立域席待安排。

## Task 8 · root Codex 第二意见（68452c9 / 1f02131 scoped）

核对全部生产变更与新增真实 QueryClient/provider/hydration 测试。client 配置被快照化，默认 UUID scope 随实例稳定；显式 backend/identity 冻结且拒绝已知 API key 误填。query、prefetch、mutation keys、失效及乐观回滚均包含同一 scope，旧 pending 完成的原回调隔离有实际异步测试及去掉 mutationKey 的反事实失败证据。provider 以 scope 重挂子树，从而重置 preview 与本地 mutation 状态；显式 scope 的真实性及隐藏 cookie 变化由宿主负责。

宿主 server-only helper 验证 session 后仅序列化公开 expiry，backend 是非秘密部署标识。layout 与两动态页的 prefetch 使用同一规则；provider prop 更新与 JSON hydrate 测试覆核。auth 格式未改，同 token 视为同会话；生产新增 LEDGER_CACHE_BACKEND_ID 是须记录的配置迁移。构建无 env 通过不等同生产 request 已配置，文档如实区分。

原变更新增 web/test 未接入 CI 的 Minor 已由 1f02131 修复：直接 Vitest 命令继承 web cwd，位于 SDK build 后，无忽略失败；同一次修订纠正静态 gate 全量 10 个基线文件的报告。主控核小 diff PASS，功能源码未变，不重复全套。独立域席另执行 build、21 SDK 与 8 host 范围测试；额外静态 gate FAIL2 仍如实保留。PASS，待独立席最终报告归档。

## Task 7 · root T1：PASS（0c3138d）

完整核对 parser、两套 skin 的十个页面与 unit/MSW 组件测试。ASCII 十进制整数字符串经 safe integer 检查后才形成请求，reject 小数、指数、尾随文本和越界值；负 system holder 在查询保留，模板执行只接受正用户，空可选筛选与零 sentinel 保持既有含义。合法最小负值和最大正值有实际请求参数断言，不只测 parser。bad input 不发 fetch 或 mutation；加载和错误分支未产生新 hook 条件调用。

作者 build/types/codegen/ESLint 与 46 files / 381 tests 通过，39 项目标/parity 检查通过。主控逐行静态复核无 blocker；完整组合回归在最终 frontend gate 运行。基线 index key 告警留 Task20 明确修复，不冒充本轮全静态通过。

## Task 10 · root Codex 第二意见：PASS（46ac7b6）

完整阅读 main、三个真实 PG 测试、README 与报告，核对已安装 preset/自定义 POINTS 配置的 NormalSide、BalanceRole 及字面量最终余额。逐币 journal 平衡与独立经济 oracle 分别验收；fee 四行全部反向仍平衡但增大用户余额，错误 gift rate 仍平衡但产出10而非2，两种负例都由外层事务传播 oracle 错误而完整回滚。正常幂等 UID/行数不变与改变汇率冲突有实证。

示例复用现有配置类型和 bundle，没有增加 DSL。普通 executable 是提交后审计，不声称自动回滚已提交业务；fee journal 不自动保护 hold 的宿主责任明确。独立域报告 10-review.md PASS；作者 PG race 三个测试2.970s、vet通过，本席不重复相同 suite。可串行集成。

## Task 9 · root T1：PASS（a9a1298）

完整阅读client diff、19项运行时测试、23项编译反例和09.md；对照生成paths/components、实际preview handler和idempotency middleware。operation body提取直接复用生成契约，scalar adapters保留header/body协议，Classification保留现有必填策略。Booking metadata收紧为真实string map，新增optional字段可用，旧DTO迁移明确记录。

preview canonical amounts 原样发送，单amount便捷输入只转换这一键；混合或缺失形式在async方法内fetch前拒绝，不改变原对象。编译测试使用真实生成类型及fresh literals，避免宽对象赋值漏测字段；原holder测试只修不合法fixture，没有移除危险holder断言。作者build/types/codegen/client143项、全包388项PASS。额外静态2类基线失败已计入Task20；最终合并Task7后的组件回归另验收。无blocker。

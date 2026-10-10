# 通用 Ledger：20 轮交付验收

原始审查基线为 `d00fdebef4199d331787057127cc29a0ecb7cd87`。20 轮改动已全部集成，最后一轮冻结提交为 `e3870ff11b96c42b2bc11e023b9e68558245399f`，集成提交为 `d9b40a83fbdb74d7a741e5ecf95239c06ec2b7c5`。独立 [whole-branch Final](../../plans/iteration-reports/20-review.md) 与主控第二意见均 PASS，无需追加修复波次。这里保留已实际观察的实现、本地验收与集成门禁证据；最终 main 提交的 Linux CI 状态以 GitHub checks 为准。

## 架构结论

保留每币种数量账本、USD 估值读模型、宿主报价与执行编排这三层。BTC、法币、积分和可互换礼物数量都可以配置为 Currency；不同币种各自借贷平衡，USD 估值不会把原币数量改写成 USD，也不自动赋予资产兑换或赎回资格。独立礼物实例、批次到期、权益资格及链上资产托管身份仍由宿主表达。

本次迭代收紧了原有层之间的契约，没有引入 rate 持久化、通用流程 DSL 或强制经过 USD 的兑换路径。FixedRate 继续提供确定的定比换算；真实市场按数量报价、费用、有效期、最小输出和外部成交恢复仍属于宿主或未来可选 exchange package。数据库回滚不能撤销外部成交，详见 [市场执行 ADR](../../adr/2026-10-10-market-execution.md)。

## 已交付能力与缺陷闭环

| 范围 | 本次结果 | 证据入口 |
|---|---|---|
| 冻结策略与金额边界 | 冻结按 policy + currency 判断净额；Round / ConvertAt / Allocate 在目标指数算术前检查计算边界；Reserve / Booking 先校验再展开金额 | [02](../../plans/iteration-reports/02.md)、[03](../../plans/iteration-reports/03.md)、[04](../../plans/iteration-reports/04.md) |
| SDK / UI 身份与契约 | numeric holder 超出 JS safe integer 时拒绝；两种 skin 严格解析十进制；Reserve 与其他 mutation 消费真实生成 DTO；保留有明确转换规则的便捷参数 | [05](../../plans/iteration-reports/05.md)、[06](../../plans/iteration-reports/06.md)、[07](../../plans/iteration-reports/07.md)、[09](../../plans/iteration-reports/09.md) |
| 管理端缓存 | query、mutation invalidation、server prefetch 与 hydration 共用 backend + identity scope；旧 pending mutation 只影响原 scope；宿主真实 session 刷新参与测试 | [08](../../plans/iteration-reports/08.md) |
| 原子资金流程 | 配置经济效果示例、available 余额约束的 Exchange、原子 Capture、签名组合示例及 credits 消费接入 | [10](../../plans/iteration-reports/10.md)、[11](../../plans/iteration-reports/11.md)、[12](../../plans/iteration-reports/12.md)、[13](../../plans/iteration-reports/13.md)、[14](../../plans/iteration-reports/14.md) |
| 估值和扩展边界 | USD valuation 保留原币，显式缺价/过期/未来价格，partial subtotal 不充作完整 total；市场执行保持 ADR | [15](../../plans/iteration-reports/15.md)、[16](../../plans/iteration-reports/16.md) |
| 独立消费和文档 | 可选 Go modules 的仓外消费 gate；SDK tarball 六个非 Hero 入口消费；接入、迁移和兼容边界写入指南 | [17](../../plans/iteration-reports/17.md)、[19](../../plans/iteration-reports/19.md)、[20](../../plans/iteration-reports/20.md) |
| 最终验收阻断修复 | destructive migration 使用独立 PG cluster；幂等匹配按落库微秒 floor；两种 skin 的 preview 重复行稳定 key 与静态布局槽位 | [20](../../plans/iteration-reports/20.md)、[测试环境](../../TESTING.md) |

原始 R1–R6 分别由 Task 2、6/7、8、4、5、3 修复；修复前探针保持历史原样，不把其“成功复现缺陷”当作修复通过。具体兼容变化见 [BREAKING](../../BREAKING.md)，各轮完成状态见 [进度](../../plans/2026-10-10-ledger-20-progress.md)。

## 最终验证

四个 Go module（root、chains/evm、anchors/r2、anchors/r2/internal/miniotest）分别完成 build、vet、uncached race 和 golangci-lint。root `make test` 总耗时 355.25s，其中 postgres 348.676s；EVM 1.383s、R2 4.108s，miniotest 实际执行命令但模块没有独立 test files，R2 suite 实际启动 MinIO。三个仓外 Go consumer 的 tidy/build/run 与 import 边界检查、sqlc-diff、API/文档/workflow 合约检查通过。测试专用 PG 独立 cluster 与微秒幂等回归都有确定性反事实 RED 和修复后 GREEN。详见 [第 20 轮完整命令和结果](../../plans/iteration-reports/20.md)。

前端在最终组合基线上完成 SDK ESM/DTS build、typecheck、50 文件 423 tests，以及宿主 8 tests、typecheck、Next.js production build 和 lint。SDK 与宿主 delivery-gate 均为 `FAIL=0 WARN=0`。原始 SDK `FAIL=2` 记录对应三处注释误报和八处 JSX index key；注释从源文件修正，生成 schema 由 codegen 更新，未修改 gate。重复 preview 回归覆盖两种 skin，保留合法重复、行顺序和 DOM 身份。

仓库外临时 npm consumer 安装实际 `npm pack` tarball，六个非 Hero 入口均完成运行时 import 和 TypeScript 编译，并确认 `@heroui/react` 没有安装；现有 Next 构建覆盖 Hero 入口。未 publish，未覆盖历史 consumer 探针输出。源码 checkout / tarball 消费通过不等于远程 release tag 已发布或已验证。

生产依赖 `npm audit --omit=dev` 为 0；完整开发依赖 audit 仍有 20 项（1 low、4 moderate、14 high、1 critical），详情见 [依赖审计](dependencies.md)。这不是全部依赖无风险的声明。

## 保留的使用约束

- `Capture` 是 Go-only 的普通 unsigned 门面，没有新增 REST / React SDK 端点；签名部署使用现有交易外授权、交易内 `PostAuthorized` 组合。Settle 只解除 hold，不能单独代替扣账。事务内错误必须传播，RunInTx 没有隐式 savepoint；零用量使用 Release。
- SDK numeric wire 仅支持 JS safe integer 范围，未承诺完整 Go int64。新的外部大整数 / string ID 可以由宿主稳定映射到安全 holder；已有大 int64 holder 的账户必须通过 int64-capable 后端或另版 string wire 接入，不能静默重映射成新账户、截断或借 metadata 绕过身份边界。
- 显式 cache scope 是宿主对真实 backend / identity 的声明。隐藏 cookie 的身份或权限变化必须更换真实 scope，或重挂 provider 并使用新 QueryClient，清除已有子树本地 preview / mutation 状态；同一个显式 identity 不会因为 apiKey 改变自动证明成另一个用户。SSR 与浏览器需共享非秘密稳定 scope。
- 估值完整性、内部 booked coverage 和外部储备真实性是不同保证。市场 swap 执行、成交对账、链上托管和远程发布仍有各自的工程边界。
- 本地验证不替代最终 Linux CI 的三条 30 秒 fuzz、真实 anvil E2E 和远程流水线。未执行或未观察完成的项目不标 PASS。

后续优先用真实宿主接入验证已有 Capture / Exchange / signed composition，再根据实际市场执行需求实现 ADR 中的窄接口。无需为“通用”继续扩大核心账本对业务或行情存储的依赖。

第 20 轮实际集成再次执行 uncached root race：root 198.110s、postgres 370.515s、service 193.160s，全部通过。20 轮完整进度与提交可追溯到 [进度表](../../plans/2026-10-10-ledger-20-progress.md)。

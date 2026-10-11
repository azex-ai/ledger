# shadcn 管理端真实栈验收

基线：`99871921df986a738272ae4a7f33fb357f5f6e03`。目标是验证一个独立 Go / Next.js 宿主以最小接入消费当前 Ledger 包后，能实际完成管理工作；修复本轮确认的权限、数值和交互歧义。沿用用户的修复后提交推送授权，不发布 package/tag。

## 管道与边界

Input：用户管理端场景、现有 read/write/admin scopes、当前包公开入口。Output：可运行最小宿主、可复现验收脚本、截图及问题处置报告。Handoff：包消费/环境 → 浏览器验收与权限/金额专项核对 → 串行修复。Gate：真实 PostgreSQL、production Next.js、真实登录/client/BFF/Go handler、定向回归与必要的 build/test；涉及权限/金额/跨端契约按 T2，独立域评审 + 主控第二意见 + 一次 Final，最多一波 Final 修复。Memory：项目记录复现和接入限制；无新跨项目模式则不重复写入。

保留已有 shadcn 视觉和公共组件，不重新设计主题。最小宿主实际消费构建后的 npm tarball 和 Go module，不能用仓内源码 alias 或 dev 无鉴权模式代替接入验收。宿主提供身份与服务端凭证；不把任意浏览器 role 参数作为授权证明，不将管理员 API key 放进客户端。没有在线资金、外部转账或生产数据操作。

## 验收矩阵

| 范围 | 可观察通过条件 |
|---|---|
| 独立接入 | 新宿主生产构建并启动，使用当前候选包；记录源码 SHA、实际构建及包版本；没有 HeroUI 运行时依赖 |
| 登录与代理 | 匿名、错误口令、过期/篡改 session 被拒；有效登录后真实浏览器 → BFF → Go API 成功；敏感凭证不出现在 HTML、客户端包或浏览器请求 |
| 权限 | 按后端现有 read/write/admin 与 capability 契约验证允许和拒绝；绕过按钮直接调用 API 仍被拒；隐藏导航不当作授权 |
| 配置管理 | 币种、科目、journal type、模板的查询和允许的新增/编辑/停用可用；重复、非法值及约束错误有可理解反馈 |
| 账务管理 | journals/entries、holder/currency 筛选、详情、booking 状态、reservation、reconciliation/trial balance 等已提供功能能真实调用；禁用 add-on 与未配置能力明确反馈 |
| 数值 | 使用 USD/USDC/BTC/POINTS 等不同指数与大额、微小额、零值、负值；字符串 wire 不丢精度，不把不同 Currency 相加成金额；显示与独立后端/SQL oracle 对照 |
| 状态与歧义 | loading/error/empty/data 完整；pending 不当成功；Settle/扣账、账面/可用/held、借贷方向、不可逆更正及权限不足的语义准确 |
| 界面 | 桌面与 390px，无整页水平溢出；表格/长金额/标识可读；表单标签、键盘、操作结果和错误状态可观察 |
| 可重复验收 | 保留复现步骤、测试数据、权限矩阵、截图路径、原始测试结果与修复前后证据；未执行项明确标记 |

## 执行约束

- 主控 worktree：`/Users/aaron/projects/_worktrees/ledger/codex/shadcn-admin-acceptance-261011`，分支同末段。每个 worker 独占自己的 worktree。
- 真实栈使用专用 PostgreSQL 集群/数据库、明确端口。浏览器验收期间不得在同库运行重建/迁移测试。
- 优先复用现有 fullstack 接入与公开 API，先确认缺口再补窄适配；不扩成新的通用 RBAC 产品或全库审计。
- 保留 shadcn / HeroUI 共有 headless 契约；本轮主验收 shadcn，影响共有逻辑时验证另一 skin 的编译与既有契约。

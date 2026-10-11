# shadcn 管理端验收

状态：进行中。基线 `99871921df986a738272ae4a7f33fb357f5f6e03`；本报告在真实栈复验完成前不作为通过结论。

## 接入与验收边界

以独立 Go module 和 Next.js 宿主消费候选 Ledger 包，前端使用构建后的 npm tarball。宿主负责登录、session、同源代理和服务端 API key；Ledger 负责 scope/capability 判定。测试身份为 read、write、admin、read + deposit_review。生产构建、真实 PostgreSQL 和浏览器操作均使用专属资源。

本轮验证管理 UI 的现有公开能力，包括配置、记账、查询、预留、对账和可选能力的拒绝路径。不同 currency 保留各自单位；跨币种数量求和不代表 USD 估值。没有生产资金、外部交易或线上资产操作。

## 基线发现

| ID | 现象与影响 | 当前证据 / 状态 |
|---|---|---|
| A01 | 既有 dashboard session/BFF 没有 Origin 边界；SameSite=Lax 不能阻止同站不同 Origin 的请求 | session 跨 Origin 登录/退出接受，资金/配置写入待真实浏览器复现 |
| A02 | 登录 JSON 的 `password: 42` 导致 500 | 已通过既有 production dashboard 复现 |
| M01 | `1234.56` 显示为 `1,234.5`，18 位小数也被缩略，管理页面没有完整值入口 | 源码与渲染探测确认；极小值仍非零，没有将此误报为归零 |
| M02 | 同一 journal 的 USD 与 BTC 数量显示为无单位的 Debit/Credit 总额 | USD `1.23` + BTC `0.00000002` 可显示为 `1.2300`；这不是有效的资产估值 |
| M03 | Balances 未清楚区分账面余额与扣除 reservation 后的可用余额；趋势默认首币种且标题没有单位 | 待用 booked / held / available 及 Settle 实际前后值复验 |
| M04 | Journal 的 Fund Flow 箭头暗示 Debit→Credit 是资金流；Entries 的 ID 列实际填入 entry_type | 源码确认，按真实借贷语义修正文案与列 |
| U01 | 创建 currency 可把 `2.5` 作为 exponent 发出，列表没有显示 exponent | 渲染探测确认；需前端拒绝非整数并展示计量精度 |

## 数值基准

独立 oracle 使用十进制定点值，避免拿 UI 自己的格式化函数证明自己正确。存储为 NUMERIC(30,18)，因此大额测试选用合法的 12 位整数加 18 位小数，而不是向数据库写入超出其整数范围的 JS 安全整数边界。

- Holder 1001：USD `1234.56`、USDC `1.234567`、BTC `0.00000001`、POINTS `42`、ATOM `999999999999.123456789012345678`。
- Holder 1002：ATOM `0.000000000000000001`。
- USD reservation `34.56`：booked `1234.56`、held `34.56`、available `1200`；单独 Settle 只解除预留，不应减少 booked。
- 混合 journal：USD `1.23`、BTC `0.00000002` 分别借贷平衡，不能用跨币种总量代替分币种核对。

最终修复提交、实际执行矩阵、截图及复验结果将在验收完成后补入本报告。

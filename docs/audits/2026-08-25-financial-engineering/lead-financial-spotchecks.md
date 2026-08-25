# 审计补充 — Team Lead 对 territory A 的抽查

> 原 territory A（金融正确性）的 agent 未产出即失联，已重派范围更窄的替补。
> 本文记录我在等待期间自己做的两项抽查，**其中一项是被证伪的怀疑**。

## 结论

**money path 的类型纪律是干净的。** 两项抽查，一项完全通过，一项发现死代码但不可达。

## 抽查 1：float64 是否渗入 money path — 通过

`financial.md` 的硬规则是「金额一律 decimal，绝不 float64」。全仓 grep（排除测试与生成物）：

- `observability/prometheus.go` —— 8 处 float64，**全部是 Prometheus 的 gauge/histogram 要求**
  （该库的 API 本身只接受 float64）。属可观测性，不属记账。`decimalToFloat` 命名明确、
  注释写明用途。**判定：正当，非违规。**
- 其余源码零命中。

## 抽查 2：`anyToDecimal` 的 float64 分支 — 我的怀疑被证伪

- **位置**：`postgres/convert.go:122-124`
- **初始怀疑**：`anyToDecimal` 有 `case float64: slog.Warn("possible precision loss"); return decimal.NewFromFloat(val)`。
  它被用在 **10 个聚合查询结果**上，其中 `postgres/reserver_store.go:270` 的
  `activeReserved` 直接参与 **Reserve 的可用余额计算** —— 也就是决定这笔钱能不能动的地方。
  若该分支可达，就是在最要紧的位置静默丢精度，且唯一信号是 `slog.Warn`。
- **实测**（跑真库、打印 pgx 返回的具体 Go 类型）：

  | 查询形态 | pgx v5 返回类型 | 精度 |
  |---|---|---|
  | `COALESCE(SUM(numeric),0)` 有行 | `pgtype.Numeric` | 完整（`12345678901234567890 × 10⁻¹⁸`） |
  | `COALESCE(SUM(numeric),0)` 空集 | `pgtype.Numeric` | 0 |
  | 裸 `SUM(numeric)` 空集 | `nil` | 由 `v == nil → decimal.Zero` 正确处理 |

- **判定：怀疑不成立。** pgx v5 不会为 numeric 列返回 float64，该分支在当前代码下不可达。

## 发现

### [Minor] 死的防御分支在金额路径上选择了「警告并继续」而不是「拒绝」

- **位置**：`postgres/convert.go:122-124`
- **判定**：CONFIRMED（分支存在且行为如此）/ 不可达性亦 CONFIRMED（见上表实测）
- **失效场景**：当前不可达，所以**没有活跃风险**。但它是一颗被埋下的、朝错误方向倒的种子：
  若将来有人给某个聚合查询加了 `::float8` 转换（或换了驱动），这条路径会被激活，
  而它的行为是**丢精度后继续**，唯一信号是一行 `slog.Warn`。

  两个叠加因素让这个信号更弱：
  1. 用的是**包级 `slog`**，不是消费方注入的 `core.Logger` —— 库消费方配置的日志管道
     根本收不到它（`audit-concurrency` 在 `event_store` 的「claim lost」上报了同一形态）
  2. 金额精度损失是**静默且不可逆**的：一旦 `decimal.NewFromFloat` 执行完，
     原始精度无从恢复，后续所有对账都会在错误的数上平衡

- **为什么值得记**：`financial.md` 对这件事的措辞是绝对的（「绝不 float64」）。
  一个绝对规则的兜底分支不应该是 warn-and-continue，应该是 `return error`。
  这是「fail-open vs fail-closed」在类型转换层的实例 —— 与本次审计反复出现的主题同源。

- **可选修法**：把该 case 改成返回 error。它当前不可达，所以改动零风险；
  而一旦哪天可达了，响亮失败远好于静默算错。

## 我没能验证的

- **舍入残差归属**与 **FX 守恒**未做（那是替补 agent 的 Q1，避免重复劳动）
- **余额等式在四种条件下的成立性**未做（替补 agent 的 Q3）
- 只抽查了类型纪律这一个维度，**不构成对 territory A 的覆盖**

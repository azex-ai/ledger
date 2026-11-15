# 第 4 轮独立域评审

结论：**PASS**。本 diff 未发现 blocker，无需修复波次。

评审范围：`de583aa90a37a8ebf29b8a35db09d55fddcb012a..cf0336167d3f3c4bcc63b5e6fdddde3fe1b09725` 中 `postgres/reserver_store.go`、`postgres/booking_store.go`、`postgres/amount_magnitude_trace_test.go` 与 `04.md`。评审在独占分支 `codex/ledger20-t04-review` 完成；读取完整 diff、两个入口的相邻流程、core Validate 及 OpenTelemetry helper。未修改业务源码，未提交。此报告占 T2 域 reviewer 一席，主控另做第二意见。

输入 → 输出 → 交接 → 验证 → 记忆：金额展开顺序与 tracing 隐私契约、实现 diff → 本报告 → root 裁决/集成 → 本席静态核验及 diff 检查、实现者真实 PG 证据 → 当前注释和回归已保存校验先于展开的规则，无新增全局规则。

## 核验结果

- Reserve 在 `postgres/reserver_store.go:166`、CreateBooking 在 `postgres/booking_store.go:84` 先调用 `input.Validate()`，仅在成功分支执行 `Amount.String()`。两个 core Validate 都先调用 `ValidateAmountMagnitude`；错误信息不会为拒绝的巨大指数渲染完整金额。拒绝路径在查询和写入数据库前返回。
- 两个入口所有属性继续通过 `ledgerotel.StartSpan`，没有用 `span.SetAttributes` 绕过过滤。`pkg/otel/tracing.go:100` 仍统一应用 PolicyMinimal；新测试分别检查默认模式隐藏 amount/account_holder/idempotency_key，以及 PolicyFull 保留合法金额。
- 校验失败仍建立 span，defer End，并调用 RecordError；坏请求保留 error status 和 exception event。失败 span 不再携带 amount 属性。校验发生在 span 起点之前的计时边界变化与 `04.md` 的描述一致。
- 后续幂等、币种精度、余额与 SQL 流程未改；合法金额只改变 tracing 属性的构造时机。原来的错误包装差异亦保留，没有修改公开错误语义。
- 新测试覆盖两 store × 两 policy，使用有限值 `1E13`、`1E-19`、`-1`；验证 nil 结果、ErrInvalidInput、七类持久化行数不变、held amount 为零及失败 span。合法 2.50 请求另验证结果和 Reserve 的实际 hold。PolicyFull 下原版会附加非法 amount，故这组断言能检出原缺陷，不依赖运行巨大指数。
- tracing provider 的全局替换使用非并行测试，结束恢复 provider/default policy 并关闭本次 provider；同步 exporter 使 span End 后的断言无需后台 flush。

做得好的地方：修复同时保留错误可观测性与金额属性隐私过滤，避免仅修展开顺序却引入 tracing 回归；测试以有限金额观察原缺陷，验证资源风险时不主动制造负载。

## 验证证据与限制

本席独立执行：

```sh
git diff --check de583aa..cf0336167d3f3c4bcc63b5e6fdddde3fe1b09725
```

结果 PASS。本席完成独立静态评审，**未运行 PostgreSQL 测试**：初始等待共享重 PG 测试资源期间，主控根据已完成的相关回归及后续集成 gate，明确取消重复运行。

真实 PostgreSQL / race 证据沿用实现者 `04.md`：`go test -race -timeout 10m ./postgres -run 'Magnitude|InvalidAmount|Reserve|Booking' -count=1` PASS，39.781s；修复前有限输入 RED、`go vet ./postgres` PASS 亦为实现者证据。本报告不将这些表述为 reviewer 自行运行的结果。没有本席启动的数据库、容器或运行中测试进程。

# 第 3 轮独立域评审

结论：**PASS**。此次 diff 未发现 blocker，无需修复波次。

范围：`ab3a5d1844dd0a6c8fda85118ce4bc6a2d6f3696..22398147342018a25776fe506ab2634a8a65bf89` 中的 `core/money.go`、`core/money_exponent_test.go`、`core/money_test.go` 与 `03.md`。评审在独占分支 `codex/ledger20-t03-review` 完成，未修改业务源码、未提交。此报告占一次 T2 域 reviewer 席位；主控另做 Codex 第二意见。

输入 → 输出 → 交接 → 验证 → 记忆：已冻结的目标 exponent 范围与实现 diff → 本报告 → root 裁决/集成 → 独立 core 回归 → 目标精度和输入金额精度须分别设限，已由实现注释和回归记录，无新增全局规则。

## 核验

- `core/money.go:52` 与 `:100` 在金额验证、舍入及 ConvertAt 乘法前限制目标 exponent；`:142` 的 Allocate 在空权重检查后、任何金额运算前限制 exponent。`validateMoneyExponent` 使用整数比较，不对外部 int32 取负，因此 MinInt32 不会绕过检查或产生溢出。
- Round / ConvertAt 的合法范围为 `[-36,36]`，Allocate 为 `[0,36]`，与冻结契约一致。金额 magnitude 与权重校验保留。Allocate 的 Truncate、Shift/BigInt 和最终 `-exponent` 均位于范围检查之后。
- 合法目标下的舍入分派、默认模式回退及精确有理数分配算法未变。新测试以正负 1250 的百位舍入、36 位的 1/3 分配结果和总额守恒验证兼容性；既有 Round/ConvertAt/Allocate 测试同时通过。
- 新测试覆盖首个越界值、边界、int32 极值、正负金额与零；fuzz 不再跳过非法目标。断言要求 `ErrInvalidInput`，不会把旧版接受异常 exponent 的行为认证为通过。本评审未执行旧版巨大 exponent，修复前 RED 采用实现者记录的有限 ±37 测试证据。
- 负目标精度的 RoundUp 可产生超出存储 magnitude 的结果，这是既有算术语义；`03.md` 已准确说明 helper 输出仍须经过记账入口验证。本次没有新增“所有 helper 输出均可持久化”的承诺。

做得好的地方：复用既有工作精度边界，明确区别 Currency 的 18 位存储精度；修改只增加前置拒绝，没有改动合法范围内的金额算法。

## 独立验证

```sh
go test -race -timeout 3m ./core -run 'Round|ConvertAt|Allocate|AmountMagnitude|Money|FixedRate|ConversionQuote' -count=1
git diff --check ab3a5d1..22398147342018a25776fe506ab2634a8a65bf89
```

结果：PASS，`github.com/azex-ai/ledger/core 1.441s`；diff 检查 PASS。包含新增 extreme target 回归及 fuzz seed corpus，未启动额外 fuzz 预算，未使用 PostgreSQL。无运行中的验证进程。

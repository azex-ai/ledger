# 审计报告 — operability（可运维性）

> Territory G，Team Lead 自审。契约 §2 原本没有这一格 —— 这是我定契约时漏掉的维度，
> 补上是因为按本波的主题（「看起来绿其实没跑」），可运维性正是这类问题的老巢。

## 摘要

四条发现，三条 Major。最要紧的一条是本波 P0 修复自身的洞：为区分「没找到违规」与
「验证完整」而引入的 `FullCoverage` 信号，**在结构上永远无法为真** —— 因为套件里有一个
永久跳过的 check，而它一票否决整个覆盖信号。于是这个信号从落地起就是恒假的，恒假的信号
等同于没有信号。

第二条：一条告警被喂错了语义 —— `balance_drift_units` 收到的是余额而非漂移，且永不复位，一旦触发即长鸣；而它与真正的 checkpoint 篡改探测器共用一条告警规则，消音会把后者一起消掉。

第三条：链上资金路径的六个指标全部在发，**零告警、零 RUNBOOK 条目** —— 其中两个是偿付能力
事件（归集到无法归属的代币；已入账的充值在链上消失）。有仪表，无人看。

另外要说明：我一开始怀疑「告警引用了不存在的指标」和「DR 声称的覆盖面对不上」，**两条都是
我自己 grep 错造成的误判**，核实后撤回。列在这里是因为审计报告应当包含被推翻的假设。

## 发现

### [Major] `full_coverage` 永远为假，使这个信号从设计之初就不可用

- **位置**：`service/reconcile.go:922-938`（check #8）+ `service/reconcile.go:434-441`（计算）
- **判定**：CONFIRMED
- **失效场景**：
  `FullCoverage` 的算法是「任一 check 的 `Complete` 为 false 则整体为 false」。
  check #8 `pending_journal_timeout` 是**无条件加入套件**的，且**恒返回 `Complete: false`**
  （它等待一个尚未实施的 `journals.status` schema 特性）。两者相乘的结果是
  **`full_coverage` 在每一次运行中都是 false，没有任何输入能让它为真**。

  这不只是一个字段值不对。本波 P0 引入 `Complete` / `FullCoverage` 的**全部理由**是
  「一个扫了半个 fleet 却什么都没找到的运行，不等于一个验证了全部的运行」——
  即让运维方拥有一个可信的「体检合格」信号。而这个信号恒假，于是：
  - `service/reconcile.go:444` 的 `case overallPassed && fullCoverage:` → **死代码**，
    "reconcile: full suite passed" 这行日志永远不会打印
  - 运维方唯一能用的判据退回 `overall_passed`，也就是 P0 想要区分开的那个弱信号
  - **恒假的信号会被忽略**，和恒红的门禁会被关掉是同一回事

- **最小复现**：
  ```
  ledger-cli reconcile --full | jq '.full_coverage'   # 任何库、任何数据 → false
  ```
- **为什么现有机制没拦住**：两个测试都只断言它为 **False**
  （`service/reconcile_full_test.go:208`、`service/reconcile_full_integration_test.go:148`
  —— 后者的断言文案是「one incomplete check must sink the report's coverage signal」）。
  **没有任何测试断言过它能为真**，所以「它永远不为真」这件事对测试套件而言不可见。
  这正是契约 §1.4 说的形态：测试验证的命题（「不完整会沉掉信号」）是对的，
  但它比要紧的命题（「完整时信号会浮起来」）窄。

- **可选修法**（不在本次范围，交 Aaron 拍板）：一个永远不运行的 check 不是 check ——
  要么把它移出套件（用别的方式记住这个待办），要么让 `FullCoverage` 的分母排除
  「永久禁用」类，与「本次运行未覆盖完」区分开。后者更贵但保留了语义。

### [Major] `balance_drift_units` 被喂了余额而不是漂移，会永久锁死并连带消掉 checkpoint 篡改探测器

- **位置**：`service/rollup.go:230`（唯一写入点）/ `core/metrics.go:58`（接口声明）/
  `deploy/helm/ledger/templates/prometheusrule.yaml`（`LedgerBalanceDrift`）
- **判定**：CONFIRMED
- **失效场景**：

  接口声明是 `BalanceDrift(classCode string, currencyID int64, **delta** decimal.Decimal)`，
  指标 Help 写的是「Drift between expected and actual balance」。而**全仓唯一的调用点**
  在 rollup 的「借方常规账户出现负余额」分支里，传入的是 `newBalance` —— **余额本身，不是漂移**：

  ```go
  if newBalance.IsNegative() && ns == core.NormalSideDebit {
      s.logger.Warn(...)
      s.metrics.BalanceDrift(classCode, item.CurrencyID, newBalance)   // ← 传的是余额
  }
  ```

  实现是 `.Set()`，且**没有任何路径把它复位为 0**。所以一旦某个 (class, currency) 出现过一次
  负余额，该 label 组合的 gauge 就**永久停在那个负数上**，直到进程重启。

  告警是 `ledger_balance_drift_units != 0 or ledger_reconcile_gap_units != 0` ——
  **一条规则、两条腿**：

  | 腿 | 状态 |
  |---|---|
  | `reconcile_gap_units` | ✅ 接线正确，对账算出的 gap 有两个喂入点（`service/reconcile.go:1298`、`:1411`）。**这是真正的 checkpoint 篡改探测器** |
  | `balance_drift_units` | ❌ 被喂余额、永不复位 → 一旦触发即长鸣 |

  真正的危害不是误报本身，而是**误报的处置方式**：on-call 面对一条永不消退的告警，
  消音的是**整条 `LedgerBalanceDrift` 规则** —— 于是那条管用的腿（对账 gap）**被一起消掉**。
  而 checkpoint 篡改正是本波整个完整性加固的核心攻击场景（设计稿 §2 M-class），
  这条告警是它在运维层的唯一出口之一。

- **最小复现**：让任一借方常规账户余额转负一次（业务 bug 或攻击），rollup 处理该维度后
  `ledger_balance_drift_units{class=...} = <负数>`，`LedgerBalanceDrift` 触发且不再恢复。
- **为什么现有机制没拦住**：类型是对的（都是 `decimal.Decimal`），编译器无从发现；
  指标语义与调用点语义的偏离**没有任何测试或门禁覆盖**。这是契约 §1.4 的又一形态 ——
  被检查的是类型，要紧的是语义。
- **附带**：「借方常规账户出现负余额」本身是一个值得告警的条件，
  但它现在是通过一个语义不符的 gauge 间接表达的，且和真漂移挤在同一条规则里。
  拆成独立指标 + 独立告警更合适（交 Aaron 拍板，不在本次范围）。

### [Major] 链上资金路径的六个指标零告警、零 RUNBOOK

- **位置**：`observability/prometheus.go`（发出）vs `deploy/helm/ledger/templates/prometheusrule.yaml`（无）
  vs `docs/RUNBOOK.md`（无）
- **判定**：CONFIRMED
- **失效场景**：代码注册 30 个指标，告警规则覆盖 10 个。缺口里有六个是**故障/损失指标**，
  且全部来自 crypto-deposit + sweep 特性：

  | 指标 | Help 原文含义 | 后果 |
  |---|---|---|
  | `ledger_sweep_unattributed_total` | 归集批次收到**无账本归属**的代币 | **钱到了，没人拥有** |
  | `ledger_deposit_reorg_detected_total` | 已确认的充值**从规范链上消失**（深度重组） | **账本记了一笔链上不存在的钱** |
  | `ledger_chain_cursor_lag_blocks` | 充值 watcher 落后链头的块数 | 落后越多，未被看见的充值越多 |
  | `ledger_registration_rescan_failed_total` | 地址注册的历史回扫失败 | 注册前发生的充值永远找不回 |
  | `ledger_rollup_items_failed_total` | rollup 队列项处理失败后释放 claim | 该维度余额停止更新 |
  | `ledger_template_failed_total` | 记账模板执行失败 | 该笔业务没有记账 |

  前两个直接影响偿付能力。**没有任何告警会响，RUNBOOK 里六个词一个都搜不到**（逐个 grep 确认为 0）。
  现有 11 条告警全部围绕核心账本（对账/漂移/rollup 积压/事件投递/幂等/journal 失败），
  **整条链上资金路径的可观测性投入了仪表却没有接出口**。

- **为什么现有机制没拦住**：没有任何机制会检查「新增的指标是否有人消费」。
  crypto-deposit 特性合入时（`136e8cd`）加了指标，没加告警，也没加 runbook 条目 ——
  这与本波已提炼的 `global-namespace-allocation` 同形：**新增的东西默认不被覆盖，
  而没有任何东西会说「你漏了」**。

### [Minor] `ledger-cli` 的 help 文案说 10 个 check，实际 14

- **位置**：`cmd/ledger-cli/main.go:234`
- **判定**：CONFIRMED（`grep -c "checks = append" service/reconcile.go` = 14）
- **失效场景**：运维方按 help 预期看到 10 条结果，实际 14 条，会怀疑自己跑错了命令或版本不一致。
  在事故处置中，对工具输出的信心是有代价的。
- **为什么没拦住**：文案与实现之间没有任何联结。

## 被我自己推翻的两个假设（保留记录）

1. **「告警引用了代码里不存在的指标」** —— 我第一次 grep 时代码侧返回 0 个指标，
   看起来 10 条告警全是假的。实际是指标用 `Namespace: ns` + `Name: "..."` 拼接，
   我的正则只匹配完整字面量。**修正后：10 个告警指标全部存在，无假告警。**
2. **「DR 声称的对账覆盖面对不上」** —— 我从字符串字面量只 grep 到 5 个 check 名，
   而 DR.md 声称覆盖 16 条 invariant。实际全量对账跑 14 个 check，大部分内联命名。
   **修正后：DR 的覆盖声称基本站得住。**

两次都是 naive grep 的假阴性。记在这里是因为**审计里被推翻的假设和成立的发现同样重要** ——
它标出了哪些地方「看起来可疑但其实没问题」，下一个审计者不必重走。

## 移交

- **→ audit-threat**：`ledger_ro` 是全库明细 SELECT（已知待办），但结合本报告的
  「六个指标无人看」，值得问：只读凭证泄露后，攻击者能从指标端点（`/metrics`）
  读到什么业务信息？`reserved_amount_units` / `journal_entry_count` 带不带敏感 label？
- **→ audit-tests**：`service/reconcile_full_test.go:208` 是「断言了一个必然为真的命题」的
  活标本（断言 FullCoverage 为 false，而它永远为 false）。这类还有多少？

## 我没能验证的

- **DR 演练本身没跑过**。`docs/DR.md` 的 PITR / 恢复流程我只做了静态阅读，没有真的
  restore 一个库跑一遍。「恢复流程是否真的可执行」这个最要紧的问题**未验证**。
- **告警阈值是否合理**未评估（`values.yaml` 的 `thresholds` 那几个数字从何而来、
  在什么规模下会误报或漏报）—— 需要生产数据才能判断，静态读不出来。
- **`ledger_balance_drift_units` 等 gauge 的更新时机**未追（它们由谁在什么周期写入？
  如果只在 reconcile 时更新，那么 reconcile 挂掉时这个 gauge 会**停在最后一个好值**上，
  告警就永远不会响 —— 这是个 PLAUSIBLE 的次生风险，我没走完）。

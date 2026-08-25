# 金融工程视角全面审计 — Phase 0 契约

> 2026-08-25 · Status: CONTRACT（先于审计执行，squad 的唯一 SoT）
> 目标：从**金融工程 Go 账本库**的角度，多角度审计 41k 行源码 / 30k 行测试。
> 本文只定**审计的边界、产出格式与纪律**，不含结论。

## 0. 为什么要定契约而不是直接 spawn

六个 agent 各自读同一个仓库，必然出现两种失败：**互相重复**（三个人都报「金额用了 decimal，很好」）
与**共同盲区**（六个人都默认「有测试覆盖 = 没问题」）。territory 按**问题**切而不是按**文件**切 ——
金融 bug 天然跨文件，按目录分片会让「reserve 在 core 定义、在 postgres 加锁、在 service 编排」
这类问题掉进缝里。

## 1. 审计纪律（每个 agent 都必须遵守）

### 1.1 只读

**不修改任何文件。** 产出是报告。发现的问题由 Team Lead 统一定级、验证、决定是否修。
理由：六个 agent 并发改同一个仓库必然互踩；且**未经交叉验证的修复比不修更危险**（金融代码）。

### 1.2 每条发现必须给 `file:line`

没有 `file:line` 的发现一律作废。「某处可能存在竞态」不是发现，「`postgres/x.go:123` 在持锁前读了
余额」才是。

### 1.3 区分 CONFIRMED 与 PLAUSIBLE

- **CONFIRMED** —— 你读了完整调用链，或跑了命令验证。要写出**最小复现路径**（不必真跑）。
- **PLAUSIBLE** —— 看起来不对但没走完验证。**必须标注**，不许伪装成确定。

宁可交一份有 PLAUSIBLE 标注的报告，也不要一份读起来笃定但掺了推测的。

### 1.4 「有测试」不等于「被验证」

⚠️ **这条来自本仓库刚发生的真事**，不是理论洁癖：

- 一个对外订阅 API 从发布起从未对任何真实消费方工作过，**而测试套件一直是绿的** ——
  因为测试手动做了一步消费方不会做的接线（`service/worker.go` 的 `SetLocalPoller`，
  已修，见 `5c0047c`）。
- 防篡改校验会把伪造 journal 报成 VERIFIED，**因为它只比对「结论变了吗」而不检查
  「结论是不是负面的」**（已修，见 `7d92712`）。

所以看到一个测试时，问的不是「有没有测试」，而是：
1. 这个测试**如果被测代码整个不执行**，会失败吗？（非空洞性）
2. 这个测试做的准备工作，**真实调用方也会做吗**？（消费方对等性）
3. 它断言的命题，和它名字声称的命题，**是同一个吗**？

**「我认为这里缺测试」是有效发现，且优先级不低于代码缺陷。**

### 1.5 不要编造具体值

日期、数量、版本号、「X 修复了 Y」这类归因，**只写你从文件里直接读到的**。本波已发生两起
编造（一个不存在的事件日期；一个把 `grant_coverage_gap` 说成修了 ownership —— 文件名对、
内容不对）。**归因类断言必须打开被引用的文件核**，不能只看文件名。

### 1.6 报告里不要复述「做得好的地方」

除非它与一条发现直接相关（如「X 做对了，但相邻的 Y 没有，这个不一致本身是风险」）。
Aaron 要的是缺陷清单，不是表扬信。

## 2. 六个 territory（按问题切，不按目录切）

每个 agent 拥有一组**问题**。看到不属于自己的问题 → **写进报告的「移交」段**，不要自己深挖。

### A. 金融正确性（financial-correctness）

**拥有**：金额的类型、精度、舍入、符号；借贷平衡；余额推导；货币；对账的数学。

必答：
- 有没有任何路径让金额碰到 `float64`？（含 JSON、含中间计算、含日志格式化、含前端边界）
- `NUMERIC(30,18)` 与 `currencies.exponent` 的关系：**声明的精度**与**存储精度**在哪些路径上会
  不一致？超精度是拒绝还是静默舍入？
- 舍入发生在哪些点、用什么模式？多腿 journal 的舍入残差归谁？（FX preset 尤其：
  `presets/fx.go` 声称「settlement 吸收净额」——真的守恒吗）
- `normal_side` 与 entry 的 debit/credit 如何决定余额符号？系统对手方（负 holder）的符号约定
  在哪些地方被重复实现？
- 余额 = `checkpoint.balance + SUM(entries > last_entry_id)` —— 这个等式在**分区表**、
  **effective_at 回溯**、**reversal**、**并发 rollup** 下各自还成立吗？
- 部分冲销（`reverse-partial`）的累计守恒：冲销总额不超过原额，是在哪里、用什么隔离级别保证的？

入口：`core/journal.go` `core/template.go` `core/checkpoint.go` `presets/` `postgres/rollup*.go`
`service/rollup.go` `service/reconcile*.go`

### B. 并发与一致性（concurrency）

**拥有**：锁、隔离级别、TOCTOU、幂等、重试、事务边界、竞态。

必答：
- Reserve/Settle 的 TOCTOU 窗口：从「读余额」到「写 reservation」之间，**谁持有什么锁**？
  两个并发 Reserve 同一 holder 会怎样？
- advisory lock 的 key 空间有没有碰撞可能？（`hashtextextended` 的 key 派生）
- `GetBalance` 用 REPEATABLE READ —— 在它与 rollup worker 并发时，读到的是哪个时点？
- 幂等三态（同 key 同 payload / 同 key 异 payload / 新 key）在**每一个**写入口都实现了吗？
  有没有哪个写路径漏了幂等？
- **DB 事务内禁止外部调用**（`financial.md` 红线）—— grep 全仓，事务内有没有 HTTP / 签名 /
  外部 API 调用？签名路径尤其（`Attestor` 是外部调用）
- worker 的 claim/lease 机制：lease 过期后旧 worker 的写入会不会覆盖新 worker？
- 每个 goroutine 有没有 `ctx.Done()` 退出路径？（`golang.md` 红线）

入口：`core/reserve.go` `postgres/reserver_store.go` `postgres/balance*.go` `service/worker.go`
`idempotency.go` `postgres/*_store.go` 的事务边界

### C. 消费方表面（consumer-surface）

**拥有**：这个库**作为库被别人 import 时**的体验与陷阱。

⚠️ 这个 territory 的存在理由：本波刚发现一个对外 API 从未工作过，而它有测试。
**这类缺陷只有站在「我是第一次用这个库的人」的位置才看得见。**

必答：
- 把 `ledger.go`（facade）当作唯一入口通读：每个导出方法，**一个只读文档和签名的人会怎么用错？**
- 有没有第二个「必须先调 A 才能用 B、但 B 的签名不体现这一点」的接线陷阱？（Subscribe 那一类）
- `RunInTx` 的 `*Service` 是短生命周期克隆 —— 有没有办法让它逃逸出回调？逃逸了会怎样？
- 零值可用性：哪些导出 struct 的零值会静默产生错误行为（而不是报错）？
- `examples/` 是这个库的实际使用文档 —— 它们展示的用法**是推荐用法吗**？有没有 example
  在演示一种真实场景下会出问题的写法？
- 错误面：消费方能否区分「我用错了」/「暂时失败可重试」/「账本拒绝了这笔业务」？
  哨兵错误够用吗？

入口：`ledger.go` `core/interfaces.go` `examples/` `docs/frontend.md` `README.md`

### D. 威胁模型完整性（threat-model）

**拥有**：安全边界、攻击面、防篡改层、密钥、权限。

**前提**：既有威胁模型是「**DB 写凭证已泄露，程序端可信**」（见
`docs/plans/2026-08-21-tamper-evident-ledger-design.md`）。你的工作**不是**复述它，而是找它的洞。

必答：
- 在既有模型内：拿到 `ledger_app` 凭证的攻击者，**还能做什么**是设计没想到的？逐表过
  ACL + trigger，找「ACL 说可写、trigger 没拦」或反之的不一致
- 模型的边界之外但仍现实的场景：**迁移凭证泄露**（它能 `CREATE OR REPLACE` 守卫函数）、
  **备份泄露**、**只读副本泄露**
- 签名密钥：`Attestor` 的密钥在哪些路径上可能与 `DATABASE_URL` 落入同一失效域？
  （`custody.md`：凭证不落可失陷面）
- webhook 入站：签名验证、时间窗、nonce 重放缓存 —— 有没有绕过路径？
- 面向用户的错误信息有没有泄露内部实现？（`user-facing-surfaces.md`）
- `dev_credit` preset 凭空造余额 —— 它的门禁在生产真的关不上吗？

入口：`postgres/sql/migrations/001_baseline.up.sql` §12/§14、`core/auth*.go` `service/attest*.go`
`server/` 的中间件与 handler、`channel/`

### E. 测试的可信度（test-credibility）

**拥有**：测试证明了什么、没证明什么、以及哪些绿是假的。

**不要统计覆盖率。** 要回答的是「这 30k 行测试，哪些在真的兜底，哪些只是在场」。

必答：
- 逐个 money-path 找它的 pin：**如果这段代码整个删掉，哪个测试会红？** 找不到的 = 无保护
- 找空洞断言：只断言 `NoError` 而不断言结果、断言了一个必然为真的命题、mock 掉了被测逻辑本身
- 找「测试做了消费方不会做的准备」（Subscribe 那一类的兄弟）
- 找**互斥的测试**：两个测试断言了矛盾的行为（说明契约本身没定）
- `docs/INVARIANTS.md` 的 34 条：逐条问「它的 `Pinned by` 真的能证伪这条 invariant 吗」——
  引用存在有门禁，但**引用的测试是否真的验证了那条 invariant，没有任何门禁**

入口：全部 `*_test.go`、`docs/INVARIANTS.md`

### F. 结构与演化（structure）

**拥有**：六边形边界、依赖方向、契约演化、schema 设计。

必答：
- `core/` 的纯净性：有没有 import `net/http` / `pgx` / `slog` / `chi`（`golang.md` 红线）
- port 定义在消费方吗？有没有 1-3 方法之外的胖接口？
- 有没有「加一个 adapter 需要改 core」的地方？（依赖倒置是否真的成立）
- schema：外键、索引、分区策略与实际查询模式匹配吗？有没有索引缺失导致的全表扫描风险？
- `api-contract.md` 合规：包络、snake_case、uid、金额字符串、RFC3339、分页 —— 逐端点核
- 演化：哪些改动会破坏现有消费方而当前没有任何机制会发现？

入口：`core/` `ledger.go` `server/routes.go` `docs/openapi.yaml`
`postgres/sql/migrations/001_baseline.up.sql`

## 3. 产出格式（统一，Team Lead 要能横向比对）

每个 agent 写一个文件：`/private/tmp/claude-501/-Users-aaron-projects-ledger/d59b530e-39a8-43ad-8e8c-370377dc138a/scratchpad/audit/<territory>.md`

```markdown
# 审计报告 — <territory>

## 摘要
<三到五句。最要紧的发现是什么，整体判断是什么。不要罗列。>

## 发现

### [Critical] <一句话标题>
- **位置**：`path/file.go:123`
- **判定**：CONFIRMED | PLAUSIBLE
- **失效场景**：<具体输入/时序 → 具体错误结果。金额要给数字。>
- **最小复现**：<步骤，不必真跑>
- **为什么现有机制没拦住**：<哪个测试/门禁本该发现它却没有>

### [Major] ...
### [Minor] ...

## 移交
<不属于我 territory 但我看见的问题，一行一条，注明该归谁>

## 我没能验证的
<诚实列出：因为时间/复杂度没走完的路径。这一段空着比编满更有价值。>
```

**严重度**：Critical = 会算错钱 / 丢钱 / 让攻击者拿到钱；Major = 正确性风险但有兜底，或
安全纵深被削弱；Minor = 会误导维护者但不直接致错；**不要报代码风格**。

## 4. Team Lead 的职责（不下放）

1. **逐条验证 CONFIRMED**（本波已证明 agent 报告需要复核：两起编造、一起错误归因）
2. Critical / Major **停下来给 Aaron 拍板**，不自行修（`codex-coop.md` 纪律 B）
3. 交叉比对六份报告，找**共同盲区**（六个人都没提的东西往往才是真盲区）
4. 合并去重后统一定级

## 5. 明确不在本次范围

- 前端 `web/`（另一个技术栈，另开一轮）
- 性能优化（除非慢到构成可用性风险）
- 代码风格 / 命名 / 注释措辞
- 已知且已记录的待办（`ledger_ro` 聚合视图、发版版本号）

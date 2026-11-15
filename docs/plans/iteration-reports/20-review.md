# 第 20 轮：全分支独立 Final

## 结论：PASS

评审候选为冻结提交 `e3870ff11b96c42b2bc11e023b9e68558245399f`，工作树为 `codex/ledger20-t20-review`。全目标比较基线为 `d00fdebef4199d331787057127cc29a0ecb7cd87`；Task20 比较基线为 `412d5019ab63e65e6c268dc9027eb1882eb232aa`。开始及检查结束时 HEAD 均与候选一致；本席只新增此报告，不修改生产源码、测试、历史证据或候选提交。

本轮以 acceptance-critic / evidence-auditor 视角完成一次独立全分支 Final，结合此前各轮独立域评审、主控第二意见及最终组合差异，未发现需要阻止该候选集成的 Critical / Major 问题，也没有新增待修 Minor。无需启动定向 fix wave。此结论针对冻结候选的实现与已观察到的本地证据；不代替后续 Linux CI、集成或推送结果。

## 对用户目标的验收

| 验收点 | 独立判断与证据 |
|---|---|
| 通用计量单位与逐币双分录 | Currency 保留原币数量和精度，借贷平衡按币种约束。配置示例复用 CurrencyInput、TemplateBundle 和 Render，没有将异币数量相加或强制经过 USD。配置 fee / gift 的负例即使仍借贷平衡，也会被独立业务结果 oracle 拒绝；事务内验收失败检查持久行数、余额和 hold 不变。普通 main 的事后验收没有被描述为自动回滚。 |
| 可选 USD 估值 | valuation 是宿主读模型，按 Currency UID 匹配；金额先做大小校验，精确乘积保留至求和后再舍入。缺价、过期及未来价格不会充成零或混入完整 total；有价 subtotal 与完整 total 分开。未将估值包装成兑换权或外部储备证明。 |
| 可扩展兑换 | FixedRate 继续承担确定性换算；Exchange 在提交前检查卖腿 available 净减、买腿 available 净增，以及其他用户分类分别净零。相同分类的真实取消保留，异币、异用户及错误对手方拒绝。市场报价、有效期、外部执行及未知结果恢复明确留在宿主/ADR，没有声称数据库回滚能撤销外部成交。 |
| 资金流程组合 | Capture 的模板与幂等锁先于 settlement 行锁；settlement 和 journal 同事务，错误必须传播。最终实际分录检查用户、币种及 available 净扣款；available 分类账面余额与扣 held 后可用额度的术语一致。签名示例在事务外授权/验签，事务内 PostAuthorized 与结算；unsigned discharge 对 verified hold 的保守限制未被隐藏。 |
| Go 独立消费 | 三个仓外 Go host 各有独立 go.mod，GOWORK=off，显式使用候选 replacements，实际 tidy/build/run 并检查生产 import。root 消费不吸入可选链/存储 SDK，生产 import 不包含测试容器 fixture。未将本地替换成功等同于尚未验证的远程发布 tag。 |
| React / Next.js 消费 | SDK 真实 tarball 的六个非 Hero 入口完成 TS 编译及运行时 import；现有 Next host 覆盖 Hero 消费。mutation body 复用生成契约；holder 请求与响应边界拒绝 unsafe integer。query、mutation、预取、hydration 共用 scope；Provider scope 变化重挂子树，旧 pending mutation 仍归原 scope。显式身份和隐藏 cookie 的宿主责任有迁移说明。 |

上述判断结合最终源码、前 19 轮报告/独立评审、`lead-reviews.md`、`docs/CONSUMING.md`、`docs/BREAKING.md` 与最终审计报告。此前独立域评审已验证的实现没有因 Final 被重复扩成全库第二轮审计；本席重点核对最终组合后的共享 helper、锁与事务顺序、配置身份、API 消费和文档承诺。

## Task20 新增边界的独立核对

### 破坏性 migration fixture

`internal/postgrestest/isolated.go:22` 的 SetupIsolatedRawDB 将 full Down/Up 放到独立 cluster；`:39` 使用 PostgreSQL system identifier 校验真实身份，相同 cluster、读取失败及缺少权限均拒绝。不同数据库名或连接字符串不能绕过 guard。显式 isolated URL 不会静默退回普通 server；未配置时创建自己的 PG17 container。普通 DATABASE_URL 仍可用，无 Docker 的完整测试须另外提供独立初始化的专用 cluster，这一环境变化已写入 TESTING。

检查了启动等待、有限超时、失败时容器清理，以及 Cleanup 后进先出使测试数据库先删除、临时容器后终止的顺序。外部配置的 server 不会被终止；普通共享容器仍按既有进程生命周期清理，没有修改生产 migration/advisory lock 来掩盖测试隔离问题。

`postgres/migration_roundtrip_test.go:28` 在普通 fixture 留下 ledger_owner 拥有的真实 canary 对象，记录 role OID 和 table owner，再在独立 cluster 完成 Up/Down/Up，最后验证普通对象、role 和 owner 不变。已独立阅读 `fixture-mutant-red.log`：只改回 SetupRawDB 即确定性出现 role 对象依赖错误 SQLSTATE 2BP01；`same-cluster-red.log` 显示 guard 在 destructive migration 前拒绝；GREEN 与 pgx5 configured 分支日志对应最终实现。该证据直接区分修复与旧缺陷，不依赖重复全套测试碰并发概率。

### journal 幂等时间精度

`postgres/idempotency_match.go:31` 仅把显式 input EffectiveAt 按 time.Microsecond 截断后与落库时间比较，吻合既有签名 canonical 精度；zero/default 语义及其他 payload 比较不变。没有改签名域、历史数据、迁移或用示例时钟绕过共同边界。

独立核对 `postgres/journal_idempotency_precision_test.go:15` 的五条公开写入路径，以及 `examples/signed-capture/capture_test.go:357`。固定纳秒值、同微秒、微秒对齐、同 instant 不同时区合法重放；相邻微秒及金额变化仍冲突。断言覆盖 UID、余额、journal/entry/rollup 行数，signed Capture 另核 verified balance、held、settled 与 receipt 数。`precision-red.log` 证明旧比较逻辑在相同固定输入下失败；修复后的目标 race 日志通过，测试没有把原 bug 认证为正确。

### 合法重复 preview

`web/packages/ledger-react/src/lib/preview-entry-keys.ts:5` 用全部 wire 分录字段及同内容出现序号生成无歧义 key，保留合法重复、原顺序与原对象；没有杜撰 wire 中不存在的身份。两套 TemplatesPage 都使用该 helper，真实组件测试检查重复行、重排 DOM 身份、总额及 React key 警告。静态 skeleton 槽位仅表达固定布局位置，没有通过改 index 变量名绕过 gate。注释误报从源文件修正，schema 由 codegen 生成，gate 本身未弱化。

## 实际证据与验证边界

本席独立执行 HEAD/工作树核对及 diff 检查，独立静态阅读上述实现、关键调用链和回归断言，并抽查 Task20 `.local/ledger20/` 原始日志。没有重复运行 root race、PG、npm 或容器套件；以下执行结果属于实现者证据，经本席核对，不冒称本席运行：

- 四个 Go module 的 build/vet/race/lint；root 完整 race 墙钟 355.25s。miniotest 模块命令通过但无自身测试用例。
- fixture 与幂等定向 RED/GREEN、configured pgx5 和默认临时容器路径。
- 三个仓外 Go consumer、六个 SDK tarball 入口、sqlc 与 API/文档合约检查。
- SDK 50 文件/423 tests、host 8 tests、Next build/lint、两个 frontend gate 的 FAIL=0/WARN=0。
- 冻结提交后的 codegen:check；原始 `post-commit-codegen.log` 及 `.team/reports/49-g1.md` 记录通过。

Task20 范围 `git diff --check` 通过。全分支检查只命中历史原始 `docs/audits/2026-10-10-architecture-review/probes/money-probe.log:1` 起三行尾随空格；排除此原始证据文件后通过。按保留历史探针要求未改写该文件，这不是源码或交付正确性 blocker。

剩余边界均已公开且不因本次 PASS 消失：numeric SDK 不支持完整 int64；显式 cache scope 的真实性由宿主保证；普通 Capture 是 Go-only unsigned API；verified discharge 保守占用和事务错误传播仍需遵守；市场执行为 ADR；本地 checkout/tarball 通过不等于远程版本发布验证。生产依赖 audit 为零，完整开发依赖 audit 仍有 20 项，不宣称供应链全部清零。此前 FuzzAllocate 的 deadline 日志没有新增 crasher 证据，不据此盲改算法或扩大超时；后续 Linux 三条 fuzz、anvil E2E 和最终远程 CI 本席未执行、未观察通过。

本席评审到此完成；交主控归档、集成并按既定发布流程验证，不追加无新依据的审查轮次。

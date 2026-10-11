# shadcn 管理端验收

本轮用独立 Go module + production Next.js 宿主消费 Ledger，完成真实 PostgreSQL 与 Chromium 验收。核心管理流程已能操作；发现并修复了 SDK/HTTP 契约、金额显示、操作语义、移动表单及 dashboard session 边界问题。最终真实 UI 69 项、四角色 HTTP 权限 119 项、既有 dashboard session 15 项全部通过；SQL 独立核对与完整 Go race 门禁通过。

## 交付

- `examples/admin-acceptance`：最小 shadcn 管理宿主、四角色登录、服务端 key 绑定、同源 BFF、可运行种子与独立构建脚本。Go 使用明确的候选源码 replace；npm 使用实际 pack tarball，不是 workspace symlink，未安装 HeroUI。此证据验证候选可独立消费，不冒称已发布 Go/npm 版本。
- `scripts/admin-acceptance/ui.py`：真实浏览器流程、SQL/Decimal 数值基准、390px 布局与 selected label 检查。`role_matrix.py` 验证真实 BFF/Go 权限。两个脚本均只允许隔离 loopback fixture，不保存金融操作草稿。
- SDK 继续保留 currency 原始单位。账面余额与预留分开，跨币种数量不当作统一金额；USD 价格与兑换并非本次管理 UI 的功能。

## 已修复的实际问题

| 问题 | 影响 | 修复与证据 |
|---|---|---|
| 余额 SDK 漏传必需 `currency_uid` | 真正接入 Go 时返回 400，mock 未体现契约 | client/hook/SSR/两 skin 同步币种参数；list/aggregate 缓存 key 区分形状；真实五币查询成功 |
| Journal detail 误认为 wire 是嵌套对象 | HTTP 200 后 React 读取 undefined 崩溃 | 用 generated flat schema 在 client 边界适配回公开 `{journal,entries}`；真实详情可读 |
| 缩略金额无完整值入口 | USD `1234.56` 与 18 位小数无法完整核对 | 共用 ExactAmount button/tooltip，键盘、点击可得原 decimal string；保留全局分档显示规范 |
| Journal 把 USD/BTC 数量显示成同一总额 | 容易误解为有经济意义的估值 | detail 按 currency 精确求借贷合计；list/recent 移除无单位总额，无 N+1 |
| Booked/held/Settle 文案含混 | 容易把解除预留理解为真实扣款 | 显式 booked 与币种；Settle/Partial/Finalize 说明只记录结算并释放 hold、不提交 charge；实际前后 SQL/API核对 |
| 借贷箭头及假 Entry ID 列 | 把 Debit→Credit 误作统一资金流，entry_type 当 ID | 删除误导图示/列；保留中性借贷与科目信息 |
| Currency exponent 可提交 `2.5`，System 科目不可配置 | 精度和 owner 配置不完整 | 限定整数0..18、列表展示 exponent、明确 Holder/System 选择；非法值不落库 |
| 模板移动表单输入重叠，选中项显示 UUID | 390px 下部分输入不可用，提交前难以核对 | 响应式单列/行 grid；5处 selected name/code 明确显示；真实创建与提交 |
| 既有 dashboard 缺 unsafe Origin 校验 | 同站不同 Origin 请求可借有效 session 写配置 | session/BFF 副作用前拒绝 missing/null/foreign Origin；HTTP与浏览器历史失败证据保留 |
| Origin 修复遇 Next loopback URL 规范化 | `127.0.0.1` 合法登录被误拒 | 默认校验请求 Host authority +框架 scheme，显式 public origin优先；不扩大 Forwarded 信任 |
| 非字符串密码及非 ASCII 签名触发 500 | 异常 session 输入没有受控拒绝 | runtime 类型与 ASCII hex64 约束；合法会话保持兼容 |
| Capture 测试在建库等待时耗尽操作 deadline | gate 在真正并发操作前失败 | fixture 完成后启动原15s操作预算；主动持锁18s后真实race仍通过，不延长并发超时 |

没有把极小值显示成零误报为问题；`0.000000000000000001` 保持非零。此次数值核对未发现核心记账计算丢精度，修复主要位于接入、展示及语义边界。

## 验证范围与结果

最终运行源码为 clean commit `2d33d0f89702367e32e756a378c52385eec6eeef`。报告、验收脚本与测试探针的后续修改没有改变运行源码。实际打包 `@azex/ledger-react@0.6.0` tarball SHA-256：`a7bb7da477f517d1eae9c642c9a60b5c0638c4a02b4255068308c0f981cb07cb`。三个独立本地 PostgreSQL 17 集群分别用于 UI runtime 和两个测试实例，迁移测试没有使用 UI 数据库。

| 检查 | 结果 | 可复核证据 |
|---|---|---|
| 独立包消费 | PASS | `GOWORK=off` Go build/vet/deps；npm tarball 安装、typecheck、production Next build、3项宿主 auth 测试；`final-stage-02.log` |
| 真实 Chromium UI | **69/69**，0 uncaught browser errors | `final-ui-03/results.json`；11核心页面、390px、真实创建/记账/冲正/预留/权限拒绝 |
| 四角色 BFF → Go HTTP | **119/119** | `final-role-matrix.jsonl`，前缀 `AUTHROLE_EBA42CAFD7` |
| 既有 dashboard 正常与拒绝路径 | **15/15** | `final-dashboard-session.json`；默认未设 public origin，实际 127.0.0.1 登录/读/写/退出成功 |
| 操作后独立 SQL | PASS | `final-sql-after-ui03.json`，前缀 `UI_D4E95411` |
| SDK / dashboard 回归 | **438 / 72 tests** | producer final `sdk-tests.log` / `root-tests.log`；SDK build/typecheck、Next build、lint均通过 |
| 独立 money / auth 复核 | **155+7 / 51+64 tests** | #53 money/auth、#54 auth 报告；数字为各阶段实际执行，含重复覆盖，不合计成总用例数 |
| Capture fixture 主动争锁 | PASS | 持迁移锁18s，测试19.5037s完成；两并发测试 race 2.091s通过；操作预算仍15s |
| 完整 Go integration gate | PASS | `integrate-runtime-delivery.log`：root 182.366s、postgres 272.078s、service 138.550s，完整 race suite |

浏览器插件没有连接的浏览器，故使用任务专用 Python Playwright/Chromium，只访问自己的 loopback fixture。上述 UI 与 HTTP 检查没有 mock 后端。

- Seed holder1001：USD `1234.56`、USDC `1.234567`、BTC `0.00000001`、POINTS `42`、ATOM `999999999999.123456789012345678`；holder1002 ATOM `0.000000000000000001`。使用独立 SQL entry sums + Python Decimal 对照 API，再核对真实 UI，未用 formatter 自证。
- Seed USD booked `1234.56`、held `34.56`、available `1200`。新增自己的20 hold，Partial5后 available1185，Finalize后1200；新增30 hold，Settle25后 available仍1200。booked始终1234.56，没有额外 journal。已有 seed hold 未被结算。
- 模板真实创建、预览、记账1.23和冲正；按科目余额恢复零。USD0.001实际请求422/14006，无 journal 落库。pending USDC5.000001未记入余额。
- HTTP权限按 reader/writer/admin/read+deposit_review 分别验证；reader实际UI写入403且无存储，换admin后成功。admin不自动获得deposit_review capability；reviewer在未接服务时503，与无cap的403区分。完整历史与边界见 [权限报告](../2026-10-11-admin-acceptance/permissions.md)。

## 接入注意与未覆盖边界

`getBalances`、`useBalances`、`prefetchBalances`、`ledgerKeys.balances` 新增必需 currency UID 参数；详见 [BREAKING](../../BREAKING.md)。这是 SDK 对既有 HTTP 必需参数的修正，未改 Go wire。`getJournal` 的公开 `{journal,entries}` 形状保持。

Bundled dashboard 在代理重写 Host 或终止 TLS 时需配置 server-only `DASHBOARD_PUBLIC_ORIGIN`；浏览器 Origin 与 scheme/host/port 精确匹配。实验宿主只在 loopback HTTP 放宽 Secure cookie，角色口令与本地签名器是验收 fixture，不是生产 SSO/密钥托管实现。

最小宿主接线11个核心页面。Deposit Review、Withdrawal、Sweep 未接可选服务，因此不显示对应入口；其拒绝/未接线响应已检验，不声称完成这些业务操作。没有真实链上执行、行情源、USD估值或swap UI。趋势/Snapshots本轮主要验证币种口径与空态，未生成完整历史数据；HeroUI仅做共享契约及必要兼容同步，未进行完整视觉验收。operator scope是管理权限，不能代替宿主多租户/终端用户的数据隔离策略。

## 证据与评审

基线 `99871921df986a738272ae4a7f33fb357f5f6e03`；host `bd1c990`，SDK/auth `7ffb3db`、`a60cd0d`，runtime修正 `f90db2c`。最终 runtime/build provenance 如上；宿主源码摘要为 `c2bd6e54149c1687c986848cb7163f1f2585327341bf931b02cf077992ce4da5`。原始证据保存在任务归档，包含失败与通过记录，私有配置、cookie、token及API key不进入报告。

独立 money域155+7、auth域51及增量64回归通过。一次 whole-branch Final 已 **PASS**，当时未发现 Blocker / Major / Minor；随后 main 门禁触发下述一次测试探针修复，增量独立复核通过。评审冻结 `204eea5902ecb46886a1f5919a4d1a37b6f2a388`，与随后纯历史合并 `9455429bf413be0004063d9c32cd8e240836627d` 的 tree 完全一致；评审独立核验归档 hash、宿主源码摘要及运行源码与交付内容对应关系。完整结论见归档 `reports/shadcn26-final.md`。未将unit/MSW、基线复现或旧版本绿灯代替最终真实栈结果。react-doctor留下表头相似与大页面复杂度等维护性warning，没有为了评分扩大重构。


### 截图

- [混合币种 Journal：按 currency 分别平衡](screenshots/mixed-journal-desktop.png)
- [390px 模板：字段可用，选中项显示名称](screenshots/template-create-mobile.png)
- [极小额：完整原始 decimal 可查看](screenshots/tiny-atom-mobile.png)
- [Reader 创建被拒绝，错误可见](screenshots/reader-create-forbidden.png)

模板截图使用 full-page，背景延伸不代表弹窗溢出；弹窗内滚动及实际提交已通过脚本验证。表格保留自身水平滚动，页面与 main 不水平溢出。

### 复现与原始记录

独立宿主的运行步骤见 [README](../../../examples/admin-acceptance/README.md)，权限脚本见 [AUTH](../../../scripts/admin-acceptance/AUTH.md)，浏览器脚本见 [UI](../../../scripts/admin-acceptance/UI.md)。执行需要重新生成专用凭证与隔离数据库，不能复用生产 key 或生产数据库。

本机原始归档：`/Users/aaron/.claude/session-summaries/2026-10-11-shadcn-admin-evidence`。归档包含候选 SHA、build/runtime manifest、日志、SQL/Decimal 输出、截图和独立评审，复制时扫描本轮已知凭证并校验 SHA-256；私有 env、数据库目录、依赖与会话凭证不归档。

失败证据同样保留：基线真实 wire 400/React崩溃、权限问题、移动表单问题；第一修复候选的 loopback正常登录403；Capture fixture在迁移锁等待期间超时。另有三类仪器/脚本问题单独标明：Playwright APIRequestContext 不应用浏览器 loopback Secure-cookie 例外、selector文字包含装饰图标导致过严断言、psql输出缓冲导致首次持锁时机记录不准确。这些没有当作产品缺陷；分别改用实际浏览器请求、保留语义的文本断言与 pg_locks 观测后完成复验。

### 后续迭代依据

本轮消费端暴露了两个应固定的工程约束：MSW fixtures 必须对应 generated HTTP wire 与必需查询参数；真实框架 Request、浏览器会话和独立 SQL oracle 需要作为包接入验收保留。数据库 fixture provisioning 的等待不应占用并发操作自身的 deadline。

下一步可以为另一个实际业务宿主复用这套接入验收，再按需要接入 swap/报价与可选服务。每个 currency 的双分录数量平衡继续留在账本核心；USD估值、价格有效期和兑换执行由独立业务层表达，不能在管理端用跨币种数量合计来代替。


### 最终门禁发现的测试探针问题

首次 main 集成门禁在 `TestCaptureReplayRejectsUnverifiableHistory/bad-signature` 的初始 Reserve 被测试探针拒绝，日志为 `integrate-main.log`。旧探针把同库所有连接的事务都视作 service 在签名时持有的事务，超出了它实际要守的 runtime pool 边界。原失败日志没有 PID，不能断言该次具体来自哪个进程，也没有将 PostgreSQL 累计统计延迟误当作 `pg_stat_activity` 的证据。

本轮唯一 Final 修复波次为 `7575b64e143e65657d25a47624814d32c846aa01`，只改 `examples/signed-capture/capture_test.go`。串行 fixture 绑定实际 runtime pool，要求签名/验签时没有借出连接，并断言不存在异步 AfterRelease hook；此条件比无显式事务更严格，仅适用于这个没有并发使用者的测试。全部原经济、签名、replay 和 rollback 断言保持不变。

对照先在旧实现确定性复现：runtime没有借出连接，独立 admin pool 保持真实 BEGIN，旧 SQL 仍拒绝签名。新实现完整目标包 race 通过（6.336s）；独立 reviewer 另跑两个新测试（1.960s）：其他 pool 不干扰、runtime 的 T/E 事务状态仍拒绝 Sign/Verify、rollback 后恢复。没有 sleep、重试或降低金额/签名断言。原始证据见归档 `producer55/` 与 `reports/55-g1.md`、`reports/55-money-review.md`；这属于第一次 Final 后的一次窄修复及增量复核，没有重启 whole-branch Final。

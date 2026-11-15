# Task 08 独立 T2 域评审

**结论：PASS。** 在本轮 backend / session cache isolation 契约内，未发现阻断问题。主审提出的宿主测试 CI 缺口已通过 scoped 修订补齐；静态 gate 的基线失败没有被计作全绿。

- Reviewer：`ledger20-money-review`，Task 8 / bus #37 / generation 1；不是本轮实现者。
- BASE：`4df363d887d2981496692b58b4b45bdce1164f21`。
- 功能源码评审和独立运行：`68452c957a00c855eb65b79132a5ad03dafeaf4e`。
- 最终评审对象：`1f02131f6a1c3932bab09d23a18fff6839076a5c`。相对功能 HEAD 仅修改 workflow 与作者报告，已用 `git diff/show` 核实；没有修改或重置 reviewer worktree 源码。
- 独占 worktree：`/Users/aaron/projects/_worktrees/ledger/codex/ledger20-t08-review`。
- 已加载：routing；复用完整加载的 always、金融工程 profile、安全 charter 与 code-review-standards；本轮补读 nextjs-engineer、security-engineer、security-review、Next.js / TypeScript 规则。
- Pipeline：冻结任务、作者报告及主审宿主范围裁决 → 独立安全域审与窄验证 → 本报告交主审 → 主审第二意见及集成 gate → 报告归档。

## 判定依据

### Client 与 provider

`createLedgerClient` 默认只生成一次 `crypto.randomUUID()`，使用带 `instance` 标签的冻结 tuple；显式共享使用独立的 `shared/backend/identity` tuple。`cacheScope` 通过 getter 暴露。请求所需的 `baseUrl`、`apiKey` 和 configured fetch 在构造时快照，调用方事后修改配置对象不会把旧 scope 的请求转向其他后端。

Provider 对三个请求配置字段和显式 scope 的值做 memo，不以 scope 对象引用为身份。配置变化重建 client；scope 改变使 context 子树重挂，重置本地状态与自有 QueryClient。注入的 QueryClient 可以保留旧数据，但不会被新 scope 读取；README 明确区分隔离与内存清除。

显式 scope 是宿主的非秘密身份声明。API key 变化而声明不变时仍共享缓存、同 URL 隐藏 cookie 变化不能自动识别，都是明确契约，不能误报为 SDK 自动保证了真实身份判断。已知 apiKey 被放入显式 scope 时会拒绝，错误不会回显该 key。

### Query、mutation 与异步完成

已逐项核对管理 hooks、分类 lookup、11 个 server prefetch、统一 key/prefix 工厂、直接 mutation 与通用 wrapper 的调用点，没有残留无 scope 管理缓存失效路径。乐观 queue 更新的查询前缀包含 scope；rollback 使用保存的完整 scoped query key。

`mutationKey` 也包含 scope，避免旧 pending mutation 被新 context 的 callbacks 覆盖。本 reviewer 核对了本 worktree 安装的 `@tanstack/query-core/src/mutationObserver.ts`：key 改变时 reset observer；只有 key 未变且 mutation pending 才更新旧 mutation options。这与真实 Promise 延迟测试的验证方向一致。

测试保持 hook 挂载并切换 context，直接断言 A 完成后只使 A 失效、B 的数据与有效状态不变，以及 A 的 optimistic rollback 不影响 B；不是单纯验证 key 工厂的数组形状。作者报告的反事实移除 `mutationKey` 会使通用 mutation 用例失败，与已检查的依赖实现相符；本 reviewer 没有再次修改源码执行该反事实。

通用幂等 map 将 scope 和 payload 一起作为索引。同一个正常 Provider 内，直接 mutation 的其他局部 ref 随身份子树重挂而重置；Provider 契约不应被解释成允许宿主任意替换内部 context 且保留其全部跨身份局部状态。

### Next.js 宿主与秘密边界

`getDashboardCacheScope` 是 `server-only`，先进入动态 cookies 边界，再解析配置。有效 cookie 先经过现有 `verifySession`，只将公开 expiry 写入 identity；未验签的 token 不进入 session namespace。anonymous 与 auth-off development 独立，生产缺失 logical backend ID 明确失败。根布局和两处实际 prefetch 使用同一 resolver，SSR 和 browser 的 URL 可以不同而 scope 一致。

已检查登录、注销原有 `router.refresh()` 调用、Provider 接收的更新 prop，以及 proxy / BFF 原有授权边界。新 helper 没有改 token 格式或服务端授权机制。现有 token 仅包含 expiry 和签名，同毫秒生成同 token 的既定边界已由文档与测试说明，不声称每次登录有独立 nonce。

实际传给浏览器的是 logical backend 和 verified public expiry，不是 raw cookie、HMAC、signing secret、API key 或 internal URL。SDK SSR 测试真实执行 JSON dehydrate/hydrate；宿主测试使用真实 Provider，验证身份变化后旧数据显示立即消失、本地状态重置和同源 BFF 请求。此证据没有被夸大为浏览器端到端登录测试。

## 独立验证范围

全部运行在 reviewer 独立 worktree，使用其自己的 node_modules；没有 PostgreSQL 操作，没有修改功能源码。命令外层 Python timeout：npm ci 600s、SDK build 240s，其余 180s。

| 检查 | 独立结果 |
| --- | --- |
| `npm ci`（web） | PASS，13s；报告既有 23 项依赖告警，非本轮新增依赖 |
| `npm run -w @azex/ledger-react build` | PASS，含 ESM 与 DTS |
| `npm test -w @azex/ledger-react -- test/provider/cache-scope.test.tsx test/server/scoped-hydration.test.tsx test/hooks/scoped-mutations.test.tsx` | 3 文件、21 项 PASS，1.39s |
| `npx vitest run --config test/vitest.config.mts` | 宿主 1 文件、8 项 PASS，1.33s |
| SDK `delivery-gate/scripts/gate.sh .` | `tsc --noEmit` PASS；`FAIL=2 WARN=0`，gate 原始 exit 1 |
| 静态 gate 两个 regex 全量扫描并比较 BASE | 10 个命中文件全部逐字节相同 |
| 最终 scoped 修订 diff | 仅 workflow + 作者报告；新宿主测试在 build 之后，继承 web cwd，无 continue-on-error / 吞退出码 |

没有无疑点重跑作者的 369 项全包测试、codegen、完整 Next production build 或移除部署变量后的 build；这些仍属于作者证据，未冒称独立重跑。没有执行远端 GitHub Actions 或真实浏览器 E2E。

## 已处置事项与剩余限制

1. **Minor，已关闭**：新增宿主 8 项测试最初未接入 CI。最终 HEAD 在 `.github/workflows/ledger-react.yml` 的 SDK build/test 后加入既有 host test 命令；无依赖、包脚本或 lockfile 改动。本 reviewer 已独立执行相同命令，并核对新增 workflow 直接传播失败。
2. **报告精度，已关闭**：原报告按 gate 的 `head -5` 输出统计 7 个文件。全量实际为 10 个，作者修订已纠正。完整清单：`client/schema.ts`、两种 `ClassificationsPage`、`components/dashboard/recent-journals.tsx`、`components/loading-skeleton.tsx`、两种 `TemplatesPage`、`heroui/shared.tsx`、两种 wallet `transaction-list.tsx`。前三个 any 命中来自注释文本；其余为基线 index key。所有命中与 BASE 字节一致，不能称 gate 全绿，也不应把这批未变更 UI 混入 Task 8。
3. `LEDGER_CACHE_BACKEND_ID` 是新增生产请求期必填配置，应按 README 配置非秘密逻辑 ID。scope 不替代授权、不自动识别隐藏 cookie，也不保证清除宿主 QueryClient 保留的数据。
4. 安全依赖告警由既定 Task 18 处理，本轮没有改动 package/lockfile。此域审 PASS 不代表整个项目依赖、远端 CI 或发布 gate 已全部通过。

报告交主审用于 T2 双席汇总与后续集成。本 reviewer 未 commit、push 或修改 bus 的最终评审状态。

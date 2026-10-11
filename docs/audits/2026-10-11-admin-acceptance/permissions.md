# 管理端权限审计 — bus #51 / gen 1

owner：`admin-auth-audit`；风险 T2。基线 `99871921df986a738272ae4a7f33fb357f5f6e03`，独占 worktree `/Users/aaron/projects/_worktrees/ledger/codex/admin-auth-audit-261011`，分支 `codex/admin-auth-audit-261011`。

当前结论：**既有 production dashboard 验收 FAIL：1 Major + 1 Minor，均已真实复现；新独立宿主的本席权限验收 PASS**。新宿主 119 项 HTTP 权限检查、真实浏览器 Origin 对照和 10 项 UI/身份切换检查通过，不能据此宣称旧 dashboard 已修。主控已另派旧 host 窄修复，尚待其冻结候选复核。只读生产代码，未改 SDK/server、未运行 PG/Docker 或 destructive migration、未提交推送。

## 权限实际落点

- `server/middleware_auth.go`：API key 身份由服务端配置决定，`read < write < admin`。`deposit_review` 为独立 capability，admin 不隐含它；read + capability 可以审核及重放充值。认证失败为 401/10101，权限不足为 403/10150。
- `server/routes.go`：每条 operator route 有 scope/capability middleware。health/ready 为探针例外；webhook 使用 channel 自己的签名；`/holder/*` 使用 holder token，不能与 operator API key 混用。`/holder-tokens` 仍为 write。
- 额外领域限制独立于 scope：system classification 的 raw journal、受保护或触碰 system classification 的 template 默认拒绝；deposit-tolerance 还要求 admin。admin 并不能自动跳过这些限制。可选服务未接线返回 503/18102；达到 handler 的 503 只证明通过授权，不证明服务执行成功。
- SDK 没有可信角色判定或 RBAC 授权状态；client 按公开 API 发请求。`LedgerAdminProps.navItems` 明文声明是 UI 配置，不是 API 授权。两套页面不因导航过滤而获得后台授权；页面中的创建/审核按钮不能充当安全边界。read 是 operator 全局查询权限，不是单 holder 的行级隔离。
- 既有 `web/` 是单口令 operator host：session → BFF → 唯一服务端 LEDGER_API_KEY。浏览器 Authorization/Cookie 不转发上游，BFF 自己附服务端 key；任何浏览器 role 参数都不构成身份。新宿主须将已验证 session 绑定到对应的服务端 key，不能在所有身份后面统一代理成 admin。
- `examples/fullstack` 明示 dev、无 API key、宽 CORS，是已知示例限制，不记为本轮新漏洞，也不能用作生产权限验收。

## 可执行允许／拒绝矩阵

以下每个拒绝须通过直接 HTTP/BFF 请求验证，不能只观察按钮不存在。给角色写请求都使用独立 canary code/idempotency key；拒绝后通过查询确认对应对象不存在或状态未变。

| 请求或场景 | 匿名 / 坏凭证 | read | write | admin，无 cap | read + deposit_review |
|---|---|---|---|---|---|
| Go GET `/api/v1/system/health` | 200 | 200 | 200 | 200 | 200 |
| GET currencies/journals/entries/balances/bookings/reservations/trial-balance | 401 | 200 | 200 | 200 | 200 |
| POST `/balances/batch`，合法 holder_ids | 401 | 200 | 200 | 200 | 200 |
| POST `/templates/{code}/preview`，已存在模板、合法 amounts | 401 | 200 | 200 | 200 | 200 |
| POST `/bookings` 或合法非系统 journal / reservation | 401 | 403 | 成功 | 成功 | 403 |
| POST currency/classification/journal-type/template 创建或停用 | 401 | 403 | 403 | 成功 | 403 |
| PUT `/accounts/{holder}/policy`、POST reconcile / periods/close | 401 | 403 | 403 | 通过授权，合法 fixture 成功 | 403 |
| POST `/deposits/{uid}/review/approve`、reject、dead-letter replay | 401 | 403 | 403 | 403 | 通过授权；未接 add-on 时 503/18102 |
| POST 受保护的 `deposit_confirm` template；手写 system classification journal | 401 | 403 | 403 | 403（默认配置） | 403 |
| GET 未接线 add-on 的 review queue/deposit address | 401 | 503/18102 | 503/18102 | 503/18102 | 503/18102 |

无效业务 payload 得到 400/404 不能替代上述“成功”用例；capability 未接线的 503 则必须和无 cap 的 403 对照。外部 holder token 与匿名探针的例外也不能被误判为 operator auth 绕过。

| 宿主边界 | 可执行检查及期望 |
|---|---|
| 登录 | 同源正确密码 200 并设置 HttpOnly/Secure session；错误密码、篡改/过期 cookie 拒绝；非字符串输入为受控 4xx，不产生 500 |
| BFF 匿名 | 缺 cookie、坏 cookie、仅伪造 Bearer/X-Role/role 参数均不能获得 operator 数据或写权限 |
| 身份绑定 | read session + admin 字段/伪造 Authorization 仍只能 read；浏览器实际请求、HTML/JS 与日志不出现后台 key |
| CSRF | 合法 session + unsafe method + foreign/null/missing Origin 拒绝；同源合法请求仍成功。单独做真实浏览器同站跨端口 text/plain POST；以 DB/API canary 变化判断，不能只看 CORS 使响应不可读 |
| 缓存切换 | 登出/重新登录不同身份更新非秘密 cacheScope，Provider 子树重挂；前一身份的数据/preview/mutation 状态不留在新身份页面 |
| 直接 Go API | 没有 key 为 401，错误 key 为 401；read key 直接 POST 管理配置为 403；有效 admin key 只在服务端测试进程使用，不交给浏览器 |

## 已执行证据

1. 本席在独占 worktree 执行既有 Go route/spec 授权 gate、scope/capability、Bearer、充值自批限制及 protected-template 的定向 race：`go test -race -count=1 -timeout 2m ./server -run '^(TestRouteAuthorization_.*|TestParseAPIKeys|TestScopeAllows|TestCapabilityIndependentOfScope|TestAuth_ReadsRequireKeyWhenConfigured|TestAuth_WebhookPathExemptFromBearerAuth|TestAuth_ScopeEnforcement|TestDepositReview_SelfMintSelfApprove_MI2|TestPostTemplate_ProtectedCodeIsRefused|TestPostDepositTolerance_RefusesProtectedTemplatesByDefault)$'`，PASS，1.465s。这里的 handler 测试使用现有 test doubles，不冒充真实 PG/Next 栈。原始日志 `.local/auth-audit/existing-auth-gates.log`。
2. 由主控构建/启动的原始 `web/` production host `http://127.0.0.1:43111`，本席调用真实 session/BFF：匿名和伪造 Bearer 401、错误密码 401、正确登录 200、HttpOnly/Secure/Lax、过期/篡改 session 401。`password: 42` 返回 500；foreign Origin 登录和退出均 200。原始输出 `.local/auth-audit/existing-session-red.jsonl`，11 检查/3 不符合安全期望，exit 1。此时上游尚未启动，未声称完成越权写入复现。
3. Go 上游 `127.0.0.1:48110` 就绪且旧 BFF 配置匹配 admin key 后，完整 HTTP probe 的 foreign/missing/null Origin text/plain POST `/currencies` 均返回 201，并由实际 GET currency list 确认唯一 canary 存在。同源 positive control 也为 201。`.local/auth-audit/existing-origin-http-red.jsonl`：24 检查/9 项安全期望失败，exit 1；只停用脚本自己的四个 canary。
4. **真实浏览器复现**：隔离 Chrome context 在 43111 同源登录，先验证同源写入 201；随后打开临时 `http://127.0.0.1:53569` 攻击页，发出带浏览器凭证的 text/plain POST。Chrome CDP `requestWillBeSentExtraInfo` 确认 foreign Origin、text/plain 和 session cookie 自动附带（只保存存在布尔值）；没有 OPTIONS preflight。实际网络 201，攻击页只读到 opaque/status 0，但同源 GET 确认 `BROWSERCSRF_CF74273001_FOREIGN` 已落库。`.local/auth-audit/existing-origin-browser-red.jsonl`，exit 1。测试没有手工给浏览器设置 Cookie 请求头，结束时停用其自建 canary 并关闭临时 server/context。

首次浏览器探测也观察到写入，但 Playwright 在 opaque response 的 request.all_headers() 中没有暴露 Origin/Cookie，故脚本按证据不足返回 2；该输出保留在 `existing-origin-browser-instrumentation-gap.jsonl`，未当成通过。随后仅补 CDP 非秘密字段观测，得到上面完整复现。没有靠重复请求掩盖系统失败。

5. 新独立宿主 `http://127.0.0.1:43110` 与真实 Go `http://127.0.0.1:48110`，四个已验证 session 与服务端 key 分别对应 reader/writer/admin/reviewer；`.local/auth-audit/new-host-role-matrix.jsonl` **119 checks / 0 failed / exit 0**。同时经 BFF 与直接 Go API 验证 semantic POST、配置创建、pending booking 创建、独立 capability、protected template、身份伪造、过期/篡改 token 和 Origin。read session 即使附真正 admin Authorization（只在服务端探测进程读取私有测试 key，不交给浏览器）、X-Role/body/query 角色字段，仍 403。admin 无 capability 为 403，reviewer 到未接线服务为 503/18102，没有冒称审批已经执行。
6. 四身份测试的副作用核对 `.local/auth-audit/new-host-storage-check.json`：专用 holder 990051 只有 writer/admin 各一条 pending booking，直接 Go 重放使用相同 key，没有多建；两条都没有 journal/reservation 引用，reservation 数为 0、USD 余额行数为 0。reader/reviewer 配置请求均无 canary；仅 admin 创建两条唯一 currency，随即停用。本席没有关闭期间、修改已有 policy、结算已有 hold 或创建记账分录。
7. 同一真实 Chrome Origin probe 对新宿主的对照 `.local/auth-audit/new-host-origin-browser-green.jsonl` **exit 0**：同源写入 201；攻击页 `http://127.0.0.1:59271`，CDP 证实 Strict session cookie 仍自动附带，但 BFF 返回 403，foreign canary 不存在。其效果来自 Origin 检查，不能归因于把 Lax 换成 Strict。
8. 真实包 UI `.local/auth-audit/new-host-role-browser.jsonl` **10 checks / 0 failed / exit 0**：用登录表单进入 reader，实际点击 Currencies/Create 后 403，DOM 提供 `You don't have permission` 权限错误文本，弹窗保留输入且没有落库；退出清 cookie，重新登录 admin 得到新 identity/nonce，旧弹窗和权限错误消失；实际 UI 创建 201、唯一 canary 可查询，随后只停用本 canary。document.cookie 不可读 session，观察到的页面 HTML、14 个已加载 script 和浏览器请求未包含 ledger key / 签名 secret；客户端资源也未包含测试口令。这是实际观察的资源范围，不是对所有未来构建的普遍不泄漏证明。

UI 初稿脚本在收到 201/成功提示后立即读取 dialog.is_visible，正逢关闭动画，产生一项 false；同期截图已显示关闭。该原始输出保留 `new-host-role-browser-immediate-snapshot.jsonl` 和 `screenshots/*-first.png`。改为等待 dialog 隐藏后重新执行这一 UI 流程通过，未改产品代码、未删除失败证据，也未扩大等待超时掩盖业务错误。最终截图为 `screenshots/reader-create-forbidden.png` 与 `screenshots/admin-create-success.png`；截图处于 toast 入场阶段，错误文本的存在与可见性由定位器检查，不据此声称已完成提示的视觉对比度验收。

私有配置由主控以 0600 JSON 提供，测试读取但不记录其值；日志没有口令、token、cookie 或 API key。可重复的防御性四角色矩阵保留在 `scripts/admin-acceptance/role_matrix.py`，运行说明见同目录 `AUTH.md`。本次只提交矩阵、说明与结论；已完成的浏览器专项仅保留结果和原始证据，探索性复现脚本不进入交付。临时浏览器与探测服务均已关闭，主控/producer 服务未停止。

## 已确认问题

- **Minor，已复现：session 登录未检查 password 类型。** `web/src/app/api/session/route.ts:27` 只做 TS cast，`:33` 将 `42` 传给 `verifyPassword`；`web/src/lib/dashboard-auth.ts` 的 hash.update 抛出，真实 production 请求返回 500。最小修复是明确只接受字符串，错误类型受控拒绝；不要把开发 TypeScript 类型当作网络输入校验。
- **Major，真实浏览器已复现：cookie BFF 缺少 unsafe-method Origin 边界。** `web/src/app/api/v1/[...path]/route.ts:37–69` 验 session 后直接转发并加服务端 key；`server/server.go:669–689` 的 CORS 只设置响应头，`pkg/httpx/response.go:85` 解码 JSON 不要求 Content-Type。攻击前提是操作员已经登录、浏览器访问同站不同 Origin 的攻击者页面（本次同主机不同端口；同站并不等于同源）。不声称任意跨 site 网站都可让 Lax cookie 自动附带。实测影响是未获操作员意图确认的管理配置写入，唯一 currency 已持久化；同 BFF 其他 POST 权限由所配置后台 key 决定，未用真实资金转移扩大实验。最小修复在 cookie host 的 unsafe methods 和 session 登录/退出执行可信 Origin 校验，拒绝 foreign/null/missing Origin；同源正例保留。仅更改 CORS 响应头或把 Lax 换为 Strict 不能排除同站跨源条件。

本席当前权限验收到此收口。归档矩阵脚本仅补可配置 fixture holder 与通用拒绝 Origin，完成语法和 CLI help 检查；遵照主控停止追加请求的安排，没有对归档版重复执行网络矩阵。119 项结果对应上面保留的实际运行原始输出。剩余事项是主控安排的旧 dashboard 窄修复和后续独立 Final；本报告不替代尚未通知本席开始的 Final，也不把 SDK balance 缺少 currency_uid 等其他席位发现混入授权失败。本轮不重开上一轮架构审计或改写此前评审。

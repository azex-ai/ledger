# Web 生产依赖修复记录

日期：2026-10-10。Task 18 / bus #47，风险 T1。基线 `4316088f0b623dcc04a16e6947d9b71a7bfd0d25`，分支 `codex/ledger20-t18`。验证环境为 macOS、Node `v24.21.0`、npm `11.19.0`，全部安装和构建使用本任务 worktree 的独立 `web/node_modules`。

`npm audit --omit=dev --json` 从 3 个受影响包（2 high、1 critical）降为 0，干净 `npm ci` 后复核仍为 0。SDK 和 Next 生产构建、两层 TypeScript 检查、43 个文件的 257 个测试通过。完整审计仍有 20 个开发依赖包条目；详见下表，本轮不声称完整供应链已无漏洞。

## 版本与官方依据

| 包 | 基线 → 修复 | 选择依据 |
| --- | --- | --- |
| `next` | `16.3.4` → `16.3.8` | 官方 [v16.3.8 安全发布](https://github.com/vercel/next.js/releases/tag/v16.3.8) 包含图像优化 SSRF、SSG/ISR 缓存污染、Draft Mode 缓存泄漏及 metadata image 路由修复。先前 `next/og` RCE 的 [官方 advisory](https://github.com/vercel/next.js/security/advisories/GHSA-vcvr-r3jv-pc5j) 已在 `16.3.6` 修复，`16.3.8` 覆盖该修复。 |
| `eslint-config-next`、`@next/*` | `16.3.4` → `16.3.8` | 与 Next 补丁版本保持一致；未变更 ESLint major。 |
| `sharp`、`@img/sharp-*` | `0.35.4` → `0.35.5` | [官方 GHSA-wq5f-xc86-pv6w](https://github.com/lovell/sharp/security/advisories/GHSA-wq5f-xc86-pv6w) 指定修复版本 `>=0.35.5`；预编译包带有修复后的 librsvg `2.63.2`。 |
| `@img/sharp-libvips-*` | `1.3.3` → `1.3.4` | sharp `0.35.5` 指定的平台依赖，随 sharp 一并更新。 |
| `source-map-js` | `1.2.1` → `1.2.2` | [维护者 v1.2.2 发布](https://github.com/7rulnik/source-map-js/releases/tag/v1.2.2) 明确修复恶意 indexed source map 导致的 DoS；对应 [GHSA-68fv-2mgg-jv7q](https://github.com/advisories/GHSA-68fv-2mgg-jv7q)。 |

基线 Next 的 audit `via` 中包含 `GHSA-vcvr-r3jv-pc5j`、`GHSA-3w37-wq28-93x7`、`GHSA-4jqv-mc3x-m676`、`GHSA-39w2-rjm5-chcv`、`GHSA-f87g-xv8r-7p7x`、`GHSA-mcj8-r9mp-w47p`、`GHSA-cjq9-62q9-8jv4`。安装 `16.3.8` 后，这些条目均从生产审计中消失。该列表是依赖版本检测结果，不表示应用实际满足每个漏洞的利用条件。

核对了 npm registry 的 `version`、`repository`、`engines`、`peerDependencies`、`optionalDependencies`，并通过网页读取上述维护者发布和 advisory。也确认了 [Next v16.4.0](https://github.com/vercel/next.js/releases/tag/v16.4.0) 已发布，但 `16.3.8` 已能清除本次生产审计，故采用同 minor 的较小补丁。Next 与 sharp 要求 Node `>=20.9.0`，Next 的 React peer 范围接受现有 `19.2.8`。React、TypeScript `5.9.3` 和 SDK 的 manifest 均未升级。

`web/package.json` 仅改 Next 和对应 ESLint 配置两个版本。lockfile 的 40 个版本变化全部属于上述包族；另有 npm 自动重算的 `peer` 标记和平台 `libc` 元数据。没有新顶层依赖或 `overrides`。sharp 的现有 `^0.35.4` 范围接受 `0.35.5`；source-map-js 在既有范围内更新。当前 lockfile 锁定安全版本，但没有改变宿主在另一 lockfile 中自行解析依赖的责任。

## 审计残留逐项说明

以下是干净安装后的完整 `npm audit --json`：1 low、4 moderate、14 high、1 critical，exit 1。计数是 npm 报告的包条目，包含上层包的传递影响，不等同于 20 个独立 advisory。`npm audit --omit=dev --json` exit 0，各 severity 均为 0。依赖来源用 `npm explain <包> --json` 核实。

| 残留包 | severity | 当前来源与处置 |
| --- | --- | --- |
| `proxy-addr` | critical | `shadcn → @modelcontextprotocol/sdk → express` 的开发工具链；留给开发依赖修复，不能因此视为无风险。 |
| `@modelcontextprotocol/sdk` | high | `shadcn` 开发依赖；本轮未运行其 MCP 服务。后续应修复该工具链。 |
| `@next/eslint-plugin-next` | high | ESLint 配置经 `fast-glob` 受传递影响；Next runtime 的审计已清零，lint 工具链仍待修复。 |
| `eslint-config-next` | high | 上一条传递影响的直接开发依赖；已随 Next 更新，但传递告警尚存。 |
| `@redocly/openapi-core` | high | SDK `openapi-typescript` 生成工具经 `js-yaml` 受传递影响；单列后续 codegen 工具链修复。 |
| `@ts-morph/common` | high | `shadcn → ts-morph` 开发工具的传递影响；随该工具链处理。 |
| `ts-morph` | high | `shadcn` 开发工具的传递影响；随该工具链处理。 |
| `shadcn` | high | 直接开发工具依赖，多条传递影响汇总；不在本轮扩大到 CLI 版本迁移。 |
| `brace-expansion` | high | ESLint、Redocly、ts-morph 各自的 minimatch 子树；后续需检查每个嵌套版本，不能只改顶层。 |
| `braces` | high | `micromatch → fast-glob` 子树，供 ESLint/shadcn 使用；留给开发依赖修复。 |
| `fast-glob` | high | ESLint 和 shadcn 的多个嵌套实例，由 `micromatch` 传递影响；随各父依赖处理。 |
| `micromatch` | high | 上述 glob 工具链，经 `braces` 受影响；留给开发依赖修复。 |
| `fast-uri` | high | `shadcn → MCP SDK → ajv` 子树；留给该工具链修复。 |
| `js-yaml` | high | ESLint、Redocly 和 shadcn 配置读取工具共享的开发依赖；后续需验证 lint/codegen/CLI。 |
| `undici` | high | 告警节点是 shadcn 的 `undici@7.29.0`；jsdom 另有 `8.10.2`，不在此次告警节点内。 |
| `hono` | moderate | shadcn 的 MCP SDK 服务依赖；留给开发工具链修复。 |
| `ip-address` | moderate | shadcn 的 socks 与 MCP SDK/express-rate-limit 子树；留给开发工具链修复。 |
| `postcss-selector-parser` | moderate | shadcn CLI 的开发依赖；未作为 Next 生产依赖保留告警。 |
| `qs` | moderate | shadcn 的 MCP SDK/express/body-parser 子树；留给开发工具链修复。 |
| `esbuild` | low | SDK tsup 与 Vite/Vitest 使用的开发构建工具；后续修复时重跑打包与测试。 |

这些残留中的一部分可在兼容范围内继续更新，但需要单独验证开发工具链，本轮限定生产 advisory 修复。npm 还给部分汇总条目建议 `eslint-config-next@14.2.35` 或 `shadcn@1.0.0`；它们是跨 major 回退，不能机械执行 `npm audit fix --force`。本轮没有忽略规则、severity 降级或审计过滤配置；`--omit=dev` 是计划指定的生产检查，同时保留了完整审计结果。

## 命令与验证结果

工作目录：`/Users/aaron/projects/_worktrees/ledger/codex/ledger20-t18/web`。命令通过 Python `subprocess.run` 执行，表中的秒数为实际显式 timeout；构建/测试使用 `check=True`，审计保留 exit code 后解析 JSON。等价调用形式如下：

```python
import subprocess
subprocess.run(["npm", "run", "-w", "@azex/ledger-react", "build"], timeout=240, check=True)
```

| 命令 | timeout | 结果 |
| --- | --- | --- |
| `npm ci`（基线） | 600s | PASS；完整审计 23，生产审计 3。 |
| `npm view next@16.3.8 version engines peerDependencies optionalDependencies repository --json` | 60s | PASS；同样核实 `next@16.4.0`、`eslint-config-next@16.3.8`、`sharp@0.35.5`、`source-map-js@1.2.2`。 |
| `npm install`（两个 manifest 版本修改后） | 600s | PASS。 |
| `npm update sharp source-map-js --save=false` | 600s | PASS；更新 lockfile 中兼容范围内的传递依赖。 |
| `npm ci`（修复后，重新安装本 worktree 依赖） | 600s | PASS；855 个安装包，无安装错误。 |
| `npm audit --omit=dev --json` | 120s | PASS，exit 0，total 0。 |
| `npm audit --json` | 120s | exit 1，total 20，详见残留表。 |
| `npm run -w @azex/ledger-react build` | 240s | PASS，9 个入口的 ESM/DTS 构建完成。 |
| `npm run build` | 600s | PASS，Next 16.3.8 生产构建与 16 个静态页面生成完成。 |
| `npm run -w @azex/ledger-react typecheck` | 180s | PASS。 |
| `npm test -w @azex/ledger-react` | 240s | PASS，43 files / 257 tests，7.66s。 |
| `bash /Users/aaron/.agents/skills/delivery-gate/scripts/gate.sh .`（web） | 180s | PASS，包含 `npx tsc --noEmit`，FAIL=0 / WARN=0。 |
| 同一 delivery-gate（`web/packages/ledger-react`） | 180s | `tsc --noEmit` PASS；静态扫描 FAIL=2，见下文。 |
| `git diff --check`（仓库根） | — | PASS。 |

SDK 通用静态扫描复现两个基线失败类别：注释中的 `as any` / `has any` 被误判为类型逃逸，及既有 skeleton/template 列表的 `key={i}`。对应文件包括 `src/heroui/pages/ClassificationsPage.tsx:33`、`src/components/pages/ClassificationsPage.tsx:38`、`src/client/schema.ts:376`、`src/heroui/shared.tsx:111`、`src/heroui/pages/TemplatesPage.tsx:467`。这些源码与基线完全一致；本轮限定依赖和报告，不把源码整治混入补丁，也不把整个通用 gate 标为通过。

另外通过 60s timeout 的 Node 进程读取 `sharp.versions` 并将 2×2 的良性 SVG 转为 PNG：`sharp=0.35.5`、`rsvg=2.63.2`、`vips=8.18.7`，输出 95 bytes。此项确认当前 macOS 原生包可用。没有在 Linux 上复现漏洞、执行真实生产部署或验证全局自装 librsvg；若宿主选择全局 librsvg，仍须按 sharp 官方 advisory 单独确认 `2.63.2`。

## 交接

仅修改 `web/package.json`、`web/package-lock.json` 与本轮两份文档。没有引入其他依赖分支，没有调整 SDK 源码、应用配置或 server。本轮基线尚未含 Task 5/6，测试结果对应本轮基线与本次依赖补丁；集成后的最终检查由主控执行。生产 audit 是本次查询时的数据库快照，后续新增 advisory 需要持续复核。

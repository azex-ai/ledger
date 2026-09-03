# 整改后独立复审（R3）— 2026-09-03

> 五个零前情 agent 对整改后 main `c854c6e` 的只读审计。契约 `docs/plans/2026-09-03-independent-review-contract.md`。
> 它们没读过前两轮的报告、TODO 与 lead 的口径；本 README 由 lead 在收齐后写「对答案」。整改在 `docs/plans/2026-09-03-wave5-contract.md`（Wave 5）。

## 一句话结论

> **三轮审计、四波整改都在防「篡改已存在的行」；没有一轮问过「追加一行合法形状的行」。**
> INSERT 路径既不拦也不留痕（24 个 BEFORE UPDATE + 13 个 BEFORE DELETE 守卫、6 个 AFTER UPDATE 审计、0 个 INSERT 期守卫），于是往模板表追加一条 leg 让诚实充值多认 N 倍、往 bookings 插一行让 recheck job 签出凭空入账、往游标表前跳让真实充值永远不入账——三个 territory 从三个方向撞上同一个根因。第二个新形态是 `pg_temp` 遮蔽：守卫函数没钉 `search_path`，一张同名 TEMP 表让借贷平衡触发器对空表算平。第三个是我们自己的门禁：README 可运行门禁只跑 4/24 个块，Quick Start 两块不编译。

## 三轮对照

| 报告 | Critical | Major | Minor | 新发现（前三轮从未提出） | 复发（前三轮修过的形态） |
|---|---|---|---|---|---|
| money-out | 2 | 4 | 4 | INSERT 路径无守卫无审计（C-1 C-2 M-1 M-4）；I-11 gated 路径 pin 空转（M-3） | `reversal_of` 伪造链接仍有口（M-2，I-51 的边界） |
| install-roles | 2 | 5 | 7 | `pg_temp` 遮蔽（C1 C2）；`application_name` 可自报绕过守卫（M2）；021/024 上限参照物可写（M3 M4） | 共享集群二次安装 dirty（M1，D-M2 的邻居）；R2 凭证钉死 TAMPERED（M5，C-M3 的邻居） |
| consumer | 2 | 10 | 16 | README 教「迁移凭证 = 运行时凭证」且无信号（C1）；README 可运行门禁只跑 4/24 块（C2） | pending bundle 闸不存在（M2）；`SetDepositIngester` 接线陷阱（M6） |
| onchain-ops | 2 | 9 | 12 | `chain_cursors` 无护栏无审计（C-1）；dead-letter 无读面（C-2）；`auto_reverse` 单次观测（M-1） | RUNBOOK 与代码不一致多处（M-4 M-5 M-7） |
| gates-pins | 0 | 9（F-1…F-9，其中 3 条标高） | — | `journals.idempotency_key` 的 UNIQUE 删掉整套零失败（I-3 的 15 个 pin 全在 Go 层）；25 个 append-only 触发器 22 个无 pin；db-only 登记对象是全文件子串匹配（F-1 F-2 F-3）；Docker 缺失 `make test` 假绿（F-9） | I-50 符号门禁可绕（F-4）、metrics 门禁只查名字（F-5）、census 数属性名（F-6）——三条都是上一轮刚加的门禁 |

**账务引擎本身**：consumer 与 money-out 都实测双分录、幂等三态、TOCTOU 预留、reversal、solvency、16 项对账、`RunInTx` clone 拒绝点按文档表现；money-out 拆 I-49 / I-65 / I-64 / I-32 / I-33 / I-37 的 pin 全部载重。**问题全在「谁能往表里放一行」与「文档教的接法」。**

## 处置
Wave 5（`2026-09-03-wave5-contract.md`）：W5-insert（INSERT 守卫与审计、`chain_cursors` 证据与回看重扫、attestation 链约束）、W5-searchpath（钉 `search_path`、去重集合出 pg_temp、撤 TEMPORARY）、W5-readme（全部 24 块必编译、Quick Start 拆凭证、运行时凭证 warning）、W5-onchain-ops、W5-money-misc、W5-gates。六支于 2026-09-04 01:45 SGT 全部合入 main `0e7512a`，随后进入下面的复核。

## 复核（recheck，2026-09-04）

五名复审员各自对 main `5a11533`（代码 `0e7512a`）用**原始 PoC / 原始 mutation** 逐条重跑，报告在 `recheck/`。判据：零 Critical 复发。

| 复审员 | 原 Critical 复发 | 已闭合 / 部分 / 未闭合（Minor） | 复核中新发现 | 处置 |
|---|---|---|---|---|
| money-out | **0**（C-1 由 029 guard、C-2 由 I-69 corroborate；反向确认各红 3 条） | 6 / 2 / 4（m-1 由 PLAUSIBLE 升 CONFIRMED） | **N-1 Critical**：同一真实链上 log 复制成 N 个 booking 全部签出（含配 DepositConfirmer）；N-2/N-3 Minor | 契约 5.16 → **migration 032**（deposit 身份唯一索引 + `corroborateBeforeConfirm` 问「已入账？」+ N-2 review 过渡不抢占真 `channel_ref` + RUNBOOK §21 discard 时限），I-71 |
| install-roles | **0**（C1/C2 由 030；反向确认装回 pre-030 守卫 PoC 复现） | 10 / 3 / 3 | **N1 Critical**：`SET CONSTRAINTS ALL IMMEDIATE`（零权限）让 030 的 journals 级延迟检查零分录空过、逐行检查因 xmin 全跳过，三形态提交单边分录 | 契约 5.15 → **migration 031**（删 xmin 跳过与 journals 级触发器，逐行无条件聚合；时机电池从 `tgdeferrable` 目录派生；装回 030 的跳过必须能攻破的反向确认 pin），I-68 修订；m4/m1 顺带 |
| consumer | **0**（C1 运行时角色信号、C2 README 编译门禁，均机制性） | 6 / 4 / 11 Minor 未动 | F-M10 新口径反了一半（GOPROXY 依赖；`go mod tidy` 两个原因）；`.env.example` 与新 Quick Start 冲突；openapi 又多两条 api.md 未收录 | lead 直接修文档/契约面 `f1d3992` |
| onchain-ops | **0**（C-1 UPDATE 三面锁死；C-2 dead-letter 可计数可读可 replay；I-69 三条全过） | 15 / 3 / 5 | 新链首次 INSERT 游标无上限（029 头注结论句对 INSERT 分支过宽）；webhook 路径 `ErrConflict` dead-letter 不计数、Backlog 仅 Reader 部署采样；RUNBOOK 把 decimals×exponent 闸引用成 I-69 | 契约 5.17 → 并入 032（`ledger_chain_cursors_insert_guard` + owner-only `ledger_seed_chain_cursor`；conflict 分支走计数路径 + 独立 backlog 采样 job；`identity_already_booked` reason） |
| gates-pins | **0**（F-1..F-9、P-1..P-6 原 mutation 全部红在门禁本身或新 pin） | 11 修 / 2 按拍板保留 | R-4 中：五个导出 money helper 对 `1E999999999` 不返回，`FuzzAllocate` 结构性造不出正指数；R-3 release workflow 两 submodule job 无 govulncheck；R-2 `entry_attestations_no_delete` 无 pin。两处 lead 集成改动判合理未放松 | 契约 5.18 → w5-gates-2（存储界/算术界分离；`Round`/`ConvertAt` 返回 error；`FuzzAllocateFromStrings`；release 矩阵门禁；census 改函数集合） |

**复核的净结论**：Wave 5 的修复没有一条复发；但复核在修复本身里找出两条新 Critical（N1、N-1），两者都在复核当日修完并合入（031、032），且各带「拆掉修复 → PoC 必须重新成功」的反向确认 pin。最终 main `79ff80a`：Go 全包 -race、`chains/evm`、`anchors/r2`、golangci-lint 0、`sqlc diff`、openapi codegen、ledger-react 225 用例全绿；INVARIANTS 到 I-71。

### 复核给出的方法教训（进 lead 口径）

1. **guard 不得依赖任何调用方能改的东西——写得进去的存储，或挪得动的时机。** 030 修掉了「状态」那一半（pg_temp memo），却用「时序」换了它（`journals.xmin` 跳过 + 延迟触发器）；`SET CONSTRAINTS` 归调用方。写 gate 时要把「调用方可调的求值开关」从目录枚举出来逐个重放攻击电池（`tgdeferrable`），而不是只在默认模式测一遍。
2. **「链上有没有」不等于「是否已入账」。** I-69 让 booking 的主张可被链回答，但一条真实 log 的忠实副本满足全部比对；只有账本自己能回答「我数过几次」。身份唯一性要落在 DB 约束上，键必须是行的真实身份而不是写者自选的列。
3. **一个 fuzz target 的有效性只对它的输入编码成立。** `FuzzAllocate` 用 `NewFromBigInt(x,-exp)` 永远造不出正指数；「30 秒预算有效」的结论不能跨 target 推广。存储界与算术界要分开（helper 的职责就是降精度），且上下两个方向的指数都要拦。
4. **复核报告的自我更正要跟进公告口径**：REST 面被 `parseWireAmount` 拒 e/E 只是偶然挡住；I-70 类问题真正的可达面是库模式与 webhook `ParseSighting`。
5. **实验基础设施也会静默失败**：共享 Docker 下手起容器 name 冲突被 `>/dev/null 2>&1` 吞掉，实验写进了别人的库。唯一 name + 端口探测 + 不吞报错。

### 未闭合（记 TODO「Lead 追加」，不阻塞发版评估）
install-roles m2/m3/m7；consumer 11 条 Minor 与 N-2（首轮存证 ERROR 噪声）、F-M1 残余 3（HTTP 面无端点挂 lifecycle）、F-M3 残余（无闸视角可用额读接口）；money-out m-1..m-4、N-3 结构性修法；onchain-ops Minor-5/6/7/9/11、M-6 残余、5 条「其他观察」、sweep booking 身份索引、receipt 表身份；gates-pins P-4/P-5、branch protection（需 Aaron 在 GitHub 侧确认）；w5-searchpath-2 记的零分录 journal 对账检查与「调用方可改求值时机」面的系统扫描。

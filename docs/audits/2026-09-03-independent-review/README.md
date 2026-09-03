# 整改后独立复审（R3）— 2026-09-03

> 五个零前情 agent 对整改后 main `c854c6e` 的只读审计。契约 `docs/plans/2026-09-03-independent-review-contract.md`。
> 它们没读过前两轮的报告、TODO 与 lead 的口径；本 README 由 lead 在收齐后写「对答案」。整改在 `docs/plans/2026-09-03-wave5-contract.md`（Wave 5）。

## 一句话结论（收齐四份时的初判，gates-pins 待）

> **三轮审计、四波整改都在防「篡改已存在的行」；没有一轮问过「追加一行合法形状的行」。**
> INSERT 路径既不拦也不留痕（24 个 BEFORE UPDATE + 13 个 BEFORE DELETE 守卫、6 个 AFTER UPDATE 审计、0 个 INSERT 期守卫），于是往模板表追加一条 leg 让诚实充值多认 N 倍、往 bookings 插一行让 recheck job 签出凭空入账、往游标表前跳让真实充值永远不入账——三个 territory 从三个方向撞上同一个根因。第二个新形态是 `pg_temp` 遮蔽：守卫函数没钉 `search_path`，一张同名 TEMP 表让借贷平衡触发器对空表算平。第三个是我们自己的门禁：README 可运行门禁只跑 4/24 个块，Quick Start 两块不编译。

## 三轮对照

| 报告 | Critical | Major | Minor | 新发现（前三轮从未提出） | 复发（前三轮修过的形态） |
|---|---|---|---|---|---|
| money-out | 2 | 4 | 4 | INSERT 路径无守卫无审计（C-1 C-2 M-1 M-4）；I-11 gated 路径 pin 空转（M-3） | `reversal_of` 伪造链接仍有口（M-2，I-51 的边界） |
| install-roles | 2 | 5 | 7 | `pg_temp` 遮蔽（C1 C2）；`application_name` 可自报绕过守卫（M2）；021/024 上限参照物可写（M3 M4） | 共享集群二次安装 dirty（M1，D-M2 的邻居）；R2 凭证钉死 TAMPERED（M5，C-M3 的邻居） |
| consumer | 2 | 10 | 16 | README 教「迁移凭证 = 运行时凭证」且无信号（C1）；README 可运行门禁只跑 4/24 块（C2） | pending bundle 闸不存在（M2）；`SetDepositIngester` 接线陷阱（M6） |
| onchain-ops | 2 | 9 | 12 | `chain_cursors` 无护栏无审计（C-1）；dead-letter 无读面（C-2）；`auto_reverse` 单次观测（M-1） | RUNBOOK 与代码不一致多处（M-4 M-5 M-7） |
| gates-pins | 待 | 待 | 待 | | |

**账务引擎本身**：consumer 与 money-out 都实测双分录、幂等三态、TOCTOU 预留、reversal、solvency、16 项对账、`RunInTx` clone 拒绝点按文档表现；money-out 拆 I-49 / I-65 / I-64 / I-32 / I-33 / I-37 的 pin 全部载重。**问题全在「谁能往表里放一行」与「文档教的接法」。**

## 处置
Wave 5（`2026-09-03-wave5-contract.md`）：W5-insert（INSERT 守卫与审计、`chain_cursors` 证据与回看重扫、attestation 链约束）、W5-searchpath（钉 `search_path`、去重集合出 pg_temp、撤 TEMPORARY）、W5-readme（全部 24 块必编译、Quick Start 拆凭证、运行时凭证 warning）、W5-onchain-ops、W5-money-misc、W5-gates（待 gates-pins）。全部合入后 R3 五名复审员各自复核自己的发现，零 Critical 复发才进入发版评估。

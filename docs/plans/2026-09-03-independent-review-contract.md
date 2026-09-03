# 整改后独立复审 + 对抗式审计 — Phase 0 契约

> 2026-09-03 · Status: **RECHECK（复审报告已出并全部由 Wave 5 修复合入 main `0e7512a`；2026-09-04 起五名复审员对各自发现做复核）** · 目标 = 整改后的 main `c854c6e`。
> Aaron 2026-09-03 拍板：全量整改完成后再做一轮**独立**的 review 与对抗式审计，通过后才考虑合并与发版。
> 方法沿用 `2026-08-25-financial-audit-contract.md` §1（纪律）§3（产出格式）；本文只定独立性规则与攻击面。

## 0. 「独立」的定义（这一轮与前两轮的区别）

前两轮的 agent 拿到的是 lead 整理过的 TODO、口径与复核记录——它们审的是「lead 认为的问题」。这一轮：

- **新 agent、零前情**：prompt 里**不给** `lead-verification.md`、`TODO.md`、两份 w3-review、契约 §7 的口径表。只给：仓库、`docs/INVARIANTS.md`、`docs/plans/2026-08-21-tamper-evident-ledger-design.md`（威胁模型）、`CLAUDE.md`、以及一句话「这个库刚做完一轮 200+ 条的整改，你的工作是证伪它」。
- **先自己找，再对答案**：报告交付后，lead 才把三轮审计的清单给它们做**差异对比**——它们找到而我们没找到的，是这一轮的真实产出；我们修了而它们仍能攻破的，是复发。
- **只读 + 独立 worktree**：反转实验一律在 `wt create ledger r3-<name>` 的 worktree 里，主树不碰（本轮实证：共享工作树互相投毒、lead 误删 worker worktree）。

## 1. Territory（5 个，按攻击面切）

| # | 名 | 攻击面（给 agent 的原话） | 类型 |
|---|---|---|---|
| R3-A | money-out | 「持有 `ledger_app` 凭证，让钱离开系统或让账本多认一笔钱。每个决定金额的算式逐项列出读哪张表、谁能写。」 | security-reviewer / opus |
| R3-B | install-and-roles | 「持有迁移凭证 / 应用凭证 / 只读凭证 / R2 凭证各一种，各自的 blast radius；安装机制（`postgres.Migrate`）在 fresh cluster、共享 cluster、单凭证部署三种形态下的行为。」 | security-reviewer / opus |
| R3-C | gates-and-pins | 「对仓内每一个 `*_test.go` 里自称门禁（gate）或 pin 的测试做 mutation：它本该抓到而抓不到的漂移。附 INVARIANTS I-1…I-65 逐条『pin 能否证伪』。」 | code-reviewer / opus |
| R3-D | consumer-first-hour | 「你是第一次 import 这个库的工程师，只读 README / COOKBOOK / examples / godoc，按文档把充值→预留→结算→提现→对账跑通；每一处文档与代码不符、每一处默认关着而没告诉你的保护、每一处签名不体现的接线顺序。」 | code-reviewer / opus |
| R3-E | onchain-and-ops | 「链上入账到账本、归集出链、对账与取证：单次 RPC 观测做不可逆决策的地方、失败后游标/状态机的去向、on-call 凌晨三点按 RUNBOOK 能不能查到发生了什么。」 | code-reviewer / opus |

## 2. 产出

每人一份 `docs/audits/2026-09-03-independent-review/<name>.md`（格式同 2026-08-25 §3：摘要 / 发现 [Critical|Major|Minor] 带 `file:line` 与 CONFIRMED|PLAUSIBLE / 移交 / 我没能验证的）。lead 交叉比对后写 `README.md`：三轮对照表（新发现 / 复发 / 已关闭），并据此决定是否进入合并与发版评估。

## 3. 通过标准（进入发版评估的门）

- 零 Critical 复发（前两轮已修的形态不得再被攻破）
- 新 Critical 为零，或全部修复并再过一次 R3-A / R3-B 的复核
- R3-C 的门禁 mutation 盲区不得有「守形状不守语义」类
- R3-D 从 README 起手能跑通全链路且默认关着的保护有信号

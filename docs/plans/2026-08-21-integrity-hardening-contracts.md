# Phase 0 契约层 — 完整性加固 Wave 1（P1–P7）

> 2026-08-21 · Status: CONTRACT（**dev 的唯一 SoT，先于所有实现**）
> 设计 SoT：`docs/plans/2026-08-21-tamper-evident-ledger-design.md`（读它拿 WHAT 与理由）
> 本文只定 **跨任务的契约与资源分配**，不含实现细节。
> 存在理由：P1–P7 有五个共享写盘点（migration 号、033 函数体、`core/interfaces.go`、
> `INVARIANTS.md` 尾部、`sqlcgen/` 生成物）。没有本文，多个 dev 上去第一件事就是抢 042。
>
> **红线：本文由 Team Lead 独占写权。dev 只读。** 需要改契约 → `bus send team-lead` 提请，不要自己改。

## 1. migration 号分配（独占，不得挪用）

| 号 | 归属 | 内容 |
|---|---|---|
| `042` | P1 | roles + grants（`ledger_owner` / `ledger_app` / `ledger_ro`） |
| `043` | P2 | checkpoint rebuild 支撑 + 对账 resume cursor 持久化 |
| `044` | P3 | per-journal / per-currency 借贷平衡 deferred constraint trigger |
| `045` | P4 | mutation guards（classifications / reservations / period_closes / journals.event_id） |
| `046` | P5 | `journals` 的 auth 三列 |
| `047` | P6 | `ledger_attestations` + `entry_attestations` |
| `048` | 预留 | P7（Merkle 换列语义，若需要） |

- 每个 migration **必须**有 `.up.sql` + `.down.sql`；不可回滚需显式注明理由（`deployment.md`）。
- 必须可重入（`IF NOT EXISTS` / `ON CONFLICT DO NOTHING`）。
- 加索引用 `CREATE INDEX CONCURRENTLY`。
- **已合入的 migration 永不修改** —— 号被占了就用下一个空号并 `bus send team-lead` 报备。

## 2. `ledger_journals_block_arbitrary_update()` 的演进链

033 已把规则写进注释：**任何给 `journals` 加列的 migration 必须同步重建该函数**。
P4 与 P5 都要改它 —— 不是冲突，是**顺序接力**。每个 migration
`CREATE OR REPLACE` 到「截至本 migration 为止的完整列表」：

| 阶段 | 受保护列 |
|---|---|
| 现状（033） | `id, journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of, created_at, effective_at, uid` |
| `045`（P4） | 上列 **+ `event_id`**，但**放行 `NULL → 非NULL` 的单次跃迁**（033 注释描述过、但从未实现的那个 set-once 语义），并给该列补 FK |
| `046`（P5） | 045 的列 **+ `auth_digest`, `auth_signature`, `auth_key_id`** |

⚠️ **A4 的教训**：033 的注释说「WHEN clause below still permits」——
018:137-140 的 trigger 是无条件 `BEFORE UPDATE FOR EACH ROW`，**那个 WHEN 子句不存在**。
045 要么真的加 WHEN 子句，要么在函数体内实现 set-once 判断。**不要再留一句描述不存在机制的注释。**

## 3. `.sql` 查询文件分配（避免 sqlcgen 生成物冲突）

`sqlc` 按 `.sql` 文件名分文件生成。**新查询一律进本任务独占的新文件**，不要往共享文件里塞：

| 任务 | 独占的新 `.sql` | 允许修改的既有 `.sql` |
|---|---|---|
| P1 | —（纯 DDL） | 无 |
| P2 | `queries/integrity_checkpoint.sql` | **独占** `queries/reconcile.sql`、`queries/checkpoints.sql` 的修改权 |
| P3 | `queries/integrity_balance.sql` | 无（新 per-journal check 进独占文件，**不要**改 `reconcile.sql`） |
| P4 | —（纯 DDL） | 无 |
| P5 | `queries/integrity_auth.sql` | `queries/journals.sql`（仅 INSERT 加三列） |
| P6 | `queries/integrity_attestations.sql` | 无 |

改完一律 `make sqlc` 并把 `postgres/sqlcgen/` 的生成物一起提交；`make sqlc-diff` 是 CI 门禁。

## 4. `core/interfaces.go` 的 port 定义（逐字，dev 直接用）

只有 P5 与 P6 碰这个文件，且 **P6 blocked-by P5**（串行），故无并发写。

```go
// Attestor abstracts the key that authorizes a posting. The private key lives
// in a KMS/HSM and never enters the database or the app config: a DB
// compromise must not yield the ability to mint a valid authorization.
// Deliberately distinct from core.Signer (EVM sweep transactions) -- different
// key, different blast radius, never the same instance.
type Attestor interface {
	Sign(ctx context.Context, digest []byte) (signature []byte, keyID string, err error)
}

// AuthVerifier needs only the public key, so verification can run entirely
// outside the database host -- that independence is the whole point.
type AuthVerifier interface {
	Verify(ctx context.Context, digest, signature []byte, keyID string) error
}

// Anchor publishes an attestation head to storage the ledger's own database
// credentials cannot reach. Implementations live in the consumer's composition
// root (object-lock bucket in a separate cloud account; optionally a public
// chain). The library ships only a local-filesystem implementation for dev.
type Anchor interface {
	// Publish is idempotent per seq: re-publishing the same seq with identical
	// bytes must succeed, with different bytes must return an error.
	Publish(ctx context.Context, seq int64, head []byte) error
	// Head returns the highest seq the anchor knows about, or 0 if empty.
	// It must read from the anchor, never from the ledger database.
	Head(ctx context.Context) (seq int64, head []byte, err error)
}
```

**命名红线**：不得复用 `core.Signer`（`core/interfaces.go:437`，那是 EVM sweep tx 签名）。

## 5. `INVARIANTS.md` 编号预分配

| ID | 归属 | 规则 |
|---|---|---|
| I-22 | P1 | `ledger_app` role 无 DDL / DROP / TRUNCATE 权限 |
| I-23 | P2 | checkpoint / system_rollups / balance_snapshots 可由 entries 精确重算，drift = 0；检测不自动修复 |
| I-24 | P3 | 每个 journal 在 **DB 层** per-currency 借贷平衡（不依赖应用层） |
| I-25 | P4 | 参与余额计算的非-journal 表 insert 后不可任意改 |
| I-26 | P5 | 每个 journal 携带有效 KMS 授权签名；无签名/验签失败的 journal 不可提现 |
| I-27 | P6 | attestation 链完整：seq 连续、prev_root 链接、签名有效、每条 entry 恰好被一个 attestation 覆盖 |
| I-28 | P6 | 最新外部锚 head 与 DB attestation 链一致 |

规则：
- **各 phase 只追加自己那一节**，按编号顺序插入，不动别人的段落。
- 追加前必须**已有 pin test 且实测通过** —— 无 pin 不得写进 `INVARIANTS.md`
  （该文件是 canonical contract，写进去就是承诺）。
- 合并冲突时以编号顺序为准。

## 6. 依赖图（board 的 blocked-by 依此设置）

```
P1 (roles) ──┬──> P3 (DB balance trigger)
             └──> P4 (mutation guards)
P2 (checkpoint) ── 独立，可立即开工
P5 (per-journal 签名) ──> P6 (batch digest + 外部锚) ──> P7 (Merkle, 可选)
```

- **P1 是 P3/P4 的前提**：没有 role 分离，新加的 trigger 同样能被 `DROP`，等于没加。
- **P3 有任务内前置**：先跑一次全量 per-journal 扫描确认存量无违规，**再**上 trigger。
  存量有违规就上 trigger = 上线即拒写。这一步的结论必须 `bus checkpoint` 记录。
- **P2 独立**，但它改的 `reconcile.go` check#2 刚被 P0 动过（`main` 已含），从 `main` 开分支即可。
- **P7 不在本波交付要求内**：仅当 P6 完成且 Aaron 明确要 inclusion proof / 定位能力时才做。

## 7. 未拍板项 → 做成接口位，不要替 Aaron 选默认值

`abstractions.md`：能不决定就不决定，结构里留接口、不留具体选型。

| 未拍板项 | dev 的正确做法 |
|---|---|
| P5：KMS 不可用时 fail-closed / fail-open | 做成注入配置 `AttestorFailureMode`（枚举）。**缺省值 = `Run()` 启动报错**，不是任一档。沿用 M3.1 secure-by-default 先例（crypto-deposit 设计 §9.2 addendum）。等 `bench-postjournal` 的延迟数字回来后 Aaron 配一个值，代码不改 |
| P5：覆盖全部 journal 还是仅 money-path | 做成 per-journal-type 的注入配置，缺省同上：启动报错 |
| P5：提现门阈值 | 注入配置，缺省报错（不得是 0 = 关闭） |
| P6：外部锚载体（S3 / R2 / 双写 / 哪个云账号） | 只实现 `Anchor` port + 本地文件系统 adapter（dev only，必须显式构造）。生产 adapter **不做** |

**禁止**：自己挑一个"合理默认值"然后在 PR 里说"可配置"。这一项的默认值直接等于信任边界敞开。

## 8. 写盘隔离与分支

- 每个任务独立 worktree，**一律走 `wt`**（`hive.md` 纪律 / `codex-coop.md` 纪律 A）：
  ```
  wt create ledger integrity-p1-roles      # 落 /Users/aaron/projects/_worktrees/ledger/integrity-p1-roles
  wt path   ledger integrity-p1-roles      # 给脚本用
  wt remove ledger integrity-p1-roles      # 完成合并后必须清理，不允许"先留着"
  ```
- 分支名 = feat 名：`integrity-p<N>-<slug>`（flat，不带斜杠）。
- **基线一律 `main`**（P0 已合入 `44c32eb`）。
- Team Lead 统一合并；**流水线 > 批处理**：一个完成即 review + 合并，不等全部完成。

## 9. 每个任务的 Done 标准（通用，任务描述里不再重复）

1. `make build` / `make vet` / `make lint` 全绿；`make sqlc-diff` 干净（若碰过 `.sql`）。
2. `make test` 全绿（testcontainers 真 Postgres，无 mock DB）。
3. **新 invariant 必须有 pin test，且验证过"未修复时确实失败"** ——
   不能只看到通过就算完（`working-agreements` §3：如果这步根本没跑，输出会有任何不同吗？）。
4. 契约面若变动（`docs/openapi.yaml` / `web/packages/ledger-react/src/client/schema.ts`）
   必须同步，且只做 expand（加字段），不改既有字段语义（`api-contract.md` §8 / `deployment.md`）。
5. 修 bug 时**一并修掉指向它的过期注释**。P0 已发现三处同形状的
   「注释把 bug 固化成文档」（`reconcile.sql` 的 `pass (0,0)`、check#8 的意图注释、
   `journal_dr_cr` 声称 DB constraint 仍在 enforce）。不要再制造第四处。
6. `bus done <id> <agent>`；有新 failure mode 就 `bus learn <agent> "<经验>"`。

## 10. 禁止事项（跨任务共同）

- 不改本文（Team Lead 独占写权）。
- 不动别人分配到的 migration 号 / `.sql` 文件。
- 不在领域层（`core/`、`<domain>/`）import `net/http` / `pgx` / `slog` / `chi`（`golang.md`）。
- 金额一律 `decimal.Decimal` + `NUMERIC(30,18)` + JSON 字符串（`financial.md`）。
- **DB 事务内禁止外部 API 调用**（`financial.md`）—— P5 的 KMS 调用必须在事务外，
  这是设计稿 §7.2 把 digest 放在 uid-space 的唯一理由，不要"优化"回 id-space。
- 不用 `_ = someVar` 压编译错误；不丢弃 error（`golang.md`）。
- 不写「未来可能要支持 X」进当前结构（YAGNI），但保留接口位。

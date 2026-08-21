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
| `049` | P1-**migrate 阶段** | `REVOKE ALL ON SCHEMA public FROM PUBLIC` + ownership 转给 `ledger_owner`。**必须与 `DATABASE_URL` 切换同一次发布上线** —— 它会让在座连接角色失去全部权限（2026-08-21 review 发现 042 原稿把这两步混进 expand，等于破坏性 cutover 伪装成 expand）。042 保持纯增量：只建 role + GRANT，不 REVOKE 任何东西、不动 ownership |

- 每个 migration **必须**有 `.up.sql` + `.down.sql`；不可回滚需显式注明理由（`deployment.md`）。
- ⚠️ **权限类改动的测试陷阱**（2026-08-21 实例）：testcontainers 用 `test`
  （`internal/postgrestest/postgrestest.go:61`）、docker-compose 用 `ledger`
  （`POSTGRES_USER`）—— **两者都是容器初始用户 = 真 superuser，绕过一切权限检查**。
  任何 REVOKE / ownership / GRANT 类改动，pin test 若不显式建一个**非-superuser 角色**
  来扮演在座连接身份，就测不出「旧角色被踢掉」这一类回归。托管 Postgres 上 master user
  **不是**真 superuser（RDS 的 `rds_superuser` 不绕过权限检查），所以这类 bug 只在生产暴露。
- 必须可重入（`IF NOT EXISTS` / `ON CONFLICT DO NOTHING`）。
- 加索引用 `CREATE INDEX CONCURRENTLY`。
- **已合入的 migration 永不修改** —— 号被占了就用下一个空号并 `bus send team-lead` 报备。

## 2. `ledger_journals_block_arbitrary_update()` — 改成通用比较，不再逐 migration 接力

> **2026-08-21 Team Lead 裁决（推翻本节原方案）。** 起因：`p5-authsig` 指出原方案有个
> 无法安全落地的排序冲突 —— 契约要求 046 保护「045 的列 + auth 三列」，但 045 尚未写出，
> P5 无法知道 P4 的 `event_id` set-once 实现；而 golang-migrate 按数字顺序执行
> （045 先于 046），两个 migration 各做一次 `CREATE OR REPLACE`，**后者会静默覆盖前者**，
> 于是 045 刚加的 `event_id` 保护在 046 跑完后消失。它拒绝猜测而是上报，判断正确。

原方案的根因不是排序，是**结构**：033 把列清单**硬编码**在函数体里，并把
「任何给 journals 加列的 migration 必须记得重建此函数」写成一条**要人记住的规则**。
`working-agreements` §5：能被结构强制的，就不该靠人记忆 —— 而这条规则已经被违反过一次
（033 自己就是在修 025/031 漏加列的后果）。

**新方案：函数改成通用比较**，用 `to_jsonb(OLD/NEW)` 减去一个显式的可变列白名单：

```sql
CREATE OR REPLACE FUNCTION ledger_journals_block_arbitrary_update() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    -- 唯一允许 post-insert 变化的列。加列到这个数组是一次显式、可评审的决定；
    -- 不在数组里的列（含所有未来新增列）默认受保护 —— fail-closed。
    mutable CONSTANT text[] := ARRAY['event_id'];
BEGIN
    -- event_id 的 set-once 语义：只允许 NULL -> 非NULL 的单次跃迁。
    IF OLD.event_id IS NOT NULL AND NEW.event_id IS DISTINCT FROM OLD.event_id THEN
        RAISE EXCEPTION 'ledger: journals.event_id is set-once and already set'
            USING ERRCODE = 'check_violation';
    END IF;

    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on journals is not allowed except the set-once event_id backfill; use a reversal journal instead'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
```

为什么这是对的：
- **新增列默认受保护**（零值语义指向最保守那一态，同 P0 给 `CheckResult.Complete` 选零值=未完成的理由）。
- **排序冲突消失**：只有一个 migration 需要碰这个函数。
- **033 那条"要人记住的规则"作废** —— 045 落地时要同步把 033 里那句
  "any migration that adds a column to journals MUST also recreate this function"
  改成「已由通用比较结构性保证，不再需要逐 migration 维护」。
- 成本：`journals` 的 UPDATE 只发生在 `event_id` 回填，频率极低，`to_jsonb` 开销可忽略。

### 归属（覆盖 §1 的分配）

| migration | 对该函数做什么 |
|---|---|
| `045`（P4） | **安装上面这个通用版本**（含 `event_id` set-once + 给该列补 FK）。同时改 033 的注释。 |
| `046`（P5） | **什么都不做 —— 删掉现有的 `CREATE OR REPLACE`**。三个 auth 列由通用比较自动覆盖，无需重建函数。在 046 里留一句注释说明为什么不需要（并指向本节）。 |

⚠️ **A4 的教训仍然适用**：033 的注释说「WHEN clause below still permits」，而
`018:137-140` 的 trigger 是无条件 `BEFORE UPDATE FOR EACH ROW`，**那个 WHEN 子句不存在**。
set-once 语义要在**函数体内**实现（如上），不要依赖一个不存在的 WHEN 子句，
也不要再留一句描述不存在机制的注释。

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
**但也不要为一个不会发生的选型建配置** —— 那是另一种过度设计（`discipline.md` §2 YAGNI）。

**部署前提（2026-08-21 Aaron 拍板）**：单体服务 + 同区域云。据此：

| 事项 | dev 的正确做法 |
|---|---|
| P5 签名密钥载体 | **不做选型，也不做配置旋钮**。`Attestor` port 保留（保证载体可换、领域层不见实现），默认实现 = **本地 ed25519**，密钥从注入配置读。对单体部署它就是生产实现，**不标 "dev only"** |
| P5 签名失败处理 | **不做 `AttestorFailureMode`**。本地进程内签名基本不会失败；真失败就 error 往上抛（`discipline.md` §6）。将来若换远程 adapter，重试/降级是**那个 adapter 内部的事**，不进 port 语义、不进领域层配置 |
| P5 覆盖范围 | **默认对所有 journal 签名**，不做 per-journal-type 旋钮。本地签名是微秒级，在一个已跑 16 次串行 DB 往返、~2.6ms 的操作上不构成开销（实测见 §7.1） |
| P5 提现门阈值 | **移出 P5**，归 P2（余额/策略域），不要混进签名任务 |
| P6 外部锚载体 | 只实现 `Anchor` port + 本地文件 adapter。生产 adapter 不做 —— 这一项**仍然**是真的未选型（跟密钥载体不同：锚定的意义就在于「在 DB 触不到的地方」，载体选择有实质差别） |

**仍然必须显式的一条**（`working-agreements` §3，防"以为在签其实没签"）：
没配密钥时不得静默跳过签名。二选一并在实现里写清楚 ——
① `Attestor` 为 nil ⟹ 特性整体关闭，三列保持空，验证侧把「无签名」判为「特性未启用」而非「验签失败」；
② 配了就必须加载成功，否则启动报错。

### 7.1 为什么延迟不是门槛（实测，2026-08-21）

`postgres/benchmarks_test.go`，M3 Max + testcontainers loopback：

| | 实测 |
|---|---|
| `PostJournal`（single / fanout） | **2.55 / 2.62 ms**，统计上无差异 |
| `ReserveSettle` | 2.24 ms |
| `GetBalance_ColdCheckpoint` | 0.74 ms |

`postJournalWithQueries` 对一个 2-entry journal 已经做 **16 次串行 DB 往返**
（缩放式 `9 + D + D' + 2N`；典型 deposit/withdrawal 是 15–20 次）。
单体部署下一次本地签名是**微秒级**，不在这个噪声之上。

⚠️ 但有一条**结构性**结论必须保留：因为签名发生在**取 advisory lock 之前**（设计稿 §7.2 的
uid-space digest 决定），它的延迟是**纯加性的，不延长任何锁的持有时间** ——
慢签名不会演变成热账户上的锁堆积。**这是不许把 digest 挪回 id-space 的第二个理由**
（第一个是 `financial.md` 禁止事务内外部调用）。

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

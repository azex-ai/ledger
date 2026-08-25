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
| `048` | P7（**已确认要做**，Aaron 2026-08-21 拍板：定位与 inclusion proof 两样能力都需要） | Merkle 换列语义：`batch_digest` → `merkle_root`，其余结构不变（P6 已留该接口位） |
| `050` | P2-补 | `checkpoint_rebuilds` append-only 审计表（挂 018 的 `ledger_block_mutation()`）。理由：`RebuildCheckpoint` 与自动修复有**相同的证据销毁性质**，只是多了一个人类决定；drift 非零的那一行就是入侵证据，必须活得比日志长 |
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

## 2.5 reconcile check 名 / 号分配（2026-08-21 补，起因：P2×P3 真冲突）

> **这是我漏掉的一类共享写盘点。** 我分配了 migration 号和 `.sql` 文件，却没分配
> `service/reconcile.go` 里的 check 名与序号 —— 而它是同一类资源。后果：P2 与 P3 各自
> 新增 check、各自改 `RunFullReconciliation`，并且 **`journal_dr_cr` 这个名字在两个分支里
> 含义不同**（P2 保留它作全局等式；P3 把全局那个改名 `global_dr_cr_equality` 并让新的
> 逐-journal check 接管此名）。5 个文件真冲突：`service/reconcile.go`、
> `postgres/reconcile_queries.go`、两个 reconcile 测试、`docs/INVARIANTS.md`。

### 裁决：P3 的命名胜出（已合入 main）

| check 名 | 语义 | 归属 |
|---|---|---|
| `global_dr_cr_equality` | 全局 Σdebit = Σcredit（原 `journal_dr_cr` 的真实行为） | P3 ✅ 已合 |
| `journal_dr_cr` | **真正逐 journal / 逐 currency** 校验 | P3 ✅ 已合 |
| `checkpoint_balance` | checkpoint vs entries 重算（P0 修过覆盖盲区） | 既有 |
| `system_rollup_integrity` | system_rollups 直接对 entries 重算 | P2（待 rebase） |
| `snapshot_integrity` | balance_snapshots 对 entries 重算 | P2（待 rebase） |

理由：`journal_dr_cr` 这个名字原本就在说谎（它跑的是全局等式），P3 的改名是 M1 修复的
组成部分，不是风格偏好。P2 的两个新名无冲突。

### 规则（后续所有任务）

1. **新增 check 必须先在本表登记名字**，`bus send team-lead` 要一行，不要自己挑。
2. **不要用序号命名函数**（`runCheck11X`）—— 序号会因别人插队而失真。用语义名
   （`runCheckSystemRollupIntegrity`）。既有的 `runCheck1..10` 不动，新增的一律语义名。
3. **check 总数只出现在测试断言里**，不进任何 prose（见 §10 的对应禁令）。
4. `postgres/reconcile_queries.go` 是共享 adapter：加方法可以，**不要重排既有方法**，
   减少 diff 冲突面。

### 合并顺序（Lead 执行）

`main ← P3`（✅ 已合，`044` + 命名修正）→ `main ← P2`（rebase 后重贴它的两个 check）
→ 其余按完成顺序。**P2 需要 rebase 到含 P3 的 main**，并把自己的两个 check 接在 P3 的命名之后。

## 2.6 hash domain separator 分配（2026-08-21 补，第四次同类失误）

> **又一个我漏掉的跨任务共享资源。** 我分配了 migration 号（§1）、`.sql` 文件（§3）、
> reconcile check 名（§2.5），却没分配 **hash domain separator 的字节值** —— 它同样是
> 全局命名空间。后果：P5-fix 把 auth digest 的 separator 改成 `0x02`，而 P6 已合入的
> `core/attestation.go:67` 的 `batchDigestDomain` **也是 `0x02`** —— 同一个包里两个不同的
> hash 构造用同一个 separator。

domain separation 的意义正是**不依赖「两边字节布局刚好不同」**。真撞上需要跨布局的
preimage collision（不可行），但一旦有人日后重构任一布局，这个保证就静默变弱了。
而编码是本设计里**唯一改不掉**的东西。

### 分配表（`core` 包内全局唯一）

| 值 | 归属 | 备注 |
|---|---|---|
| `0x00` | **RFC 6962 leaf 前缀** | ⚠️ **外部规范强制**，为互操作性而定，**不得挪用、不得改** |
| `0x01` | **RFC 6962 node 前缀** | 同上。（历史：auth digest V1 曾用 `0x01`，已退役，不得复用） |
| `0x02` | attestation batch digest（`core/attestation.go`，P6，已合入） | 不动 |
| `0x03` | attestation root hash（同上） | 不动 |
| `0x10` | **journal auth digest**（`core/auth.go`） | 从 `0x02` 改为此值 —— P5-fix 正在重算 golden vectors，此时改代价最低 |
| `0x11` | attestation root hash **v2**（含 `merkle_root`，P7） | 见设计稿 §9.4。P6 的 `0x03` 是 v1 的验证规则，语义不动 |
| `0x12`+ | 未分配 | 新增 hash 构造先 `bus send team-lead` 要号 |

### 规则

1. **新增任何 hash 构造前，先在本表登记 separator 值。**
2. `0x00` / `0x01` **永久保留给 RFC 6962**。P7 的 Merkle 叶子/内部节点必须用这两个值
   （规范要求），所以其他构造永不使用它们。
3. 退役的 separator 值（auth V1 的 `0x01`）**不得复用** —— 与「字段语义永不复用」
   （`deployment.md`）同理。
4. 改 separator = 破坏性编码变更，必须重算全部 golden vectors。

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
6. **`bus done` 之前必须已 commit，汇报里必须带 commit hash。** 工作区里的改动不算交付 ——
   任何 `wt remove` / `git checkout` 都会销毁它。若刚做过 `git merge main`，先确认
   `git status --short` 无 `UU`（冲突文本解完还要 `git add` 才算解决）。
7. **合并 main 之后必须重跑 `make test`。** 合并前的绿是过期证据 ——
   2026-08-21 P3 合入 main 带来了 `journal_entries` 的 per-journal 平衡 trigger（044），
   任何在此之前跑绿的分支都可能撞上它（P3 自己就因此暴露出 `seedJournal` 的非原子性）。
8. `bus done <id> <agent>`；有新 failure mode 就 `bus learn <agent> "<经验>"`。

> **Lead 侧对应义务**（2026-08-21 补，源：P4 实例）：收到 done 后**先**
> `git log --oneline <base>..HEAD` 验证有 commit，**再**读文件内容评审。
>
> ⚠️ **但不要用 `git status --short` 做这个判断**（2026-08-21 我自己踩的坑）：
> `--short` **隐藏** "interactive rebase in progress" 那行 banner。`rebase` replay 期间
> 分支 ref 指向 onto 目标，`git log main..HEAD` 为空、冲突文件显示 `UU` ——
> 看起来和「工作从未提交」完全一样，实际上原 commit 一直安全躺在 reflog 里。
> 正确姿势：`git status` **长格式**，或查 `.git/rebase-merge` / `.git/rebase-apply` 是否存在，
> 或直接 `git reflog <branch>`。
>
> 更普适的一条：**快照式观察一个正在动的工作树，歧义信号要按「可能正在变化」解释，
> 不要按最坏情况下断言** —— 尤其不要据此向上汇报。

## 10. 禁止事项（跨任务共同）

- 不改本文（Team Lead 独占写权）。
- 不动别人分配到的 migration 号 / `.sql` 文件。
- 不在领域层（`core/`、`<domain>/`）import `net/http` / `pgx` / `slog` / `chi`（`golang.md`）。
- 金额一律 `decimal.Decimal` + `NUMERIC(30,18)` + JSON 字符串（`financial.md`）。
- **DB 事务内禁止外部 API 调用**（`financial.md`）—— P5 的 KMS 调用必须在事务外，
  这是设计稿 §7.2 把 digest 放在 uid-space 的唯一理由，不要"优化"回 id-space。
- 不用 `_ = someVar` 压编译错误；不丢弃 error（`golang.md`）。
- 不写「未来可能要支持 X」进当前结构（YAGNI），但保留接口位。
- **不要把可变数量硬编码进人读措辞**（2026-08-21 P2 实例）：suite 从 10 个 check 扩到 12 个后，
  「10-check」在 10 处成了过期事实，含 `core/reconcile_extra.go` 里 `FullReconciler`
  **port 自己的 doc comment** 与 `ledger.go` 的库门面注释。措辞里去掉数字
  （「the full reconciliation suite」），数量只保留在**机器可校验**的那一处
  （测试断言）—— 那一处不改就会失败。这与 §2 把列清单从函数体里拿掉是同一个道理：
  **能结构强制的不靠人全局搜索替换**（`working-agreements` §5）。
  ⚠️ 历史条目（`CHANGELOG.md`）不要改 —— 它记录的是当时的事实。


---

# Wave 2 契约层 — 提现时验证（2026-08-23）

> 起因：Aaron 指出「提现时应该有一层 verify」，并给出载重理由 ——
> **DB 不可信、app 可信、私钥只在 app 够得到的地方 ⟹ app 是唯一能做这个判断的位置，
> 而它手上的材料（签名）已经齐了。** Wave 1 造了材料，没让 app 在要紧时刻去看它。
>
> **本节先于任何实现写完**（Phase 0）。Wave 1 我四次因为没提前分配全局命名空间而返工，
> 这次按下面的清单**逐类枚举**，不等撞上。

## W2-0 全局命名空间盘点（新增任务前先过一遍这张表）

| 命名空间 | 本波分配 |
|---|---|
| migration 号 | `053` = T2（若需 schema 支撑）；`054` = 预留给 T4 |
| invariant ID | `I-31` = T1；`I-32` = T2。（`INVARIANTS.md` 现到 I-30） |
| hash domain separator | 本波预期不新增；`0x12` **预留给 T4**（attested 内容含授权结论 ⟹ root hash v3） |
| reconcile check 名 | `unauthorized_journals` = T2 |
| `.sql` 查询文件 | T2 独占 `queries/integrity_verified_balance.sql`；T1 无新查询文件 |
| `core/interfaces.go` | T1 追加 `AuthorizeReversal`；T2 追加 `VerifiedBalanceReader`。**只追加不重排** |
| `core.AuthStatus` 枚举值 | **不得自行新增** —— 要加先 `bus send team-lead` |
| worker 配置字段 | 本波不新增 job |

## W2-1 语义裁决：验证余额是账户级 fail-closed

朴素定义「排除未授权 journal 后求和」**是错的且危险**：

`ReverseJournal` / `ReverseJournalFraction` / `ExecuteTemplateBatch` / tx-mode `PostJournal`
目前都产出 `unsigned_tx_mode`。冲正 journal **减少**余额 —— 把它排除掉，验证余额会
**高于**真实余额。攻击者什么都不用做，系统自己多报可用资金。**排除必须永不抬高余额，
而按 journal 排除做不到这一点**（一笔 journal 对某账户的净额可正可负）。

**裁决**：任一贡献 journal 未授权 ⟹ 该账户的验证余额 **UNDEFINED**，调用方**拒绝**放款。
不是「算一个小一点的数」。这与 `working-agreements` §3 同源：**验不了 ≠ 通过**。

推论：验证余额可用的前提是 money path 上**所有** journal 都能被签名 —— 因此 T1 存在。

## W2-2 三个并行任务与接缝

```
T1 扩大签名覆盖（冲正 + 模板批量）   ── 独立，可立即开工
T2 VerifiedBalanceReader + Reserve 接线 ── 定义接缝；实现可先用朴素路径
T3 验签成本实测                      ── 只读，只写报告，完全独立
                                        ↓ 数字决定是否需要
T4 attestation 分摊（把授权结论绑进被签名内容）── 本波不启动，触发条件见下
```

**接缝（`abstractions.md`：定义边界，实现延后）**：T2 定义
`VerifiedBalanceReader` port 的**语义与返回形状**（含 UNDEFINED 这一态怎么表达）；
T4 只是**换掉它背后怎么算出答案**。两者不冲突，T4 落地时 T2 的调用方零改动。

**T4 的启动条件**：T3 测出朴素路径在真实 entry 规模下不可接受。**不提前建**
（`discipline.md` §2）—— 但 T2 必须把 port 定成 T4 能无痛替换的形状。

## W2-3 策略归属（不重复 Wave 1 的错）

**机制在库，策略在消费方。** 库提供 `VerifiedBalanceReader` 与 UNDEFINED 语义；
**「用不用验证余额」「什么金额以上用」由消费方决定** —— 库不设默认阈值、不替消费方选。
唯一的库侧硬约束：**UNDEFINED 不得被静默当成 0 或当成通过**。

## W2-4 Wave 1 的教训在本波仍适用（不重复列）

§9 的 Done 标准、§10 的禁止事项、以及这几条踩过的坑全部继承：
044 的平衡 trigger（直写 `journal_entries` 的测试必须单事务）、
045 的 `event_id` 是 nullable FK（不要写 `0`）、
`git status --short` 会隐藏 rebase 中断、
done 汇报必须带 commit hash、合并 main 后必须重跑、
`INVARIANTS.md` 的顺序与 pin 引用有机器门禁（含族引用 `Foo_*` 的已知局限）。


---

# Wave 3 契约层（2026-08-23）

> 起因：Aaron 拍板「T4 与 7 月 CARRY 遗留都要做」。
> **Lead 先更正一处自己的误报**：CARRY #3（openapi 校验进 CI）此前被我报成
> 「已被验证缺失两次」—— **错的**。`.github/workflows/ledger-react.yml` 在
> `docs/openapi.yaml` 变动时（push 与 pull_request 都触发）跑 `codegen:check`
> （`openapi-typescript` + `git diff --exit-code`），YAML 语法错与 schema.ts 漂移**都已覆盖**。
> 我把「我本地没自动跑」误当成了「CI 没有 gate」。真实缺口只是**本地缺快速检查**。

## W3-0 全局命名空间分配（开工前枚举，不等撞上）

| 命名空间 | 分配 |
|---|---|
| migration 号 | `054` = T4（早已预留）；`055` = W3-A（若需记录 ingester 身份） |
| invariant ID | `I-33` = T4；`I-34` = W3-A（职责分离）。`INVARIANTS.md` 现到 I-32 |
| hash domain separator | `0x12` = T4 的 root hash v3（含授权结论），早已预留 |
| reconcile check 名 | 本波预期不新增；要加先 `bus send team-lead` |
| `.sql` 查询文件 | T4 独占 `queries/integrity_attested_auth.sql`；W3-A 无新查询文件 |
| **`server.Scope` 枚举值** | ⚠️ **W3-A 独占** —— 这是对外契约（API key scope），**不得由其他任务新增** |
| `core.AuthStatus` 枚举值 | 仍然：**不得自行新增** |
| `core/interfaces.go` | 只追加不重排（T4 与 W3-A 可能同时碰） |

## W3-A 职责分离（CARRY #2 的 mi2 + mi5）

### mi2 —— 已确认的真洞

`server/routes.go` 的 `ScopeWrite` 组同时包含
`POST /bookings`、`POST /bookings/{uid}/transition`
与 `POST /deposits/{uid}/review/approve`。**一把 ScopeWrite key 能自造大额充值并自批**，
使 crypto-deposit 设计 §9.2 的 `AutoCreditCeiling` 上限闸失效
（该闸的全部意义是防单一来源无上限铸币）。

**库的义务是「让两者可分离」**，不是替消费方决定谁批准谁。当前三个 scope
（read/write/admin）**结构上做不到分离** —— 这是库的缺陷。

### mi5 —— 静默卡死

第二源对账长期不可用时，合法充值**永久卡在 `confirming`**，无信号。
这是 `working-agreements` §3 的形态：什么都没发生与做完了不可区分。
连续失败 N 次后必须**进 review 或告警**，N 由消费方注入（**库不设默认值**，
沿用 M3.1 secure-by-default 先例）。

## W3-B T4 —— attestation 分摊验签

实测依据（`.local/bench-verify-2026-08-23.md`）：朴素路径 crossover 在
**~10–12 条贡献 journal**，成本随账户生命周期线性无界；每 journal ~216–240µs 中
**只有 ~36µs 是密码学，~84% 是 DB 往返**。所以要打的是**往返次数**，不是签名速度。

批量取数（一次取回全部 journal 的 auth 材料 + entries）能拿 **~5–6x 常数级**改善
（报告 §4.5，**标注为估算不是实测**），**但改不了 O(N) 的形状**。

**T4 = 把「每笔 journal 的授权验证结论」绑进被签名、被外部锚保护的 attestation 内容**，
worker 每批验一次；提现时只需验 attestation 链（O(批数)）+ 未 attest 的尾巴（上限 =
attestation 间隔）。这是 CT 的做法，也是 P5 与 P6 两件已有成果的自然组合。

**接缝已就位**：`core.VerifiedBalanceReader`（W2-T2）的 port 形状不变，
T4 只换 `postgres.VerifiedBalanceStore` 内部实现，**调用方零改动**。

## W3-C CARRY 收口（#1 / #3 / #4）

**以实跑为准，不以阅读为准**：
- `#1`：`web/` 的 `npm ci && build && typecheck && test` 实跑；house-rules 门禁按
  `nextjs.md` 逐条核（金额 `financial-display`、数据展示四态、a11y、list keys、用户表面不泄露实现）
- `#3`：CI 已覆盖（见上）。**只需补一个本地快速检查**（`make` 目标），让人在 push 前就发现
- `#4`：`docs/frontend.md` 已有 24 处相关命中，**先核实缺什么再补**，不要重写已有内容

三条都**先给证据再动手**；确认已完成的直接 `bus done` 并附证据，不要为了「有产出」而改。


---

# Wave 4 契约层 — migration 压平（2026-08-25）

> Aaron 拍板：这个库没有历史用户，**按全新库对待** —— 把 53 个增量 migration 压成
> 一个 baseline，让第一次使用的人没有「迁移」这个环节。并且 **baseline 直接落在
> post-049 状态**（`ledger_owner` 拥有一切 / `PUBLIC` 已 revoke / `ledger_app` 最小权限）。

## W4-0 为什么这次压平能消掉最大一块复杂度

042/049 那套 **expand → migrate → contract** 编排，存在的唯一理由是「有一个在座的连接角色
不能被 strand」。**全新安装没有在座角色** —— 库还没跑起来，没有连接要保护。所以角色、授权、
ownership 转移可以一步到位。

这一波的**第一个 Critical 就是那套编排造成的**（042 原稿把 REVOKE + ownership 混进
expand，测试全绿因为测试身份是 superuser）。压平之后它整个消失，连同 049 的
`schema_migrations` 窄 re-grant、down-chain 组合风险、以及 7 处依赖中间版本的测试。

**代价**：失去「证明某个 migration 能安全应用到既有库」的能力。无历史用户时这个能力没有价值
（`ledger-no-compat-constraint`）。

## W4-1 命名空间重置

| 命名空间 | 重置后 |
|---|---|
| migration 号 | `001_baseline`；后续新 migration 从 `002` 起 |
| invariant ID | **不动** —— I-1..I-34 是行为契约，与 migration 编号无关 |
| hash separator | **不动** |
| 其余（`.sql` 文件 / check 名 / Scope / AuthStatus） | **不动** |

## W4-2 baseline 的构成（实测确定，非推断）

对一个跑完全部 53 个 migration 的库实测：

- **37 张表 / 64 个索引 / 5 个 function / 15 个 trigger**
- **唯一有 seed 数据的表是 `classifications`（2 行）** —— 其余全空
- 011 是真 seed；017/018/025/031/035/037/044/045/051 里的 INSERT/UPDATE 都是对**既有数据**的
  回填或清洗，全新库无数据可回填，**整个丢掉**

⚠️ seed 行必须取**最终状态**，不是 011 的原文 —— 032 加了 `balance_role`、038 加了
`display_label`，那两行被后续 migration 改过。

## W4-3 手写 baseline，不用 pg_dump 输出

`pg_dump --schema-only` 是 3509 行机器生成物，**丢掉了 migration 里全部解释性注释**
（044 为什么用 pg_temp dedup 而不是 004 的 per-row、045 每条 guard 防的是哪条攻击路径、
049 为什么必须留两个窄 re-grant、033 为什么改成通用 `to_jsonb` 比较）。读 baseline 的人
将不知道任何一条保护为什么存在 —— 而这个库的价值有一半在那些理由里。

**做法**：手写 `001_baseline.up.sql`，把既有 migration 的 prose 搬过来重组；
用 pg_dump 输出**作对照物**，不作产物。

**硬门禁（不可协商）**：

```
库 A：新库 → 跑全部 53 个旧 migration
库 B：新库 → 只跑 001_baseline
pg_dump --schema-only 两边 → diff 必须为空
classifications 两行数据 → 必须逐字段相同
```

diff 非空就是没做完，不允许「差异可忽略」。

## W4-4 测试怎么处理（区分「断言」与「过渡」）

12 个 `TestMigrationNNN_*` 里，要**分开对待**：

- **断言 schema 事实的**（如「`period_closes` 表存在」「`journal_entries` 有主键」）——
  事实仍然为真，**改写成对 baseline 的断言，不要删**。删掉等于丢掉 pin。
- **本质是过渡的**（`m.Migrate(41)` 然后应用 042、非-superuser 在座角色 stranding、
  down-chain 组合）—— 主题随压平消失，**删除**，并在删除的 commit 信息里说明它证明过什么、
  为什么不再需要。
- `migration_files_test.go`（号唯一 + up/down 配对）**保留**，仍然有效。
- `migration_roundtrip_test.go`（down 链可执行 + 可重新应用）**保留** —— 单 migration 下它
  变简单但仍然有意义（baseline 的 down 必须真能拆干净）。

## W4-5 文档要跟着改

压平会让若干处文档变成描述一个不存在的机制（本波已提炼的 `phantom-mechanism-comment`）：

- `docs/RUNBOOK.md` §9 的 042/049 cutover 步骤 —— 全新安装不再有这个过程
- 设计稿 §3 的 expand→migrate→contract 三阶段
- `INVARIANTS.md` I-22 里引用 042/049 的表述与 `Pinned by`（**注意 pin 引用存在性有机器门禁**）
- `CLAUDE.md` 的 File Layout 里 migration 相关描述
- 消费方契约（设计稿 §14）里「049 与 DATABASE_URL 切换同发布」那条 —— 不再适用

**不要留一句描述已废机制的话。**

## W4-6 安装前提（baseline 落在 post-049 的代价）

跑 baseline 的连接必须能 `CREATE ROLE`（superuser 或有 CREATEROLE）。这是一次性的、
明确的前提，写进 README/RUNBOOK 的安装说明。

理由（Aaron 拍板）：库的默认应该是安全的那一侧。「默认不安全、指望消费方记得再跑一步」
更糟 —— 这一波已经证明「要人记住的规则会被违反」。


## W4-7 压平时发现的既有缺陷：ownership 归集漏了 function（Major）

准备参考 dump 时实测发现，**不是压平引入的，是压平顺手暴露的**。

migration 049 的 ownership 归集循环了 `pg_tables` 和 `pg_sequences`（049 up 的 99/111 行），
**没有 `pg_proc`**。全链跑完后的实测结果：

| 对象 | 应属 | 实属 |
|---|---|---|
| `ledger_block_mutation()` | `ledger_owner` | 迁移执行者 |
| `ledger_journals_block_arbitrary_update()` | `ledger_owner` | 迁移执行者 |
| `ledger_classifications_guard()` | `ledger_owner` | 迁移执行者 |
| `ledger_reservations_guard()` | `ledger_owner` | 迁移执行者 |
| `check_journal_currency_balance()` | `ledger_owner` | 迁移执行者 |
| `checkpoint_rebuilds` + 序列 + 4 索引 | `ledger_owner` | 迁移执行者 |

**为什么要紧**：函数所有者可以 `CREATE OR REPLACE` 它。把 `ledger_block_mutation` 的函数体
换成 `BEGIN RETURN NEW; END`，**journals / journal_entries 的 append-only 防护就静默失效了，
而且不产生任何形如 `DROP TRIGGER` 的 DDL** —— 审计日志里看不出防护被拆过。

**严重度 Major 不是 Critical**：`ledger_app`（应用连接的身份）从来不是这些函数的所有者，
所以既有威胁模型「DB 写权限泄露、程序端可信」没有被突破。被削弱的是 049 的**意图** ——
把「能改动保护性对象的主体」收敛成一个已知角色。留在迁移执行者名下，等于把这个集合扩大到
「谁跑的迁移」，而那在多数部署里是一份更广泛共享的管理凭证。

**两个成因是同一个 pattern 的两次发作**（`global-namespace-allocation` 的近亲）：

1. 049 归集时**枚举了对象类型**（表、序列），漏掉的类型静默不受保护 —— 没有任何机制会说
   「你漏了 pg_proc」
2. 050 在 049 **之后**建了 `checkpoint_rebuilds`，自动归执行者所有。052（名字就叫
   `grant_coverage_gap`）**回头补了它的 grant，没补它的 ownership** —— 发现了同一个
   migration 引入的第一个缺口，没发现第二个

**压平如何按构造修掉它**：baseline 建完全部对象后做一次覆盖全类型的归集，不存在「某个类型
被漏掉」或「某个对象建得太晚」的可能。**并且 gate 把它变成机器检查**：任何不归
`ledger_owner` 的对象都会让门禁红（gate.sh 第 3 项）—— 这正是 `working-agreements` §5
「能被结构强制的，不靠记忆和自觉」。

## W4-8 门禁自身的两个现场教训

写 gate 的过程本身复现了本波两条已提炼的 pattern，记下来因为它们**发作于门禁建设期**，
而那正是最危险的时点（`gate-before-running-seals-red`）：

1. **`docker exec` 未加 `-i`，heredoc 从未送达** —— 修正 ownership 的 `DO $$` 块根本没执行，
   而命令**退出码为 0**。若不是随后对照了 dump 里 `OWNER TO` 的计数，会拿一份未修正的
   参考物当基准，让门禁在错误的对照下永远绿。**「未运行 ≠ 通过」在这里的形态是
   「未运行且退出码是 0」。**
2. **pg_dump 18 的 `\restrict` / `\unrestrict` token 每次 dump 随机生成** —— 不归一化的话
   门禁永远红，且红的理由与 schema 无关。**一个永远红的门禁会被关掉**，所以归一化不是
   便利性处理，是门禁能存活的前提。

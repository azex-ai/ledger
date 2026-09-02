# 审计报告 — tamper-evident（territory C）

> 基线 commit `fd87cae`。只读审计，未修改任何被审计文件。
> 行号以 `fd87cae` 为准。跑过：`go test ./core/... -run 'TestCanonical|TestEncodeAmount|TestAttestationRootHash|TestMerkle'`（绿），
> 另用一个临时 `postgres:17` 容器（已删除）验证了两条 SQL 语义断言（见 M-7）。

## 摘要

密码学原语这一层是扎实的：digest 的字节布局有长度前缀与域分隔、金额定点编码、
微秒截断（I-46）与 `journals.idempotency_key` 的全局 UNIQUE 一起排除了跨 journal 的
digest 复用；上一轮的 C3（缓存判定跳过 live check）、pending_store 未签名、
`AttestInterval` 未合默认值、`ErrUnauthorizedJournal` 无 bizcode、提现闸无 HTTP 面，
逐条核过，都真的接到了真实路径上，且钉子拆掉会红。

问题全部在**「验证结论怎么被使用」这一层**，而且是同一个形状的三次重复：
**一个被正确实现的检查，它算出来的那个数/那个信号，最后没有被用在决定钱去哪的地方。**

- 最重的一条（Critical）：`RequireVerifiedBalance` 闸只验签名，随后 `Reserve` 实际可预留的
  金额仍然取自 `balance_checkpoints`（checkpoint + delta）—— 而设计 §0 明确把 checkpoint 列为
  **不可信缓存**、要求「提现路径全量重算，不读 checkpoint」。闸内已经算出了可信数字
  （`RecomputeBalance` 的返回值），代码把它丢掉了。持有 `ledger_app` 凭证（本设计的标准威胁）
  的攻击者一条 `UPDATE balance_checkpoints` 就能在**全部签名有效**的前提下把钱提走。
- 其次（Major）：`VerifyLedger` 第 4 步声称采样「最新」的 N 条 journal，实际 SQL 是
  `id > 0 ORDER BY id ASC LIMIT N` —— 采的是**最旧**的 N 条，新伪造的 journal 永远进不了样本；
  而且 §8.4 明写的「LEFT JOIN 找未覆盖 entry」这一步根本没实现。
- 第三（Major）：锚「变空 / 回退」被判成 `DRIFT`（文档定义为「良性」），`ledger-cli verify`
  对 DRIFT 退出码 0。删掉 anchor 文件（`anchordev`）或往 R2 的那个 key 写一版新对象，
  就能把全部外部校验静默关掉且不报警。

另有一条实测确认的可用性/取证破坏面：`journals` 表可插入 `total_debit = total_credit = 'NaN'`
（通过全部 CHECK），触发 `mustNumericToDecimal` 的 panic，一条 INSERT 就能让验证读侧与
worker 进程崩溃（M-7）。

---

## 发现

### [Critical] C-1：提现闸只验签名，实际放款金额仍取自攻击者可写的 `balance_checkpoints`

- **位置**：
  - `postgres/reserver_store.go:123-133`（闸的调用点）
  - `postgres/reserver_store.go:201-209`（把可信余额 `_` 丢掉的那一行）
  - `postgres/verified_balance_store.go:164-167`（`VerifiedBalance` 返回的正是 entries-only 的重算值）
  - `postgres/reserver_store.go:274-295`（真正决定能不能预留的那个数）
  - `postgres/sql/queries/checkpoints.sql:44-71`（`ListComputedBalancesForHolders` = `cp.balance + Σ(delta)`）
  - `postgres/sql/migrations/001_baseline.up.sql:1498-1502`（无 `ledger_block_mutation` UPDATE trigger 的表 → `ledger_app` 拿到 `UPDATE`；`balance_checkpoints` 正是这类表）
- **判定**：CONFIRMED（完整调用链读通；未实跑攻击，因为审计只读且不许改文件写测试）
- **失效场景**：
  1. 用户 U 在货币 C 的 `main_wallet`（`balance_role='available'`）上有 **100**，全部由**合法签名**的 journal 构成。
  2. 攻击者持有 `ledger_app` 凭证（设计 §1 第一行威胁），执行
     `UPDATE balance_checkpoints SET balance = balance + 1000000 WHERE account_holder=U AND currency_id=C AND classification_id=<main_wallet>;`
     —— 该表没有 append-only trigger（必须可 UPDATE，rollup 要写它），因此 §14 的 GRANT 循环
     把 `SELECT, INSERT, UPDATE` 授给了 `ledger_app`。这条 UPDATE 会成功。
  3. 消费方按 `examples/tamper-evident` 教的方式做「安全的提现」：
     `Reserve(..., Amount=100000, RequireVerifiedBalance: true)`。
  4. 闸跑 `requireVerifiedAvailableBalance` → 对每个 available 分类调 `VerifiedBalance`。
     每笔 contributing journal 的签名都**真的有效**（攻击者没碰 journal），
     `VerifyJournalAuth` 全过 → 闸放行。`VerifiedBalance` 内部 `RecomputeBalance` 算出的
     真值 **100** 在 `reserver_store.go:206` 被 `_` 丢弃。
  5. `reserveWithQueries` 用 `sumBalancesByRoleWithQueries` 求可用额 → 走 checkpoint + delta →
     **1000100** → `available.LessThan(100000)` 为假 → 预留成功 → Settle → 100000 出金。
- **最小复现**：上述 5 步；断言 `Reserve(RequireVerifiedBalance:true)` 在 checkpoint 被 +1000000
  之后仍然成功。全仓没有任何测试做这件事 —— `postgres/verified_balance_pin_test.go:292/336`
  的两条 Reserve 钉子分别只覆盖「伪造 journal 被拒」与「全签名放行」，**没有一条动 checkpoint**。
- **为什么现有机制没拦住**：
  - 设计 §0 决策表「余额真相」一行写的是「checkpoint 是**不可信缓存**。提现路径全量重算，
    不读 checkpoint」；§4（P2）重复了一遍「提现 / 大额路径强制走 `RecomputeBalance`」。
    `ledger.go:543-548` 的 `CheckpointIntegrity()` 文档也照抄了这条要求。
    **但库自己唯一的提现原语 `Reserve` 从不调用它**，即使 `RequireVerifiedBalance=true`。
  - I-32 的 `Pinned by` 里那句「it is an authorization check, not a stricter amount check」
    把这件事写成了**有意的语义**，于是没有任何钉子会因为这个缺口变红 —— 规则本身把缺口合法化了。
  - `core/reserve.go:64-65` 的「on top of, not instead of, the normal
    available-balance-covers-Amount check」在字面上确实披露了它，但 `examples/tamper-evident/main.go:194-235`
    把这个闸演示成「the withdrawal is refused before any money moves」，并且只演示了
    **伪造 journal**，从不演示 **篡改 checkpoint** —— 唯一的教学材料把它教成了完整的闸。
  - 异步侧确实会发现（reconcile check #2 `checkpoint_balance` 比对 checkpoint 与 entries），
    但那是周期性 job，钱已经走了。
- **修复方向（供 lead 拍板，不代表已定）**：`requireVerifiedAvailableBalance` 返回它算出的
  per-classification 可信余额，`reserveWithQueries` 在 `RequireVerifiedBalance=true` 时用这份
  重算值（而不是 role sums）当 `availableBase`。注意这会改变 I-32 的措辞
  （它现在明说「不是更严的金额检查」），属于**规则本身要改**的那一类，不是文档对齐代码。

---

### [Major] M-1：`VerifyLedger` 第 4 步采样的是**最旧**的 N 条 journal，不是文档四处声称的「最新」

- **位置**：
  - `service/attest_verify.go:399-400`（`// Step 4: sample the most recent journals` + `journals.ListJournals(ctx, "", cfg.JournalSampleSize)`）
  - `postgres/query_provider.go:88-108`（`ListJournals` → `ListJournalsCursor`）
  - `postgres/sql/queries/journals.sql:96-100`（`WHERE id > cursor ORDER BY id ASC LIMIT n`）
  - `postgres/convert.go:535-542`（`decodeCursorString("") == 0`）
  - 同一句错误声明还出现在 `service/attest_verify.go:73-75`、`:455-457`、`cmd/ledger-cli/main.go:318`
- **判定**：CONFIRMED（四段代码连成一条链，无分支）
- **失效场景**：cursor 传 `""` → `cursorID = 0` → `id > 0 ORDER BY id ASC LIMIT 20`。
  这在任何一个已有 >20 条 journal 的账本上都是**恒定的同一批最早 20 条**。
  于是：① 今天新伪造的 journal（`auth_status` 取列默认 `unsigned_no_attestor`）
  **永远不会**被第 4 步看到，`:454` 那条「carry no signature ... either forged」的告警永不触发；
  ② 每次 verify 重复验同样 20 条已知良好的行，`JournalsSampled` 是个恒定值。
  第 4 步存在的理由（把直接 SQL 插进来的伪造行抓出来）在超过 20 条 journal 之后即失效。
- **最小复现**：签名开启的账本上先正常 post 25 条 journal，再用直接 SQL 插一条无签名的伪造 journal，
  跑 `VerifyLedger` → 报 `VERIFIED`（若该 seq 尚未被 attest 覆盖，见 M-2）。
- **为什么现有机制没拦住**：`service/attest_verify_test.go` 每个用例都是
  `postgrestest.SetupDB(t)` 起的**全新库**，全库 journal 数远小于 20 —— 「最旧 20 条」
  与「全部」是同一个集合，顺序错误在结构上不可见（审计契约 §1.4 第 2 问：
  「测试做的准备工作，真实调用方也会做吗」—— 这里是反过来，测试构造的规模让被测命题退化成恒真）。
  `examples/tamper-evident` 同理（journal 数个位数），所以它演示的「verify 抓到伪造」也依赖这个巧合。
- **注意**：`service/reconcile.go:1394-1398` 的同名调用**注释是对的**
  （"oldest first ... the SAME oldest slice every run"），并且在打到 page limit 时诚实地
  `Complete=false`。同一个函数、两个相反的注释 —— `VerifyLedger` 这一侧既写错了方向，
  也没有任何 partial-coverage 标记。

---

### [Major] M-2：`VerifyLedger` 没有实现设计 §8.4 第 3 步的「未覆盖 entry」检查

- **位置**：`service/attest_verify.go:130-397`（step 2+3 整段，只走 `ListAttestationsFrom` →
  `EntriesForAttestation`，全程不查 `UncoveredEntries`）；对照
  `service/attestation.go:37-41`（`AttestationStore.UncoveredEntries` 只有 `RunAttestBatch` 一个调用方，
  `service/attestation.go:137`）
- **判定**：CONFIRMED（`grep -rn "UncoveredEntries"` 全仓只有 `RunAttestBatch` 与 store 实现）
- **失效场景**：设计 §8.4 step 3 原文是「从 DB entries 重算 batch_digest 比对；**LEFT JOIN 找未覆盖的 entry**」。
  第二句没写。后果：任何**尚未被 attestation 覆盖**的 entry 对 `VerifyLedger` 完全不可见 ——
  它只遍历已存在的 attestation 行并核对它们**自己声明**覆盖的那些 entry。
  一条直接 SQL 插入的伪造 journal，其 entry 落在未覆盖尾部时：
  - 链检查全过（它不在任何批次里）
  - 批次 digest / merkle root 全过
  - 第 4 步因 M-1 看不到它
  → 报 `VERIFIED`。
  攻击者只要保证 attest job 不推进（停 worker、或用 M-7 让 worker 崩溃、或利用
  `svc.Worker` 从未被消费方启动的默认状态），这个窗口就是**无限长**。
- **最小复现**：不起 Worker，`WithAttestor` 正常配置，手工 `RunAttestBatch` 一次覆盖历史，
  然后直接 SQL 插伪造 journal，不再 attest，跑 `VerifyLedger` → `VERIFIED`。
- **为什么现有机制没拦住**：I-27 的标题声称「every entry covered exactly once」，
  但它的 `Enforced by` 列出的只有 ① `RunAttestBatch`（写侧，最终一致）、
  ② `entry_attestations` 的 `PRIMARY KEY (entry_id)`（只防**重复**覆盖，不防**没有**覆盖）、
  ③ `ListUncoveredEntries` 这条 SQL 本身（存在 ≠ 被 verify 调用）。
  **没有任何检查回答「现在有多少 entry 未被覆盖」**，也没有任何指标暴露这个数。
  `docs/INVARIANTS.md:1749-1758` 把 `VerifyLedger` 列为 I-27 的执行者，但它执行的只是前半句。

---

### [Major] M-3：锚为空 / 回退被判为 `DRIFT`（良性），且 CLI 退出码为 0 —— 抹掉锚即可静默关闭外部校验

- **位置**：
  - `service/attest_verify.go:395-397`（`anchorSeq > maxSeqSeen` → tampered）与 `:465-467`（`anchorSeq < maxSeqSeen` → **DRIFT**）
  - `service/attest_verify.go:382-384`（`a.Seq == anchorSeq` 才比对 head —— `anchorSeq==0` 时这条永不成立，seq 从 1 起）
  - `service/attest_verify.go:25-29`（DRIFT 的定义：「a benign, expected inconsistency」）
  - `cmd/ledger-cli/main.go:349-351`（只有 `TAMPERED` / `NOT_RUN` 才 `os.Exit(1)`）
  - `anchordev/local_file.go:94-98`（文件不存在 → `(0, nil, nil)`，不是 error）
  - `anchors/r2/r2.go:190-200` + `:219-235`（404 → `(0, nil, nil)`）
- **判定**：CONFIRMED
- **失效场景**：`Head()` 的契约是「最高 seq，空则 0」，**没有任何机制区分「从未发布过」与
  「发布过但被抹掉/回退了」**。而 `VerifyLedger` 把「锚落后于 DB」一律解释成 catch-up 未完成：
  - `anchordev.LocalFileAnchor`：锚文件与应用同主机（该包自己承认这不是生产载体）。
    `rm anchor.txt` → `Head()` 返回 `(0, nil, nil)` → 402 行的比对不触发、396 行的比对不触发 →
    若链本身自洽则 `report.Status = DRIFT`，reasons 一句「anchor is behind the DB chain by N」→
    `ledger-cli verify` **退出码 0**。
  - `anchors/r2`：ledger 侧 token 按 RUNBOOK 只有 `GetObject + PutObject`，但 `PutObject`
    足以往同一个 key 写一版新对象（Object Lock 保留旧**版本**，不阻止新版本）。写一个
    `{"seq":0,"head":""}` 或任意更旧的 seq，`Head()` 读的是**当前版本**，同样退化成 DRIFT。
    `r2.Publish` 里 `seq < curSeq` 的拒绝是**客户端**行为，攻击者直接用 S3 API 绕过。
- **最小复现**：跑通 `examples/tamper-evident`（它用 LocalFileAnchor），在最后一次
  `VerifyLedger` 之前删掉 anchor 目录里的 `head.json`，观察状态从 `TAMPERED`/`VERIFIED` 变成 `DRIFT`。
- **为什么现有机制没拦住**：`anchortest` 的 6 个 phase 里**没有一个断言 head 单调不回退**
  （`anchortest/conformance.go:131-260`），而这恰好是 `VerifyLedger` 用来区分
  「DRIFT（良性）」与「TAMPERED」的那条性质。该包的 "What this suite deliberately does not check"
  一节（`:40-63`）明确把「非精确重放的 seq 排序」列为不检查项 —— 也就是说，
  **一个允许 seq 回退的 Anchor 实现是"合规"的**，而 `VerifyLedger` 却假设它不会回退。
- **修复方向**：`anchorSeq == 0 && maxSeqSeen > 0` 至少应当是 `NOT_RUN`（fail-closed，
  同 §8.4「NOT_RUN ≠ VERIFIED」那条红线）；`anchorSeq` 相对上次观测值下降应当是 TAMPERED；
  `anchortest` 加一条 head 非回退 phase。

---

### [Major] M-4：R2 载体的「可信 head」是一个可变 key，不可变性只存在于没有任何代码会读的历史版本里

- **位置**：`anchors/r2/r2.go:79-90`（「storing the current (seq, head) pair as a single JSON object at Config.Key」）、
  `:176-184`（无条件 `PutObject`，无 If-Match / If-None-Match）、`:190-217`（`GetObject` 不带 versionId）；
  `docs/RUNBOOK.md:978-987`（ledger 侧 token 需 `PutObject`）、`:982-989`（「versioning keeps every past version ... even though only the latest version is ever read by Head」）
- **判定**：CONFIRMED（代码层面）／对 R2 的 Object Lock 具体行为参见「我没能验证的」
- **失效场景**：设计 §8.3 对载体的第 2 条要求是「**写入后不可改**，否则攻击者改锚即可让篡改自洽」。
  在这个实现里，被 Object Lock 保护的是 key 的**每个历史版本**；但
  ① `Head()` 只读当前版本；② 全仓没有任何代码（`Head`、`VerifyLedger`、`ledger-cli`、
  `service.AttestationService`）会去枚举版本历史。因此**实际被消费的那个 head 是完全可写的**，
  写它所需的权限恰好就是 ledger 自身部署里那把 token。
  RUNBOOK 把「审计轨迹靠版本保留」写成了这条性质的兑现（`:982-989`），
  但审计轨迹**没有被任何验证路径读**，所以它兑现的是「事后取证时人可以手工翻」，
  不是「verify 会发现」。
- **为什么现有机制没拦住**：`anchortest` 的 `MismatchedReplayErrorsAndDoesNotCorrupt` phase
  只能通过 `Publish` 这个接口观察，而攻击者不走 `Publish`；该包自己在
  `anchortest/conformance.go:78-86` 诚实声明了这一点。`anchors/r2/r2_test.go:20-35` 建的
  MinIO 桶开了 Object Lock 但**不设 retention**，所以连「旧版本确实拿不回来」都没有测过。
- **修复方向（不定级，供拍板）**：要么 `Head()` 走 `ListObjectVersions` 取最高 seq 而不是最新版本，
  要么改成 one-object-per-seq（`Key/seq-N`）+ `If-None-Match:"*"` 条件写，
  让「已发布的 seq 不可被同一 token 改写」变成服务端强制而不是客户端约定。

---

### [Major] M-5：密钥轮换在库自带实现下结构上不可能，而轮换会让提现闸对所有存量持有人 fail-closed

- **位置**：
  - `authdev/ed25519.go:86-106`（`LocalVerifier.keys` 未导出；`NewLocalVerifier(pub, keyID)` 只收**一把** key；无 Add/Register 方法 —— `grep '^func ' authdev/ed25519.go` 确认导出面只有 4 个符号）
  - `authdev/ed25519.go:96-103`（自己写着 "a production verifier should" 做 NotAfter，本实现不做）
  - `core/auth.go:471-477` → 未知 key 的 `Verify` 错误被包成 `ErrUnauthorizedJournal`
  - `postgres/verified_balance_store.go:193-195` → 该 journal 所在维度整体 UNDEFINED
  - `cmd/ledger-cli/main.go:316-317, 337`（CLI 也只接受**一个** `--pubkey-hex` / `--key-id`）
  - `docs/INVARIANTS.md:3821-3831`（I-45 给操作员的指引是「register the key to restore verification coverage」）
- **判定**：CONFIRMED
- **失效场景**：
  - 未知 key 的处置本身是**正确的 fail-closed**（回答 lead 的问题：攻击者用自己的 key 签
    → `keyID` 不在 map → `ErrUnknownAuthKey` → 被包成 `ErrUnauthorizedJournal` →
    `VerifiedBalance` UNDEFINED → 闸拒绝。**没有「用自己的 key 签」的通道**）。
  - 代价落在合法轮换上：把签名 key 从 `k1` 轮到 `k2` 之后，`WithAttestor` 只能塞进一个
    `LocalVerifier`，它只认 `k2`。所有 `auth_key_id = k1` 的历史 journal 一律 `ErrUnknownAuthKey`
    → 任何有历史的 holder 的 `VerifiedBalance` 永久 UNDEFINED →
    **`RequireVerifiedBalance=true` 的提现对全体存量用户永久拒绝**；
    `unauthorized_journals` 检查同时对每条历史 journal 报 finding（`Passed=false` 永久红）。
  - I-45 的指引「register the retired key」在库自带实现里**做不到**：没有 API。
    消费方必须自己写一个多 key 的 `core.AuthVerifier`。而 `authdev` 的包注释
    （`authdev/ed25519.go:1-14`）明说它对单体部署是 "production-ready implementation,
    not a placeholder" —— 一个生产就绪的实现不支持它自己文档里描述的例行运维动作。
  - 设计 §13「本期不做」只写了「签名密钥轮换**自动化**……轮换走 runbook」，
    而 `docs/RUNBOOK.md` 里**没有**任何 P5 签名密钥轮换的条目
    （`grep -n -i rotat docs/RUNBOOK.md` 命中的四处分别是 webhook HMAC 与 holder token）。
- **为什么现有机制没拦住**：I-45 的三条钉子只验「未知 key 与伪造签名被区分开」，
  没有一条验「轮换后系统仍可用」。轮换路径全仓无测试、无 runbook、无 API。

---

### [Major] M-6：`ledger-cli verify` 只能接 `anchordev.LocalFileAnchor`，而 RUNBOOK 指定的生产载体是 R2

- **位置**：`cmd/ledger-cli/main.go:45, 315, 337`（唯一的 anchor 入口是 `--anchor-file` → `anchordev.NewLocalFileAnchor`）
  vs. `docs/RUNBOOK.md:894-905`（`anchors/r2` 是选定的生产载体）与 `:962-968`
  （「Verification-side token（whatever reads the anchor independently — **`ledger-cli verify`**, an auditor's own tooling）」）
- **判定**：CONFIRMED
- **失效场景**：按 RUNBOOK 把 R2 部署好、把只读 token 发给审计方之后，**没有任何随库交付的工具能用它**。
  `ledger-cli verify` 唯一的选择是指向一个本地文件，而 `anchordev/local_file.go:1-12` 自己声明
  「a file on the same host as the ledger's own database ... defeats the purpose it exists to serve」。
  操作员的现实选择只剩两个：自己写一个调 `svc.VerifyLedger(ctx, r2Anchor, cfg)` 的程序，
  或者把 R2 上的 head 手工抄成本地文件 —— 后者把「不信 DB 也不信应用」这条整个作废。
  另外 CLI 只收一个 `--pubkey-hex/--key-id`，与 M-5 同源。
- **为什么现有机制没拦住**：`anchors/r2` 是独立 module，`cmd/ledger-cli` 在 root module，
  接进来需要 root 依赖 AWS SDK —— 这正是 module 拆分要避免的。也就是说这不是漏写，
  是**结构上没有出口**，需要设计决定（例如把 verify 做成消费方 composition root 里的一个函数调用，
  并在 RUNBOOK 里删掉「`ledger-cli verify` 能读生产锚」的说法）。

---

### [Major] M-7：一条 `INSERT INTO journals` 写入 `NaN` 即可 panic 掉整个验证读侧与 worker 进程

- **位置**：
  - `postgres/sql/migrations/001_baseline.up.sql:316-317`（`CHECK (total_debit = total_credit)` + `CHECK (total_debit > 0)`）
  - `postgres/convert.go:36-58`（`numericToDecimal` 对 NaN 返回 error；`mustNumericToDecimal` **panic**）
  - `postgres/convert.go:195-196`（`journalFromRow` 对 `total_debit` / `total_credit` 用 `mustNumericToDecimal`）
  - 受害路径：`postgres/query_provider.go:88-108`（`ListJournals`）→
    `service/attest_verify.go:400`（VerifyLedger step 4）、
    `service/reconcile.go:1445`（`unauthorized_journals`）、
    `server/handler_journals.go`、`cmd/ledger-cli`
  - `service/worker.go`（`runLoop` / errgroup 全程**无 `recover()`**，`grep -n 'recover()' service/*.go` 无命中）
- **判定**：CONFIRMED（两条 SQL 语义在临时 `postgres:17` 容器上实测；Go 侧为读码）
  - 实测 1：`create table t (amount NUMERIC(30,18) NOT NULL CHECK (amount > 0)); insert into t values ('NaN');` → `INSERT 0 1`，`amount > 0` 为 `t`
  - 实测 2：`create table j (total_debit NUMERIC(30,18) NOT NULL, total_credit NUMERIC(30,18) NOT NULL, CHECK (total_debit = total_credit), CHECK (total_debit > 0)); insert into j values ('NaN','NaN');` → `INSERT 0 1`
  - `select 'NaN'::numeric = 'NaN'::numeric;` → `t`（Postgres 里 NaN 等于 NaN、且大于一切非 NaN 值）
- **失效场景**：攻击者持 `ledger_app`（有 `journals` 的 INSERT）插入一行
  `total_debit = total_credit = 'NaN'` 且**不插任何 entry**
  （per-journal balance 的 constraint trigger 挂在 `journal_entries` 上，零 entry 不触发）。
  此后任何 `ListJournals` / `GetJournal` 都会走到 `mustNumericToDecimal` → `panic`：
  - `VerifyLedger` step 4 → panic（`ledger-cli verify` 直接崩，不是 `TAMPERED` 也不是 `NOT_RUN`）
  - full reconcile 的 `unauthorized_journals` → panic → worker 的 errgroup goroutine 无 recover →
    **整个 worker 进程退出** → rollup / attestation / expiration 全停 →
    attestation 链不再推进（配合 M-2，之后插入的伪造行永远不会被覆盖、永远不会被发现）
  - HTTP 侧有 `chi middleware.Recoverer`（`server/server.go:519`），只崩单个请求，但接口从此不可用
- **最小复现**：签名开启的库上执行上述 INSERT，再跑 `ledger-cli verify` 或触发一次 full reconcile。
- **为什么现有机制没拦住**：`mustNumericToDecimal` 的注释写着
  "this should not happen with valid DB constraints" —— 那个前提是错的，`NUMERIC(30,18)` 的
  精度/标度 typmod 对 NaN 不生效，`> 0` 与 `=` 对 NaN 也不是拦截器。没有任何测试写过 NaN。
- **注**：`journal_entries.amount` 走同一条 CHECK（同样接受 NaN），但那里被
  per-journal balance trigger 兜住了（`NaN <> 0` 为真 → 报 unbalanced），所以 entry 侧当前不可达；
  `reservations.reserved_amount` / `bookings.amount` 是同一形状且**没有**平衡兜底 —— 见「移交」。

---

### [Major] M-8：`unauthorized_journals` 在「一条已签名 journal 都没扫到」时仍报 `Passed=true, Complete=true`

- **位置**：`service/reconcile.go:1460-1470`（`if j.AuthKeyID == "" { continue }` —— 未签名的一律跳过，`checked` 不增）
  与 `:1500-1508`（只要 `len(journalList) < pageLimit` 就 `Complete` 保持 true，`Passed` 保持 true，
  finding 文案 `"%d signed journal(s) verified out of %d scanned"`，`checked` 可以是 0）
- **判定**：CONFIRMED
- **失效场景**：一个 `WithAttestor` 已配置、但**全部写路径都走 `RunInTx` 而没有采用
  `Authorize`/`PostAuthorized`** 的消费方（`ledger.go:463-476` 明确说这是允许的），
  其 journal 全部是 `auth_status=unsigned_tx_mode`、`auth_key_id=''`。
  于是 `checked == 0`，检查报 `Passed=true, Complete=true`，
  `metrics.ReconcileCheckResult("unauthorized_journals", true)`（`service/reconcile.go:539-541`）
  发**绿色**指标，日志打 `"reconcile: full suite passed"`。
  即「整个账本 100% 无法验证」与「整个账本已验证通过」在机器可读信号上完全一致 ——
  正是 `working-agreements.md` §3 与本仓刚刚为此删掉 check #8 的那个形状。
  finding 的文本里确实带了 `0`，但没有人对文本告警。
- **为什么现有机制没拦住**：I-32 的 `Pinned by` 有
  `TestFullReconciliation_UnauthorizedJournals_SkipsNeverSignedJournal`，
  它钉的是「跳过未签名的不算 tamper」（正确），没有一条钉「全跳过时不许报绿」。

---

### [Major] M-9：锚发布持续失败对提现闸没有任何影响，且全无指标

- **位置**：`service/attestation.go:227-235`（`Publish` 失败仅 `logger.Error`）、
  `:313-343`（`catchUpAnchor` 失败仅 log 并 return）；
  `core/metrics.go` / `observability/prometheus.go` 中 `grep 'Attest\|Anchor'` **零命中**
- **判定**：CONFIRMED
- **失效场景**：设计 §8.3 结尾原文：「失败处理：本地重试队列 + 告警，不阻塞 journal 写入。
  **连续失败超阈值只影响 verify 判定与提现门**。」
  实际交付：verify 侧部分实现（锚落后 → DRIFT，见 M-3 该判定本身也偏软）；
  **提现门完全不看锚** —— `requireVerifiedAvailableBalance` 只验 per-journal 签名，
  与 attestation / anchor 状态无关。同时没有任何 metric 暴露
  「锚落后多少个 seq」「上次成功 publish 是什么时候」「attestation 链多久没推进」，
  唯一信号是一行 ERROR 日志（默认 logger 是 `core.NopLogger()`，`ledger.go:145`——
  消费方没调 `WithLogger` 时这行日志**去向 /dev/null**）。
- **为什么现有机制没拦住**：I-28 只断言「最新锚 head 与 DB 链一致」，
  没有任何不变量或钉子涉及「锚停更多久算异常」。

---

### [Minor] m-1：`anchordev.LocalFileAnchor` 只有散文门禁，没有机器门禁

- **位置**：`anchordev/local_file.go:1-16`（"DEV / TEST ONLY" 全在包注释里）；
  构造函数 `NewLocalFileAnchor` 是普通导出符号，包不在 `internal/` 下，无任何 env / flag 闸
- **判定**：CONFIRMED
- **说明**：同一个仓库对另一个「凭空造余额」的 dev-only 特性 `dev_credit` 用的是双闸
  （`Config.DevCreditEnabled` + ENV=dev + 启动 Warn，`server/handler_devcredit.go`）。
  两个 dev-only 特性、两套完全不同的强制力度，这个不一致本身是风险
  （`working-agreements.md` §5：能被结构强制的不靠自觉）。
  当前唯一的运行期信号是 `service/worker.go:262-266` 那条「no anchor configured」的 Warn ——
  而它只在 anchor 为 **nil** 时触发；配了 `LocalFileAnchor` 的生产部署看起来和配了 R2 的一模一样，
  `Run` 的 `attestation_anchor: true` 也是 true。

### [Minor] m-2：`anchortest` 不断言 head 单调不回退

- **位置**：`anchortest/conformance.go:131-260`（6 个 phase：空读 / 写读 / 更高 seq / 幂等重放 / 冲突重放 / 独立客户端）
- **判定**：CONFIRMED
- **说明**：`VerifyLedger` 的 `DRIFT` vs `TAMPERED` 判定（`service/attest_verify.go:395-397, 465-467`）
  完全建立在「anchor 的 seq 不会倒退」这条性质上，而该性质既不在 `core.Anchor` 的 doc 契约里，
  也不在一致性套件里 —— `anchortest/conformance.go:45-56` 反而明确说
  「一个接受乱序 seq 的实现并不因此不合规」。库对自己依赖的性质取了「不检查」的立场。
  同样没有覆盖的：删除后 `Head` 的行为（`anchordev` 与 `r2` 都选择返回 `(0,nil,nil)`，
  即「被删」与「从未写过」不可区分，这正是 M-3 的根）。

### [Minor] m-3：I-26 的 scope note 与 I-31 及现码矛盾

- **位置**：`docs/INVARIANTS.md:1528-1531`（「`PostJournal`'s tx-mode branch, `ExecuteTemplateBatch`,
  and `ReverseJournal`/`ReverseJournalFraction` **still never sign**」）
  vs. `docs/INVARIANTS.md:2029`（I-31 标题：「Reversals and template batches **sign** under a
  configured Attestor in pool mode」）
  vs. `postgres/ledger_store.go:548-566`（pool 模式逐条 `attestJournal`）、
  `postgres/ledger_store.go:709-716`（`ReverseJournal` pool 模式 `AuthorizeReversal`）
- **判定**：CONFIRMED
- **说明**：设计文档 §2「文档缺陷」一节自己写过「I-1 高估了自己……这条过期声明正是 M1 那句
  错误注释的同源问题 —— 它会误导下一次同类分析」。同一形状在 I-26 上复发了：
  一个读 I-26 的人会得出「模板批量从不签名」的错误结论。
  `core/auth.go:378-384` 的 `AuthStatusUnsignedTxMode` 注释是对的（写明了 board #15 已收口），
  三处口径两对一错。

### [Minor] m-4：未配 `WithAttestor` 的默认消费方，`FullCoverage` 永远为假、`unauthorized_journals` 指标永远红

- **位置**：`ledger.go:429-434`（无条件 `full.SetAuthCheck(s.queryStore, s.authVerifier)`，
  `s.authVerifier` 默认 nil）→ `service/reconcile.go:1436-1443`（verifier nil → `Complete=false`）
  → `service/reconcile.go:526-534`（`fullCoverage` 是所有 check 的 AND）
  → `service/reconcile.go:539-541`（`ReconcileCheckResult(name, Passed && Complete)` → false）
- **判定**：CONFIRMED
- **说明**：`ledger.New(pool)` 开箱即用的消费方每一次 full reconcile 都会拿到
  `full_coverage: false` 与一条红色 `unauthorized_journals` 指标，日志固定打
  `"reconcile: full suite found no violations but coverage was incomplete"`。
  与刚被删掉的 check #8 的区别是「这条**可以**变绿（去配 Attestor）」，所以不是结构恒假；
  但对一个没打算用 P5 的消费方来说，效果等同 —— 而告警疲劳的代价是一样的。
  值得考虑：verifier 为 nil 时这条 check 应当**不进 checks 列表**，而不是进列表后投一张
  `Complete=false`。

### [Minor] m-5：`CanonicalJournalDigest` 关于 Metadata 的威胁陈述与实际 DB 守卫不符（偏保守，会误导维护者）

- **位置**：`core/auth.go:215-231`（「a party who can write to the journals table directly ...
  could in principle alter metadata on an existing row without invalidating auth_signature」）
  vs. `postgres/sql/migrations/001_baseline.up.sql:1286-1288` +
  `ledger_journals_block_arbitrary_update` 函数体（`mutable CONSTANT text[] := ARRAY['event_id']`，
  其余任何列变化一律 `RAISE EXCEPTION`）
- **判定**：CONFIRMED
- **说明**：上一轮的 Minor（「`CanonicalJournalDigest` 不覆盖 Metadata 且未声明」）已经修好 ——
  现在声明得很充分。但声明里描述的攻击在 `ledger_app` 这一档**已经被 003/baseline 的
  journals 守卫挡住**（metadata 不在 mutable 白名单里），只有 owner/superuser 能改。
  注释末尾还建议「是否该给 journals.metadata 加 append-only 守卫是个正交的威胁模型问题」——
  那个守卫**已经存在**。这条不会致错，但会让下一个读它的人重复做一次已经做过的评估。
  另：签名域**确实**未覆盖的、且能改变记账结果或提现资格的字段，逐个核过 =
  **没有**（`journal_type_uid` / `idempotency_key` / `actor_id` / `source` / `effective_at` /
  `reversal_of_uid` / 全部 entry 的 holder+currency+classification+type+amount 均在域内；
  域外只有 `EventUID`（provenance，I-26 已披露）与 `Metadata`）。

### [Minor] m-6：`attestJournal` 把「幂等重放命中」标成 `unsigned_no_attestor`

- **位置**：`postgres/ledger_store.go:299-306`（命中 `GetJournalByIdempotencyKey` 时返回
  `journalAuth{status: core.AuthStatusUnsignedNoAttestor}`，digest/signature 皆空）
- **判定**：CONFIRMED（该值当前不会落库 —— `postJournalWithQueries` 在 idempotency lock 下
  重查会命中同一行并直接返回，`journals` 无 DELETE 权限所以那一行不可能消失）
- **说明**：这是一个「靠注释维持的不变量」：一旦哪天有人给 `postJournalWithQueries`
  加一条「重查没命中就继续插入」的分支（或换用一个不做重查的写路径），
  这个标签就会把一条**本该 signed** 的 journal 落成 `unsigned_no_attestor`，
  而 `unsigned_no_attestor` 正是 `VerifyLedger` 用来判「forged or predates the key」的那个值
  （`service/attest_verify.go:454-458`）—— 一条合法 journal 会被报成疑似伪造。
  更诚实的取值是引入一个 `replay` 状态或直接返回一个显式的 sentinel，而不是复用一个有语义的值。

### [Minor] m-7：C3 修复后，钉子函数的头部注释仍然描述被推翻的旧行为

- **位置**：`postgres/attested_auth_pin_test.go:25-36`（函数名已是
  `TestVerifiedBalance_CachedAuthorizedVerdictDoesNotSkipTheLiveCheck`，头部注释仍写
  「VerifiedBalance must still succeed with the correct balance, proving it trusted the cached,
  pre-corruption verdict」）vs. 函数体 `:74-93` 的断言（`require.Error`）与那段写得很好的
  「This assertion is the inverse of what it used to be」说明
- **判定**：CONFIRMED
- **说明**：只是文档债，但位置很关键 —— 它是 C3 这条 Critical 的主钉子，
  第一眼读到的注释与断言相反。

---

## 上轮修复复核

按契约要求，对落在本 territory 的上轮修复逐条问三件事（接到真实路径？拆掉会红？别处复制了同形缺陷？）。

| 上轮条目 | 现状 | 接到真实路径？ | 拆掉哪个实现会红 | 别处有无同形缺陷 |
|---|---|---|---|---|
| **C1** 防篡改全链未接线（六 territory 命中） | **部分修复**。`ledger.go:855-857` 在 `WithAttestor` 时自动 `SetAttestor`（anchor 为 nil）；`ledger.go:908-914` 补上了 `AttestInterval`/`AttestBatchSize` 的默认合并；`ledger.go:363-400` 新增 `VerifyLedger` facade；提现闸有了 HTTP 面（`server/handler_reservations.go:26,108`）。**但「四步接线」变成了「三步 + 一个默认关着的锚」**：消费方仍需自己 ① `WithAttestor` ② 起 `svc.Worker` ③ 自备 `core.Anchor` 并 `SetAttestor` 覆盖 ④ 逐调用点传 `RequireVerifiedBalance`。第 ③ 步没做时 `Run` 有 Warn（`service/worker.go:262-266`），但默认 logger 是 NopLogger。 | 是（自动接线在 `Worker()` 里，不在测试里） | 删 `ledger.go:855-857` → `service/worker_attestation_log_test.go` 相关钉子红 | **有**：见 M-2/M-3 —— 接线接上了，但接上之后的验证结论仍有两处不追问（未覆盖 entry、锚回退） |
| **C2** 配置表可写（攻击者让应用替他签） | 已修（`003_config_table_guards`），本轮抽查 `journals` 的 `ledger_journals_block_arbitrary_update`（白名单只有 `event_id`）与 baseline §14 的 GRANT 分类循环，逻辑自洽 | 是（DB 层强制） | 把 `mutable` 数组扩一列即可让 `grant_coverage_test` / I-25 钉子红 | 无（但见 C-1：`balance_checkpoints` **按设计**不能加守卫，而那正是 C-1 的攻击面 —— C2 的守卫覆盖了「记账依据」，没覆盖「余额缓存」） |
| **C3** 缓存判定跳过 live check | **已修且钉得很硬**。`postgres/verified_balance_store.go:140-162` 只在 `Unauthorized` 方向短路，其余（含 `Authorized`）一律 live check | 是（`VerifiedBalance` 是闸的唯一实现） | `postgres/attested_auth_pin_test.go` 的 `CachedAuthorizedVerdictDoesNotSkipTheLiveCheck` 与 `RefusesTamperedEntryAmount` 两条都会红；后者篡改的是 entry 金额，是真正有钱可赚的那一类 | 无同形复制。**但**：live check 覆盖的是签名，闸最终放行的金额仍来自 checkpoint —— 见 **C-1**。C3 修好的是「谁签的」，没修「多少钱」 |
| **D-attest** `5a07d74` 存证链默认关着 | 已修（见 C1 行）。`ledger.go:807-816` 的文档诚实写明 anchor 为 nil | 是 | 见 C1 行 | 见 M-2/M-3 |
| **I-45** `eebe646` 区分未知密钥与篡改签名 | **已修，且未知密钥是 fail-closed 的**：`authdev/ed25519.go:115-124` 包 `ErrUnknownAuthKey`；`core/auth.go:474-476` 用双 `%w` 同时保留 `ErrUnauthorizedJournal`，所以 `VerifiedBalanceReader` 这条粗粒度调用方行为不变（拒绝）。**攻击者用自己的 key 签得不到任何通道** | 是 | 去掉 `authdev` 的 `ErrUnknownAuthKey` 包裹 → `TestNewLocalAttestor_RejectsUnknownKeyID` + `TestFullReconciliation_UnauthorizedJournals_FlagsUnknownKeyAsDistinctFromForgery` 红 | **有一处新缺口**：修复把「轮换是正常运维」写进了 I-45 的论证，但库自带的 verifier 不支持多 key → 见 **M-5** |
| **精度与微秒截断** `884adcc` / `059c22a` | 已修且接到真实路径：`canonicalTimestamp`（`core/auth.go:160-162`）被 `CanonicalJournalDigest:270` 与 `encodeAttestedEntry:220` 两侧同时调用，签名侧的 `effectiveAt` 由 `resolveEffectiveAt`（`postgres/ledger_store.go:239-244`）解析一次后同时喂给签名与落库，pgx 写入时的 floor 与 `time.Truncate` 同向 | 是 | 去掉 `canonicalTimestamp` 调用 → I-46 的钉子在 Linux（真纳秒时钟）下红；⚠️ 在 macOS 上不一定红（`time.Now()` 本就微秒对齐）—— 这条钉子有平台依赖 | 无 |
| **`RunInTx` 永久 `unsigned_tx_mode`**（上轮 Major） | **未消除，改为「披露 + 提供出口」**：`ledger.go:463-476` 明写「once posted it stays that way forever ... VerifiedBalanceReader treats any dimension with such a contributing journal as **permanently UNDEFINED, with no remediation API**」，并给出 `Authorize`/`PostAuthorized` 的正确姿势 + 内部范例（`service/onchain.go:939-946`） | 是（`postgres/ledger_store.go:428-441` 明确标 `unsigned_tx_mode`） | `service/onchain_integration_test.go:45` 一带的钉子 | **风险仍在**：这是一个「一次踩中即永久不可逆」的陷阱（journal append-only，无法补签），唯一补救是冲销重发。且它与 **M-8** 组合会产生一个静默态：全库 `unsigned_tx_mode` → `unauthorized_journals` 报绿 |
| **`ConfirmPending`/`CancelPending` 不签名**（上轮 Major） | **已修**：`postgres/pending_store.go:286-296` 在 pool 模式下先 `Authorize` 再 `Begin`，`:277-280` 用 `PostAuthorized` | 是 | 把 `:286-296` 改回 `run(s.q.WithTx(tx), s.ledger.WithDB(tx), nil)` → 该路径重新落 `unsigned_tx_mode` | 无 |
| **提现闸无 HTTP 面**（上轮 Major） | 已修（`server/handler_reservations.go:26,108`；钉子 `server/handler_reservations_test.go:20-52` 两个方向都断言） | 是 | 删 handler 里那一行赋值 → 钉子红 | — |
| **`ErrUnauthorizedJournal` 无 bizcode**（上轮 Major） | 已修（`pkg/httpx/response.go:224-225` → `14010`，`pkg/bizcode/errors.go:89-100` 有很好的 why 注释，且明确避开 `Retryable` 默认） | 是 | — | — |
| **`full_coverage` 恒假**（上轮） | 已修：check #8 被删除而不是打补丁（`service/reconcile.go:481-494`）。**但**默认（未配 Attestor）消费方仍然恒假 —— 见 **m-4**；「扫了 0 个仍报 Passed」见 **M-8** | — | — | — |
| **`CanonicalJournalDigest` 不覆盖 Metadata 且未声明**（上轮 Minor） | 已修（`core/auth.go:215-231` 声明得很充分），但声明内容与 DB 守卫现状不符 —— 见 **m-5** | — | — | — |

**一句话**：上一轮的修复**全部真的接到了路径上**，钉子也拆得动会红 —— 这一轮找到的东西都不是
「修复没落地」，而是**修复停在了它自己那一层**：签名验到了，金额没跟着换（C-1）；
链接上了，覆盖率没人问（M-2）；锚接上了，锚自己的健康没人管（M-3/M-9）。

---

## 移交

- **territory A（financial-correctness）**：`NUMERIC(30,18) NOT NULL CHECK (x > 0)` 在 Postgres 里
  **接受 `'NaN'`**（本报告实测，见 M-7）。同形状且没有平衡 trigger 兜底的至少有
  `reservations.reserved_amount`（`001_baseline.up.sql:475`）、`bookings.amount`（`:541,556`）、
  `account_policies.min_balance`；对应 `postgres/convert.go:328,369-370,414-415,449` 全部是
  `mustNumericToDecimal`（panic）。请评估金额语义与 panic 面。
- **territory B（concurrency）**：`service/attestation.go:120-137` 的 `LatestAttestation` +
  `UncoveredEntries` 是**事务外**的两次独立读，`InsertAttestation` 才开事务；
  依赖 `NewLockedJob("attestation")` 的 leader 选举保证单写者。若 advisory lock 失效或
  跨集群多实例，`seq` 的 UNIQUE 会挡住，但「两个批次读到同一批 uncovered entries」的行为需要确认。
- **territory D（threat-model）**：
  - `ledger_app` 可 INSERT `entry_attestations`（它没有 INSERT 禁令，只有 no-UPDATE/no-DELETE），
    因此可以给自己的伪造 entry 预先写一条覆盖行让它躲过 `ListUncoveredEntries`。
    `VerifyLedger` 会通过 `entry_count` 与 batch digest 抓到，但请确认这条路径在 D 的 ACL 盘点里。
  - bizcode `14010` 的对外文案是 `"journal missing or has invalid authorization signature"`
    （`pkg/bizcode/errors.go:100`），直接把内部机制暴露给终端用户（`user-facing-surfaces.md`）。
  - `docs/RUNBOOK.md:978-987` 要求 ledger 侧 R2 token 持 `PutObject` —— 该权限足以改写
    verify 唯一会读的那个对象版本（M-4），请纳入 D 的凭证失效域分析。
- **territory E（consumer-surface）**：`ledger.New(pool)` 默认状态下**关着**的保护清单
  （签名 / attest job（需自己起 Worker）/ 锚 / 提现闸）没有任何一处在 facade 层被提示；
  `svc.Worker()` 的 anchor 默认 nil，只有一行 Warn 且默认 logger 是 NopLogger（`ledger.go:145`）。
- **territory F（test-credibility）**：
  - `service/attest_verify_test.go` 全部用新库、journal 数 < 20，使 M-1 的顺序错误在结构上不可见。
  - I-46 的钉子在 macOS 上可能恒绿（`time.Now()` 本就微秒对齐），只有 Linux CI 能证伪。
  - I-27 的 `Pinned by` 里没有一条验「未被覆盖的 entry 会被报出来」——因为那个检查不存在（M-2）。
- **territory I（operability）**：attestation / anchor **零 metric**（M-9）；
  `service/worker.go` 全程无 `recover()`，任一 job 的 panic 直接带走进程（M-7）。
- **territory H（structure）**：`cmd/ledger-cli` 在 root module、`anchors/r2` 是独立 module，
  导致生产锚与唯一验证工具结构上无法相遇（M-6）——这是 module 拆分的一个未被记录的后果。

---

## 我没能验证的

- **C-1 未实跑**。审计契约要求只读且只写报告文件，所以没有写测试去实跑
  「篡改 checkpoint → `RequireVerifiedBalance=true` 仍放行」。结论来自完整调用链
  （`reserver_store.go:123→206→274→292` + `checkpoints.sql:44-71` + baseline §14 GRANT 分类），
  每一段都无分支，但**它仍然只是读码**。建议 lead 亲手跑一遍再定级。
- **Cloudflare R2 的 Object Lock 是否真的支持 COMPLIANCE 模式、以及在开启默认 retention 的桶上
  `PutObject` 覆写同一 key 的确切行为**，我没有权威来源可核（本机无 R2 凭证，
  `anchors/r2/r2_test.go` 用的是 MinIO 且不设 retention）。M-4 的论证只依赖
  「`Head()` 读当前版本、全仓无代码读版本历史」这两条**代码事实**，
  不依赖 R2 的具体 retention 语义；但 `docs/RUNBOOK.md:955-960` 那句
  「compliance mode means not even the account root can shorten it」我**没有**核实，
  它可能是对的也可能不是。
- **M-7 的 Go 侧 panic 未实跑**（只跑了两条 SQL 语义断言）。`mustNumericToDecimal` 的 panic 是
  读码确定的；「worker 进程因此退出」依赖 `service/worker.go` 无 recover，我用 grep 确认了
  `service/*.go` 里没有 `recover()`，但没有实跑一个 panic 看 errgroup 的最终行为。
- **`anchors/r2` 的测试没跑**（需要 MinIO testcontainer；判断为对本 territory 结论无增益，
  因为 M-4 的论点不在该 module 的测试覆盖范围内）。
- **P7 Merkle 的密码学正确性**只做了结构性阅读（RFC 6962 域分隔、奇数叶不复制末节点、
  `LocateMismatches` 的等长前提），没有独立重实现对拍。
  设计 §9.4 附注说 lead 已用第三实现对拍过 CT 的 8-entry 测试向量，我沿用该结论，未复核。
- **`ExecuteTemplateBatch` / `ReverseJournalFraction` 的 preAuth 与落库内容一致性**
  （`reversal_fraction_store.go:107-191` 那段「preAuth 按 digest 比对，不匹配就拒绝」）
  只粗读，没有走完并发分支。
- **`chains/evm` / `service/onchain.go` 的签名路径**只确认了它用
  `AuthorizeTemplate` + `PostAuthorized` 的正确形状（`service/onchain.go:939-946`），
  没有走完 —— 归 territory G。

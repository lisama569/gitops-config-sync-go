# gitops-config-sync-go

本地多环境配置编排与漂移检测演示系统。用本地 JSON 文件模拟：

- **期望态**：多个配置来源按优先级合并渲染的结果；
- **集群观测态**：`state/<env>/observed.json`；
- **稳定版本**：`state/<env>/revisions/*.json`（只追加，供回滚）；
- **协调锁**：`state/<env>/lock.json`（带租约，多协调者互斥）。

不依赖任何外部组件，仅用 Go 标准库实现。

## 目录结构

| 包 | 职责 |
| --- | --- |
| `internal/types` | 领域模型（Resource / Rendered / ObservedResource / Revision / DriftRecord） |
| `internal/sources` | 从 `<root>/<env>/*.json` 加载多个配置来源 |
| `internal/render` | 按优先级深合并来源、校验类型、内容哈希版本（确定性可复现） |
| `internal/state` | 观测态 / 版本快照 / 协调锁的原子文件读写 |
| `internal/sync` | 依赖拓扑分波次应用、健康检查、失败回滚、显式回滚 |
| `internal/drift` | 期望态 vs 观测态的字段级漂移判定与策略 |
| `internal/coordinator` | 加锁→渲染→漂移判定→（按需）同步的完整协调入口 |
| `cmd/gitops-sync` | CLI |

## 配置来源格式

`<config-root>/<env>/<priority>-<name>.json`，文件名中的整数为默认优先级。
文件内容支持三种形态：

```json
// 1) 完整对象
{"name":"base","priority":10,"resources":[{"kind":"ConfigMap","name":"x","spec":{"k":"v"}}]}
// 2) 资源数组（名称/优先级取自文件名）
[{"kind":"Deployment","name":"app","spec":{"replicas":2},
  "dependsOn":[{"kind":"ConfigMap","name":"x"}],"healthCheck":"get /healthz"}]
// 3) "kind/name" 映射
{"Service/web":{"spec":{"port":80}}}
```

### 合并与覆盖规则

1. 来源按 `Priority` **升序**处理；优先级相同按名称升序（确定性）。
2. 同名资源（`kind/name`）的 `spec` **递归深合并**：
   - `map` 与 `map` 按键合并，键顺序不影响结果；
   - 标量/数组在同一路径相遇：
     - **值相同** → 幂等重复声明，允许；
     - **值不同且两个来源优先级相同** → 拒绝，原因 `FieldConflict`，
       错误中带双方来源名、字段路径与两个值，**绝不静默取其一**；
     - **值不同且当前来源优先级更高** → 覆盖（base→overlay 的预期分层语义）；
   - `map` 与标量在同路径相遇视为“形状冲突”，适用同样的冲突/覆盖规则。
3. `dependsOn` 取各来源声明的并集（排序去重）。
4. 值只允许 JSON 兼容类型（`string/float64/bool/nil/map/[]any`，整数归一化为 `float64`）；
   不合法类型 → `InvalidType`；
   来源目录/文件缺失 → `SourceMissing`；
   map 键与 `kind/name` 不一致 → `ResourceIDMismatch`；
   来源重名 → `DuplicateSource`。
5. **可复现**：不写入墙钟时间参与身份；版本号是对环境名+规范 JSON 的 FNV 内容哈希。
   同一组来源无论传入顺序如何，渲染结果与 `Revision` 完全一致（见
   `TestRenderDeterministic`）。

## 同步与回滚语义

### 分波次应用

1. 先在期望态全集上做依赖校验与 Kahn 拓扑分层：
   - 依赖不在期望态中 → `DependencyMissing`（应用前拒绝，不触碰观测态）；
   - 依赖成环 → `DependencyCycle`；
   - 同一波内资源按 ID 排序，波次顺序就是就绪顺序。
2. 每一波在**内存**中逐个资源“应用 + 健康检查”（健康检查可注入失败）：
   - 应用失败 → `ApplyError`；健康检查失败 → `HealthCheckFailed`（两者可区分）；
   - 任一步失败，**整波放弃**，内存结果不落盘。
3. 所有波次成功后，观测态与新版本快照才**一次性原子落盘**
   （临时文件 + `rename`）。因此磁盘上永远只有“上一个稳定态 / 新稳定态”，
   **不存在部分生效的中间状态**。
4. 若快照落盘失败，观测态会恢复到应用前快照。
5. 已存在且 Spec 一致、版本一致的托管资源直接跳过 ——
   **重复应用同一期望态幂等**，不产生新版本（`TestApplyIdempotent`）。

### 回滚

- `revisions/` 只追加；`current.json` 指向当前稳定版本。
- 显式回滚 `--to-revision N`：目标必须存在，否则 `RevisionMissing`；
  回滚同样经过依赖校验、分波次与健康检查，失败时观测态保持现状，
  成功后产生**新的版本快照**（历史不删除，可审计）。
- 不带版本号回滚表示“撤销最近一次同步”（需要至少两个历史版本）。
- `TestApplyHealthFailureRollsBack` 与
  `TestApplyFailureKeepsPreviousStableRevision` 验证：v1 成功、v2 失败后，
  观测态与 `current` 指针仍精确停留在 v1。

## 漂移检测与并发协调策略

### 漂移判定（字段级依据）

| 类别 | 含义 |
| --- | --- |
| `SpecChanged` | 当前观测 Spec 与协调者上次写入的快照 `lastAppliedSpec` 不一致（上次收敛后被人工改动），记录逐字段 `path/expected/observed` |
| `Outdated` | 观测 Spec 等于 `lastAppliedSpec`、但与新期望不一致（期望态合法升级，等待重新发布），允许同步收敛 |
| `Missing` | 期望态有、观测态无（部署缺口） |
| `Extra` | 观测态中受本系统托管、但期望态已无此资源；非托管资源忽略 |

> 关键点：必须区分「人工改动」与「合法配置升级」。判据不是版本号是否相等
> （手工改动也可能乱改版本号），而是观测态里的 **`lastAppliedSpec` 快照**：
> 当前值偏离了我们最后写入的值才算人工改动；当前值仍是旧发布版本则算待收敛。

Spec 完全一致 → 无漂移，重复协调因此空转。

### 人工改动策略（绝不静默纠正）

协调者在同步前先做漂移检测。一旦发现 `SpecChanged`：

- `Mark`：在观测态写 `drifted/driftMark/driftDiffs` 标记；
- `Alert`：调用 `Alerter` 告警（CLI 打印 `ALERT ...` 行，含字段依据）；
- `AlertAndMark`（默认）：两者都做；
- **本次不应用**，人工值原样保留，直到人工解决漂移；
  新配置到达也不会借同步之名悄悄覆盖人工改动
  （见 `TestManualDriftMarkedAndAlertedNotCorrected`）。

`Missing/Extra` 属于部署缺口，允许继续同步收敛。

### 并发协调

- 每个环境一把文件锁 `state/<env>/lock.json`，带协调者 ID、随机 token 与租约 TTL：
  - 无锁时用 `O_CREATE|O_EXCL` 原子创建；
  - 租约内他人获取 → `ErrLockHeld`，本次协调返回 `Skipped`（不应用、不覆盖）；
  - 租约过期的陈旧锁可被接管，接管后复核 token 防止双持；
  - 释放锁校验持有者+token，不能误删别人的锁。
- `TestConcurrentReconcileOnlyOneApplies`：8 个协调者并发处理同一环境，
  恰有 1 个 `Applied`，其余全部 `Skipped`，版本历史只有 1 条。

## 验证方法

```bash
# 全部单元测试（含竞态检测）
go test -race -count=1 ./...

# 仅看协调过程日志（输入、波次、漂移依据都会打印到测试日志）
go test -v ./internal/coordinator/ ./internal/sync/ ./internal/render/

# CLI 端到端
go build -o /tmp/gitops-sync ./cmd/gitops-sync
/tmp/gitops-sync reconcile --config <root> --state <dir> --env <env>
/tmp/gitops-sync rollback  --config <root> --state <dir> --env <env> --to-revision 1
```

退出码：`0` 成功/空转；`1` 协调失败（拒绝原因打印到 stderr）；`2` 参数错误。

### 测试覆盖矩阵

| 需求场景 | 测试 |
| --- | --- |
| 优先级覆盖 / 深合并 | `TestRenderPriorityOverride` |
| 覆盖冲突（值冲突 + 形状冲突） | `TestRenderFieldConflict` / `TestRenderShapeConflict` |
| 非法类型 / 来源缺失，原因可区分 | `TestRenderInvalidType` / `TestLoadRejections` |
| 渲染可复现 | `TestRenderDeterministic` |
| 依赖乱序仍按拓扑分波 | `TestPlanWavesRespectsDependencyOrder` |
| 依赖缺失 / 依赖环 | `TestPlanWavesMissingDependency` / `TestPlanWavesCycle` |
| 健康失败/应用失败回滚且原因可区分 | `TestApplyHealthFailureRollsBack` / `TestApplyErrorDistinctFromHealth` |
| 失败保持上一个稳定态一致 | `TestApplyFailureKeepsPreviousStableRevision` |
| 重复应用幂等 | `TestApplyIdempotent` / `TestReconcileIdempotent` |
| 显式回滚 / 回滚目标缺失 | `TestRollbackToRevision` / `TestRollbackMissingRevision` |
| 漂移字段依据 / 缺失 / 额外 / 配置升级 | `TestDetectSpecChangedWithFieldBasis` / `TestDetectMissingAndExtra` / `TestDetectOutdatedConfigUpgrade` |
| 人工改动只标记告警不纠正 | `TestManualDriftMarkedAndAlertedNotCorrected` |
| 多协调者顺序接管连续收敛、版本号不撞车 | `TestSequentialReconcilesAcrossCoordinators` |
| 并发重复协调只应用一次 | `TestConcurrentReconcileOnlyOneApplies` |
| 锁互斥、陈旧租约接管、token 保护 | `TestLockMutualExclusionAndLease` |

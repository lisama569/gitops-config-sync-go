# gitops-config-sync-go

用本地文件模拟期望态与集群观测态的多环境配置编排与漂移检测流程。
配置在合并、同步与回滚时的结果完全可预期、可复现。

## 结构

| 包 | 职责 |
|---|---|
| `internal/state` | 配置来源加载、按优先级键级合并、确定性渲染（`Source` / `Merge` / `Render`） |
| `internal/sync` | 依赖拓扑分波、按波应用、资源回收、健康检查与回滚（`Plan` / `Apply`） |
| `internal/cluster` | 文件模拟的集群观测态（每资源一个 JSON 文件，健康标记文件） |
| `internal/drift` | 期望态 vs 观测态的双向内容哈希比对，含多出资源识别（`Detect`） |
| `internal/reconcile` | 跨实例互斥、崩溃可接管、幂等的协调器与漂移策略（`Reconciler`） |

## 合并与覆盖规则

来源是本地 JSON 文件，加载时赋予优先级（数值越大越高）：

```json
{"resources": {"db": {"replicas": 2, "dependsOn": ["net"], "config": {"image": "pg:15"}}}}
```

- **最小单位叠加**：标量字段（`replicas` / `dependsOn`）以整字段为单位、`config` 以
  **单个键**为最小单位合并。高优先级来源只覆盖它显式设置的字段/键，未触及的部分
  从低优先级来源**原样保留**（例如 overlay 只改 `config.mode` 时，base 里的
  `config.image` 不受影响）。
- **冲突边界**：只有**同一个字段 / 同一个 config 键**被**同优先级**来源赋予**不同值**
  才算冲突，拒绝渲染并返回 `*state.RejectError{Reason: ConflictingOverride}`
  （`Field` 定位到具体键，如 `db.config.mode`），绝不静默取其一。
  同优先级来源设置**互不相交**的 config 键时直接叠加，不算冲突；同值也不算冲突。
- **类型校验**：字段有固定 schema（`replicas` 非负整数、`dependsOn` 字符串数组、
  `config` 字符串字典），类型不合法返回 `RejectError{Reason: InvalidFieldType}`；
  未知字段同样拒绝。
- **来源缺失**：文件不存在返回 `RejectError{Reason: SourceMissing}`。
- **可复现**：渲染结果按资源名排序、Config 键有序，哈希为规范化 JSON 的 SHA-256；
  同一环境下多次渲染哈希必然一致（见 `TestRenderReproducible`、
  `TestMergeConfigKeyLevelRenderReproducible`）。

三种拒绝原因（`SourceMissing` / `InvalidFieldType` / `ConflictingOverride`）可通过
`errors.As` 取 `*RejectError` 后比较 `Reason` 加以区分。

## 同步与回滚语义

- **分波**：`sync.Plan` 对 `dependsOn` 做拓扑分层，同波资源无相互依赖、按名排序；
  依赖未定义 → `FailError{Reason: DependencyNotReady}`，成环 → `DependencyCycle`。
- **应用**：`sync.Apply` 先对集群快照，再逐波写入；每个资源写入前检查其依赖
  在集群中已存在且健康，每波结束后对该波做健康检查（健康由集群目录下
  `health/<name>` 标记文件模拟，缺省健康）。
- **回收（prune）**：全部波次成功后，观测态中存在但期望态已移除的资源会被清理，
  记入 `Result.Pruned`；不会因为期望态里没有就当作不存在。回收发生在所有波次
  成功之后，因此同步失败回滚时不会发生部分回收（见
  `TestApplyPrunesRemovedResources`、`TestApplyFailureKeepsPrunableResources`）。
- **失败归类**：`DependencyNotReady` / `HealthCheckFailed` / `ApplyFailed`，
  均可经 `errors.As` 取 `*sync.FailError` 区分。
- **回滚**：任一波失败立即用同步前快照整体恢复观测态，不留部分生效的中间态；
  回滚后期望态（上一稳定版本）与观测态保持一致（见
  `TestApplyHealthFailureRollsBack`、`TestReconcileRollbackKeepsStableState`）。

## 漂移检测与并发协调

- **漂移判定依据**：双向比对内容哈希——
  - 期望与观测都在但哈希不同 → `Modified`；
  - 期望中存在、集群中缺失 → `Missing`；
  - **集群中存在、期望态中完全没有 → `Extra`**（多出来的资源不会被忽略）。
  判定所依据的期望/观测哈希都记录在 `drift.Entry` 中并在测试日志中打印。
- **人工改动识别**：协调器在集群目录维护 `_manifest_<env>.json`（上次成功应用的
  期望态哈希）。当本次期望态哈希等于 manifest 哈希、观测态却偏离（含多出资源）时，
  判定为绕开流程的人工改动。
- **漂移策略**：
  - `PolicyMark`：将漂移报告落盘为 `_drift_<env>.json` 并返回 `StatusDriftMarked`，
    **不静默纠正、不擅自删除**，观测态保持原样；
  - `PolicyAlert`：在 Mark 基础上额外返回 `ErrDriftAlert`，提示人工介入；
  - `PolicyCorrect`：按正常同步流程纠正回期望态。
- **多实例互斥**：互斥分两层——进程内按环境互斥锁 + 集群目录下的锁文件
  `_lock_<env>.json`（内容为持有者标识与租约到期时间，经临时文件 + hard link
  原子创建）。多个互相独立、甚至不同进程里的 `Reconciler` 实例同时处理同一环境时，
  同一时刻只有一个持有锁；同一份期望态只会被真正落地一次，其余实例获得锁后发现
  观测态已符合期望态而返回 `StatusNoop`，不会重复应用或相互覆盖（见
  `TestMultiInstanceReconcileAppliesOnce`，集群写入次数被精确断言）。
- **接管条件**：锁是带租约的（`Reconciler.LockTTL`，默认 30s）。持有者正常结束会
  主动释放；持有者在协调中途异常退出时锁文件遗留，但租约到期后任何后续实例都可
  删除旧锁、接管并继续把状态收敛到位，环境不会被永久锁住（见
  `TestLockTakeoverAfterHolderCrash`）。注意 `LockTTL` 必须大于单次协调的最大耗时，
  否则租约可能在持有者仍在工作时被接管。
- **忙锁**：锁被存活实例持有且租约未过期时，其他实例轮询等待至
  `Reconciler.LockWait`（默认 10s）超时，返回 `ErrLockBusy`（可用 `errors.Is`
  判定），由调用方决定稍后重试。
- **幂等**：重复应用同一期望态时漂移检测为空，直接 `Noop`，不再写集群；
  manifest 与漂移标记均为原子写（临时文件 + rename），异常退出不留半截文件。

## 验证方法

```sh
go vet ./...
go test -race ./...          # 或 go test -v ./... 查看输入/波次/漂移依据日志
# 并发相关用例多跑几轮验证稳定性：
go test -race -count=10 -run 'Concurrent|MultiInstance|LockTakeover|LiveLock' ./internal/reconcile/
```

测试覆盖：

| 场景 | 测试 |
|---|---|
| 同优先级覆盖冲突 | `TestMergeConflictingOverride`（拒绝原因可区分） |
| config 键级叠加 / 键保留 | `TestMergeConfigKeyLevelOverride` |
| 同优先级互不相交的键 | `TestMergeConfigDisjointKeysSamePriority` |
| 同优先级同键不同值 | `TestMergeConfigSameKeyConflictSamePriority`（定位到具体键） |
| 字段类型非法 / 来源缺失 | `TestMergeInvalidFieldType` / `TestLoadSourceMissing` |
| 渲染可复现 | `TestRenderReproducible` / `TestMergeConfigKeyLevelRenderReproducible` |
| 依赖乱序分波 / 依赖环 | `TestPlanWavesRespectDependencies` / `TestPlanDependencyCycle` |
| 健康检查失败回滚 | `TestApplyHealthFailureRollsBack` |
| 资源从期望态移除后回收 | `TestApplyPrunesRemovedResources` / `TestReconcilePrunesRemovedResources` |
| 失败回滚不发生部分回收 | `TestApplyFailureKeepsPrunableResources` |
| 多出资源识别为漂移 | `TestDetectExtraResource` / `TestExtraResourceMarkedPerPolicy` / `TestExtraResourceAlertPolicy` |
| 并发重复协调（单实例） | `TestConcurrentReconcileAppliesOnce`（仅 1 次 Applied） |
| 多实例互斥落地一次 | `TestMultiInstanceReconcileAppliesOnce`（8 个独立实例） |
| 持有者崩溃后接管 | `TestLockTakeoverAfterHolderCrash` |
| 存活锁等待超时 | `TestLiveLockReturnsBusy`（`ErrLockBusy`） |
| 幂等重复应用 | `TestReconcileIdempotent` |
| 人工改动标记不纠正 | `TestManualDriftMarkedNotCorrected` / `TestManualDriftAlertReturnsError` |
| 失败后回到上一稳定态 | `TestReconcileRollbackKeepsStableState` |

## 兼容范围

- 仅依赖 Go 标准库，Go 1.25+；文件锁基于 `os.Link` 的原子语义，要求集群目录所在
  文件系统支持 hard link（本地 ext4/xfs/tmpfs 等均可；NFS 等网络文件系统不在
  保证范围内）。
- 多实例互斥以共享同一集群目录为前提；跨进程、跨机器（共享目录）场景下租约
  接管语义相同，但跨机器时钟漂移需小于 `LockTTL`。
- 既有单实例行为（合并拒绝原因、分波/回滚、幂等 Noop、Mark/Alert 策略）保持不变，
  旧有用例全部继续通过。

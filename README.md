# gitops-config-sync-go

用本地文件模拟期望态与集群观测态的多环境配置编排与漂移检测流程。
配置在合并、同步与回滚时的结果完全可预期、可复现。

## 结构

| 包 | 职责 |
|---|---|
| `internal/state` | 配置来源加载、按优先级合并、确定性渲染（`Source` / `Merge` / `Render`） |
| `internal/sync` | 依赖拓扑分波、按波应用、健康检查与回滚（`Plan` / `Apply`） |
| `internal/cluster` | 文件模拟的集群观测态（每资源一个 JSON 文件，健康标记文件） |
| `internal/drift` | 期望态 vs 观测态的内容哈希比对（`Detect`） |
| `internal/reconcile` | 并发安全、幂等的协调器与漂移策略（`Reconciler`） |

## 合并与覆盖规则

来源是本地 JSON 文件，加载时赋予优先级（数值越大越高）：

```json
{"resources": {"db": {"replicas": 2, "dependsOn": ["net"], "config": {"image": "pg:15"}}}}
```

- **最小单位叠加**：标量字段（`replicas` / `dependsOn`）按整字段解析；`config`
  按**键**为最小单位叠加——高优先级来源只覆盖它显式设置的键，低优先级来源里
  未被碰到的键原样保留（见 `TestMergeConfigKeyLevelOverlay`）。
- **不相交键不算冲突**：几个同优先级来源各自设置互不相交的 `config` 键时正常
  合并，不会被当成冲突拒绝（见 `TestMergeConfigSamePriorityDisjointKeys`）。
- **冲突拒绝**：只有**同一个键**（或同一个标量字段）被**同优先级**来源赋予
  **不同值**时才拒绝渲染，返回 `*state.RejectError{Reason: ConflictingOverride}`，
  `Field` 定位到具体键（如 `db.config.pool`），绝不静默取其一；同值视为一致。
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
- **失败归类**：`DependencyNotReady` / `HealthCheckFailed` / `ApplyFailed`，
  均可经 `errors.As` 取 `*sync.FailError` 区分。
- **回滚**：任一波失败立即用同步前快照整体恢复观测态，不留部分生效的中间态；
  回滚后期望态（上一稳定版本）与观测态保持一致（见
  `TestApplyHealthFailureRollsBack`、`TestReconcileRollbackKeepsStableState`）。

## 漂移检测与并发协调

- **漂移判定依据**：对期望态中每个资源计算内容哈希，与集群观测到的内容哈希逐一
  比对；不一致记 `Modified`，集群缺失记 `Missing`；反向地，观测态里存在、期望态
  里完全没有的资源记 `Extra`，**不会被忽略**。判定所依据的期望/观测哈希都记录在
  `drift.Entry` 中并在测试日志中打印。
- **人工改动识别**：协调器在集群目录维护 `_manifest_<env>.json`（上次成功应用的
  期望态哈希 + 受管资源清单）。当本次期望态哈希等于 manifest 哈希、观测态却偏离
  （含人为增删资源）时，判定为绕开流程的人工改动。
- **漂移策略**：
  - `PolicyMark`：将漂移报告落盘为 `_drift_<env>.json` 并返回 `StatusDriftMarked`，
    **不静默纠正**，观测态保持人工改动后的值（多余资源也保留）；
  - `PolicyAlert`：在 Mark 基础上额外返回 `ErrDriftAlert`，提示人工介入；
  - `PolicyCorrect`：按正常同步流程纠正回期望态，多余资源一并清理。
- **资源回收（prune）**：某资源从期望态移除后，下一次成功同步会把它从观测态删掉
  （依据 manifest 中的受管清单识别，见 `TestPruneResourceRemovedFromDesired`）；
  从未被本环境管理的多余资源不在 prune 范围内，只按上述策略标记/告警/纠正。
- **多实例互斥**：互斥是双层的——进程内每环境一把互斥锁，叠加集群目录下的
  锁文件 `_lock_<env>`（`O_CREATE|O_EXCL` 创建，内容为持有者标识）。因此多个
  独立 `Reconciler` 实例、甚至不同进程同时处理同一环境时也真正互斥：同一期望态
  只被落地一次（`StatusApplied`），其余实例获得锁后发现观测态已符合期望态而返回
  `StatusNoop`，不会重复应用或相互覆盖（见
  `TestMultiInstanceReconcileAppliesOnce`，集群写入次数被精确断言）。
- **异常退出与接管**：锁文件按 mtime 判定存活，超过 `LockTTL`（默认 5s）未刷新
  视为持有者在协调中途异常退出，后续实例删除旧锁、重新竞争并接管，把状态继续
  收敛到位（manifest 只在完整落地后写入，部分应用的观测态会被漂移检测发现并重放，
  见 `TestStaleLockTakeoverAfterCrash`）；锁未过期时竞争者等待至
  `LockTimeout`（默认 30s）后返回 `ErrLockTimeout`，不会误接管
  （见 `TestFreshLockBlocksUntilTimeout`）。释放锁时校验持有者标识，避免误删
  已被接管的新锁。
- **幂等**：重复应用同一期望态时漂移检测为空，直接 `Noop`，不再写集群。

## 兼容范围

- 旧版 manifest（无 `managed` 字段）可继续读取：缺失时视为无受管资源，首次协调后
  自动写入新格式。
- 合并行为变化：`config` 从整块覆盖改为按键叠加。依赖"高优先级来源整体替换
  config"的旧配置，需把要清除的键显式覆盖为新值（不再支持整组隐式清空）。
- 漂移检测新增 `Extra` 类别：集群目录下若有与本环境无关的资源文件，现在会被
  报告为漂移（Mark/Alert 策略只标记不删除）；建议每个环境使用独立的集群目录。
- 集群目录新增 `_lock_<env>` 锁文件；协调正常结束后自动清除，异常退出时按
  `LockTTL` 过期接管。

## 验证方法

```sh
go vet ./...
go test -v ./...                    # 查看输入/波次/漂移判定依据日志
go test -race -count=5 ./...        # 并发用例配合 -race 多跑几轮验证稳定性
```

本地复现关键场景：

```sh
# 键级合并与冲突边界
go test -v ./internal/state/ -run 'TestMergeConfig'

# 多实例互斥、崩溃接管、锁超时
go test -v -race ./internal/reconcile/ -run 'TestMultiInstance|TestStaleLock|TestFreshLock'

# 资源回收与多余资源漂移
go test -v ./internal/reconcile/ -run 'TestPrune|TestExtra'
go test -v ./internal/drift/ -run 'TestDetectExtra'
```

测试覆盖：

| 场景 | 测试 |
|---|---|
| 同优先级标量冲突 | `TestMergeConflictingOverride`（拒绝原因可区分） |
| config 键级叠加 / 不相交键 / 同键冲突 | `TestMergeConfigKeyLevelOverlay` / `TestMergeConfigSamePriorityDisjointKeys` / `TestMergeConfigSameKeyConflict` |
| 字段类型非法 / 来源缺失 | `TestMergeInvalidFieldType` / `TestLoadSourceMissing` |
| 渲染可复现（含键级合并） | `TestRenderReproducible` / `TestMergeConfigKeyLevelRenderReproducible` |
| 依赖乱序分波 / 依赖环 | `TestPlanWavesRespectDependencies` / `TestPlanDependencyCycle` |
| 健康检查失败回滚 | `TestApplyHealthFailureRollsBack` |
| 并发重复协调（单实例） | `TestConcurrentReconcileAppliesOnce`（仅 1 次 Applied） |
| 多实例互斥落地一次 | `TestMultiInstanceReconcileAppliesOnce` |
| 崩溃后过期锁接管并收敛 | `TestStaleLockTakeoverAfterCrash` |
| 新鲜锁不误接管 | `TestFreshLockBlocksUntilTimeout` |
| 幂等重复应用 | `TestReconcileIdempotent` |
| 人工改动标记不纠正 | `TestManualDriftMarkedNotCorrected` / `TestManualDriftAlertReturnsError` |
| 期望态移除资源被回收 | `TestPruneResourceRemovedFromDesired` |
| 多余资源识别为漂移 | `TestDetectExtra` / `TestExtraResourceMarkedNotRemoved` / `TestExtraResourceCorrected` |
| 失败后回到上一稳定态 | `TestReconcileRollbackKeepsStableState` |

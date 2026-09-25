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

- **优先级覆盖**：同一字段被多个来源设置时，最高优先级来源的值获胜；低优先级来源未被覆盖的字段保留。
- **冲突拒绝**：同一字段被**同优先级**的多个来源赋予**不同值**时，拒绝渲染，返回
  `*state.RejectError{Reason: ConflictingOverride}`，绝不静默取其一；同值则视为一致，不冲突。
- **类型校验**：字段有固定 schema（`replicas` 非负整数、`dependsOn` 字符串数组、
  `config` 字符串字典），类型不合法返回 `RejectError{Reason: InvalidFieldType}`；
  未知字段同样拒绝。
- **来源缺失**：文件不存在返回 `RejectError{Reason: SourceMissing}`。
- **可复现**：渲染结果按资源名排序、Config 键有序，哈希为规范化 JSON 的 SHA-256；
  同一环境下多次渲染哈希必然一致（见 `TestRenderReproducible`）。

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
  比对；不一致记 `Modified`，集群缺失记 `Missing`，判定所依据的期望/观测哈希都
  记录在 `drift.Entry` 中并在测试日志中打印。
- **人工改动识别**：协调器在集群目录维护 `_manifest_<env>.json`（上次成功应用的
  期望态哈希）。当本次期望态哈希等于 manifest 哈希、观测态却偏离时，判定为绕开
  流程的人工改动。
- **漂移策略**：
  - `PolicyMark`：将漂移报告落盘为 `_drift_<env>.json` 并返回 `StatusDriftMarked`，
    **不静默纠正**，观测态保持人工改动后的值；
  - `PolicyAlert`：在 Mark 基础上额外返回 `ErrDriftAlert`，提示人工介入；
  - `PolicyCorrect`：按正常同步流程纠正回期望态。
- **并发协调**：`Reconciler` 为每个环境持有独立互斥锁，同一环境的协调串行执行。
  多个协调者同时处理同一环境时，只有一个实际应用（`StatusApplied`），其余在
  获得锁后发现观测态已符合期望态而返回 `StatusNoop`，不会重复应用或相互覆盖
  （见 `TestConcurrentReconcileAppliesOnce`，集群写入次数被精确断言）。
- **幂等**：重复应用同一期望态时漂移检测为空，直接 `Noop`，不再写集群。

## 验证方法

```sh
go vet ./...
go test -race ./...   # 或 go test -v ./... 查看输入/波次/漂移依据日志
```

测试覆盖：

| 场景 | 测试 |
|---|---|
| 同优先级覆盖冲突 | `TestMergeConflictingOverride`（拒绝原因可区分） |
| 字段类型非法 / 来源缺失 | `TestMergeInvalidFieldType` / `TestLoadSourceMissing` |
| 渲染可复现 | `TestRenderReproducible` |
| 依赖乱序分波 / 依赖环 | `TestPlanWavesRespectDependencies` / `TestPlanDependencyCycle` |
| 健康检查失败回滚 | `TestApplyHealthFailureRollsBack` |
| 并发重复协调 | `TestConcurrentReconcileAppliesOnce`（仅 1 次 Applied） |
| 幂等重复应用 | `TestReconcileIdempotent` |
| 人工改动标记不纠正 | `TestManualDriftMarkedNotCorrected` / `TestManualDriftAlertReturnsError` |
| 失败后回到上一稳定态 | `TestReconcileRollbackKeepsStableState` |

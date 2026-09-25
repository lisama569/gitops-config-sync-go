// Package types 定义配置编排系统共享的领域模型。
//
// 所有资源都用 Resource 表示：Kind/Name 唯一确定一个资源，
// Spec 为渲染后的配置主体。为保证可复现，Spec 的值仅允许
// string、float64、bool、nil、map[string]any、[]any。
package types

import "time"

// ResourceID 唯一标识一个环境内的资源。
type ResourceID struct {
	Kind string
	Name string
}

func (r ResourceID) String() string { return r.Kind + "/" + r.Name }

// Source 是一个带优先级的配置来源。
// Priority 数值越大优先级越高；同名资源做深合并，
// 标量字段被多个来源以不同值覆盖时判定为冲突。
type Source struct {
	Name      string
	Priority  int
	Resources map[ResourceID]*Resource
}

// Resource 表示单个配置资源。
type Resource struct {
	Kind         string            `json:"kind"`
	Name         string            `json:"name"`
	Spec         map[string]any    `json:"spec"`
	DependsOn    []ResourceID      `json:"dependsOn,omitempty"`
	HealthCheck  string            `json:"healthCheck,omitempty"`
	SourceOrigin map[string]string `json:"sourceOrigin,omitempty"`
}

// ID 返回资源标识。
func (r *Resource) ID() ResourceID { return ResourceID{Kind: r.Kind, Name: r.Name} }

// Rendered 是一个环境渲染完成后的期望态快照。
type Rendered struct {
	Env        string
	Resources  map[ResourceID]*Resource
	Revision   int64
	RenderedAt time.Time
}

// ObservedResource 是集群侧（观测态）中一个资源的记录。
type ObservedResource struct {
	Resource
	AppliedRevision int64
	AppliedAt       time.Time
	Managed         bool
	// ManagedBy 是当前持有协调锁的协调者标识。
	ManagedBy string
	// LastAppliedSpec 是最近一次由协调者写入时的 Spec 快照，
	// 用于把“期望态合法升级（待收敛）”与“应用后被人工改动”区分开。
	LastAppliedSpec map[string]any `json:"lastAppliedSpec,omitempty"`
	// Drifted/DriftKind/DriftDiffs 是检测到人工改动后按策略写下的标记。
	Drifted    bool        `json:"drifted,omitempty"`
	DriftMark  DriftKind   `json:"driftMark,omitempty"`
	DriftDiffs []FieldDiff `json:"driftDiffs,omitempty"`
}

// DriftKind 描述漂移的类别。
type DriftKind string

const (
	// DriftNone 无漂移。
	DriftNone DriftKind = ""
	// DriftSpecChanged 观测态 Spec 与期望态不一致（疑似人工改动）。
	DriftSpecChanged DriftKind = "SpecChanged"
	// DriftMissing 期望态中存在、观测态中缺失。
	DriftMissing DriftKind = "Missing"
	// DriftOutdated 观测态来自旧的期望内容版本（合法配置升级，待收敛），允许同步。
	DriftOutdated DriftKind = "Outdated"
	// DriftExtra 观测态中存在但不在期望态中（仅在管理托管资源时关注）。
	DriftExtra DriftKind = "Extra"
)

// FieldDiff 记录一个漂移字段的判定依据。
type FieldDiff struct {
	Path     string
	Expected any
	Observed any
}

// DriftRecord 是单资源漂移判定结果。
type DriftRecord struct {
	ID      ResourceID
	Kind    DriftKind
	Diffs   []FieldDiff
	Managed bool
}

// Revision 是一次成功同步落盘的稳定状态。
type Revision struct {
	Number int64
	// DesiredRevision 是该快照对应期望态的内容哈希。
	DesiredRevision int64
	Env             string
	RenderedAt      time.Time
	AppliedAt       time.Time
	Resources       map[ResourceID]*Resource
}

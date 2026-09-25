// Package state 定义期望态的数据模型与来源合并规则。
package state

import "fmt"

// RejectReason 区分渲染/合并被拒绝的原因。
type RejectReason string

const (
	ReasonSourceMissing       RejectReason = "SourceMissing"       // 来源文件缺失
	ReasonInvalidFieldType    RejectReason = "InvalidFieldType"    // 字段类型不合法
	ReasonConflictingOverride RejectReason = "ConflictingOverride" // 同优先级来源冲突覆盖
)

// RejectError 携带可区分的拒绝原因。
type RejectError struct {
	Reason RejectReason
	Source string
	Field  string
	Detail string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("render rejected [%s] source=%q field=%q: %s", e.Reason, e.Source, e.Field, e.Detail)
}

// Source 表示一个配置来源（本地 JSON 文件）。
type Source struct {
	Name     string // 来源名（通常为文件路径）
	Priority int    // 数值越大优先级越高
	// Resources: resourceName -> raw config
	Resources map[string]map[string]any
}

// Resource 是合并后的资源期望态。
type Resource struct {
	Name      string            `json:"name"`
	DependsOn []string          `json:"dependsOn,omitempty"`
	Config    map[string]string `json:"config"`
	Replicas  int               `json:"replicas"`
}

// DesiredState 是渲染后的最终期望态，内容确定可复现。
type DesiredState struct {
	Env       string     `json:"env"`
	Resources []Resource `json:"resources"`
	Hash      string     `json:"hash"`
}

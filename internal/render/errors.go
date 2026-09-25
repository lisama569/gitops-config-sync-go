// Package render 按优先级合并多个来源并渲染出最终期望态。
package render

import "fmt"

// Reason 描述渲染被拒绝的可区分原因。
type Reason string

const (
	// ReasonSourceMissing 声明的来源文件缺失（由 sources.Loader 归类）。
	ReasonSourceMissing Reason = "SourceMissing"
	// ReasonInvalidType 字段值类型不合法（不是允许的 JSON 兼容类型）。
	ReasonInvalidType Reason = "InvalidType"
	// ReasonFieldConflict 同优先级的多个来源以不同值/不同形状覆盖同一字段。
	ReasonFieldConflict Reason = "FieldConflict"
	// ReasonResourceIDMismatch 资源在文件中的 key 与 kind/name 字段不一致。
	ReasonResourceIDMismatch Reason = "ResourceIDMismatch"
	// ReasonDuplicateSource 同一环境中出现两个同名来源。
	ReasonDuplicateSource Reason = "DuplicateSource"
	// ReasonEmptySource 来源没有名称或没有任何资源。
	ReasonEmptySource Reason = "EmptySource"
)

// RenderError 携带可区分的拒绝原因与定位信息，绝不静默取其一。
type RenderError struct {
	Reason Reason
	// Source 触发问题的来源名；跨来源冲突时为第一个来源。
	Source string
	// OtherSource 冲突的另一方来源名。
	OtherSource string
	// Resource 出问题的资源（可空）。
	Resource string
	// Path 出问题的 spec 字段路径，如 "server.port"。
	Path string
	// Values 冲突双方在该路径上的值，便于日志核对。
	Values  [2]any
	Message string
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("render rejected [%s] source=%s other=%s resource=%s path=%s: %s",
		e.Reason, e.Source, e.OtherSource, e.Resource, e.Path, e.Message)
}

// Is 让 errors.Is 可按 Reason 归类失败。
func (e *RenderError) Is(target error) bool {
	t, ok := target.(*RenderError)
	if !ok {
		return false
	}
	return t.Reason == e.Reason
}

func errInvalidType(source, resource, path string, v any) *RenderError {
	return &RenderError{
		Reason:   ReasonInvalidType,
		Source:   source,
		Resource: resource,
		Path:     path,
		Values:   [2]any{v, nil},
		Message:  fmt.Sprintf("value of type %T is not a supported JSON-compatible type", v),
	}
}

func errFieldConflict(source, other, resource, path string, a, b any) *RenderError {
	return &RenderError{
		Reason:      ReasonFieldConflict,
		Source:      source,
		OtherSource: other,
		Resource:    resource,
		Path:        path,
		Values:      [2]any{a, b},
		Message:     fmt.Sprintf("field overridden differently by equal-priority sources: %v != %v", a, b),
	}
}

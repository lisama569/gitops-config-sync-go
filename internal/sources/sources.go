// Package sources 负责从本地文件读取配置来源（期望态的原始输入）。
//
// 目录布局：<root>/<env>/*.json，每个文件是一个来源，文件名必须为
// "<priority>-<name>.json"，priority 为 >=0 的整数；文件内支持三种形态：
//
//  1. {"name":..., "priority":..., "resources":[{kind,name,spec,...}, ...]}
//  2. 直接给出资源数组 [ {kind,name,spec,...}, ... ]（名称/优先级取自文件名）
//  3. 以 "kind/name" 为键的资源映射 { "Deployment/api": {...} }
//
// 来源目录不存在或文件缺失按 SourceMissing 归类；JSON 语法或字段
// 类型问题按 InvalidType 归类；文件名不合法同样拒绝，不静默跳过。
package sources

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lisama569/gitops-config-sync-go/internal/render"
	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

// Loader 从本地目录加载多个配置来源。
type Loader struct {
	root string
}

// NewLoader 创建加载器，root 为存放各环境来源文件的根目录。
func NewLoader(root string) *Loader {
	return &Loader{root: root}
}

// sourceFile 是文件内允许的完整形态。
type sourceFile struct {
	Name      string         `json:"name"`
	Priority  int            `json:"priority"`
	Resources []resourceFile `json:"resources"`
}

// resourceFile 是文件内单个资源的 JSON 形态。
type resourceFile struct {
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Spec        map[string]any `json:"spec"`
	DependsOn   []depRef       `json:"dependsOn,omitempty"`
	HealthCheck string         `json:"healthCheck,omitempty"`
}

type depRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Load 加载指定环境的全部来源。
// 来源缺失、字段类型不合法等问题在这里被归类为可区分的错误（*render.RenderError）。
func (l *Loader) Load(env string) ([]types.Source, error) {
	dir := filepath.Join(l.root, env)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &render.RenderError{Reason: render.ReasonSourceMissing, Source: dir,
				Message: fmt.Sprintf("config directory for env %q does not exist", env)}
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return nil, &render.RenderError{Reason: render.ReasonSourceMissing, Source: dir,
			Message: fmt.Sprintf("no .json source files found for env %q", env)}
	}
	sort.Strings(files)

	var out []types.Source
	for _, name := range files {
		src, err := l.loadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

func (l *Loader) loadFile(path string) (types.Source, error) {
	base := filepath.Base(path)
	fileName := strings.TrimSuffix(base, ".json")
	parts := strings.SplitN(fileName, "-", 2)
	if len(parts) != 2 || parts[1] == "" {
		return types.Source{}, &render.RenderError{Reason: render.ReasonSourceMissing, Source: base,
			Message: `source file name must be "<priority>-<name>.json"`}
	}
	prio, err := strconv.Atoi(parts[0])
	if err != nil || prio < 0 {
		return types.Source{}, &render.RenderError{Reason: render.ReasonSourceMissing, Source: base,
			Message: `source file name must start with a non-negative integer priority, e.g. "10-base.json"`}
	}
	defaultName := parts[1]

	raw, err := os.ReadFile(path)
	if err != nil {
		return types.Source{}, &render.RenderError{Reason: render.ReasonSourceMissing, Source: base, Message: err.Error()}
	}

	// 先尝试完整形态；再尝试数组；最后尝试 "kind/name" 映射。
	var full sourceFile
	if err := json.Unmarshal(raw, &full); err == nil && full.Resources != nil {
		if full.Name != "" {
			defaultName = full.Name
		}
		if full.Priority != 0 {
			prio = full.Priority
		}
		return buildSource(defaultName, prio, full.Resources, base)
	}

	var list []resourceFile
	if err := json.Unmarshal(raw, &list); err == nil && list != nil {
		return buildSource(defaultName, prio, list, base)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err == nil && m != nil {
		// 排除完整形态的包装键。
		if _, isFull := m["resources"]; isFull {
			return types.Source{}, &render.RenderError{Reason: render.ReasonInvalidType, Source: base,
				Message: `"resources" must be an array of resource objects`}
		}
		list = make([]resourceFile, 0, len(m))
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var rf resourceFile
			if err := json.Unmarshal(m[key], &rf); err != nil {
				return types.Source{}, &render.RenderError{Reason: render.ReasonInvalidType, Source: base,
					Resource: key, Path: "$", Message: err.Error()}
			}
			kp := strings.SplitN(key, "/", 2)
			if len(kp) != 2 || kp[0] == "" || kp[1] == "" {
				return types.Source{}, &render.RenderError{Reason: render.ReasonInvalidType, Source: base,
					Resource: key, Message: `map key must be "kind/name"`}
			}
			if rf.Kind == "" {
				rf.Kind = kp[0]
			}
			if rf.Name == "" {
				rf.Name = kp[1]
			}
			if rf.Kind != kp[0] || rf.Name != kp[1] {
				return types.Source{}, &render.RenderError{Reason: render.ReasonResourceIDMismatch,
					Source: base, Resource: key, Message: "key does not match kind/name fields"}
			}
			list = append(list, rf)
		}
		return buildSource(defaultName, prio, list, base)
	}

	return types.Source{}, &render.RenderError{Reason: render.ReasonInvalidType, Source: base,
		Message: "source file must contain a resource array, a kind/name map, or a {name,priority,resources} object"}
}

func buildSource(name string, priority int, files []resourceFile, file string) (types.Source, error) {
	if len(files) == 0 {
		return types.Source{}, &render.RenderError{Reason: render.ReasonEmptySource, Source: file,
			Message: "source contains no resources"}
	}
	src := types.Source{Name: name, Priority: priority, Resources: map[types.ResourceID]*types.Resource{}}
	for i, rf := range files {
		if rf.Kind == "" || rf.Name == "" {
			return types.Source{}, &render.RenderError{Reason: render.ReasonResourceIDMismatch, Source: file,
				Resource: fmt.Sprintf("#%d", i), Message: "resource kind/name must not be empty"}
		}
		if rf.Spec == nil {
			rf.Spec = map[string]any{}
		}
		res := &types.Resource{
			Kind:        rf.Kind,
			Name:        rf.Name,
			Spec:        rf.Spec,
			HealthCheck: rf.HealthCheck,
		}
		for _, d := range rf.DependsOn {
			res.DependsOn = append(res.DependsOn, types.ResourceID{Kind: d.Kind, Name: d.Name})
		}
		id := res.ID()
		if _, dup := src.Resources[id]; dup {
			return types.Source{}, &render.RenderError{Reason: render.ReasonResourceIDMismatch, Source: file,
				Resource: id.String(), Message: "duplicate resource within one source file"}
		}
		src.Resources[id] = res
	}
	return src, nil
}

package render

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

// Renderer 合并多个来源渲染期望态。
//
// 合并规则（确定性、可复现）：
//   - 来源按 Priority 升序处理，Priority 相同按名称升序；
//   - 同名资源的 Spec 做递归深合并：
//     map 与 map 按键合并；标量/数组在同一路径相遇时，
//     值相同视为幂等重复；值不同且两个来源优先级相同 -> FieldConflict；
//     值不同且当前来源优先级更高 -> 覆盖（GitOps 预期的按层覆盖）；
//   - DependsOn 取各来源声明的并集（按 ID 排序去重）；
//     HealthCheck 与标量字段相同的冲突规则；
//   - 字段值只允许 JSON 兼容类型，整数会归一化为 float64；
//   - RenderedAt 不写墙钟时间，Revision 为内容哈希，保证同输入可复现。
type Renderer struct{}

// New 创建渲染器。
func New() *Renderer { return &Renderer{} }

// Render 按优先级合并来源；冲突/非法类型不得静默取其一，直接返回带原因的错误。
func (r *Renderer) Render(env string, srcs []types.Source) (*types.Rendered, error) {
	if err := validateSources(srcs); err != nil {
		return nil, err
	}

	// 确定性排序：优先级升序，同优先级名称升序。
	ordered := make([]types.Source, len(srcs))
	copy(ordered, srcs)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].Name < ordered[j].Name
	})
	priorityOf := make(map[string]int, len(ordered))
	for _, s := range ordered {
		priorityOf[s.Name] = s.Priority
	}

	out := &types.Rendered{Env: env, Resources: map[types.ResourceID]*types.Resource{}}
	// 全局资源 ID 集合，按确定顺序遍历，保证输出稳定。
	idSet := map[types.ResourceID]struct{}{}

	for _, src := range ordered {
		ids := make([]types.ResourceID, 0, len(src.Resources))
		for id := range src.Resources {
			ids = append(ids, id)
		}
		sortIDs(ids)
		for _, id := range ids {
			res := src.Resources[id]
			if res.Kind == "" || res.Name == "" {
				return nil, &RenderError{Reason: ReasonResourceIDMismatch, Source: src.Name,
					Resource: id.String(), Message: "resource kind/name must not be empty"}
			}
			if res.ID() != id {
				return nil, &RenderError{Reason: ReasonResourceIDMismatch, Source: src.Name,
					Resource: id.String(),
					Message:  fmt.Sprintf("map key %q does not match kind/name %q", id, res.ID())}
			}
			idSet[id] = struct{}{}
			normalized, err := normalizeValue(res.Spec, src.Name, id.String(), "")
			if err != nil {
				return nil, err
			}
			spec, _ := normalized.(map[string]any)

			existing, ok := out.Resources[id]
			if !ok {
				existing = &types.Resource{
					Kind:         id.Kind,
					Name:         id.Name,
					Spec:         map[string]any{},
					SourceOrigin: map[string]string{},
				}
				out.Resources[id] = existing
			}
			if err := mergeSpec(existing, src, spec, priorityOf); err != nil {
				return nil, err
			}
			mergeMeta(existing, res)
		}
	}

	out.Revision = contentHash(env, out.Resources)
	return out, nil
}

// validateSources 检查来源名非空、不重复。
func validateSources(srcs []types.Source) error {
	seen := map[string]struct{}{}
	for _, s := range srcs {
		if s.Name == "" {
			return &RenderError{Reason: ReasonEmptySource, Message: "source name must not be empty"}
		}
		if _, dup := seen[s.Name]; dup {
			return &RenderError{Reason: ReasonDuplicateSource, Source: s.Name,
				Message: "duplicate source name within one render"}
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

// mergeSpec 把 src 中的 spec 深合并进 existing。
func mergeSpec(existing *types.Resource, src types.Source, spec map[string]any, priorityOf map[string]int) error {
	return mergeMap(existing.Spec, spec, existing.SourceOrigin, src.Name, priorityOf, existing.ID().String(), "")
}

func mergeMap(dst, src map[string]any, origin map[string]string, srcName string, prio map[string]int, resID, prefix string) error {
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		nv := src[k]
		ov, exists := dst[k]
		if !exists {
			dst[k] = deepCopy(nv)
			claim(origin, path, nv, srcName)
			continue
		}
		om, oIsMap := ov.(map[string]any)
		nm, nIsMap := nv.(map[string]any)
		if oIsMap && nIsMap {
			if err := mergeMap(om, nm, origin, srcName, prio, resID, path); err != nil {
				return err
			}
			continue
		}
		if valuesEqual(ov, nv) {
			// 幂等重复声明：保留先到的来源归属。
			continue
		}
		// 同一路径形状不同（map vs 标量/数组）或标量值不同：
		// 找出该路径现有子树的归属来源，按优先级判定冲突/覆盖。
		owner := subtreeOwner(origin, path)
		if owner == "" {
			owner = origin[path]
		}
		// 标量/数组相遇且值不同：同优先级 -> 冲突；更高优先级 -> 覆盖。
		if owner == "" || prio[owner] == prio[srcName] {
			return errFieldConflict(firstNonEmpty(owner, srcName), srcName, resID, path, ov, nv)
		}
		if prio[srcName] > prio[owner] {
			release(origin, path, ov)
			delete(origin, path)
			dst[k] = deepCopy(nv)
			claim(origin, path, nv, srcName)
			continue
		}
		// 低优先级试图覆盖高优先级：忽略，保持高优先级值。
	}
	return nil
}

// claim 把 value 下所有叶子路径的归属标记为 srcName。
func claim(origin map[string]string, prefix string, value any, srcName string) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			claim(origin, p, v[k], srcName)
		}
	default:
		origin[prefix] = srcName
	}
}

// release 删除旧值在 origin 中登记的叶子路径。
func release(origin map[string]string, prefix string, value any) {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			release(origin, p, child)
		}
	default:
		delete(origin, prefix)
	}
}

// subtreeOwner 返回占用 path 子树任意叶子路径的来源名（确定性：取排序后第一个）。
func subtreeOwner(origin map[string]string, path string) string {
	prefix := path + "."
	var owners []string
	seen := map[string]struct{}{}
	for p, who := range origin {
		if p == path || (len(p) > len(prefix) && p[:len(prefix)] == prefix) {
			if _, ok := seen[who]; !ok {
				seen[who] = struct{}{}
				owners = append(owners, who)
			}
		}
	}
	sort.Strings(owners)
	if len(owners) == 0 {
		return ""
	}
	return owners[0]
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// mergeMeta 合并依赖（并集、排序去重）与 HealthCheck（标量冲突规则）。
func mergeMeta(existing, incoming *types.Resource) {
	depSet := map[types.ResourceID]struct{}{}
	for _, d := range existing.DependsOn {
		depSet[d] = struct{}{}
	}
	for _, d := range incoming.DependsOn {
		depSet[d] = struct{}{}
	}
	deps := make([]types.ResourceID, 0, len(depSet))
	for d := range depSet {
		deps = append(deps, d)
	}
	sortIDs(deps)
	existing.DependsOn = deps

	if incoming.HealthCheck != "" {
		existing.HealthCheck = incoming.HealthCheck
	}
}

// normalizeValue 递归校验值是否为 JSON 兼容类型，并把整数归一化为 float64。
func normalizeValue(v any, source, resource, path string) (any, error) {
	switch x := v.(type) {
	case nil, bool, string, float64:
		return x, nil
	case int:
		return float64(x), nil
	case int8:
		return float64(x), nil
	case int16:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case float32:
		return float64(x), nil
	case map[string]any:
		out := make(map[string]any, len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cp := k
			if path != "" {
				cp = path + "." + k
			}
			nv, err := normalizeValue(x[k], source, resource, cp)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			ne, err := normalizeValue(e, source, resource, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out[i] = ne
		}
		return out, nil
	default:
		return nil, errInvalidType(source, resource, path, v)
	}
}

func valuesEqual(a, b any) bool {
	return deepEqual(a, b)
}

// deepEqual 对 map 做键序无关比较，其余委托给标准库。
func deepEqual(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok || bok {
		if !aok || !bok || len(am) != len(bm) {
			return false
		}
		for k, av := range am {
			bv, ok := bm[k]
			if !ok || !deepEqual(av, bv) {
				return false
			}
		}
		return true
	}
	return scalarEqual(a, b)
}

func scalarEqual(a, b any) bool {
	// 归一化后数值统一为 float64，DeepEqual 即可。
	return fmt.Sprintf("%#v", a) == fmt.Sprintf("%#v", b)
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}

func sortIDs(ids []types.ResourceID) {
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Kind != ids[j].Kind {
			return ids[i].Kind < ids[j].Kind
		}
		return ids[i].Name < ids[j].Name
	})
}

// contentHash 对环境名与全部资源的规范 JSON 求 FNV 哈希作为内容版本。
func contentHash(env string, resources map[types.ResourceID]*types.Resource) int64 {
	ids := make([]types.ResourceID, 0, len(resources))
	for id := range resources {
		ids = append(ids, id)
	}
	sortIDs(ids)
	canonical := struct {
		Env       string            `json:"env"`
		Resources []*types.Resource `json:"resources"`
	}{Env: env, Resources: make([]*types.Resource, 0, len(ids))}
	for _, id := range ids {
		canonical.Resources = append(canonical.Resources, resources[id])
	}
	b, _ := json.Marshal(canonical)
	h := fnv.New64a()
	h.Write(b)
	// 转为正数 int64，便于作为可读的版本号。
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// Package sync 按依赖波次应用期望态并支持回滚。
package sync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lisama569/gitops-config-sync-go/internal/cluster"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
)

// FailReason 区分同步失败原因。
type FailReason string

const (
	ReasonDependencyCycle    FailReason = "DependencyCycle"
	ReasonDependencyNotReady FailReason = "DependencyNotReady"
	ReasonHealthCheckFailed  FailReason = "HealthCheckFailed"
	ReasonApplyFailed        FailReason = "ApplyFailed"
)

// FailError 携带可区分的同步失败原因。
type FailError struct {
	Reason   FailReason
	Resource string
	Detail   string
}

func (e *FailError) Error() string {
	return fmt.Sprintf("sync failed [%s] resource=%q: %s", e.Reason, e.Resource, e.Detail)
}

// Result 记录一次同步的波次、已应用资源与被回收资源。
type Result struct {
	Waves   [][]string
	Applied []string
	Pruned  []string // 观测态中存在但期望态已移除、本次被清理的资源
}

// Plan 将资源按依赖关系拓扑分波：同一波内无相互依赖，按名字排序保证可复现。
// 依赖未定义或成环时返回 *FailError。
func Plan(resources []state.Resource) ([][]string, error) {
	byName := map[string]state.Resource{}
	for _, r := range resources {
		byName[r.Name] = r
	}
	// 依赖必须指向已定义资源。
	for _, r := range resources {
		for _, dep := range r.DependsOn {
			if _, ok := byName[dep]; !ok {
				return nil, &FailError{
					Reason:   ReasonDependencyNotReady,
					Resource: r.Name,
					Detail:   "dependency " + dep + " is not defined in desired state",
				}
			}
		}
	}

	remaining := map[string]state.Resource{}
	for name, r := range byName {
		remaining[name] = r
	}
	var waves [][]string
	for len(remaining) > 0 {
		var wave []string
		for name, r := range remaining {
			ready := true
			for _, dep := range r.DependsOn {
				if _, stillWaiting := remaining[dep]; stillWaiting {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, name)
			}
		}
		if len(wave) == 0 {
			stuck := make([]string, 0, len(remaining))
			for name := range remaining {
				stuck = append(stuck, name)
			}
			sort.Strings(stuck)
			return nil, &FailError{
				Reason:   ReasonDependencyCycle,
				Resource: strings.Join(stuck, ","),
				Detail:   "dependency cycle detected",
			}
		}
		sort.Strings(wave)
		for _, name := range wave {
			delete(remaining, name)
		}
		waves = append(waves, wave)
	}
	return waves, nil
}

// Apply 分波应用期望态：
//   - 每波应用后对该波资源做健康检查，依赖未就绪/健康失败/写入出错都会失败；
//   - 任一失败立即回滚到同步前快照，不留部分生效的中间态；
//   - 全部波次成功后，回收观测态中存在但期望态已不包含的资源（prune），
//     不能因为期望态里没有就当作不存在。
func Apply(c *cluster.Cluster, desired state.DesiredState) (Result, error) {
	waves, err := Plan(desired.Resources)
	if err != nil {
		return Result{}, err
	}
	snapshot, err := c.Snapshot()
	if err != nil {
		return Result{}, fmt.Errorf("snapshot before sync: %w", err)
	}

	byName := map[string]state.Resource{}
	for _, r := range desired.Resources {
		byName[r.Name] = r
	}

	res := Result{Waves: waves}
	rollback := func(cause error) (Result, error) {
		if rerr := c.Restore(snapshot); rerr != nil {
			return res, fmt.Errorf("rollback after %v failed: %w", cause, rerr)
		}
		return res, cause
	}

	for _, wave := range waves {
		for _, name := range wave {
			r := byName[name]
			// 依赖就绪检查：所有依赖必须已存在于集群且健康。
			for _, dep := range r.DependsOn {
				if _, ok, err := c.Get(dep); err != nil {
					return rollback(&FailError{Reason: ReasonDependencyNotReady, Resource: name, Detail: err.Error()})
				} else if !ok {
					return rollback(&FailError{Reason: ReasonDependencyNotReady, Resource: name, Detail: "dependency " + dep + " not present in cluster"})
				}
				healthy, err := c.Healthy(dep)
				if err != nil {
					return rollback(&FailError{Reason: ReasonDependencyNotReady, Resource: name, Detail: err.Error()})
				}
				if !healthy {
					return rollback(&FailError{Reason: ReasonDependencyNotReady, Resource: name, Detail: "dependency " + dep + " is unhealthy"})
				}
			}
			if err := c.Apply(r); err != nil {
				return rollback(&FailError{Reason: ReasonApplyFailed, Resource: name, Detail: err.Error()})
			}
			res.Applied = append(res.Applied, name)
		}
		// 波次健康检查：本波资源全部健康才进入下一波。
		for _, name := range wave {
			healthy, err := c.Healthy(name)
			if err != nil {
				return rollback(&FailError{Reason: ReasonHealthCheckFailed, Resource: name, Detail: err.Error()})
			}
			if !healthy {
				return rollback(&FailError{Reason: ReasonHealthCheckFailed, Resource: name, Detail: "post-apply health check failed"})
			}
		}
	}

	// 回收：观测态里存在、期望态已移除的资源在全部波次成功后清理。
	// 放在最后执行，失败回滚路径不会走到这里，快照恢复语义不受影响。
	var pruned []string
	for name := range snapshot {
		if _, keep := byName[name]; !keep {
			pruned = append(pruned, name)
		}
	}
	sort.Strings(pruned)
	for _, name := range pruned {
		if err := c.Remove(name); err != nil {
			return rollback(&FailError{Reason: ReasonApplyFailed, Resource: name, Detail: "prune: " + err.Error()})
		}
		res.Pruned = append(res.Pruned, name)
	}
	return res, nil
}

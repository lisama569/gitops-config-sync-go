package sync

import (
	"sort"
	"time"

	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

// Logger 记录同步过程（输入、波次、健康判定），测试中可注入 *log.Logger。
type Logger interface {
	Printf(format string, args ...any)
}

// noopLogger 默认丢弃日志。
type noopLogger struct{}

func (noopLogger) Printf(string, ...any) {}

// ApplyHook 用于测试：让指定资源在应用或健康检查阶段失败。
type ApplyHook struct {
	// FailApply 为 true 时该资源应用动作返回错误（模拟中途出错）。
	FailApply map[types.ResourceID]bool
	// FailHealth 为 true 时该资源健康检查返回 false。
	FailHealth map[types.ResourceID]bool
}

// Applier 把渲染结果同步到观测态（本地文件模拟集群）。
type Applier struct {
	store HealthStore
	hc    HealthChecker
	hook  *ApplyHook
	log   Logger
	now   func() time.Time
}

// HealthStore 是 Applier 需要的状态存取能力。
type HealthStore interface {
	Observed(env string) (map[types.ResourceID]*types.ObservedResource, error)
	SaveObserved(env string, obs map[types.ResourceID]*types.ObservedResource) error
	SaveRevision(env string, rev *types.Revision) (int64, error)
	LastRevision(env string) (*types.Revision, error)
	RevisionNumbers(env string) ([]int64, error)
	Revision(env string, number int64) (*types.Revision, error)
}

// HealthChecker 判定资源健康状态，测试中可注入失败。
type HealthChecker interface {
	Healthy(res *types.Resource) (bool, error)
}

// New 创建 Applier。
func New(store HealthStore, hc HealthChecker) *Applier {
	return &Applier{store: store, hc: hc, hook: &ApplyHook{}, log: noopLogger{}, now: time.Now}
}

// WithHook 注入故障注入钩子。
func (a *Applier) WithHook(h *ApplyHook) *Applier {
	if h != nil {
		a.hook = h
	}
	return a
}

// WithLogger 注入过程日志。
func (a *Applier) WithLogger(l Logger) *Applier {
	if l == nil {
		l = noopLogger{}
	}
	a.log = l
	return a
}

// Apply 分波次应用；中途失败必须回滚到上一个稳定状态，不留中间态。
//
// 语义：
//   - 先在期望态全集上做依赖校验与波次规划（缺失依赖/环直接拒绝，不触碰观测态）；
//   - 每一波在内存中应用全部资源并逐一做健康检查；
//     任一步失败，整波放弃，观测态保持上一个稳定版本（应用前快照），返回分类错误；
//   - 全部波次通过后，才一次性原子落盘观测态与新版本快照，
//     因此磁盘上永远只有“上一个稳定态”和“新稳定态”，不存在部分生效的中间态；
//   - 观测态中已存在且 Spec 与期望一致的资源视为幂等命中，不重复应用。
func (a *Applier) Apply(env string, desired *types.Rendered, coordinator string) (*types.Revision, error) {
	a.log.Printf("[sync] apply start env=%s coordinator=%s desiredRevision=%d resources=%d",
		env, coordinator, desired.Revision, len(desired.Resources))

	prev, err := a.store.LastRevision(env)
	if err != nil {
		return nil, err
	}

	// 起点为当前观测态（失败时保持不变）。
	working, err := a.store.Observed(env)
	if err != nil {
		return nil, err
	}
	a.log.Printf("[sync] input observed resources=%d desired resources=%d", len(working), len(desired.Resources))

	waves, err := planWaves(env, desired)
	if err != nil {
		a.log.Printf("[sync] rejected before any apply: %v", err)
		return nil, err
	}
	changed := false
	for i, wave := range waves {
		a.log.Printf("[sync] wave %d/%d size=%d members=%s",
			i+1, len(waves), len(wave), joinIDs(wave))
		waveOK := true
		for _, id := range wave {
			res := desired.Resources[id]
			if cur, ok := working[id]; ok && deepEqualSpec(cur.Spec, res.Spec) &&
				cur.AppliedRevision == desired.Revision && cur.Managed {
				a.log.Printf("[sync] wave %d resource=%s idempotent skip (already at revision %d)",
					i+1, id, desired.Revision)
				continue
			}
			changed = true
			if a.hook.FailApply[id] {
				err = syncErr(ReasonApplyError, env, i+1, id.String(), "",
					"apply action failed (injected)")
				waveOK = false
				break
			}
			working[id] = &types.ObservedResource{
				Resource:        *deepCopyResource(res),
				AppliedRevision: desired.Revision,
				AppliedAt:       a.now(),
				Managed:         true,
				ManagedBy:       coordinator,
				LastAppliedSpec: deepCopyAny(res.Spec).(map[string]any),
			}
			a.log.Printf("[sync] wave %d resource=%s applied; checking health=%q",
				i+1, id, res.HealthCheck)
			healthy, herr := a.checkHealthy(res)
			if herr != nil || !healthy {
				if herr != nil {
					err = syncErr(ReasonHealthCheckFailed, env, i+1, id.String(), "", herr.Error())
				} else {
					err = syncErr(ReasonHealthCheckFailed, env, i+1, id.String(), "",
						"health check reported not ready (injected)")
				}
				waveOK = false
				break
			}
			a.log.Printf("[sync] wave %d resource=%s healthy", i+1, id)
		}
		if !waveOK {
			// 放弃整波：working 不落盘，观测态保持上一个稳定状态。
			a.log.Printf("[sync] wave %d failed: %v — discarding wave, observed state stays at previous stable revision",
				i+1, err)
			return nil, err
		}
	}

	// 移除期望态中已不存在的托管资源（仍保持“全部波次成功后一次性落盘”）。
	for id := range working {
		if _, keep := desired.Resources[id]; !keep && working[id].Managed {
			a.log.Printf("[sync] pruning managed resource no longer desired: %s", id)
			delete(working, id)
			changed = true
		}
	}

	if !changed {
		a.log.Printf("[sync] no-op: observed state already at desired revision %d", desired.Revision)
		if prev != nil {
			// 返回真实的当前稳定快照（而非现场拼一个零号版本）。
			cur, err := a.store.Revision(env, prev.Number)
			if err != nil {
				return nil, err
			}
			if cur != nil {
				return cur, nil
			}
		}
		return &types.Revision{
			Number:          0,
			DesiredRevision: desired.Revision,
			Env:             env,
			RenderedAt:      desired.RenderedAt,
			AppliedAt:       a.now(),
			Resources:       cloneDesired(desired),
		}, nil
	}

	if err := a.store.SaveObserved(env, working); err != nil {
		return nil, err
	}
	rev := &types.Revision{
		Number:          0, // 由 store 用 O_EXCL 分配，避免并发版本号撞车
		DesiredRevision: desired.Revision,
		Env:             env,
		RenderedAt:      desired.RenderedAt,
		AppliedAt:       a.now(),
		Resources:       cloneDesired(desired),
	}
	number, err := a.store.SaveRevision(env, rev)
	if err != nil {
		// 观测态已写入但快照失败：恢复为应用前快照内容，保持二者一致。
		if rerr := a.restoreObserved(env, prev); rerr != nil {
			a.log.Printf("[sync] WARNING revision persist failure reason=%v and rollback restore failed: %v", err, rerr)
		}
		return nil, err
	}
	a.log.Printf("[sync] apply success env=%s revision=%d desiredRevision=%d", env, number, desired.Revision)
	rev.Number = number
	return rev, nil
}

// Rollback 回滚到指定版本（toRevision<=0 表示上一个稳定版本的前一个，
// 即“撤销最近一次同步”；显式给出时必须存在）。
// 回滚本身也分波次并做健康检查，失败时观测态保持当前状态，同样不留中间态。
func (a *Applier) Rollback(env string, toRevision int64, coordinator string) (*types.Revision, error) {
	target, err := a.resolveRollbackTarget(env, toRevision)
	if err != nil {
		return nil, err
	}
	a.log.Printf("[sync] rollback start env=%s coordinator=%s targetRevision=%d desiredRevision=%d",
		env, coordinator, target.Number, target.DesiredRevision)

	desired := &types.Rendered{
		Env:        env,
		Resources:  target.Resources,
		Revision:   target.DesiredRevision,
		RenderedAt: target.RenderedAt,
	}
	// 回滚也必须经过依赖校验与波次健康检查；失败时不触碰观测态。
	return a.Apply(env, desired, coordinator)
}

func (a *Applier) resolveRollbackTarget(env string, toRevision int64) (*types.Revision, error) {
	if toRevision > 0 {
		rev, err := a.store.Revision(env, toRevision)
		if err != nil {
			return nil, err
		}
		if rev == nil {
			return nil, syncErr(ReasonRevisionMissing, env, 0, "", "",
				"requested revision does not exist")
		}
		return rev, nil
	}
	nums, err := a.store.RevisionNumbers(env)
	if err != nil {
		return nil, err
	}
	if len(nums) < 2 {
		return nil, syncErr(ReasonRevisionMissing, env, 0, "", "",
			"no previous stable revision to roll back to")
	}
	return a.store.Revision(env, nums[len(nums)-2])
}

func (a *Applier) checkHealthy(res *types.Resource) (bool, error) {
	if a.hook.FailHealth[res.ID()] {
		return false, nil
	}
	if a.hc == nil {
		return true, nil
	}
	return a.hc.Healthy(res)
}

func (a *Applier) restoreObserved(env string, prev *types.Revision) error {
	if prev == nil {
		// 之前没有稳定版本：恢复为空观测态。
		return a.store.SaveObserved(env, map[types.ResourceID]*types.ObservedResource{})
	}
	obs := make(map[types.ResourceID]*types.ObservedResource, len(prev.Resources))
	for id, res := range prev.Resources {
		obs[id] = &types.ObservedResource{
			Resource:        *deepCopyResource(res),
			AppliedRevision: prev.DesiredRevision,
			AppliedAt:       prev.AppliedAt,
			Managed:         true,
			LastAppliedSpec: deepCopyAny(res.Spec).(map[string]any),
		}
	}
	return a.store.SaveObserved(env, obs)
}

// planWaves 做依赖校验并返回按拓扑序分层的波次。
// 缺失依赖 -> DependencyMissing；存在环 -> DependencyCycle。
func planWaves(env string, desired *types.Rendered) ([][]types.ResourceID, error) {
	indeg := make(map[types.ResourceID]int, len(desired.Resources))
	dependents := make(map[types.ResourceID][]types.ResourceID)
	for id, res := range desired.Resources {
		indeg[id] = len(res.DependsOn)
		for _, dep := range res.DependsOn {
			if _, ok := desired.Resources[dep]; !ok {
				return nil, syncErr(ReasonDependencyMissing, env, 0, id.String(), dep.String(),
					"dependency is not present in desired state")
			}
			dependents[dep] = append(dependents[dep], id)
		}
	}

	var waves [][]types.ResourceID
	remaining := len(indeg)
	for remaining > 0 {
		var ready []types.ResourceID
		for id, d := range indeg {
			if d == 0 {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			var cyc []types.ResourceID
			for id, d := range indeg {
				if d > 0 {
					cyc = append(cyc, id)
				}
			}
			sortIDs(cyc)
			return nil, syncErr(ReasonDependencyCycle, env, len(waves)+1, "", "",
				"dependency cycle among: "+joinIDs(cyc))
		}
		sortIDs(ready)
		waves = append(waves, ready)
		for _, id := range ready {
			delete(indeg, id)
			remaining--
			for _, child := range dependents[id] {
				indeg[child]--
			}
		}
	}
	return waves, nil
}

func cloneDesired(desired *types.Rendered) map[types.ResourceID]*types.Resource {
	out := make(map[types.ResourceID]*types.Resource, len(desired.Resources))
	for id, res := range desired.Resources {
		out[id] = deepCopyResource(res)
	}
	return out
}

func deepCopyResource(r *types.Resource) *types.Resource {
	cp := *r
	cp.Spec = deepCopyAny(r.Spec).(map[string]any)
	cp.DependsOn = append([]types.ResourceID(nil), r.DependsOn...)
	cp.SourceOrigin = map[string]string{}
	for k, v := range r.SourceOrigin {
		cp.SourceOrigin[k] = v
	}
	return &cp
}

func deepCopyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopyAny(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyAny(e)
		}
		return out
	default:
		return v
	}
}

func deepEqualSpec(a, b map[string]any) bool {
	return specEqual(a, b)
}

func specEqual(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok || bok {
		if !aok || !bok || len(am) != len(bm) {
			return false
		}
		for k, av := range am {
			bv, ok := bm[k]
			if !ok || !specEqual(av, bv) {
				return false
			}
		}
		return true
	}
	if al, aok := a.([]any); aok {
		bl, bok := b.([]any)
		if !bok || len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !specEqual(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func sortIDs(ids []types.ResourceID) {
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Kind != ids[j].Kind {
			return ids[i].Kind < ids[j].Kind
		}
		return ids[i].Name < ids[j].Name
	})
}

func joinIDs(ids []types.ResourceID) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id.String()
	}
	return out
}

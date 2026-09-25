package sync

import (
	"errors"
	"log"
	"os"
	"testing"

	"github.com/lisama569/gitops-config-sync-go/internal/state"
	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

func rid(k, n string) types.ResourceID { return types.ResourceID{Kind: k, Name: n} }

func mkRes(k, n string, spec map[string]any, deps ...types.ResourceID) *types.Resource {
	return &types.Resource{Kind: k, Name: n, Spec: spec, DependsOn: deps, HealthCheck: "get /healthz/" + n}
}

func desired(env string, rev int64, res ...*types.Resource) *types.Rendered {
	m := map[types.ResourceID]*types.Resource{}
	for _, r := range res {
		m[r.ID()] = r
	}
	return &types.Rendered{Env: env, Resources: m, Revision: rev}
}

// 依赖乱序声明时，波次仍按拓扑序排列（a <- b <- c）。
func TestPlanWavesRespectsDependencyOrder(t *testing.T) {
	logger := log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
	// 故意先放 c 再放 a，验证不依赖 map 顺序。
	d := desired("e", 1,
		mkRes("Deployment", "c", map[string]any{}, rid("Deployment", "b")),
		mkRes("Deployment", "b", map[string]any{}, rid("Deployment", "a")),
		mkRes("Deployment", "a", map[string]any{"v": float64(1)}),
	)
	waves, err := planWaves("e", d)
	if err != nil {
		t.Fatal(err)
	}
	logger.Printf("WAVE PLAN %v", waves)
	if len(waves) != 3 {
		t.Fatalf("waves = %d, want 3", len(waves))
	}
	if waves[0][0] != rid("Deployment", "a") ||
		waves[1][0] != rid("Deployment", "b") ||
		waves[2][0] != rid("Deployment", "c") {
		t.Fatalf("waves out of order: %v", waves)
	}
}

// 缺失依赖 -> DependencyMissing（依赖未就绪在应用前被拒绝）。
func TestPlanWavesMissingDependency(t *testing.T) {
	d := desired("e", 1, mkRes("Deployment", "b", nil, rid("Deployment", "ghost")))
	_, err := planWaves("e", d)
	var se *SyncError
	if !errors.As(err, &se) || se.Reason != ReasonDependencyMissing {
		t.Fatalf("err=%v want DependencyMissing", err)
	}
}

// 环 -> DependencyCycle。
func TestPlanWavesCycle(t *testing.T) {
	a := mkRes("Deployment", "a", nil, rid("Deployment", "b"))
	b := mkRes("Deployment", "b", nil, rid("Deployment", "a"))
	_, err := planWaves("e", desired("e", 1, a, b))
	var se *SyncError
	if !errors.As(err, &se) || se.Reason != ReasonDependencyCycle {
		t.Fatalf("err=%v want DependencyCycle", err)
	}
}

func newApplier(t *testing.T, hook *ApplyHook) (*Applier, *state.Store) {
	t.Helper()
	st := state.New(t.TempDir())
	return New(st, nil).WithHook(hook).WithLogger(
		log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)), st
}

// 正常路径：两波成功，观测态与快照落盘。
func TestApplySuccessWaves(t *testing.T) {
	ap, st := newApplier(t, &ApplyHook{})
	d := desired("e", 100,
		mkRes("ConfigMap", "cfg", map[string]any{"k": "v"}),
		mkRes("Deployment", "app", map[string]any{"r": float64(2)}, rid("ConfigMap", "cfg")),
	)
	rev, err := ap.Apply("e", d, "coord-1")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Number != 1 {
		t.Fatalf("revision number = %d, want 1", rev.Number)
	}
	obs, _ := st.Observed("e")
	if len(obs) != 2 || !obs[rid("Deployment", "app")].Managed {
		t.Fatalf("observed after apply = %v", obs)
	}
}

// 第二波健康检查失败：整波放弃，观测态回到上一个稳定态（空），不留中间状态。
func TestApplyHealthFailureRollsBack(t *testing.T) {
	logger := log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
	ap, st := newApplier(t, &ApplyHook{
		FailHealth: map[types.ResourceID]bool{rid("Deployment", "app"): true},
	})
	d := desired("e", 100,
		mkRes("ConfigMap", "cfg", map[string]any{}),
		mkRes("Deployment", "app", map[string]any{}, rid("ConfigMap", "cfg")),
	)
	_, err := ap.Apply("e", d, "coord-1")
	var se *SyncError
	if !errors.As(err, &se) || se.Reason != ReasonHealthCheckFailed || se.Wave != 2 {
		t.Fatalf("err=%v want HealthCheckFailed wave 2", err)
	}
	logger.Printf("CLASSIFIED wave=%d resource=%s reason=%s -> previous stable state kept",
		se.Wave, se.Resource, se.Reason)
	obs, _ := st.Observed("e")
	if len(obs) != 0 {
		t.Fatalf("observed must roll back to empty stable state, got %d resources: %v", len(obs), obs)
	}
	if last, _ := st.LastRevision("e"); last != nil {
		t.Fatalf("no revision may be persisted on failure, got %d", last.Number)
	}
}

// 中途应用出错：同样回滚，且原因可与健康失败区分。
func TestApplyErrorDistinctFromHealth(t *testing.T) {
	ap, _ := newApplier(t, &ApplyHook{
		FailApply: map[types.ResourceID]bool{rid("Deployment", "app"): true},
	})
	d := desired("e", 100,
		mkRes("ConfigMap", "cfg", map[string]any{}),
		mkRes("Deployment", "app", map[string]any{}, rid("ConfigMap", "cfg")),
	)
	_, err := ap.Apply("e", d, "coord-1")
	var se *SyncError
	if !errors.As(err, &se) || se.Reason != ReasonApplyError {
		t.Fatalf("err=%v want ApplyError (distinct from HealthCheckFailed)", err)
	}
}

// 同一期望态重复应用必须幂等：第二次无新增版本、无重复应用动作。
func TestApplyIdempotent(t *testing.T) {
	logger := log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
	ap, st := newApplier(t, &ApplyHook{})
	d := desired("e", 100,
		mkRes("ConfigMap", "cfg", map[string]any{"k": "v"}),
		mkRes("Deployment", "app", map[string]any{"r": float64(2)}, rid("ConfigMap", "cfg")),
	)
	r1, err := ap.Apply("e", d, "coord-1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := ap.Apply("e", d, "coord-1")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Number != r1.Number {
		t.Fatalf("re-apply created new revision %d -> %d", r1.Number, r2.Number)
	}
	logger.Printf("IDEMPOTENT re-apply returned same revision=%d, no duplicate application", r2.Number)
	nums, _ := st.RevisionNumbers("e")
	if len(nums) != 1 {
		t.Fatalf("revision history = %v, want exactly one", nums)
	}
}

// 成功应用 v1 后，v2 失败，观测态必须停留在 v1 并与 v1 快照一致。
func TestApplyFailureKeepsPreviousStableRevision(t *testing.T) {
	ap, st := newApplier(t, &ApplyHook{})
	d1 := desired("e", 100, mkRes("ConfigMap", "cfg", map[string]any{"v": float64(1)}))
	if _, err := ap.Apply("e", d1, "coord-1"); err != nil {
		t.Fatal(err)
	}
	// 第二版引入会失败的资源。
	ap.WithHook(&ApplyHook{FailHealth: map[types.ResourceID]bool{rid("Deployment", "new"): true}})
	d2 := desired("e", 200,
		mkRes("ConfigMap", "cfg", map[string]any{"v": float64(2)}),
		mkRes("Deployment", "new", map[string]any{}, rid("ConfigMap", "cfg")),
	)
	if _, err := ap.Apply("e", d2, "coord-1"); err == nil {
		t.Fatal("expected health failure on v2")
	}
	obs, _ := st.Observed("e")
	got := obs[rid("ConfigMap", "cfg")]
	if got == nil || got.Spec["v"] != float64(1) || got.AppliedRevision != 100 {
		t.Fatalf("observed must stay at v1, got %+v", got)
	}
	last, _ := st.LastRevision("e")
	if last.Number != 1 {
		t.Fatalf("stable revision must remain 1, got %d", last.Number)
	}
}

// 显式回滚：v1 -> v2 成功后回滚到 v1，观测态恢复且历史保留。
func TestRollbackToRevision(t *testing.T) {
	ap, st := newApplier(t, &ApplyHook{})
	d1 := desired("e", 100, mkRes("ConfigMap", "cfg", map[string]any{"v": float64(1)}))
	d2 := desired("e", 200, mkRes("ConfigMap", "cfg", map[string]any{"v": float64(2)}))
	if _, err := ap.Apply("e", d1, "coord-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Apply("e", d2, "coord-1"); err != nil {
		t.Fatal(err)
	}
	rev, err := ap.Rollback("e", 1, "coord-1")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Number != 3 || rev.DesiredRevision != 100 {
		t.Fatalf("rollback revision = %+v", rev)
	}
	obs, _ := st.Observed("e")
	if obs[rid("ConfigMap", "cfg")].Spec["v"] != float64(1) {
		t.Fatalf("observed after rollback = %v", obs)
	}
}

// 回滚到不存在的版本 -> RevisionMissing。
func TestRollbackMissingRevision(t *testing.T) {
	ap, _ := newApplier(t, &ApplyHook{})
	_, err := ap.Rollback("e", 99, "coord-1")
	var se *SyncError
	if !errors.As(err, &se) || se.Reason != ReasonRevisionMissing {
		t.Fatalf("err=%v want RevisionMissing", err)
	}
}

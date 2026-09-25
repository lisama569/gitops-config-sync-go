package coordinator

import (
	"testing"

	"github.com/lisama569/gitops-config-sync-go/internal/drift"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

// 多个协调者顺序接管同一环境、配置连续变化，版本号必须连续且观测态最终一致。
func TestSequentialReconcilesAcrossCoordinators(t *testing.T) {
	logger := testLogger(t)
	root, stateDir := t.TempDir(), t.TempDir()
	writeCfg(t, root, "prod", "10-base.json",
		`{"resources":[{"kind":"ConfigMap","name":"cfg","spec":{"v":1}}]}`)

	mk := func(id string) *Coordinator {
		return New(Config{ID: id, ConfigRoot: root, StateDir: stateDir,
			Policy: drift.PolicyMark, Logger: logger})
	}
	r1, err := mk("c1").Reconcile("prod")
	if err != nil || !r1.Applied || r1.Revision != 1 {
		t.Fatalf("first = %+v err=%v", r1, err)
	}
	// 配置变化后由 c2 接管。
	writeCfg(t, root, "prod", "20-overlay.json",
		`{"resources":[{"kind":"ConfigMap","name":"cfg","spec":{"v":2}}]}`)
	r2, err := mk("c2").Reconcile("prod")
	if err != nil || !r2.Applied || r2.Revision != 2 {
		t.Fatalf("second = %+v err=%v", r2, err)
	}
	// c3 在无变化时空转（幂等）。
	r3, err := mk("c3").Reconcile("prod")
	if err != nil || r3.Applied || r3.Skipped {
		t.Fatalf("third = %+v err=%v, want no-op", r3, err)
	}
	nums, _ := state.New(stateDir).RevisionNumbers("prod")
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 2 {
		t.Fatalf("revision history = %v, want [1 2]", nums)
	}
	obs, _ := state.New(stateDir).Observed("prod")
	cfgID := types.ResourceID{Kind: "ConfigMap", Name: "cfg"}
	if got := obs[cfgID]; got == nil || got.Spec["v"] != float64(2) {
		t.Fatalf("observed not converged to v2: %+v", got)
	}
}

package coordinator

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/lisama569/gitops-config-sync-go/internal/drift"
	"github.com/lisama569/gitops-config-sync-go/internal/render"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
	gsync "github.com/lisama569/gitops-config-sync-go/internal/sync"
	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

type alertRecord struct {
	env     string
	records []types.DriftRecord
}

type capturingAlerter struct {
	mu     stdsync.Mutex
	alerts []alertRecord
}

func (c *capturingAlerter) Alert(env string, records []types.DriftRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alerts = append(c.alerts, alertRecord{env, records})
}

func writeCfg(t *testing.T, root, env, file, content string) {
	t.Helper()
	p := filepath.Join(root, env, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testLogger(t *testing.T) *log.Logger {
	return log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
}

func newTestCoordinator(t *testing.T, id string, policy drift.Policy, hook *gsync.ApplyHook, alerter Alerter) (*Coordinator, string, string) {
	root, stateDir := t.TempDir(), t.TempDir()
	c := New(Config{
		ID: id, ConfigRoot: root, StateDir: stateDir, Policy: policy,
		Hook: hook, Logger: testLogger(t), Alerter: alerter, LockTTL: time.Minute,
	})
	return c, root, stateDir
}

// 端到端：加载 -> 渲染 -> 分波应用，观测态落盘。
func TestReconcileAppliesWaves(t *testing.T) {
	c, root, stateDir := newTestCoordinator(t, "coord-1", drift.PolicyMark, &gsync.ApplyHook{}, nil)
	writeCfg(t, root, "prod", "10-base.json", `{
		"resources":[
			{"kind":"ConfigMap","name":"cfg","spec":{"k":"v"}},
			{"kind":"Deployment","name":"app","spec":{"replicas":2},
			 "dependsOn":[{"kind":"ConfigMap","name":"cfg"}],"healthCheck":"get /healthz"}
		]}`)
	res, err := c.Reconcile("prod")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Revision != 1 {
		t.Fatalf("result = %+v", res)
	}
	obs, err := state.New(stateDir).Observed("prod")
	if err != nil || len(obs) != 2 {
		t.Fatalf("observed = %v err=%v", obs, err)
	}
}

// 覆盖冲突：渲染失败被归类为 FieldConflict，且不触碰观测态。
func TestReconcileFieldConflictClassified(t *testing.T) {
	c, root, stateDir := newTestCoordinator(t, "coord-1", drift.PolicyMark, &gsync.ApplyHook{}, nil)
	writeCfg(t, root, "prod", "10-a.json", `[{"kind":"ConfigMap","name":"x","spec":{"k":"v1"}}]`)
	writeCfg(t, root, "prod", "10-b.json", `[{"kind":"ConfigMap","name":"x","spec":{"k":"v2"}}]`)
	_, err := c.Reconcile("prod")
	var re *render.RenderError
	if !errors.As(err, &re) || re.Reason != render.ReasonFieldConflict {
		t.Fatalf("err=%v want FieldConflict", err)
	}
	obs, _ := state.New(stateDir).Observed("prod")
	if len(obs) != 0 {
		t.Fatalf("nothing may be applied on render failure, got %v", obs)
	}
}

// 并发重复协调：N 个协调者同时处理同一环境，恰有一个应用，其余 Skipped。
func TestConcurrentReconcileOnlyOneApplies(t *testing.T) {
	logger := testLogger(t)
	root, stateDir := t.TempDir(), t.TempDir()
	writeCfg(t, root, "prod", "10-base.json", `{
		"resources":[{"kind":"ConfigMap","name":"cfg","spec":{"k":"v"}}]}`)

	const n = 8
	var wg stdsync.WaitGroup
	results := make([]*ReconcileResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := New(Config{
				ID: fmt.Sprintf("coord-%d", i), ConfigRoot: root, StateDir: stateDir,
				Policy: drift.PolicyMark, Logger: logger, LockTTL: time.Minute,
			})
			<-start
			results[i], errs[i] = c.Reconcile("prod")
		}(i)
	}
	close(start)
	wg.Wait()

	applied, skipped, noop := 0, 0, 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("coord %d: %v", i, errs[i])
		}
		switch {
		case r.Applied:
			applied++
		case r.Skipped:
			// 争锁期间被有效锁挡住。
			skipped++
		default:
			// 持锁者释放后才拿到锁：此时观测态已收敛，协调必须幂等空转。
			noop++
		}
	}
	logger.Printf("CONCURRENCY RESULT applied=%d lockSkipped=%d idempotentNoop=%d (total=%d)",
		applied, skipped, noop, n)
	if applied != 1 {
		t.Fatalf("want exactly 1 apply (no duplicate application), got %d", applied)
	}
	if skipped+noop != n-1 {
		t.Fatalf("all other coordinators must skip or no-op, got skipped=%d noop=%d", skipped, noop)
	}
	nums, _ := state.New(stateDir).RevisionNumbers("prod")
	if len(nums) != 1 {
		t.Fatalf("revision history = %v, want exactly 1 (no duplicate apply)", nums)
	}
}

// 重复协调同一期望态幂等：第二次无新增版本、无 applied。
func TestReconcileIdempotent(t *testing.T) {
	c, root, stateDir := newTestCoordinator(t, "coord-1", drift.PolicyMark, &gsync.ApplyHook{}, nil)
	writeCfg(t, root, "prod", "10-base.json",
		`{"resources":[{"kind":"ConfigMap","name":"cfg","spec":{"k":"v"}}]}`)
	r1, err := c.Reconcile("prod")
	if err != nil || !r1.Applied {
		t.Fatal(err)
	}
	r2, err := c.Reconcile("prod")
	if err != nil || r2.Applied || r2.Skipped {
		t.Fatalf("second reconcile = %+v err=%v, want no-op", r2, err)
	}
	nums, _ := state.New(stateDir).RevisionNumbers("prod")
	if len(nums) != 1 {
		t.Fatalf("history = %v, want one revision", nums)
	}
}

// 人工改动：观测态被外部改成与期望不一致，协调者只标记+告警，不静默纠正。
func TestManualDriftMarkedAndAlertedNotCorrected(t *testing.T) {
	logger := testLogger(t)
	root, stateDir := t.TempDir(), t.TempDir()
	writeCfg(t, root, "prod", "10-base.json",
		`{"resources":[{"kind":"Deployment","name":"app","spec":{"replicas":3}}]}`)
	st := state.New(stateDir)

	// 第一次协调建立稳定态。
	c1 := New(Config{ID: "coord-1", ConfigRoot: root, StateDir: stateDir,
		Policy: drift.PolicyAlertAndMark, Logger: logger, LockTTL: time.Minute})
	if _, err := c1.Reconcile("prod"); err != nil {
		t.Fatal(err)
	}

	// 模拟人工 kubectl edit：直接改观测态文件，把 replicas 改成 5。
	obs, _ := st.Observed("prod")
	target := obs[types.ResourceID{Kind: "Deployment", Name: "app"}]
	target.Spec["replicas"] = float64(5)
	target.AppliedRevision = 999 // 人工改动后版本标记也被破坏
	if err := st.SaveObserved("prod", obs); err != nil {
		t.Fatal(err)
	}

	alerter := &capturingAlerter{}
	c2 := New(Config{ID: "coord-2", ConfigRoot: root, StateDir: stateDir,
		Policy: drift.PolicyAlertAndMark, Logger: logger, Alerter: alerter, LockTTL: time.Minute})
	res, err := c2.Reconcile("prod")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Drifted || res.Applied {
		t.Fatalf("manual drift must be flagged and NOT auto-corrected: %+v", res)
	}
	if len(alerter.alerts) != 1 || len(alerter.alerts[0].records) != 1 {
		t.Fatalf("alerts = %+v", alerter.alerts)
	}
	rec := alerter.alerts[0].records[0]
	if rec.Kind != types.DriftSpecChanged || rec.Diffs[0].Path != "replicas" ||
		rec.Diffs[0].Expected != float64(3) || rec.Diffs[0].Observed != float64(5) {
		t.Fatalf("alert basis = %+v", rec)
	}
	logger.Printf("DRIFT POLICY manual change kept as-is (replicas=5); marked+alerted, basis: %+v", rec.Diffs)

	// 观测态仍是人工值，没有被静默纠正。
	obs2, _ := st.Observed("prod")
	if obs2[target.ID()].Spec["replicas"] != float64(5) {
		t.Fatal("observed spec was silently corrected")
	}
	if !obs2[target.ID()].Drifted || obs2[target.ID()].DriftMark != types.DriftSpecChanged {
		t.Fatalf("drift marker not written: %+v", obs2[target.ID()])
	}
}

// 依赖乱序场景（资源依赖未就绪即无法成波）：缺失依赖被归类为 DependencyMissing。
func TestReconcileDependencyMissingClassified(t *testing.T) {
	c, root, _ := newTestCoordinator(t, "coord-1", drift.PolicyMark, &gsync.ApplyHook{}, nil)
	writeCfg(t, root, "prod", "10-base.json", `{
		"resources":[{"kind":"Deployment","name":"app","spec":{},
			"dependsOn":[{"kind":"ConfigMap","name":"ghost"}]}]}`)
	_, err := c.Reconcile("prod")
	var se *gsync.SyncError
	if !errors.As(err, &se) || se.Reason != gsync.ReasonDependencyMissing {
		t.Fatalf("err=%v want DependencyMissing", err)
	}
}

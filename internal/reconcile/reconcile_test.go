package reconcile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/gitops-config-sync-go/internal/cluster"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
)

func buildDesired(t *testing.T, env string, rs ...state.Resource) state.DesiredState {
	t.Helper()
	m := map[string]state.Resource{}
	for _, r := range rs {
		m[r.Name] = r
	}
	ds, err := state.Render(env, m)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestConcurrentReconcileAppliesOnce(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyMark)
	ds := buildDesired(t, "prod",
		state.Resource{Name: "db", Replicas: 1},
		state.Resource{Name: "web", DependsOn: []string{"db"}},
	)
	t.Logf("输入: env=%s hash=%s 资源=[db web->db], 并发协调者=8", ds.Env, ds.Hash)

	const n = 8
	results := make([]Result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := r.Reconcile(ds)
			if err != nil {
				t.Errorf("协调失败: %v", err)
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	applied := 0
	for _, res := range results {
		if res.Status == StatusApplied {
			applied++
		} else if res.Status != StatusNoop {
			t.Errorf("并发结果只能是 Applied/Noop, got %s", res.Status)
		}
	}
	t.Logf("并发结果: Applied=%d Noop=%d 集群写入次数=%d", applied, n-applied, c.ApplyCount)
	if applied != 1 {
		t.Fatalf("同一期望态并发协调只能应用一次, got %d", applied)
	}
	if c.ApplyCount != 2 { // db + web 各一次
		t.Fatalf("重复应用检测: 集群写入应恰为 2 次, got %d", c.ApplyCount)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyMark)
	ds := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 1})

	res1, err := r.Reconcile(ds)
	if err != nil {
		t.Fatal(err)
	}
	if res1 := res1Status(res1); res1 != StatusApplied {
		t.Fatalf("首次应为 Applied, got %s", res1)
	}
	writes := c.ApplyCount

	res2, err := r.Reconcile(ds)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("重复协调同一期望态: 第一次=%s 第二次=%s 写入次数=%d", res1.Status, res2.Status, c.ApplyCount)
	if res2.Status != StatusNoop {
		t.Fatalf("重复应用必须幂等(Noop), got %s", res2.Status)
	}
	if c.ApplyCount != writes {
		t.Fatalf("幂等协调不应再写集群: %d -> %d", writes, c.ApplyCount)
	}
}

func res1Status(r Result) Status { return r.Status }

func TestManualDriftMarkedNotCorrected(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyMark)
	ds := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 2})

	if _, err := r.Reconcile(ds); err != nil {
		t.Fatal(err)
	}
	// 人工改动集群。
	tampered := state.Resource{Name: "db", Replicas: 9, Config: map[string]string{}}
	if err := c.Apply(tampered); err != nil {
		t.Fatal(err)
	}

	res, err := r.Reconcile(ds)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Drift.Entries {
		t.Logf("漂移判定依据: resource=%s kind=%s expected=%s observed=%s",
			e.Resource, e.Kind, e.Expected, e.Observed)
	}
	if res.Status != StatusDriftMarked {
		t.Fatalf("人工改动应被标记, got %s", res.Status)
	}
	// 策略为 Mark: 不得静默纠正, 观测态应保持人工改动后的值。
	got, ok, _ := c.Get("db")
	if !ok || got.Replicas != 9 {
		t.Fatalf("Mark 策略下不得静默纠正, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "_drift_prod.json")); err != nil {
		t.Fatalf("应落盘漂移告警标记: %v", err)
	}
	t.Logf("Mark 策略验证通过: 漂移已标记, 观测态保持人工值 replicas=%d", got.Replicas)
}

func TestManualDriftAlertReturnsError(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyAlert)
	ds := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 2})

	if _, err := r.Reconcile(ds); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(state.Resource{Name: "db", Replicas: 7, Config: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ds)
	if !errors.Is(err, ErrDriftAlert) {
		t.Fatalf("Alert 策略应返回 ErrDriftAlert, got %v", err)
	}
	if res.Status != StatusDriftMarked {
		t.Fatalf("got %s", res.Status)
	}
	t.Logf("Alert 策略: err=%v status=%s", err, res.Status)
}

func TestReconcileRollbackKeepsStableState(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyMark)

	// 先建立一个稳定版本。
	v1 := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 1})
	if _, err := r.Reconcile(v1); err != nil {
		t.Fatal(err)
	}
	stable, _ := c.Snapshot()

	// 新版本引入不健康的 web, 同步应失败并回滚。
	if err := c.SetHealthy("web", false); err != nil {
		t.Fatal(err)
	}
	v2 := buildDesired(t, "prod",
		state.Resource{Name: "db", Replicas: 1},
		state.Resource{Name: "web", DependsOn: []string{"db"}},
	)
	res, err := r.Reconcile(v2)
	if err == nil {
		t.Fatal("应同步失败")
	}
	if res.Status != StatusRolledBack {
		t.Fatalf("失败后状态应为 RolledBack, got %s", res.Status)
	}
	after, _ := c.Snapshot()
	if len(after) != len(stable) {
		t.Fatalf("回滚后观测态应等于上一稳定态: stable=%d after=%d", len(stable), len(after))
	}
	t.Logf("同步失败已回滚: err=%v 观测态资源数=%d (与上一稳定态一致)", err, len(after))
}

func TestMultiInstanceReconcileAppliesOnce(t *testing.T) {
	c := cluster.New(t.TempDir())
	ds := buildDesired(t, "prod",
		state.Resource{Name: "db", Replicas: 1},
		state.Resource{Name: "web", DependsOn: []string{"db"}},
	)
	// 多个互相独立的协调实例（各自持有不同的锁标识）同时处理同一环境。
	const n = 6
	instances := make([]*Reconciler, n)
	for i := range instances {
		instances[i] = New(c, PolicyMark)
	}
	t.Logf("输入: env=%s hash=%s 资源=[db web->db], 独立协调实例=%d", ds.Env, ds.Hash, n)

	results := make([]Result, n)
	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func(i int, inst *Reconciler) {
			defer wg.Done()
			res, err := inst.Reconcile(ds)
			if err != nil {
				t.Errorf("协调失败: %v", err)
			}
			results[i] = res
		}(i, inst)
	}
	wg.Wait()

	applied := 0
	for _, res := range results {
		if res.Status == StatusApplied {
			applied++
		} else if res.Status != StatusNoop {
			t.Errorf("多实例并发结果只能是 Applied/Noop, got %s", res.Status)
		}
	}
	t.Logf("多实例并发结果: Applied=%d Noop=%d 集群写入次数=%d", applied, n-applied, c.ApplyCount)
	if applied != 1 {
		t.Fatalf("同一期望态只能被真正落地一次, got %d", applied)
	}
	if c.ApplyCount != 2 {
		t.Fatalf("重复应用/互相覆盖检测: 集群写入应恰为 2 次, got %d", c.ApplyCount)
	}
	// 锁必须已释放，不能残留。
	if _, err := os.Stat(filepath.Join(c.Dir, "_lock_prod")); !os.IsNotExist(err) {
		t.Fatalf("协调结束后锁文件应已释放")
	}
}

func TestStaleLockTakeoverAfterCrash(t *testing.T) {
	c := cluster.New(t.TempDir())
	ds := buildDesired(t, "prod",
		state.Resource{Name: "db", Replicas: 1},
		state.Resource{Name: "web", DependsOn: []string{"db"}},
	)
	// 模拟某实例在协调中途异常退出：锁文件残留（已过期）、
	// 观测态只落地了一半（db 已写、web 未写）、manifest 未写。
	crashed := New(c, PolicyMark)
	if err := crashed.envFileLock("prod").Acquire(); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(state.Resource{Name: "db", Replicas: 1, Config: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(c.Dir, "_lock_prod"), stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Logf("输入: 崩溃实例残留过期锁 + 部分观测态(仅 db), 期望态资源=[db web->db]")

	// 后续实例应能接管并把状态收敛到位。
	successor := New(c, PolicyMark)
	res, err := successor.Reconcile(ds)
	if err != nil {
		t.Fatalf("接管实例协调失败: %v", err)
	}
	if res.Status != StatusApplied {
		t.Fatalf("接管后应完成应用, got %s", res.Status)
	}
	for _, name := range []string{"db", "web"} {
		if _, ok, _ := c.Get(name); !ok {
			t.Fatalf("接管后资源 %s 应已收敛到期望态", name)
		}
	}
	// 收敛后再协调应为幂等 Noop。
	res2, err := successor.Reconcile(ds)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("接管收敛完成: 第一次=%s 第二次=%s 集群写入次数=%d", res.Status, res2.Status, c.ApplyCount)
	if res2.Status != StatusNoop {
		t.Fatalf("收敛后重复协调应为 Noop, got %s", res2.Status)
	}
}

func TestFreshLockBlocksUntilTimeout(t *testing.T) {
	c := cluster.New(t.TempDir())
	ds := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 1})

	holder := New(c, PolicyMark)
	if err := holder.envFileLock("prod").Acquire(); err != nil {
		t.Fatal(err)
	}
	defer holder.envFileLock("prod").Release()

	waiter := New(c, PolicyMark)
	waiter.LockTTL = time.Hour // 锁未过期，不可接管
	waiter.LockTimeout = 150 * time.Millisecond
	waiter.LockPoll = 10 * time.Millisecond
	_, err := waiter.Reconcile(ds)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("新鲜锁未被释放时应等待并超时, got %v", err)
	}
	t.Logf("未过期锁拒绝接管: err=%v", err)
}

func TestPruneResourceRemovedFromDesired(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyMark)

	v1 := buildDesired(t, "prod",
		state.Resource{Name: "db", Replicas: 1},
		state.Resource{Name: "cache", Replicas: 1},
	)
	if _, err := r.Reconcile(v1); err != nil {
		t.Fatal(err)
	}
	t.Logf("输入: v1 资源=[db cache] 已落地")

	// v2 从期望态去掉 cache。
	v2 := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 1})
	res, err := r.Reconcile(v2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusApplied {
		t.Fatalf("期望态变化应触发应用, got %s", res.Status)
	}
	if _, ok, _ := c.Get("cache"); ok {
		t.Fatal("从期望态移除的资源应被回收")
	}
	if _, ok, _ := c.Get("db"); !ok {
		t.Fatal("仍在期望态中的资源应保留")
	}
	// 回收后再协调应幂等。
	res2, err := r.Reconcile(v2)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("回收验证: v2 协调=%s 重复协调=%s 观测态=%v", res.Status, res2.Status, mustList(t, c))
	if res2.Status != StatusNoop {
		t.Fatalf("回收后重复协调应为 Noop, got %s", res2.Status)
	}
}

func TestExtraResourceMarkedNotRemoved(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyMark)
	ds := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 1})
	if _, err := r.Reconcile(ds); err != nil {
		t.Fatal(err)
	}
	// 有人绕开流程往集群里加了期望态之外的资源。
	if err := c.Apply(state.Resource{Name: "stray", Replicas: 1, Config: map[string]string{}}); err != nil {
		t.Fatal(err)
	}

	res, err := r.Reconcile(ds)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Drift.Entries {
		t.Logf("漂移判定依据: resource=%s kind=%s expected=%s observed=%s",
			e.Resource, e.Kind, e.Expected, e.Observed)
	}
	if res.Status != StatusDriftMarked {
		t.Fatalf("多出的资源应被标记为漂移, got %s", res.Status)
	}
	found := false
	for _, e := range res.Drift.Entries {
		if e.Resource == "stray" && e.Kind == "Extra" {
			found = true
		}
	}
	if !found {
		t.Fatalf("漂移报告应包含 stray 的 Extra 条目: %+v", res.Drift.Entries)
	}
	// Mark 策略: 不静默清理, 多余资源保持原样。
	if _, ok, _ := c.Get("stray"); !ok {
		t.Fatal("Mark 策略下不得静默删除多余资源")
	}
	t.Logf("Mark 策略验证通过: Extra 已标记, stray 保留")
}

func TestExtraResourceCorrected(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := New(c, PolicyCorrect)
	ds := buildDesired(t, "prod", state.Resource{Name: "db", Replicas: 1})
	if _, err := r.Reconcile(ds); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(state.Resource{Name: "stray", Replicas: 1, Config: map[string]string{}}); err != nil {
		t.Fatal(err)
	}

	res, err := r.Reconcile(ds)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Correct 策略: status=%s 漂移条目=%+v", res.Status, res.Drift.Entries)
	if _, ok, _ := c.Get("stray"); ok {
		t.Fatal("Correct 策略下多余资源应被清理")
	}
	// 收敛后幂等。
	res2, err := r.Reconcile(ds)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != StatusNoop {
		t.Fatalf("收敛后重复协调应为 Noop, got %s", res2.Status)
	}
}

func mustList(t *testing.T, c *cluster.Cluster) []string {
	t.Helper()
	names, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	return names
}

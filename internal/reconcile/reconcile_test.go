package reconcile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

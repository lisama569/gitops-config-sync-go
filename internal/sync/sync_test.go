package sync

import (
	"errors"
	"testing"

	"github.com/lisama569/gitops-config-sync-go/internal/cluster"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
)

func desired(env string, resources ...state.Resource) state.DesiredState {
	ds, err := state.Render(env, map[string]state.Resource{})
	if err != nil {
		panic(err)
	}
	m := map[string]state.Resource{}
	for _, r := range resources {
		m[r.Name] = r
	}
	ds, err = state.Render(env, m)
	if err != nil {
		panic(err)
	}
	return ds
}

func TestPlanWavesRespectDependencies(t *testing.T) {
	// 故意乱序给出资源。
	ds := desired("prod",
		state.Resource{Name: "web", DependsOn: []string{"db"}},
		state.Resource{Name: "net"},
		state.Resource{Name: "db", DependsOn: []string{"net"}},
	)
	waves, err := Plan(ds.Resources)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("输入资源(乱序): web->db, net, db->net")
	for i, w := range waves {
		t.Logf("波次 %d: %v", i, w)
	}
	want := [][]string{{"net"}, {"db"}, {"web"}}
	if len(waves) != len(want) {
		t.Fatalf("波次数不符: got %v want %v", waves, want)
	}
	for i := range want {
		if len(waves[i]) != 1 || waves[i][0] != want[i][0] {
			t.Fatalf("波次 %d 不符: got %v want %v", i, waves[i], want[i])
		}
	}
}

func TestPlanDependencyCycle(t *testing.T) {
	ds := desired("prod",
		state.Resource{Name: "a", DependsOn: []string{"b"}},
		state.Resource{Name: "b", DependsOn: []string{"a"}},
	)
	_, err := Plan(ds.Resources)
	var fe *FailError
	if !errors.As(err, &fe) {
		t.Fatalf("应返回 *FailError, got %v", err)
	}
	if fe.Reason != ReasonDependencyCycle {
		t.Fatalf("失败原因应为 %s, got %s", ReasonDependencyCycle, fe.Reason)
	}
	t.Logf("依赖环被归类: %v", fe)
}

func TestApplyHealthFailureRollsBack(t *testing.T) {
	c := cluster.New(t.TempDir())
	// 预置一个稳定资源，作为"上一个稳定状态"。
	stable := state.Resource{Name: "stable", Replicas: 1, Config: map[string]string{"k": "v"}}
	if err := c.Apply(stable); err != nil {
		t.Fatal(err)
	}
	before, _ := c.Snapshot()

	// web 依赖 db；db 应用后健康检查将失败。
	if err := c.SetHealthy("db", false); err != nil {
		t.Fatal(err)
	}
	ds := desired("prod",
		state.Resource{Name: "db", Replicas: 1},
		state.Resource{Name: "web", DependsOn: []string{"db"}},
	)
	t.Logf("输入: db(将被标记不健康), web->db; 预置稳定资源 stable")

	res, err := Apply(c, ds)
	var fe *FailError
	if !errors.As(err, &fe) {
		t.Fatalf("应返回 *FailError, got %v", err)
	}
	if fe.Reason != ReasonHealthCheckFailed {
		t.Fatalf("失败原因应为 %s, got %s", ReasonHealthCheckFailed, fe.Reason)
	}
	t.Logf("同步失败(已回滚): %v; 失败前已应用: %v", fe, res.Applied)

	after, _ := c.Snapshot()
	if len(after) != len(before) {
		t.Fatalf("回滚后观测态应等于同步前: before=%v after=%v", keys(before), keys(after))
	}
	for name, r := range before {
		if after[name].Name != r.Name {
			t.Fatalf("回滚后资源 %s 不一致", name)
		}
	}
	if _, ok, _ := c.Get("db"); ok {
		t.Fatal("回滚后不应残留部分生效的 db")
	}
	t.Logf("回滚验证通过: 观测态=%v, 与上一稳定状态一致", keys(after))
}

func TestApplyDependencyNotReady(t *testing.T) {
	c := cluster.New(t.TempDir())
	// web 依赖未定义的 db。
	ds := desired("prod", state.Resource{Name: "web", DependsOn: []string{"db"}})
	_, err := Apply(c, ds)
	var fe *FailError
	if !errors.As(err, &fe) {
		t.Fatalf("应返回 *FailError, got %v", err)
	}
	if fe.Reason != ReasonDependencyNotReady {
		t.Fatalf("失败原因应为 %s, got %s", ReasonDependencyNotReady, fe.Reason)
	}
	t.Logf("依赖未就绪被归类: %v", fe)
}

func TestApplySuccess(t *testing.T) {
	c := cluster.New(t.TempDir())
	ds := desired("prod",
		state.Resource{Name: "web", DependsOn: []string{"db"}},
		state.Resource{Name: "db"},
	)
	res, err := Apply(c, ds)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("波次: %v 已应用: %v", res.Waves, res.Applied)
	for _, name := range []string{"db", "web"} {
		if _, ok, _ := c.Get(name); !ok {
			t.Fatalf("资源 %s 应已写入集群", name)
		}
	}
}

func keys(m map[string]state.Resource) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestApplyPrunesRemovedResources(t *testing.T) {
	c := cluster.New(t.TempDir())
	// 上一版期望态包含 db 与 cache。
	v1 := desired("prod",
		state.Resource{Name: "db", Replicas: 1},
		state.Resource{Name: "cache", Replicas: 1},
	)
	if _, err := Apply(c, v1); err != nil {
		t.Fatal(err)
	}

	// 新版期望态移除了 cache。
	v2 := desired("prod", state.Resource{Name: "db", Replicas: 2})
	t.Logf("输入: 观测态=[db cache], 新期望态=[db], cache 从期望态移除")
	res, err := Apply(c, v2)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("波次: %v 已应用: %v 已回收: %v", res.Waves, res.Applied, res.Pruned)
	if len(res.Pruned) != 1 || res.Pruned[0] != "cache" {
		t.Fatalf("cache 应被回收, got Pruned=%v", res.Pruned)
	}
	if _, ok, _ := c.Get("cache"); ok {
		t.Fatal("期望态移除后, 观测态中的 cache 必须被清理")
	}
	if _, ok, _ := c.Get("db"); !ok {
		t.Fatal("期望态保留的 db 不应被误删")
	}
}

func TestApplyFailureKeepsPrunableResources(t *testing.T) {
	c := cluster.New(t.TempDir())
	v1 := desired("prod", state.Resource{Name: "db", Replicas: 1})
	if _, err := Apply(c, v1); err != nil {
		t.Fatal(err)
	}
	// 新版本移除 db、引入不健康的 web, 同步失败应回滚且 db 不被清理。
	if err := c.SetHealthy("web", false); err != nil {
		t.Fatal(err)
	}
	v2 := desired("prod", state.Resource{Name: "web", Replicas: 1})
	t.Logf("输入: 观测态=[db], 新期望态=[web(不健康)]; db 待回收但同步将失败")
	_, err := Apply(c, v2)
	var fe *FailError
	if !errors.As(err, &fe) {
		t.Fatalf("应同步失败, got %v", err)
	}
	if _, ok, _ := c.Get("db"); !ok {
		t.Fatal("同步失败回滚后, 待回收的 db 必须仍然存在")
	}
	t.Logf("同步失败已回滚: %v; db 保留, 未发生部分回收", fe)
}

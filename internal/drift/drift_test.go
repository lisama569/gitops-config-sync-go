package drift

import (
	"log"
	"os"
	"testing"

	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

func rid(k, n string) types.ResourceID { return types.ResourceID{Kind: k, Name: n} }

func wanted(res ...*types.Resource) *types.Rendered {
	m := map[types.ResourceID]*types.Resource{}
	for _, r := range res {
		m[r.ID()] = r
	}
	return &types.Rendered{Env: "e", Revision: 1, Resources: m}
}

func observedOf(res ...*types.ObservedResource) map[types.ResourceID]*types.ObservedResource {
	m := map[types.ResourceID]*types.ObservedResource{}
	for _, r := range res {
		m[r.ID()] = r
	}
	return m
}

// 人工改动：观测态 spec 与期望不一致，返回字段级判定依据。
func TestDetectSpecChangedWithFieldBasis(t *testing.T) {
	logger := log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
	desired := wanted(&types.Resource{Kind: "Deployment", Name: "a",
		Spec: map[string]any{"replicas": float64(3), "image": "app:v2",
			"server": map[string]any{"port": float64(8080)}}})
	obs := observedOf(&types.ObservedResource{
		Resource: types.Resource{Kind: "Deployment", Name: "a",
			Spec: map[string]any{"replicas": float64(5), "image": "app:v2",
				"server": map[string]any{"port": float64(9090)}}},
		// 协调者最后写入的是 replicas=3/port=8080，当前值偏离 -> 人工改动。
		LastAppliedSpec: map[string]any{"replicas": float64(3), "image": "app:v2",
			"server": map[string]any{"port": float64(8080)}},
		Managed: true,
	})
	recs := New(PolicyMark).Detect(desired, obs)
	if len(recs) != 1 || recs[0].Kind != types.DriftSpecChanged {
		t.Fatalf("recs = %+v", recs)
	}
	paths := map[string]types.FieldDiff{}
	for _, d := range recs[0].Diffs {
		paths[d.Path] = d
	}
	logger.Printf("DRIFT BASIS resource=%s diffs=%+v", recs[0].ID, recs[0].Diffs)
	if d, ok := paths["replicas"]; !ok || d.Expected != float64(3) || d.Observed != float64(5) {
		t.Fatalf("replicas diff = %+v", d)
	}
	if d, ok := paths["server.port"]; !ok || d.Expected != float64(8080) || d.Observed != float64(9090) {
		t.Fatalf("server.port diff = %+v", d)
	}
	if _, noisy := paths["image"]; noisy {
		t.Fatalf("unchanged field must not be reported: %+v", paths)
	}
}

// 缺失与额外资源。
func TestDetectMissingAndExtra(t *testing.T) {
	desired := wanted(&types.Resource{Kind: "Deployment", Name: "a", Spec: map[string]any{}})
	obs := observedOf(
		&types.ObservedResource{Resource: types.Resource{Kind: "Service", Name: "x", Spec: map[string]any{}}, Managed: true},
	)
	recs := New(PolicyMark).Detect(desired, obs)
	kinds := map[types.ResourceID]types.DriftKind{}
	for _, r := range recs {
		kinds[r.ID] = r.Kind
	}
	if kinds[rid("Deployment", "a")] != types.DriftMissing {
		t.Fatalf("missing not detected: %+v", kinds)
	}
	if kinds[rid("Service", "x")] != types.DriftExtra {
		t.Fatalf("extra not detected: %+v", kinds)
	}
}

// 完全一致 -> 无漂移（重复协调幂等的基础）。
func TestDetectNoDrift(t *testing.T) {
	spec := map[string]any{"a": float64(1), "nested": map[string]any{"b": "x"}}
	desired := wanted(&types.Resource{Kind: "K", Name: "n", Spec: spec})
	obs := observedOf(&types.ObservedResource{
		Resource: types.Resource{Kind: "K", Name: "n",
			Spec: map[string]any{"a": float64(1), "nested": map[string]any{"b": "x"}}}, Managed: true})
	if recs := New(PolicyMark).Detect(desired, obs); HasDrift(recs) {
		t.Fatalf("unexpected drift: %+v", recs)
	}
}

// 未托管资源（非本系统创建）不应被当作 Extra 报警。
func TestDetectIgnoresUnmanagedExtra(t *testing.T) {
	desired := wanted(&types.Resource{Kind: "K", Name: "a", Spec: map[string]any{}})
	obs := observedOf(&types.ObservedResource{
		Resource: types.Resource{Kind: "K", Name: "foreign", Spec: map[string]any{}}, Managed: false})
	recs := New(PolicyMark).Detect(desired, obs)
	for _, r := range recs {
		if r.Kind == types.DriftExtra {
			t.Fatalf("unmanaged resource must not be flagged Extra: %+v", r)
		}
	}
}

// 合法配置升级：观测态 == 上次写入快照、但新期望变了 -> Outdated（允许收敛，不是人工漂移）。
func TestDetectOutdatedConfigUpgrade(t *testing.T) {
	logger := log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
	desired := wanted(&types.Resource{Kind: "Deployment", Name: "a",
		Spec: map[string]any{"replicas": float64(5)}})
	oldSpec := map[string]any{"replicas": float64(3)}
	obs := observedOf(&types.ObservedResource{
		Resource:        types.Resource{Kind: "Deployment", Name: "a", Spec: oldSpec},
		AppliedRevision: 100,
		LastAppliedSpec: oldSpec, // 当前观测值正是我们上次写入的旧版本
		Managed:         true,
	})
	desired.Revision = 200
	recs := New(PolicyMark).Detect(desired, obs)
	if len(recs) != 1 || recs[0].Kind != types.DriftOutdated {
		t.Fatalf("recs = %+v, want Outdated", recs)
	}
	logger.Printf("DRIFT BASIS config upgrade classified=%s diffs=%+v (allowed to converge)",
		recs[0].Kind, recs[0].Diffs)
}

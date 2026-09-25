package render

import (
	"errors"
	"log"
	"os"
	"testing"

	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

func testLogger(t *testing.T) *log.Logger {
	return log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
}

func src(name string, prio int, res map[types.ResourceID]*types.Resource) types.Source {
	return types.Source{Name: name, Priority: prio, Resources: res}
}

func res(kind, name string, spec map[string]any) *types.Resource {
	return &types.Resource{Kind: kind, Name: name, Spec: spec}
}

func id(kind, name string) types.ResourceID {
	return types.ResourceID{Kind: kind, Name: name}
}

// 多来源按优先级深合并，高优先级覆盖低优先级标量。
func TestRenderPriorityOverride(t *testing.T) {
	logger := testLogger(t)
	r := New()
	sources := []types.Source{
		src("base", 10, map[types.ResourceID]*types.Resource{
			id("Deployment", "api"): res("Deployment", "api", map[string]any{
				"replicas": float64(1),
				"server":   map[string]any{"host": "0.0.0.0", "port": float64(8080)},
			}),
		}),
		src("prod", 20, map[types.ResourceID]*types.Resource{
			id("Deployment", "api"): res("Deployment", "api", map[string]any{
				"replicas": float64(3),
				"server":   map[string]any{"port": float64(9090)},
			}),
		}),
	}
	logger.Printf("INPUT sources: base(prio=10) replicas=1 port=8080; prod(prio=20) replicas=3 port=9090")
	out, err := r.Render("prod", sources)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := out.Resources[id("Deployment", "api")]
	if got.Spec["replicas"] != float64(3) {
		t.Fatalf("replicas = %v, want 3 (higher priority wins)", got.Spec["replicas"])
	}
	server := got.Spec["server"].(map[string]any)
	if server["host"] != "0.0.0.0" || server["port"] != float64(9090) {
		t.Fatalf("deep merge = %v, want host kept and port overridden", server)
	}
	logger.Printf("OUTPUT merged replicas=%v server=%v origin=%v",
		got.Spec["replicas"], server, got.SourceOrigin)
}

// 同优先级来源以不同值覆盖同一字段 -> FieldConflict，绝不静默取其一。
func TestRenderFieldConflict(t *testing.T) {
	logger := testLogger(t)
	r := New()
	sources := []types.Source{
		src("a", 10, map[types.ResourceID]*types.Resource{
			id("ConfigMap", "x"): res("ConfigMap", "x", map[string]any{"key": "v1"}),
		}),
		src("b", 10, map[types.ResourceID]*types.Resource{
			id("ConfigMap", "x"): res("ConfigMap", "x", map[string]any{"key": "v2"}),
		}),
	}
	logger.Printf("INPUT equal-priority conflicting sources a.key=v1 b.key=v2")
	_, err := r.Render("env", sources)
	if err == nil {
		t.Fatal("want FieldConflict error, got nil")
	}
	var re *RenderError
	if !errors.As(err, &re) || re.Reason != ReasonFieldConflict {
		t.Fatalf("err = %v, want FieldConflict", err)
	}
	if re.Source != "a" || re.OtherSource != "b" || re.Path != "key" {
		t.Fatalf("conflict context = %+v", re)
	}
	if re.Values != [2]any{"v1", "v2"} {
		t.Fatalf("conflict values = %v", re.Values)
	}
	logger.Printf("CLASSIFIED rejection reason=%s path=%s values=%v (distinct from other failure kinds)",
		re.Reason, re.Path, re.Values)
}

// map 与标量在同一路径相遇且同优先级 -> 同样按 FieldConflict 拒绝（形状冲突）。
func TestRenderShapeConflict(t *testing.T) {
	r := New()
	sources := []types.Source{
		src("a", 10, map[types.ResourceID]*types.Resource{
			id("ConfigMap", "x"): res("ConfigMap", "x", map[string]any{"k": map[string]any{"nested": "1"}}),
		}),
		src("b", 10, map[types.ResourceID]*types.Resource{
			id("ConfigMap", "x"): res("ConfigMap", "x", map[string]any{"k": "scalar"}),
		}),
	}
	_, err := r.Render("env", sources)
	var re *RenderError
	if !errors.As(err, &re) || re.Reason != ReasonFieldConflict || re.Path != "k" {
		t.Fatalf("err = %v, want FieldConflict at path k", err)
	}
}

// 同优先级来源声明相同值 -> 幂等重复，允许通过。
func TestRenderSameValueIdempotent(t *testing.T) {
	r := New()
	sources := []types.Source{
		src("a", 10, map[types.ResourceID]*types.Resource{
			id("ConfigMap", "x"): res("ConfigMap", "x", map[string]any{"key": "v1"}),
		}),
		src("b", 10, map[types.ResourceID]*types.Resource{
			id("ConfigMap", "x"): res("ConfigMap", "x", map[string]any{"key": "v1"}),
		}),
	}
	out, err := r.Render("env", sources)
	if err != nil {
		t.Fatalf("identical values must not conflict: %v", err)
	}
	if out.Resources[id("ConfigMap", "x")].Spec["key"] != "v1" {
		t.Fatal("value not preserved")
	}
}

// 字段类型不合法 -> InvalidType，且原因可与冲突区分。
func TestRenderInvalidType(t *testing.T) {
	logger := testLogger(t)
	r := New()
	sources := []types.Source{
		src("a", 10, map[types.ResourceID]*types.Resource{
			id("ConfigMap", "x"): res("ConfigMap", "x", map[string]any{"bad": make(chan int)}),
		}),
	}
	_, err := r.Render("env", sources)
	var re *RenderError
	if !errors.As(err, &re) || re.Reason != ReasonInvalidType {
		t.Fatalf("err = %v, want InvalidType", err)
	}
	if re.Path != "bad" {
		t.Fatalf("path = %q, want bad", re.Path)
	}
	logger.Printf("CLASSIFIED rejection reason=%s source=%s path=%s", re.Reason, re.Source, re.Path)
}

// 整数被归一化；key 与 kind/name 不一致 -> ResourceIDMismatch。
func TestRenderIDMismatch(t *testing.T) {
	r := New()
	bad := &types.Resource{Kind: "ConfigMap", Name: "other", Spec: map[string]any{}}
	_, err := r.Render("env", []types.Source{
		src("a", 10, map[types.ResourceID]*types.Resource{id("ConfigMap", "x"): bad}),
	})
	var re *RenderError
	if !errors.As(err, &re) || re.Reason != ReasonResourceIDMismatch {
		t.Fatalf("err = %v, want ResourceIDMismatch", err)
	}
}

// 同一输入无论来源传入顺序如何，渲染结果与 Revision 必须可复现。
func TestRenderDeterministic(t *testing.T) {
	logger := testLogger(t)
	mk := func() []types.Source {
		return []types.Source{
			src("zeta", 20, map[types.ResourceID]*types.Resource{
				id("Deployment", "b"): res("Deployment", "b", map[string]any{"x": float64(2)}),
			}),
			src("alpha", 10, map[types.ResourceID]*types.Resource{
				id("Deployment", "a"): res("Deployment", "a", map[string]any{"y": float64(1)}),
				id("Deployment", "b"): res("Deployment", "b", map[string]any{"x": float64(1)}),
			}),
		}
	}
	out1, err := New().Render("e", mk())
	if err != nil {
		t.Fatal(err)
	}
	again := mk()
	again[0], again[1] = again[1], again[0]
	out2, err := New().Render("e", again)
	if err != nil {
		t.Fatal(err)
	}
	if out1.Revision != out2.Revision {
		t.Fatalf("revision not reproducible: %d vs %d", out1.Revision, out2.Revision)
	}
	logger.Printf("DRIFT-BASIS/CONTENT revision stable across source order: %d", out1.Revision)
}

// 同名来源重复 -> DuplicateSource。
func TestRenderDuplicateSource(t *testing.T) {
	_, err := New().Render("e", []types.Source{
		src("a", 10, nil),
		src("a", 20, nil),
	})
	var re *RenderError
	if !errors.As(err, &re) || re.Reason != ReasonDuplicateSource {
		t.Fatalf("err = %v, want DuplicateSource", err)
	}
}

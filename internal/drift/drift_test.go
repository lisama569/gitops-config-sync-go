package drift

import (
	"testing"

	"github.com/lisama569/gitops-config-sync-go/internal/cluster"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
)

func TestDetectManualModification(t *testing.T) {
	c := cluster.New(t.TempDir())
	want := state.Resource{Name: "db", Replicas: 2, Config: map[string]string{"image": "pg:15"}}
	if err := c.Apply(want); err != nil {
		t.Fatal(err)
	}
	// 人工改动：直接把观测态改成别的值。
	tampered := want
	tampered.Replicas = 9
	if err := c.Apply(tampered); err != nil {
		t.Fatal(err)
	}

	m := map[string]state.Resource{"db": want}
	ds, err := state.Render("prod", m)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Detect(c, ds)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasDrift() || len(rep.Entries) != 1 {
		t.Fatalf("应检测到 1 处漂移: %+v", rep)
	}
	e := rep.Entries[0]
	t.Logf("漂移判定依据: resource=%s kind=%s expectedHash=%s observedHash=%s",
		e.Resource, e.Kind, e.Expected, e.Observed)
	if e.Kind != "Modified" || e.Expected == e.Observed {
		t.Fatalf("漂移条目不符: %+v", e)
	}
}

func TestDetectMissing(t *testing.T) {
	c := cluster.New(t.TempDir())
	m := map[string]state.Resource{"db": {Name: "db", Replicas: 1}}
	ds, _ := state.Render("prod", m)
	rep, err := Detect(c, ds)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Kind != "Missing" {
		t.Fatalf("应检测到 Missing 漂移: %+v", rep.Entries)
	}
	t.Logf("漂移判定依据: %+v", rep.Entries[0])
}

func TestDetectNoDrift(t *testing.T) {
	c := cluster.New(t.TempDir())
	r := state.Resource{Name: "db", Replicas: 1, Config: map[string]string{}}
	if err := c.Apply(r); err != nil {
		t.Fatal(err)
	}
	ds, _ := state.Render("prod", map[string]state.Resource{"db": r})
	rep, err := Detect(c, ds)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasDrift() {
		t.Fatalf("一致时不应有漂移: %+v", rep.Entries)
	}
}

func TestDetectExtraResource(t *testing.T) {
	c := cluster.New(t.TempDir())
	inDesired := state.Resource{Name: "db", Replicas: 1, Config: map[string]string{}}
	orphan := state.Resource{Name: "leftover", Replicas: 3, Config: map[string]string{"k": "v"}}
	if err := c.Apply(inDesired); err != nil {
		t.Fatal(err)
	}
	// 观测态里多出一个期望态完全没有的资源。
	if err := c.Apply(orphan); err != nil {
		t.Fatal(err)
	}

	ds, _ := state.Render("prod", map[string]state.Resource{"db": inDesired})
	rep, err := Detect(c, ds)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasDrift() || len(rep.Entries) != 1 {
		t.Fatalf("多出来的资源应被识别为漂移, got %+v", rep.Entries)
	}
	e := rep.Entries[0]
	t.Logf("漂移判定依据: resource=%s kind=%s expected=%s observed=%s",
		e.Resource, e.Kind, e.Expected, e.Observed)
	if e.Kind != KindExtra || e.Resource != "leftover" {
		t.Fatalf("应判定为 Extra 漂移, got %+v", e)
	}
	if e.Expected != "-" || e.Observed == "-" {
		t.Fatalf("Extra 条目应为 expected=- 且带观测哈希, got %+v", e)
	}
}

func TestDetectNoDriftIgnoresNothing(t *testing.T) {
	// 期望态与观测态完全一致（无缺失也无多出）时无漂移。
	c := cluster.New(t.TempDir())
	r1 := state.Resource{Name: "db", Replicas: 1, Config: map[string]string{}}
	r2 := state.Resource{Name: "web", Replicas: 2, Config: map[string]string{"a": "b"}}
	for _, r := range []state.Resource{r1, r2} {
		if err := c.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	ds, _ := state.Render("prod", map[string]state.Resource{"db": r1, "web": r2})
	rep, err := Detect(c, ds)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasDrift() {
		t.Fatalf("完全一致时不应有漂移: %+v", rep.Entries)
	}
	t.Logf("期望 2 个资源、观测 2 个资源且哈希一致: 无漂移")
}

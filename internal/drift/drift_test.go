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

func TestDetectExtra(t *testing.T) {
	c := cluster.New(t.TempDir())
	want := state.Resource{Name: "db", Replicas: 1, Config: map[string]string{}}
	if err := c.Apply(want); err != nil {
		t.Fatal(err)
	}
	// 观测态中多出一个期望态完全没有的资源。
	stray := state.Resource{Name: "stray", Replicas: 1, Config: map[string]string{"k": "v"}}
	if err := c.Apply(stray); err != nil {
		t.Fatal(err)
	}

	ds, _ := state.Render("prod", map[string]state.Resource{"db": want})
	rep, err := Detect(c, ds)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasDrift() || len(rep.Entries) != 1 {
		t.Fatalf("多出的资源应被识别为漂移: %+v", rep.Entries)
	}
	e := rep.Entries[0]
	t.Logf("漂移判定依据: resource=%s kind=%s expected=%s observed=%s",
		e.Resource, e.Kind, e.Expected, e.Observed)
	if e.Kind != "Extra" || e.Resource != "stray" || e.Expected != "-" {
		t.Fatalf("Extra 漂移条目不符: %+v", e)
	}
}

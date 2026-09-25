package sources

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/lisama569/gitops-config-sync-go/internal/render"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 目录缺失 / 文件命名不合法 / JSON 非法 -> 不同的可区分原因。
func TestLoadRejections(t *testing.T) {
	logger := log.New(os.Stdout, "  [test:"+t.Name()+"] ", log.Ltime|log.Lmicroseconds)
	root := t.TempDir()

	if _, err := NewLoader(root).Load("ghost"); !errors.Is(err, &render.RenderError{Reason: render.ReasonSourceMissing}) {
		t.Fatalf("missing dir: %v", err)
	}
	logger.Printf("CLASSIFIED missing env dir -> SourceMissing")

	writeFile(t, filepath.Join(root, "e1", "badname.json"), "[]")
	_, err := NewLoader(root).Load("e1")
	var re *render.RenderError
	if !errors.As(err, &re) || re.Reason != render.ReasonSourceMissing {
		t.Fatalf("bad file name: %v", err)
	}
	logger.Printf("CLASSIFIED bad file name -> SourceMissing: %v", err)

	writeFile(t, filepath.Join(root, "e2", "10-base.json"), "{not json")
	_, err = NewLoader(root).Load("e2")
	if !errors.As(err, &re) || re.Reason != render.ReasonInvalidType {
		t.Fatalf("broken json: %v", err)
	}
	logger.Printf("CLASSIFIED broken json -> InvalidType: %v", err)

	writeFile(t, filepath.Join(root, "e3", "10-base.json"), `{"resources": "x"}`)
	_, err = NewLoader(root).Load("e3")
	if !errors.As(err, &re) || re.Reason != render.ReasonInvalidType {
		t.Fatalf("resources wrong type: %v", err)
	}
}

// 三种文件形态都能加载，并正确取到文件名优先级。
func TestLoadShapes(t *testing.T) {
	root := t.TempDir()
	// 完整形态
	writeFile(t, filepath.Join(root, "e", "10-base.json"), `{
		"name":"base","priority":10,
		"resources":[{"kind":"ConfigMap","name":"a","spec":{"k":"v"}}]}`)
	// 数组形态
	writeFile(t, filepath.Join(root, "e", "20-overlay.json"), `[
		{"kind":"Deployment","name":"b","spec":{"replicas":2},"healthCheck":"get /healthz",
		 "dependsOn":[{"kind":"ConfigMap","name":"a"}]}]`)
	// 映射形态
	writeFile(t, filepath.Join(root, "e", "30-map.json"), `{
		"Service/web":{"spec":{"port":80}}}`)

	srcs, err := NewLoader(root).Load("e")
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 3 {
		t.Fatalf("sources = %d, want 3", len(srcs))
	}
	byName := map[string]int{}
	for _, s := range srcs {
		byName[s.Name] = s.Priority
	}
	if byName["base"] != 10 || byName["overlay"] != 20 || byName["map"] != 30 {
		t.Fatalf("priorities = %v", byName)
	}
	var depFound bool
	for _, s := range srcs {
		for _, r := range s.Resources {
			for _, d := range r.DependsOn {
				if d.Kind == "ConfigMap" && d.Name == "a" {
					depFound = true
				}
			}
		}
	}
	if !depFound {
		t.Fatal("dependsOn not parsed from array form")
	}
}

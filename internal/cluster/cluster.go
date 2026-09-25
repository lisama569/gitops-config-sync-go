// Package cluster 用本地文件模拟集群观测态。
package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lisama569/gitops-config-sync-go/internal/state"
)

// Cluster 是文件模拟的集群：
//   - Dir/resources/<name>.json  观测到的资源
//   - Dir/health/<name>          健康标记（内容为 "false" 表示不健康，缺省健康）
//
// ApplyCount 记录实际写入次数，用于验证幂等与并发去重。
type Cluster struct {
	Dir string

	mu         sync.Mutex
	ApplyCount int
}

func New(dir string) *Cluster { return &Cluster{Dir: dir} }

func (c *Cluster) resourcesDir() string { return filepath.Join(c.Dir, "resources") }
func (c *Cluster) healthDir() string    { return filepath.Join(c.Dir, "health") }

func (c *Cluster) resourcePath(name string) string {
	return filepath.Join(c.resourcesDir(), name+".json")
}

// Apply 将资源写入集群（原子写：临时文件 + rename）。
func (c *Cluster) Apply(r state.Resource) error {
	if err := os.MkdirAll(c.resourcesDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal resource %s: %w", r.Name, err)
	}
	tmp, err := os.CreateTemp(c.resourcesDir(), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), c.resourcePath(r.Name)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	c.mu.Lock()
	c.ApplyCount++
	c.mu.Unlock()
	return nil
}

// Get 读取集群中观测到的资源；不存在时 ok=false。
func (c *Cluster) Get(name string) (state.Resource, bool, error) {
	data, err := os.ReadFile(c.resourcePath(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return state.Resource{}, false, nil
		}
		return state.Resource{}, false, err
	}
	var r state.Resource
	if err := json.Unmarshal(data, &r); err != nil {
		return state.Resource{}, false, fmt.Errorf("decode observed %s: %w", name, err)
	}
	return r, true, nil
}

// Remove 删除集群中的资源（不存在时视为成功）。
func (c *Cluster) Remove(name string) error {
	err := os.Remove(c.resourcePath(name))
	if err != nil && !errors.Is(err, isNotExist()) {
		return err
	}
	return nil
}

func isNotExist() error { return fs.ErrNotExist }

// Healthy 检查资源健康状态：health/<name> 文件内容为 "false" 时不健康，缺省健康。
func (c *Cluster) Healthy(name string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(c.healthDir(), name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(data)) != "false", nil
}

// SetHealthy 测试辅助：写入健康标记。
func (c *Cluster) SetHealthy(name string, healthy bool) error {
	if err := os.MkdirAll(c.healthDir(), 0o755); err != nil {
		return err
	}
	v := "true"
	if !healthy {
		v = "false"
	}
	return os.WriteFile(filepath.Join(c.healthDir(), name), []byte(v), 0o644)
}

// Snapshot 快照当前观测态（深拷贝），用于回滚。
func (c *Cluster) Snapshot() (map[string]state.Resource, error) {
	snap := map[string]state.Resource{}
	entries, err := os.ReadDir(c.resourcesDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return snap, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		r, ok, err := c.Get(name)
		if err != nil {
			return nil, err
		}
		if ok {
			snap[name] = r
		}
	}
	return snap, nil
}

// Restore 用快照覆盖当前观测态：删除快照外的资源，重写快照内的资源。
func (c *Cluster) Restore(snap map[string]state.Resource) error {
	current, err := c.Snapshot()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(current))
	for name := range current {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, keep := snap[name]; !keep {
			if err := c.Remove(name); err != nil {
				return fmt.Errorf("restore remove %s: %w", name, err)
			}
		}
	}
	restored := make([]string, 0, len(snap))
	for name := range snap {
		restored = append(restored, name)
	}
	sort.Strings(restored)
	for _, name := range restored {
		if err := c.Apply(snap[name]); err != nil {
			return fmt.Errorf("restore apply %s: %w", name, err)
		}
	}
	return nil
}

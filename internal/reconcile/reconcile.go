// Package reconcile 提供并发安全、幂等的协调器。
package reconcile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/lisama569/gitops-config-sync-go/internal/cluster"
	"github.com/lisama569/gitops-config-sync-go/internal/drift"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
	syncpkg "github.com/lisama569/gitops-config-sync-go/internal/sync"
)

// DriftPolicy 决定检测到人工改动后的行为。
type DriftPolicy string

const (
	// PolicyMark 只标记/告警，不静默纠正。
	PolicyMark DriftPolicy = "Mark"
	// PolicyAlert 标记并返回明确错误提示需要人工介入。
	PolicyAlert DriftPolicy = "Alert"
	// PolicyCorrect 自动纠正回期望态。
	PolicyCorrect DriftPolicy = "Correct"
)

// Status 是协调结果状态。
type Status string

const (
	StatusApplied     Status = "Applied"
	StatusNoop        Status = "Noop" // 幂等：期望态已生效
	StatusDriftMarked Status = "DriftMarked"
	StatusRolledBack  Status = "RolledBack"
)

// Result 是单次协调的输出。
type Result struct {
	Status Status
	Drift  drift.Report
}

// ErrDriftAlert 在 PolicyAlert 且检测到人工改动时返回。
var ErrDriftAlert = errors.New("manual drift detected, human intervention required")

// Reconciler 按环境串行化协调，保证并发下不重复应用、不相互覆盖。
type Reconciler struct {
	Cluster *cluster.Cluster
	Policy  DriftPolicy

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(c *cluster.Cluster, policy DriftPolicy) *Reconciler {
	return &Reconciler{Cluster: c, Policy: policy, locks: map[string]*sync.Mutex{}}
}

func (r *Reconciler) envLock(env string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.locks[env]
	if !ok {
		l = &sync.Mutex{}
		r.locks[env] = l
	}
	return l
}

type manifest struct {
	Env  string `json:"env"`
	Hash string `json:"hash"`
}

func (r *Reconciler) manifestPath(env string) string {
	return filepath.Join(r.Cluster.Dir, "_manifest_"+env+".json")
}

func (r *Reconciler) readManifest(env string) (manifest, bool, error) {
	data, err := os.ReadFile(r.manifestPath(env))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest{}, false, nil
		}
		return manifest{}, false, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, false, fmt.Errorf("decode manifest: %w", err)
	}
	return m, true, nil
}

func (r *Reconciler) writeManifest(env, hash string) error {
	data, err := json.Marshal(manifest{Env: env, Hash: hash})
	if err != nil {
		return err
	}
	return os.WriteFile(r.manifestPath(env), data, 0o644)
}

// markDrift 将漂移报告落盘为告警标记，不改动观测态。
func (r *Reconciler) markDrift(rep drift.Report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Cluster.Dir, "_drift_"+rep.Env+".json"), data, 0o644)
}

// Reconcile 对某环境执行一次协调：
//  1. 同环境互斥，并发协调者串行执行；
//  2. 观测态已符合期望态 → Noop（幂等，重复/并发协调不重复应用）；
//  3. 已生效的期望态被人工改动 → 按策略标记/告警，不静默纠正；
//  4. 否则分波同步，失败即回滚到上一稳定态。
func (r *Reconciler) Reconcile(desired state.DesiredState) (Result, error) {
	env := desired.Env
	lock := r.envLock(env)
	lock.Lock()
	defer lock.Unlock()

	rep, err := drift.Detect(r.Cluster, desired)
	if err != nil {
		return Result{}, fmt.Errorf("detect drift: %w", err)
	}
	if !rep.HasDrift() {
		return Result{Status: StatusNoop, Drift: rep}, nil
	}

	m, hasManifest, err := r.readManifest(env)
	if err != nil {
		return Result{}, err
	}
	// 人工改动判定：当前生效的期望态（manifest 哈希）与本次期望态一致，
	// 但观测态却发生了偏离 —— 说明有人绕开协调流程改了集群。
	if hasManifest && m.Hash == desired.Hash && r.Policy != PolicyCorrect {
		if err := r.markDrift(rep); err != nil {
			return Result{}, fmt.Errorf("mark drift: %w", err)
		}
		if r.Policy == PolicyAlert {
			return Result{Status: StatusDriftMarked, Drift: rep}, ErrDriftAlert
		}
		return Result{Status: StatusDriftMarked, Drift: rep}, nil
	}

	if _, err := syncpkg.Apply(r.Cluster, desired); err != nil {
		return Result{Status: StatusRolledBack, Drift: rep}, err
	}
	if err := r.writeManifest(env, desired.Hash); err != nil {
		return Result{}, fmt.Errorf("write manifest: %w", err)
	}
	return Result{Status: StatusApplied, Drift: rep}, nil
}

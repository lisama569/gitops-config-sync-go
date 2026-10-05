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
	"time"

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
// 互斥是双层的：进程内每环境一把互斥锁 + 集群目录下的锁文件，
// 因此多个独立 Reconciler 实例（甚至不同进程）处理同一环境时也真正互斥；
// 持有者中途异常退出时锁文件过期，后续实例按 TTL 接管继续收敛。
type Reconciler struct {
	Cluster *cluster.Cluster
	Policy  DriftPolicy

	// 锁参数（New 给出默认值，测试可收紧）。
	LockTTL     time.Duration // 锁文件超过该时长未刷新视为持有者已退出，可接管
	LockPoll    time.Duration // 竞争锁的轮询间隔
	LockTimeout time.Duration // 等待锁的最长时间

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	holder string // 本实例的锁持有者标识
}

func New(c *cluster.Cluster, policy DriftPolicy) *Reconciler {
	return &Reconciler{
		Cluster:     c,
		Policy:      policy,
		LockTTL:     5 * time.Second,
		LockPoll:    5 * time.Millisecond,
		LockTimeout: 30 * time.Second,
		locks:       map[string]*sync.Mutex{},
		holder:      fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
	}
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

func (r *Reconciler) lockPath(env string) string {
	return filepath.Join(r.Cluster.Dir, "_lock_"+env)
}

func (r *Reconciler) envFileLock(env string) *fileLock {
	return &fileLock{
		path:    r.lockPath(env),
		holder:  r.holder,
		ttl:     r.LockTTL,
		poll:    r.LockPoll,
		timeout: r.LockTimeout,
	}
}

type manifest struct {
	Env  string `json:"env"`
	Hash string `json:"hash"`
	// Managed 记录上次成功落地时由本环境管理的资源名，
	// 用于识别"从期望态移除、需要回收"的资源（与从未被管理的多余资源区分开）。
	Managed []string `json:"managed,omitempty"`
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

func (r *Reconciler) writeManifest(env, hash string, managed []string) error {
	data, err := json.Marshal(manifest{Env: env, Hash: hash, Managed: managed})
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
//  1. 同环境互斥（进程内锁 + 跨进程锁文件），并发/多实例协调者串行执行；
//  2. 观测态已符合期望态 → Noop（幂等，重复/并发协调不重复应用）；
//  3. 已生效的期望态被人工改动（含人为增删资源）→ 按策略标记/告警，不静默纠正；
//  4. 否则分波同步，失败即回滚到上一稳定态；
//     成功后回收期望态中已移除的受管资源，并更新 manifest。
func (r *Reconciler) Reconcile(desired state.DesiredState) (Result, error) {
	env := desired.Env
	lock := r.envLock(env)
	lock.Lock()
	defer lock.Unlock()

	flock := r.envFileLock(env)
	if err := flock.Acquire(); err != nil {
		return Result{}, fmt.Errorf("acquire env lock: %w", err)
	}
	defer flock.Release()

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
	// 回收：上次落地、本次期望态中已不存在的资源，从观测态清理掉。
	desiredNames := map[string]struct{}{}
	managed := make([]string, 0, len(desired.Resources))
	for _, res := range desired.Resources {
		desiredNames[res.Name] = struct{}{}
		managed = append(managed, res.Name)
	}
	if hasManifest {
		for _, name := range m.Managed {
			if _, keep := desiredNames[name]; keep {
				continue
			}
			if err := r.Cluster.Remove(name); err != nil {
				return Result{}, fmt.Errorf("prune removed resource %s: %w", name, err)
			}
		}
	}
	// Correct 策略下，连从未被管理的多余资源也一并清理，使观测态收敛到期望态。
	if r.Policy == PolicyCorrect {
		for _, e := range rep.Entries {
			if e.Kind != "Extra" {
				continue
			}
			if _, keep := desiredNames[e.Resource]; keep {
				continue
			}
			if err := r.Cluster.Remove(e.Resource); err != nil {
				return Result{}, fmt.Errorf("remove extra resource %s: %w", e.Resource, err)
			}
		}
	}
	if err := r.writeManifest(env, desired.Hash, managed); err != nil {
		return Result{}, fmt.Errorf("write manifest: %w", err)
	}
	return Result{Status: StatusApplied, Drift: rep}, nil
}

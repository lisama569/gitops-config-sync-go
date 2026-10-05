// Package reconcile 提供并发安全、幂等的协调器。
package reconcile

import (
	"crypto/rand"
	"encoding/hex"
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

// ErrLockBusy 在环境锁被其他存活实例持有、等待超时仍未获得时返回。
var ErrLockBusy = errors.New("environment lock held by another live instance")

// 默认锁参数。
const (
	DefaultLockTTL  = 30 * time.Second // 锁租约：持有者异常退出后，其他实例在此时间后接管
	DefaultLockWait = 10 * time.Second // 等待锁的最长时间
	lockPoll        = 20 * time.Millisecond
)

// Reconciler 按环境串行化协调，保证并发下不重复应用、不相互覆盖。
// 互斥分两层：进程内按环境互斥锁 + 集群目录下的锁文件（跨实例、跨进程生效）。
type Reconciler struct {
	Cluster *cluster.Cluster
	Policy  DriftPolicy

	// Owner 标识本实例，写入锁文件；缺省自动生成（pid+随机数）。
	Owner string
	// LockTTL 是文件锁租约时长；持有者异常退出后锁自然过期，其他实例可接管。
	LockTTL time.Duration
	// LockWait 是等待环境锁的最长时间，超时返回 ErrLockBusy。
	LockWait time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(c *cluster.Cluster, policy DriftPolicy) *Reconciler {
	return &Reconciler{
		Cluster:  c,
		Policy:   policy,
		Owner:    defaultOwner(),
		LockTTL:  DefaultLockTTL,
		LockWait: DefaultLockWait,
		locks:    map[string]*sync.Mutex{},
	}
}

func defaultOwner() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid%d", os.Getpid())
	}
	return fmt.Sprintf("pid%d-%s", os.Getpid(), hex.EncodeToString(b[:]))
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

// fileLockBody 是锁文件内容：持有者标识 + 租约到期时间（UnixNano）。
type fileLockBody struct {
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expiresAtUnixNano"`
}

func (r *Reconciler) lockPath(env string) string {
	return filepath.Join(r.Cluster.Dir, "_lock_"+env+".json")
}

// acquireFileLock 以原子链接（临时文件 + hard link）抢锁文件，实现跨实例/跨进程互斥。
// 锁文件内容一次成形地出现，不存在"已创建但内容未写完"的窗口，
// 其他实例不会把半截锁误判为可接管的残锁。
// 锁文件已存在时：租约未过期 → 轮询等待直至 LockWait 超时（ErrLockBusy）；
// 租约已过期（持有者异常退出）→ 删除旧锁并接管。返回释放函数。
func (r *Reconciler) acquireFileLock(env string) (func(), error) {
	path := r.lockPath(env)
	deadline := time.Now().Add(r.lockWait())
	for {
		body, err := json.Marshal(fileLockBody{
			Owner:     r.Owner,
			ExpiresAt: time.Now().Add(r.lockTTL()).UnixNano(),
		})
		if err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(r.Cluster.Dir, ".locktmp-*")
		if err != nil {
			return nil, err
		}
		if _, err := tmp.Write(body); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("write lock file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("close lock file: %w", err)
		}
		// hard link 是原子操作：目标已存在即失败，且锁文件一旦出现内容就完整。
		err = os.Link(tmp.Name(), path)
		os.Remove(tmp.Name())
		if err == nil {
			return func() { r.releaseFileLock(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create lock file: %w", err)
		}
		// 锁已被持有：检查是否过期可接管。
		stale, rerr := lockStale(path)
		if rerr == nil && stale {
			// 持有者异常退出，租约过期：删除旧锁后立即重试抢锁。
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("acquire lock for env %q: %w", env, ErrLockBusy)
		}
		time.Sleep(lockPoll)
	}
}

// lockStale 判断锁文件租约是否已过期；锁文件损坏时视为过期（可接管）。
func lockStale(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var b fileLockBody
	if err := json.Unmarshal(data, &b); err != nil {
		return true, nil // 损坏的锁文件不阻断接管
	}
	return time.Now().UnixNano() > b.ExpiresAt, nil
}

// releaseFileLock 仅在锁仍归本实例所有时删除，避免误删接管者的新锁。
func (r *Reconciler) releaseFileLock(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var b fileLockBody
	if err := json.Unmarshal(data, &b); err != nil || b.Owner != r.Owner {
		return
	}
	os.Remove(path)
}

func (r *Reconciler) lockTTL() time.Duration {
	if r.LockTTL > 0 {
		return r.LockTTL
	}
	return DefaultLockTTL
}

func (r *Reconciler) lockWait() time.Duration {
	if r.LockWait > 0 {
		return r.LockWait
	}
	return DefaultLockWait
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
	return writeFileAtomic(r.manifestPath(env), data)
}

// writeFileAtomic 临时文件 + rename，避免异常退出留下半截文件。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// markDrift 将漂移报告落盘为告警标记，不改动观测态。
func (r *Reconciler) markDrift(rep drift.Report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(r.Cluster.Dir, "_drift_"+rep.Env+".json"), data)
}

// Reconcile 对某环境执行一次协调：
//  1. 同环境互斥（进程内互斥锁 + 跨实例文件锁），并发/多实例协调串行执行；
//     锁持有者异常退出时租约过期，后续实例自动接管，环境不会被永久锁住；
//  2. 观测态已符合期望态 → Noop（幂等，重复/并发协调不重复应用）；
//  3. 已生效的期望态被人工改动（含观测态多出资源）→ 按策略标记/告警，不静默纠正；
//  4. 否则分波同步并回收期望态已移除的资源，失败即回滚到上一稳定态。
func (r *Reconciler) Reconcile(desired state.DesiredState) (Result, error) {
	env := desired.Env
	lock := r.envLock(env)
	lock.Lock()
	defer lock.Unlock()

	release, err := r.acquireFileLock(env)
	if err != nil {
		return Result{}, err
	}
	defer release()

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

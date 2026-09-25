// Package coordinator 提供带文件锁的并发协调，对外暴露 Reconcile 入口。
//
// 协调流程（每环境一把锁）：
//
//  1. TryAcquireLock：多个协调者并发处理同一环境时，只有一个持锁执行，
//     其余立即得到 Skipped，不重复应用、不互相覆盖；
//  2. 加载来源并渲染期望态（渲染失败按可区分原因直接返回，不触碰观测态）；
//  3. 漂移检测：
//     - SpecChanged（人工改动）绝不静默纠正：按策略标记/告警后本次不应用；
//     - Outdated/Missing/Extra（合法配置升级或部署缺口）允许继续同步收敛；
//     - 无漂移且观测态已是该内容版本：幂等空转，不产生新版本；
//  4. 分波次同步（失败由 sync 包回滚到上一个稳定态）；
//  5. 释放锁。
package coordinator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/lisama569/gitops-config-sync-go/internal/drift"
	"github.com/lisama569/gitops-config-sync-go/internal/render"
	"github.com/lisama569/gitops-config-sync-go/internal/sources"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
	"github.com/lisama569/gitops-config-sync-go/internal/sync"
	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

// Logger 协调过程日志（输入、波次、漂移依据都经它打印）。
type Logger interface {
	Printf(format string, args ...any)
}

// Alerter 接收漂移告警；测试可收集告警断言。
type Alerter interface {
	Alert(env string, records []types.DriftRecord)
}

// Coordinator 单个协调者实例。
type Coordinator struct {
	id       string
	loader   *sources.Loader
	renderer *render.Renderer
	store    *state.Store
	detector *drift.Detector
	applier  *sync.Applier
	log      Logger
	alerter  Alerter
	ttl      time.Duration
	now      func() time.Time
}

// Config 构造参数。
type Config struct {
	ID         string
	ConfigRoot string
	StateDir   string
	Policy     drift.Policy
	Health     sync.HealthChecker
	Hook       *sync.ApplyHook
	Logger     Logger
	Alerter    Alerter
	LockTTL    time.Duration
}

// New 创建协调者。
func New(cfg Config) *Coordinator {
	st := state.New(cfg.StateDir)
	ap := sync.New(st, cfg.Health).WithHook(cfg.Hook).WithLogger(cfg.Logger)
	ttl := cfg.LockTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Coordinator{
		id:       cfg.ID,
		loader:   sources.NewLoader(cfg.ConfigRoot),
		renderer: render.New(),
		store:    st,
		detector: drift.New(cfg.Policy),
		applier:  ap,
		log:      cfg.Logger,
		alerter:  cfg.Alerter,
		ttl:      ttl,
		now:      time.Now,
	}
}

// ReconcileResult 一次协调的结果。
type ReconcileResult struct {
	// Skipped 为 true 表示锁被其他协调者持有，本次没有执行任何变更。
	Skipped bool
	// Applied 为 true 表示本次发生了同步落盘。
	Applied bool
	// Drifted 为 true 表示检测到 SpecChanged（人工改动），已按策略标记/告警。
	Drifted bool
	// LockHolder 在 Skipped 时给出持锁者。
	LockHolder string
	// Drift 给出全部漂移记录（含字段级依据）。
	Drift []types.DriftRecord
	// Revision 同步成功后的稳定版本号。
	Revision int64
}

// Reconcile 执行一次完整协调。
func (c *Coordinator) Reconcile(env string) (*ReconcileResult, error) {
	res := &ReconcileResult{}
	token := newToken()

	c.logf("[reconcile] start env=%s coordinator=%s policy=%s", env, c.id, c.detector.Policy())
	held, holder, err := c.store.TryAcquireLock(env, c.id, token, c.ttl, c.now())
	if err != nil {
		if errors.Is(err, state.ErrLockHeld) {
			res.Skipped = true
			if holder != nil {
				res.LockHolder = holder.Coordinator
			}
			c.logf("[reconcile] env=%s skipped: lock held by %s", env, res.LockHolder)
			return res, nil
		}
		return nil, err
	}
	if !held {
		res.Skipped = true
		if holder != nil {
			res.LockHolder = holder.Coordinator
		}
		return res, nil
	}
	defer func() {
		if rerr := c.store.ReleaseLock(env, c.id, token); rerr != nil {
			c.logf("[reconcile] WARNING release lock failed: %v", rerr)
		}
	}()

	srcs, err := c.loader.Load(env)
	if err != nil {
		c.logf("[reconcile] env=%s load rejected: %v", env, err)
		return nil, err
	}
	c.logf("[reconcile] env=%s input sources=%d", env, len(srcs))
	desired, err := c.renderer.Render(env, srcs)
	if err != nil {
		c.logf("[reconcile] env=%s render rejected: %v", env, err)
		return nil, err
	}

	observed, err := c.store.Observed(env)
	if err != nil {
		return nil, err
	}
	records := c.detector.Detect(desired, observed)
	res.Drift = records
	c.logf("[reconcile] env=%s drift detection: %d record(s)", env, len(records))
	for _, rec := range records {
		if rec.Kind == types.DriftNone {
			continue
		}
		c.logf("[reconcile] drift env=%s resource=%s kind=%s managed=%t diffs=%s",
			env, rec.ID, rec.Kind, rec.Managed, formatDiffs(rec.Diffs))
	}

	// 人工改动（SpecChanged）绝不静默纠正：按策略标记/告警，本次不应用。
	// Outdated（期望态合法升级）/Missing/Extra 属于部署缺口，允许继续同步收敛。
	var manual []types.DriftRecord
	var gaps []types.DriftRecord
	for _, rec := range records {
		switch rec.Kind {
		case types.DriftSpecChanged:
			manual = append(manual, rec)
		case types.DriftMissing, types.DriftExtra, types.DriftOutdated:
			gaps = append(gaps, rec)
		}
	}
	if len(manual) > 0 {
		res.Drifted = true
		if c.detector.ShouldMark() {
			if err := c.markDrift(env, manual); err != nil {
				return nil, err
			}
		}
		if c.detector.ShouldAlert() && c.alerter != nil {
			c.alerter.Alert(env, manual)
			c.logf("[reconcile] alert raised for %d manually-changed resource(s)", len(manual))
		}
		c.logf("[reconcile] env=%s manual drift present; NOT auto-correcting per policy %s",
			env, c.detector.Policy())
		return res, nil
	}

	if len(gaps) == 0 {
		c.logf("[reconcile] env=%s in sync at revision %d (idempotent no-op)", env, desired.Revision)
		return res, nil
	}

	rev, err := c.applier.Apply(env, desired, c.id)
	if err != nil {
		c.logf("[reconcile] env=%s apply failed and rolled back: %v", env, err)
		return nil, err
	}
	res.Applied = true
	res.Revision = rev.Number
	c.logf("[reconcile] env=%s applied stable revision=%d", env, rev.Number)
	return res, nil
}

// Rollback 供 CLI/测试显式回滚（同样需要持锁）。
func (c *Coordinator) Rollback(env string, toRevision int64) (*ReconcileResult, error) {
	res := &ReconcileResult{}
	token := newToken()
	held, holder, err := c.store.TryAcquireLock(env, c.id, token, c.ttl, c.now())
	if err != nil {
		if errors.Is(err, state.ErrLockHeld) {
			res.Skipped = true
			if holder != nil {
				res.LockHolder = holder.Coordinator
			}
			return res, nil
		}
		return nil, err
	}
	if !held {
		res.Skipped = true
		return res, nil
	}
	defer c.store.ReleaseLock(env, c.id, token)

	rev, err := c.applier.Rollback(env, toRevision, c.id)
	if err != nil {
		return nil, err
	}
	res.Applied = true
	res.Revision = rev.Number
	return res, nil
}

func (c *Coordinator) markDrift(env string, manual []types.DriftRecord) error {
	observed, err := c.store.Observed(env)
	if err != nil {
		return err
	}
	for _, rec := range manual {
		if got, ok := observed[rec.ID]; ok {
			got.Drifted = true
			got.DriftMark = rec.Kind
			got.DriftDiffs = rec.Diffs
		}
	}
	return c.store.SaveObserved(env, observed)
}

func (c *Coordinator) logf(format string, args ...any) {
	if c.log != nil {
		c.log.Printf(format, args...)
	}
}

func formatDiffs(diffs []types.FieldDiff) string {
	if len(diffs) == 0 {
		return "-"
	}
	out := ""
	for i, d := range diffs {
		if i > 0 {
			out += "; "
		}
		out += fmt.Sprintf("%s expected=%v observed=%v", d.Path, d.Expected, d.Observed)
	}
	return out
}

func newToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Package state 用本地文件模拟集群观测态、稳定版本快照与协调锁。
//
// 目录布局（<dir>/<env>/）：
//
//	observed.json          当前观测态（集群模拟）
//	revisions/<n>.json     每次成功同步后的不可变快照
//	current.json           当前稳定版本号
//	lock.json              协调者互斥锁（带租约，防止陈旧锁）
//
// 所有写入先写临时文件再 rename，保证中途崩溃不会留下半截 JSON。
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

// Store 基于本地 JSON 文件的状态存储。
type Store struct {
	dir string
}

// New 创建存储，dir 为状态目录（每个环境一个子目录）。
func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) envDir(env string) string { return filepath.Join(s.dir, env) }

// ---- 观测态 ----

// Observed 读取某环境的观测态；尚不存在时返回空 map（视为空集群）。
func (s *Store) Observed(env string) (map[types.ResourceID]*types.ObservedResource, error) {
	path := filepath.Join(s.envDir(env), "observed.json")
	var list []*types.ObservedResource
	if err := readJSON(path, &list); err != nil {
		if os.IsNotExist(err) {
			return map[types.ResourceID]*types.ObservedResource{}, nil
		}
		return nil, err
	}
	out := make(map[types.ResourceID]*types.ObservedResource, len(list))
	for _, r := range list {
		out[r.ID()] = r
	}
	return out, nil
}

// SaveObserved 原子写入观测态（按 ID 排序，保证文件内容可复现）。
func (s *Store) SaveObserved(env string, obs map[types.ResourceID]*types.ObservedResource) error {
	list := make([]*types.ObservedResource, 0, len(obs))
	for _, r := range obs {
		list = append(list, r)
	}
	sortObserved(list)
	return writeJSON(filepath.Join(s.envDir(env), "observed.json"), list)
}

// ---- 版本快照 ----

type currentPointer struct {
	Number int64 `json:"number"`
}

// SaveRevision 保存一个稳定版本快照（必须保证 Number 全局唯一），并原子更新 current 指针。
// Number==0 时用 O_CREATE|O_EXCL 分配下一个版本号，避免多个协调者
// 基于陈旧的版本列表分配出相同编号；Number>0 时必须不存在（冲突即报错）。
func (s *Store) SaveRevision(env string, rev *types.Revision) (int64, error) {
	dir := filepath.Join(s.envDir(env), "revisions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}

	// 先确定编号（写文件前 rev.Number 必须最终化），再序列化，
	// 否则快照里会落下编号 0。
	if rev.Number <= 0 {
		existing, err := s.RevisionNumbers(env)
		if err != nil {
			return 0, err
		}
		next := int64(1)
		if len(existing) > 0 {
			next = existing[len(existing)-1] + 1
		}
		rev.Number = next
	}

	var body []byte
	for {
		var mErr error
		body, mErr = json.MarshalIndent(rev, "", "  ")
		if mErr != nil {
			return 0, mErr
		}
		path := filepath.Join(dir, fmt.Sprintf("%d.json", rev.Number))
		err := createExclusive(path, body)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return 0, err
		}
		// 编号被并发占用：递增后重序列化（编号属于快照内容）再试。
		rev.Number++
	}

	if err := writeJSON(filepath.Join(s.envDir(env), "current.json"), currentPointer{Number: rev.Number}); err != nil {
		return 0, err
	}
	return rev.Number, nil
}

// createExclusive 以 O_CREATE|O_EXCL 创建文件并写入全部内容。
func createExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RevisionNumbers 返回某环境全部已保存版本号（升序）。
func (s *Store) RevisionNumbers(env string) ([]int64, error) {
	dir := filepath.Join(s.envDir(env), "revisions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var nums []int64
	for _, e := range entries {
		var n int64
		if _, err := fmt.Sscanf(e.Name(), "%d.json", &n); err == nil {
			nums = append(nums, n)
		}
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	return nums, nil
}

// LastRevision 返回最近一个稳定版本；不存在时返回 (nil, nil)。
func (s *Store) LastRevision(env string) (*types.Revision, error) {
	var ptr currentPointer
	if err := readJSON(filepath.Join(s.envDir(env), "current.json"), &ptr); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return s.Revision(env, ptr.Number)
}

// Revision 按版本号读取快照；不存在时返回 (nil, nil)。
func (s *Store) Revision(env string, number int64) (*types.Revision, error) {
	var rev types.Revision
	path := filepath.Join(s.envDir(env), "revisions", fmt.Sprintf("%d.json", number))
	if err := readJSON(path, &rev); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &rev, nil
}

// ---- 协调锁 ----

// LockRecord 是 lock.json 的内容。
type LockRecord struct {
	Coordinator string    `json:"coordinator"`
	AcquiredAt  time.Time `json:"acquiredAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Token       string    `json:"token"`
	Env         string    `json:"env"`
}

// ErrLockHeld 表示锁正被其他协调者有效持有。
var ErrLockHeld = fmt.Errorf("coordination lock is held by another coordinator")

// TryAcquireLock 尝试获取环境锁。已被他人有效持有时返回 (false, 持有者记录, ErrLockHeld)；
// 租约过期的陈旧锁会被接管。无锁时用 O_CREATE|O_EXCL 原子创建，
// 接管陈旧锁时在 rename 后复核记录，避免两个协调者同时认为自己持锁。
func (s *Store) TryAcquireLock(env, coordinator, token string, ttl time.Duration, now time.Time) (bool, *LockRecord, error) {
	dir := s.envDir(env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, nil, err
	}
	path := filepath.Join(dir, "lock.json")
	rec := LockRecord{
		Coordinator: coordinator,
		AcquiredAt:  now,
		ExpiresAt:   now.Add(ttl),
		Token:       token,
		Env:         env,
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return false, nil, err
	}

	// 与持锁者释放（remove/rename）并发时，可能瞬时遇到 ENOENT 或读到半截文件；
	// 对这类瞬时窗口做有限重试，最终结果仍由 O_EXCL / 租约 / token 复核保证互斥。
	var lastTransient error
	for attempt := 0; attempt < 6; attempt++ {
		held, holder, err := s.tryAcquireOnce(path, b, rec, now)
		if err == nil {
			return held, holder, nil
		}
		if errors.Is(err, ErrLockHeld) {
			// 锁被有效持有：立即返回，不重试。
			return false, holder, err
		}
		if os.IsNotExist(err) || isTransientJSON(err) {
			lastTransient = err
			time.Sleep(time.Duration(attempt+1) * time.Millisecond)
			continue
		}
		return false, nil, err
	}
	return false, nil, fmt.Errorf("lock acquire gave up after transient read window: %w", lastTransient)
}

func isTransientJSON(err error) bool {
	var se *json.SyntaxError
	return errors.As(err, &se)
}

// tryAcquireOnce 执行一次无循环的获取尝试。
func (s *Store) tryAcquireOnce(path string, b []byte, rec LockRecord, now time.Time) (bool, *LockRecord, error) {
	// 第一次尝试：无锁则原子创建。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		if _, werr := f.Write(b); werr != nil {
			f.Close()
			return false, nil, werr
		}
		if cerr := f.Close(); cerr != nil {
			return false, nil, cerr
		}
		return true, &rec, nil
	}
	if !os.IsExist(err) {
		return false, nil, err
	}

	// 锁文件已存在：读出租约判断。
	var holder LockRecord
	if rerr := readJSON(path, &holder); rerr != nil {
		return false, nil, rerr
	}
	if holder.ExpiresAt.After(now) {
		return false, &holder, ErrLockHeld
	}

	// 陈旧锁：原子替换后复核。
	tmp, err := writeTemp(filepath.Dir(path), b)
	if err != nil {
		return false, nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, nil, err
	}
	var after LockRecord
	if rerr := readJSON(path, &after); rerr != nil {
		return false, nil, rerr
	}
	if after.Token != rec.Token || after.Coordinator != rec.Coordinator {
		// 并发接管中落败，交还结果给调用方。
		return false, &after, ErrLockHeld
	}
	return true, &after, nil
}

// writeTemp 在 dir 下写入一个临时文件并返回其路径。
func writeTemp(dir string, b []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".lock-tmp-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// ReleaseLock 仅当持有者与 token 匹配时释放锁。
func (s *Store) ReleaseLock(env, coordinator, token string) error {
	path := filepath.Join(s.envDir(env), "lock.json")
	var rec LockRecord
	if err := readJSON(path, &rec); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if rec.Coordinator == coordinator && rec.Token == token {
		return os.Remove(path)
	}
	return nil
}

// ---- 文件工具 ----

func readJSON(path string, v any) error {
	// 多个协调者并发时，写方用“临时文件 + rename”原子切换，
	// 但 rename 与读并发的极小窗口里可能读到 0 字节文件；
	// 对空文件 / JSON 截断错误做有限次重读，避免把切换窗口误判为损坏。
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(b) == 0 {
			lastErr = &json.SyntaxError{}
			time.Sleep(time.Duration(attempt+1) * time.Millisecond)
			continue
		}
		if err := json.Unmarshal(b, v); err != nil {
			var se *json.SyntaxError
			if errors.As(err, &se) {
				lastErr = err
				time.Sleep(time.Duration(attempt+1) * time.Millisecond)
				continue
			}
			return err
		}
		return nil
	}
	return lastErr
}

// writeJSON 先写临时文件再 rename，保证原子性。
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func sortObserved(list []*types.ObservedResource) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Kind != list[j].Kind {
			return list[i].Kind < list[j].Kind
		}
		return list[i].Name < list[j].Name
	})
}

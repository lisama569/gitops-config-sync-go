package reconcile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrLockTimeout 在规定的等待窗口内没能拿到环境锁时返回。
var ErrLockTimeout = errors.New("timed out acquiring environment lock")

// fileLock 是基于锁文件的跨进程互斥锁：
//   - 获取：O_CREATE|O_EXCL 创建锁文件，内容为持有者标识 + 时间戳；
//   - 接管：锁文件 mtime 超过 TTL 视为持有者在协调中途异常退出，
//     删除旧锁并重新竞争，环境不会被永久锁住；
//   - 释放：仅当锁文件仍属于自己（持有者标识匹配）时才删除，
//     避免误删已被他人接管的新锁。
type fileLock struct {
	path    string
	holder  string
	ttl     time.Duration
	poll    time.Duration
	timeout time.Duration
}

func (l *fileLock) Acquire() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	deadline := time.Now().Add(l.timeout)
	for {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			if _, werr := fmt.Fprintf(f, "%s\n%d\n", l.holder, time.Now().UnixNano()); werr != nil {
				f.Close()
				os.Remove(l.path)
				return werr
			}
			if cerr := f.Close(); cerr != nil {
				os.Remove(l.path)
				return cerr
			}
			return nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("acquire lock %s: %w", l.path, err)
		}
		// 锁已被占用：检查是否过期（持有者中途退出）。
		if info, serr := os.Stat(l.path); serr == nil && time.Since(info.ModTime()) > l.ttl {
			_ = os.Remove(l.path) // 过期锁，接管；与并发的接管者重新竞争创建
			continue
		}
		if time.Now().After(deadline) {
			return ErrLockTimeout
		}
		time.Sleep(l.poll)
	}
}

func (l *fileLock) Release() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // 锁已被接管者清走，无需处理
		}
		return err
	}
	holder, _, _ := strings.Cut(string(data), "\n")
	if holder != l.holder {
		return nil // 锁已被他人接管，不能误删
	}
	err = os.Remove(l.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

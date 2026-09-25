package state

import (
	"errors"
	"testing"
	"time"

	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

func TestObservedRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	obs := map[types.ResourceID]*types.ObservedResource{
		{Kind: "Deployment", Name: "a"}: {
			Resource:        types.Resource{Kind: "Deployment", Name: "a", Spec: map[string]any{"x": float64(1)}},
			AppliedRevision: 7,
			Managed:         true,
		},
	}
	if err := s.SaveObserved("e", obs); err != nil {
		t.Fatal(err)
	}
	got, err := s.Observed("e")
	if err != nil || len(got) != 1 || got[types.ResourceID{Kind: "Deployment", Name: "a"}].AppliedRevision != 7 {
		t.Fatalf("round trip: %v %v", got, err)
	}
}

func TestRevisions(t *testing.T) {
	s := New(t.TempDir())
	if rev, err := s.LastRevision("e"); err != nil || rev != nil {
		t.Fatalf("empty last revision: %v %v", rev, err)
	}
	r1 := &types.Revision{Number: 1, DesiredRevision: 100, Env: "e"}
	r2 := &types.Revision{Number: 2, DesiredRevision: 200, Env: "e"}
	if _, err := s.SaveRevision("e", r1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRevision("e", r2); err != nil {
		t.Fatal(err)
	}
	last, err := s.LastRevision("e")
	if err != nil || last.Number != 2 {
		t.Fatalf("last = %v err=%v", last, err)
	}
	nums, _ := s.RevisionNumbers("e")
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 2 {
		t.Fatalf("numbers = %v", nums)
	}
}

func TestLockMutualExclusionAndLease(t *testing.T) {
	s := New(t.TempDir())
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ok, holder, err := s.TryAcquireLock("e", "c1", "t1", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("first acquire: %v %v", ok, err)
	}
	// 第二个协调者立即被拒，且能知道持锁者。
	ok2, holder2, err := s.TryAcquireLock("e", "c2", "t2", time.Minute, now)
	if err == nil || !errors.Is(err, ErrLockHeld) || ok2 || holder2.Coordinator != "c1" {
		t.Fatalf("second acquire: ok=%v holder=%v err=%v", ok2, holder2, err)
	}
	_ = holder
	// 租约过期后可被接管。
	ok3, holder3, err := s.TryAcquireLock("e", "c2", "t2", time.Minute, now.Add(2*time.Minute))
	if err != nil || !ok3 || holder3.Coordinator != "c2" {
		t.Fatalf("stale takeover: ok=%v holder=%v err=%v", ok3, holder3, err)
	}
	// token 不匹配时不能释放别人的锁：c1 的旧 token 释放不了 c2 的锁。
	if err := s.ReleaseLock("e", "c1", "t1"); err != nil {
		t.Fatal(err)
	}
	// c2 仍持有锁。
	ok4, _, err := s.TryAcquireLock("e", "other", "x", time.Minute, now.Add(2*time.Minute))
	if err == nil || ok4 {
		t.Fatalf("c1 must not have released c2's lock: ok=%v err=%v", ok4, err)
	}
}

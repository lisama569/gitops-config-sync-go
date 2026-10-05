// Package drift 检测观测态与期望态的漂移。
package drift

import (
	"sort"

	"github.com/lisama569/gitops-config-sync-go/internal/cluster"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
)

// Entry 描述一处漂移及其判定依据（期望/观测内容哈希）。
type Entry struct {
	Resource string
	Expected string // 期望内容哈希；"-" 表示期望中不存在
	Observed string // 观测内容哈希；"-" 表示集群中缺失
	Kind     string // Modified / Missing / Extra
}

// Report 是一次漂移检测的结果。
type Report struct {
	Env     string
	Entries []Entry
}

func (r Report) HasDrift() bool { return len(r.Entries) > 0 }

// Detect 比较期望态与集群观测态，返回漂移明细。
// 判定依据（内容哈希比对）：
//   - 期望有、集群缺失            → Missing
//   - 双方都有但内容哈希不一致     → Modified
//   - 集群存在、期望态完全没有     → Extra（不忽略，交由策略标记/告警/纠正）
func Detect(c *cluster.Cluster, desired state.DesiredState) (Report, error) {
	rep := Report{Env: desired.Env}
	desiredNames := map[string]struct{}{}
	for _, want := range desired.Resources {
		desiredNames[want.Name] = struct{}{}
		wantHash, err := state.HashResource(want)
		if err != nil {
			return rep, err
		}
		got, ok, err := c.Get(want.Name)
		if err != nil {
			return rep, err
		}
		if !ok {
			rep.Entries = append(rep.Entries, Entry{
				Resource: want.Name,
				Expected: wantHash,
				Observed: "-",
				Kind:     "Missing",
			})
			continue
		}
		gotHash, err := state.HashResource(got)
		if err != nil {
			return rep, err
		}
		if gotHash != wantHash {
			rep.Entries = append(rep.Entries, Entry{
				Resource: want.Name,
				Expected: wantHash,
				Observed: gotHash,
				Kind:     "Modified",
			})
		}
	}
	// 反向检查：观测态里多出来的资源也是漂移。
	observed, err := c.List()
	if err != nil {
		return rep, err
	}
	for _, name := range observed {
		if _, ok := desiredNames[name]; ok {
			continue
		}
		got, ok, err := c.Get(name)
		if err != nil {
			return rep, err
		}
		if !ok {
			continue
		}
		gotHash, err := state.HashResource(got)
		if err != nil {
			return rep, err
		}
		rep.Entries = append(rep.Entries, Entry{
			Resource: name,
			Expected: "-",
			Observed: gotHash,
			Kind:     "Extra",
		})
	}
	sort.Slice(rep.Entries, func(i, j int) bool { return rep.Entries[i].Resource < rep.Entries[j].Resource })
	return rep, nil
}

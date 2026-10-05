// Package drift 检测观测态与期望态的漂移。
package drift

import (
	"sort"

	"github.com/lisama569/gitops-config-sync-go/internal/cluster"
	"github.com/lisama569/gitops-config-sync-go/internal/state"
)

// 漂移类别。
const (
	KindModified = "Modified" // 期望与观测都存在但内容不一致
	KindMissing  = "Missing"  // 期望中存在、集群中缺失
	KindExtra    = "Extra"    // 集群中存在、期望态中完全没有（多出来的资源）
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

// Detect 双向比较期望态与集群观测态的内容哈希，返回漂移明细。
// 判定依据：
//   - 期望哈希 != 观测哈希 → Modified；
//   - 期望中存在、集群中缺失 → Missing；
//   - 集群中存在、期望态中完全没有 → Extra（不会被忽略）。
func Detect(c *cluster.Cluster, desired state.DesiredState) (Report, error) {
	rep := Report{Env: desired.Env}
	wanted := map[string]struct{}{}
	for _, want := range desired.Resources {
		wanted[want.Name] = struct{}{}
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
				Kind:     KindMissing,
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
				Kind:     KindModified,
			})
		}
	}
	// 反向检查：观测态里多出来的、期望态完全没有的资源也算漂移。
	observed, err := c.Snapshot()
	if err != nil {
		return rep, err
	}
	for name, got := range observed {
		if _, ok := wanted[name]; ok {
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
			Kind:     KindExtra,
		})
	}
	sort.Slice(rep.Entries, func(i, j int) bool { return rep.Entries[i].Resource < rep.Entries[j].Resource })
	return rep, nil
}

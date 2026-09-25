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
	Kind     string // Modified / Missing
}

// Report 是一次漂移检测的结果。
type Report struct {
	Env     string
	Entries []Entry
}

func (r Report) HasDrift() bool { return len(r.Entries) > 0 }

// Detect 逐个资源比较期望态与集群观测态的内容哈希，返回漂移明细。
// 判定依据：期望哈希 != 观测哈希（Modified）或集群中缺失（Missing）。
func Detect(c *cluster.Cluster, desired state.DesiredState) (Report, error) {
	rep := Report{Env: desired.Env}
	for _, want := range desired.Resources {
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
	sort.Slice(rep.Entries, func(i, j int) bool { return rep.Entries[i].Resource < rep.Entries[j].Resource })
	return rep, nil
}

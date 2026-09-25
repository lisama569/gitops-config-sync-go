// Package drift 比较期望态与观测态并判定漂移。
//
// 判定原则（依据观测态保存的 LastAppliedSpec —— 协调者最近一次写入的 Spec 快照）：
//   - SpecChanged：当前 Spec 与 LastAppliedSpec 不一致，即上次收敛后被人手工改过。
//     判定依据（字段路径 + 期望值/观测值）随 DriftRecord.Diffs 返回；
//     检测器绝不静默纠正，只按 Policy 标记（写观测态漂移标记）和/或告警；
//   - Outdated：当前 Spec 等于 LastAppliedSpec、但与新期望不一致，
//     即期望态发生了合法配置升级、观测态等待重新发布（允许同步收敛）；
//   - Missing：期望态存在、观测态缺失（尚未同步）；
//   - Extra：观测态存在托管标记但不在期望态中。
//
// 期望态与观测态 Spec 完全一致时为无漂移，重复协调因此幂等。
package drift

import (
	"fmt"
	"sort"

	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

// Policy 检测到人工改动后的处理策略。
type Policy string

const (
	// PolicyMark 只标记漂移，不纠正。
	PolicyMark Policy = "Mark"
	// PolicyAlert 只告警，不纠正。
	PolicyAlert Policy = "Alert"
	// PolicyAlertAndMark 标记并告警。
	PolicyAlertAndMark Policy = "AlertAndMark"
)

// Detector 漂移检测器。
type Detector struct {
	policy Policy
}

// New 创建检测器。
func New(policy Policy) *Detector {
	if policy == "" {
		policy = PolicyAlertAndMark
	}
	return &Detector{policy: policy}
}

// Detect 逐资源比较，返回带字段级依据的漂移记录。
// 记录按资源 ID 排序，保证同一输入判定结果可复现。
//
// 如何区分“人工改动”与“合法的配置升级”（核心依据是观测态保存的
// LastAppliedSpec —— 最近一次由协调者写入的 Spec 快照）：
//   - 当前 Spec != LastAppliedSpec：上次收敛后资源被人手工改过
//     -> SpecChanged（按策略标记/告警，不自动纠正）；
//   - 当前 Spec == LastAppliedSpec、但与新期望不一致：期望态本身升级了，
//     观测态只是停留在旧发布版本 -> Outdated（配置缺口，允许同步收敛）；
//   - 期望态有、观测态无 -> Missing；观测态托管但期望态无 -> Extra。
func (d *Detector) Detect(desired *types.Rendered, observed map[types.ResourceID]*types.ObservedResource) []types.DriftRecord {
	var records []types.DriftRecord

	ids := make([]types.ResourceID, 0, len(desired.Resources))
	for id := range desired.Resources {
		ids = append(ids, id)
	}
	sortIDs(ids)
	for _, id := range ids {
		want := desired.Resources[id]
		got, ok := observed[id]
		if !ok {
			records = append(records, types.DriftRecord{ID: id, Kind: types.DriftMissing, Managed: false})
			continue
		}
		diffs := compareSpec(want.Spec, got.Spec, "")
		if len(diffs) > 0 {
			kind := types.DriftOutdated
			// 当前观测值偏离了“我们最后写入的值” -> 人工改动。
			// 旧记录可能没有 LastAppliedSpec，退化为按内容版本判断。
			if got.LastAppliedSpec != nil {
				if manualDiffs := compareSpec(got.LastAppliedSpec, got.Spec, ""); len(manualDiffs) > 0 {
					kind = types.DriftSpecChanged
				}
			} else if got.AppliedRevision == desired.Revision {
				kind = types.DriftSpecChanged
			}
			records = append(records, types.DriftRecord{
				ID: id, Kind: kind, Diffs: diffs, Managed: got.Managed,
			})
			continue
		}
		// Spec 已与期望一致：清除历史人工改动嫌疑，无漂移。
	}

	// Extra：观测态中受本系统托管、但期望态里已不存在的资源。
	extraIDs := make([]types.ResourceID, 0)
	for id, got := range observed {
		if got.Managed {
			if _, inDesired := desired.Resources[id]; !inDesired {
				extraIDs = append(extraIDs, id)
			}
		}
	}
	sortIDs(extraIDs)
	for _, id := range extraIDs {
		records = append(records, types.DriftRecord{ID: id, Kind: types.DriftExtra, Managed: true})
	}

	return records
}

// HasDrift 报告记录中是否存在任何漂移。
func HasDrift(records []types.DriftRecord) bool {
	for _, r := range records {
		if r.Kind != types.DriftNone {
			return true
		}
	}
	return false
}

// Policy 返回当前策略。
func (d *Detector) Policy() Policy { return d.policy }

// ShouldMark 是否应写漂移标记。
func (d *Detector) ShouldMark() bool { return d.policy == PolicyMark || d.policy == PolicyAlertAndMark }

// ShouldAlert 是否应发出告警。
func (d *Detector) ShouldAlert() bool {
	return d.policy == PolicyAlert || d.policy == PolicyAlertAndMark
}

// compareSpec 递归比较两个 spec，返回字段级差异（路径按序）。
func compareSpec(want, got map[string]any, prefix string) []types.FieldDiff {
	var diffs []types.FieldDiff
	keysSet := map[string]struct{}{}
	for k := range want {
		keysSet[k] = struct{}{}
	}
	for k := range got {
		keysSet[k] = struct{}{}
	}
	keys := make([]string, 0, len(keysSet))
	for k := range keysSet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		wv, wok := want[k]
		gv, gok := got[k]
		switch {
		case !wok:
			diffs = append(diffs, types.FieldDiff{Path: path, Expected: "<absent>", Observed: gv})
		case !gok:
			diffs = append(diffs, types.FieldDiff{Path: path, Expected: wv, Observed: "<absent>"})
		default:
			wm, wIsMap := wv.(map[string]any)
			gm, gIsMap := gv.(map[string]any)
			if wIsMap && gIsMap {
				diffs = append(diffs, compareSpec(wm, gm, path)...)
			} else if !equalValue(wv, gv) {
				diffs = append(diffs, types.FieldDiff{Path: path, Expected: wv, Observed: gv})
			}
		}
	}
	return diffs
}

func equalValue(a, b any) bool {
	if al, ok := a.([]any); ok {
		bl, bok := b.([]any)
		if !bok || len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !equalValue(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	return fmt.Sprintf("%#v", a) == fmt.Sprintf("%#v", b)
}

func sortIDs(ids []types.ResourceID) {
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Kind != ids[j].Kind {
			return ids[i].Kind < ids[j].Kind
		}
		return ids[i].Name < ids[j].Name
	})
}

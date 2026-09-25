package types

import (
	"encoding/json"
	"time"
)

// revisionJSON 是 Revision 落盘时的 JSON 形态（资源用有序数组，保证可复现）。
type revisionJSON struct {
	Number          int64       `json:"number"`
	DesiredRevision int64       `json:"desiredRevision"`
	Env             string      `json:"env"`
	RenderedAt      time.Time   `json:"renderedAt"`
	AppliedAt       time.Time   `json:"appliedAt"`
	Resources       []*Resource `json:"resources"`
}

// MarshalJSON 把资源 map 编码为有序数组。
func (r Revision) MarshalJSON() ([]byte, error) {
	if r.Resources == nil {
		return json.Marshal(revisionJSON{
			Number: r.Number, DesiredRevision: r.DesiredRevision, Env: r.Env,
			RenderedAt: r.RenderedAt, AppliedAt: r.AppliedAt,
		})
	}
	list := make([]*Resource, 0, len(r.Resources))
	for _, res := range r.Resources {
		list = append(list, res)
	}
	sortResources(list)
	return json.Marshal(revisionJSON{
		Number:          r.Number,
		DesiredRevision: r.DesiredRevision,
		Env:             r.Env,
		RenderedAt:      r.RenderedAt,
		AppliedAt:       r.AppliedAt,
		Resources:       list,
	})
}

// UnmarshalJSON 从有序数组恢复资源 map。
func (r *Revision) UnmarshalJSON(b []byte) error {
	var v revisionJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	r.Number = v.Number
	r.DesiredRevision = v.DesiredRevision
	r.Env = v.Env
	r.RenderedAt = v.RenderedAt
	r.AppliedAt = v.AppliedAt
	r.Resources = make(map[ResourceID]*Resource, len(v.Resources))
	for _, res := range v.Resources {
		r.Resources[res.ID()] = res
	}
	return nil
}

func sortResources(list []*Resource) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			if list[j].Kind < list[j-1].Kind ||
				(list[j].Kind == list[j-1].Kind && list[j].Name < list[j-1].Name) {
				list[j], list[j-1] = list[j-1], list[j]
			} else {
				break
			}
		}
	}
}

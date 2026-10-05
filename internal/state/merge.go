package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
)

// LoadSource 从本地 JSON 文件加载一个配置来源。
// 文件缺失时返回 *RejectError{Reason: ReasonSourceMissing}。
func LoadSource(path string, priority int) (Source, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Source{}, &RejectError{
				Reason: ReasonSourceMissing,
				Source: path,
				Detail: "source file does not exist",
			}
		}
		return Source{}, fmt.Errorf("read source %s: %w", path, err)
	}
	var payload struct {
		Resources map[string]map[string]any `json:"resources"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return Source{}, fmt.Errorf("parse source %s: %w", path, err)
	}
	if payload.Resources == nil {
		payload.Resources = map[string]map[string]any{}
	}
	return Source{Name: path, Priority: priority, Resources: payload.Resources}, nil
}

// candidate 是某字段在一个来源中的取值（已规范化，便于比较）。
type candidate struct {
	canonical string
	source    string
	priority  int
}

// resolve 选出字段最终值：最高优先级获胜；
// 若最高优先级并列且取值不同，则报 ConflictingOverride，不静默取其一。
func resolve(resource, field string, cands []candidate) (string, error) {
	best := cands[0]
	for _, c := range cands[1:] {
		if c.priority > best.priority {
			best = c
		}
	}
	var winners []candidate
	for _, c := range cands {
		if c.priority == best.priority {
			winners = append(winners, c)
		}
	}
	for _, w := range winners[1:] {
		if w.canonical != winners[0].canonical {
			names := make([]string, 0, len(winners))
			for _, x := range winners {
				names = append(names, x.source)
			}
			sort.Strings(names)
			return "", &RejectError{
				Reason: ReasonConflictingOverride,
				Source: strings.Join(names, ","),
				Field:  resource + "." + field,
				Detail: fmt.Sprintf("priority %d sources disagree on value", best.priority),
			}
		}
	}
	return winners[0].canonical, nil
}

// canonicalField 校验字段类型并给出规范化字符串表示；类型不合法时返回 *RejectError。
func canonicalField(src Source, resource, field string, raw any) (string, error) {
	bad := func(detail string) (string, error) {
		return "", &RejectError{
			Reason: ReasonInvalidFieldType,
			Source: src.Name,
			Field:  resource + "." + field,
			Detail: detail,
		}
	}
	switch field {
	case "dependsOn":
		arr, ok := raw.([]any)
		if !ok {
			return bad("dependsOn must be an array of strings")
		}
		parts := make([]string, 0, len(arr))
		for _, item := range arr {
			s, ok := item.(string)
			if !ok {
				return bad("dependsOn must be an array of strings")
			}
			parts = append(parts, s)
		}
		sort.Strings(parts)
		return strings.Join(parts, ","), nil
	case "replicas":
		num, ok := raw.(json.Number)
		if !ok {
			return bad("replicas must be an integer")
		}
		n, err := num.Int64()
		if err != nil || n < 0 {
			return bad("replicas must be a non-negative integer")
		}
		return strconv.FormatInt(n, 10), nil
	default:
		return bad("unknown field " + strconv.Quote(field))
	}
}

// Merge 按优先级合并多个来源。
// 标量字段（replicas/dependsOn）按整字段解析；config 按**键**为最小单位叠加：
// 高优先级来源只覆盖它显式设置的键，低优先级来源未被碰到的键原样保留。
// 同一字段/同一 config 键被同优先级来源赋予不同值时返回
// *RejectError{Reason: ReasonConflictingOverride}；同优先级来源设置互不相交的
// config 键不算冲突。字段类型不合法时返回 *RejectError{Reason: ReasonInvalidFieldType}。
func Merge(sources []Source) (map[string]Resource, error) {
	names := map[string]struct{}{}
	for _, src := range sources {
		for name := range src.Resources {
			names[name] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	merged := make(map[string]Resource, len(sorted))
	for _, name := range sorted {
		// 按字段收集各来源候选值；config 额外按键收集。
		perField := map[string][]candidate{}
		perConfigKey := map[string][]candidate{}
		for _, src := range sources {
			raw, ok := src.Resources[name]
			if !ok {
				continue
			}
			keys := make([]string, 0, len(raw))
			for k := range raw {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, field := range keys {
				if field == "config" {
					obj, ok := raw[field].(map[string]any)
					if !ok {
						return nil, &RejectError{
							Reason: ReasonInvalidFieldType,
							Source: src.Name,
							Field:  name + ".config",
							Detail: "config must be an object of string values",
						}
					}
					ck := make([]string, 0, len(obj))
					for k := range obj {
						ck = append(ck, k)
					}
					sort.Strings(ck)
					for _, k := range ck {
						s, ok := obj[k].(string)
						if !ok {
							return nil, &RejectError{
								Reason: ReasonInvalidFieldType,
								Source: src.Name,
								Field:  name + ".config." + k,
								Detail: "config value must be a string",
							}
						}
						perConfigKey[k] = append(perConfigKey[k], candidate{
							canonical: s,
							source:    src.Name,
							priority:  src.Priority,
						})
					}
					continue
				}
				canon, err := canonicalField(src, name, field, raw[field])
				if err != nil {
					return nil, err
				}
				perField[field] = append(perField[field], candidate{
					canonical: canon,
					source:    src.Name,
					priority:  src.Priority,
				})
			}
		}

		res := Resource{Name: name, Replicas: 1, Config: map[string]string{}}
		if cands, ok := perField["dependsOn"]; ok {
			v, err := resolve(name, "dependsOn", cands)
			if err != nil {
				return nil, err
			}
			if v != "" {
				res.DependsOn = strings.Split(v, ",")
			}
		}
		if cands, ok := perField["replicas"]; ok {
			v, err := resolve(name, "replicas", cands)
			if err != nil {
				return nil, err
			}
			n, _ := strconv.Atoi(v)
			res.Replicas = n
		}
		// config 逐键解析：只有同一个键被同优先级来源赋不同值才冲突。
		ckeys := make([]string, 0, len(perConfigKey))
		for k := range perConfigKey {
			ckeys = append(ckeys, k)
		}
		sort.Strings(ckeys)
		for _, k := range ckeys {
			v, err := resolve(name, "config."+k, perConfigKey[k])
			if err != nil {
				return nil, err
			}
			res.Config[k] = v
		}
		merged[name] = res
	}
	return merged, nil
}

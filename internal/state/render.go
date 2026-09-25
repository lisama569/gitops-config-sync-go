package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Render 将合并结果渲染为期望态：
// 资源按名称排序、Config 键有序，同一输入必得同一 Hash（可复现）。
func Render(env string, merged map[string]Resource) (DesiredState, error) {
	if env == "" {
		return DesiredState{}, fmt.Errorf("env must not be empty")
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)

	resources := make([]Resource, 0, len(names))
	for _, name := range names {
		r := merged[name]
		// 规范化：保证 nil 与空集合渲染一致。
		if r.Config == nil {
			r.Config = map[string]string{}
		}
		if r.Replicas == 0 {
			r.Replicas = 1
		}
		resources = append(resources, r)
	}

	ds := DesiredState{Env: env, Resources: resources}
	h, err := hashResources(env, resources)
	if err != nil {
		return DesiredState{}, err
	}
	ds.Hash = h
	return ds, nil
}

// hashResources 对期望态做规范化 JSON 序列化后取 SHA-256。
func hashResources(env string, resources []Resource) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Env       string     `json:"env"`
		Resources []Resource `json:"resources"`
	}{Env: env, Resources: resources}); err != nil {
		return "", fmt.Errorf("hash desired state: %w", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// HashResource 计算单个资源的内容哈希，供漂移检测使用。
func HashResource(r Resource) (string, error) {
	if r.Config == nil {
		r.Config = map[string]string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return "", fmt.Errorf("hash resource %s: %w", r.Name, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

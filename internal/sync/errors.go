package sync

import "fmt"

// Reason 描述同步阶段失败的可区分原因。
type Reason string

const (
	// ReasonDependencyMissing 依赖的资源在期望态中不存在。
	ReasonDependencyMissing Reason = "DependencyMissing"
	// ReasonDependencyCycle 依赖图存在环，无法分波次。
	ReasonDependencyCycle Reason = "DependencyCycle"
	// ReasonDependencyNotReady 某资源依赖的前置资源尚未健康，不能进入本波次。
	ReasonDependencyNotReady Reason = "DependencyNotReady"
	// ReasonApplyError 模拟“集群”应用动作中途出错。
	ReasonApplyError Reason = "ApplyError"
	// ReasonHealthCheckFailed 资源应用后健康检查失败。
	ReasonHealthCheckFailed Reason = "HealthCheckFailed"
	// ReasonRevisionMissing 回滚目标版本不存在。
	ReasonRevisionMissing Reason = "RevisionMissing"
)

// SyncError 携带分类原因、波次与资源定位，便于测试断言与日志核对。
type SyncError struct {
	Reason    Reason
	Env       string
	Wave      int
	Resource  string
	DependsOn string
	Detail    string
}

func (e *SyncError) Error() string {
	return fmt.Sprintf("sync failed [%s] env=%s wave=%d resource=%s dependsOn=%s: %s",
		e.Reason, e.Env, e.Wave, e.Resource, e.DependsOn, e.Detail)
}

// Is 支持 errors.Is 按 Reason 归类。
func (e *SyncError) Is(target error) bool {
	t, ok := target.(*SyncError)
	if !ok {
		return false
	}
	return t.Reason == e.Reason
}

func syncErr(reason Reason, env string, wave int, res, dep, detail string) *SyncError {
	return &SyncError{Reason: reason, Env: env, Wave: wave, Resource: res, DependsOn: dep, Detail: detail}
}

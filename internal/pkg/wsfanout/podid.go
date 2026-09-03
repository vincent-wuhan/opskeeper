package wsfanout

import (
	"fmt"
	"os"

	"github.com/google/uuid"
)

// EnvInstanceIDOverride 是显式覆盖 pod ID 的环境变量名。
//
// 与 internal/pkg/leader.NewManager 复用同一约定，便于运维排障时统一识别实例。
const EnvInstanceIDOverride = "OPSKEEPER_LEADER_INSTANCE_ID"

// NewPodID 构造本进程唯一标识。优先取环境变量显式覆盖；否则 hostname-uuid[:8]。
//
// 与 leader.Manager.InstanceID 同源（同样的派生算法），便于在 Redis key / 日志
// 中直观对应到 K8s Pod。
func NewPodID(override string) string {
	if override != "" {
		return override
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%s", host, uuid.NewString()[:8])
}

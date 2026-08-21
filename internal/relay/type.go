package relay

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/relay/balancer"
	"time"
)

// relayRun saves state shared by all attempts for one client request.
type relayRun struct {
	c                *gin.Context
	inAdapter        transformer.Inbound
	durationUnit     time.Duration
	internalRequest  *llm.Request
	metrics          *RelayMetrics
	iter             *balancer.Iterator
	group            dbmodel.Group
	piiFilterEnabled bool
	failedKeys       map[string]struct{}
}

// relayAttempt 保存一次上游通道尝试的状态。
type relayAttempt struct {
	*relayRun
	outAdapter      transformer.Outbound
	channel         *dbmodel.Channel
	usedKey         dbmodel.ChannelKey
	statusCode      int           // 上游 HTTP 状态码
	retryAfter      time.Duration // 429 响应中 Retry-After 指定的冷却时长
	keyCooldown     time.Duration // 临时冷却时长：慢流、流中断等需快速降低该 key 使用率
	rateLimited     bool          // 本次尝试因本地限流失败
	rateLimitWait   time.Duration // 限流等待时间
	responseWritten bool          // true after streaming writes a client-visible event
	canceled        bool          // true when the client canceled this attempt
	upstreamURL     string        // 上游完整请求 URL(request.URL)，用于失败日志
	tryNextKey      bool          // true: 试同一渠道下一个 key; false: 切下一渠道
}

func relayKey(channelID, keyID int) string {
	return fmt.Sprintf("%d:%d", channelID, keyID)
}

package relay

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/relay/balancer"
)

// relayRun 保存一次客户端请求在负载均衡循环中共享的状态。
type relayRun struct {
	c               *gin.Context
	inAdapter       transformer.Inbound
	internalRequest *llm.Request
	metrics         *RelayMetrics
	iter            *balancer.Iterator
	group           dbmodel.Group
}

// relayAttempt 保存一次上游通道尝试的状态。
type relayAttempt struct {
	*relayRun
	outAdapter    transformer.Outbound
	channel       *dbmodel.Channel
	usedKey       dbmodel.ChannelKey
	statusCode    int           // 上游 HTTP 状态码
	retryAfter    time.Duration // 429 响应中 Retry-After 指定的冷却时长
	rateLimited   bool          // 本次尝试因本地限流失败
	rateLimitWait time.Duration // 限流等待时间
}

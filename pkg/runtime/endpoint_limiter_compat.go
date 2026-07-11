package runtime

import (
	"time"

	"github.com/redis/go-redis/v9"
)

// EndpointLimiter is an empty transition type kept only until the API
// bootstrap removes its pre-v2 limiter option.
type EndpointLimiter interface{}

// NewRedisEndpointLimiter preserves bootstrap source compatibility while the
// removed v1 heartbeat/claim endpoints have no limiter to configure.
func NewRedisEndpointLimiter(redis.UniversalClient, string, time.Duration) EndpointLimiter {
	return nil
}

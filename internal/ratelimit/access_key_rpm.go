// Package ratelimit defines AccessKey RPM decisions shared by the Redis
// limiter and the gateway.
package ratelimit

import "time"

type LimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// RetryAfter returns when a request rejected at now may retry, given the
// admission time of the window entry that must expire first.
func RetryAfter(target, now time.Time) time.Duration {
	retryAfter := ceilToSecond(target.Add(time.Minute).Sub(now))
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	if retryAfter > time.Minute {
		retryAfter = time.Minute
	}
	return retryAfter
}

func ceilToSecond(duration time.Duration) time.Duration {
	if duration <= 0 {
		return duration
	}
	return ((duration + time.Second - 1) / time.Second) * time.Second
}

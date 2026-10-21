package api

import (
	"testing"
	"time"
)

// TestRetryAfterSecondsJitter Retry-After 向上取整、至少 1 秒；建议间隔不到 5 秒不加抖动，否则随机往后加 0 到 min(间隔/5s, 3) 秒，
// 不早于真实恢复时间
func TestRetryAfterSecondsJitter(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		wait     time.Duration
		min, max int64
		variants int
	}{
		{wait: -time.Second, min: 1, max: 1},
		{wait: 500 * time.Millisecond, min: 1, max: 1},
		{wait: 4 * time.Second, min: 4, max: 4},
		{wait: 5 * time.Second, min: 5, max: 6},
		{wait: 12 * time.Second, min: 12, max: 14},
		{wait: 90 * time.Second, min: 90, max: 93, variants: 2},
		{wait: 3 * time.Hour, min: 10800, max: 10803},
	} {
		seen := make(map[int64]struct{})
		for range 200 {
			seconds := retryAfterSeconds(now.Add(test.wait), now)
			if seconds < test.min || seconds > test.max {
				t.Fatalf("建议间隔 %s：Retry-After = %d，期望 %d 到 %d", test.wait, seconds, test.min, test.max)
			}
			seen[seconds] = struct{}{}
		}
		if len(seen) < test.variants {
			t.Fatalf("建议间隔 %s：200 次只出现 %d 种取值，期望至少 %d 种", test.wait, len(seen), test.variants)
		}
	}
}

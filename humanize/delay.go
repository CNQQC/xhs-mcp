package humanize

import (
	"context"
	"time"
)

func Delay(ctx context.Context, action Action) {
	sleep(ctx, sample(action))
}

// DelaySince 停留时长从 start 起算：已过去的时间计入停留，只补剩下的部分。
// 用在本来就要等别的东西的地方，让人为停留与那段等待重叠，而不是叠加。
func DelaySince(ctx context.Context, action Action, start time.Time) {
	sleep(ctx, sample(action)-time.Since(start))
}

func sample(action Action) time.Duration {
	dist, ok := defaultProvider.Timing()[action]
	if !ok {
		dist = defaultProvider.Timing()[AfterClick]
	}
	return dist.Sample()
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

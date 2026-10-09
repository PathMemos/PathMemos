// Package purge 实现已物理删除的 OSS 对象的边缘缓存批量收敛：
// 删除时把公开 URL 记入 Redis 集合，后台任务定期分批调用阿里云 CDN 刷新接口。
// 队列属辅助状态（I2）：Redis 丢失仅导致少量对象错过刷新，不产生数据错误。
package purge

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

const (
	// QueueKey 待刷新 URL 集合的 Redis 键。
	QueueKey = "purge:oss:pending"
	// MaxBatch 单次刷新接口调用携带的 URL 数（RefreshObjectCaches 上限 2000，留余量）。
	MaxBatch = 500
	// MaxQueueSize 队列容量护栏：上游长期故障时丢弃新记录，防集合无限膨胀。
	MaxQueueSize = 100000
)

// ErrQueueFull 队列达到容量护栏。
var ErrQueueFull = errors.New("purge queue full")

// maxQueueSize 当前容量护栏（测试可覆盖）；MaxQueueSize 为默认值。
var maxQueueSize = int64(MaxQueueSize)

// Queue 是待刷新 URL 的 Redis 集合。
type Queue struct{ rdb *redis.Client }

func NewQueue(rdb *redis.Client) *Queue { return &Queue{rdb: rdb} }

// Record 记录一个刚被物理删除对象的公开 URL。
func (q *Queue) Record(ctx context.Context, objectURL string) error {
	n, err := q.rdb.SCard(ctx, QueueKey).Result()
	if err != nil {
		return err
	}
	if n >= maxQueueSize {
		return ErrQueueFull
	}
	return q.rdb.SAdd(ctx, QueueKey, objectURL).Err()
}

// Pop 随机弹出至多 n 个待刷新 URL（集合无序，处理顺序无关紧要）。
func (q *Queue) Pop(ctx context.Context, n int) ([]string, error) {
	return q.rdb.SPopN(ctx, QueueKey, int64(n)).Result()
}

// PushBack 刷新失败时把 URL 放回队列，等待下一轮。
func (q *Queue) PushBack(ctx context.Context, objectURLs []string) error {
	return q.rdb.SAdd(ctx, QueueKey, objectURLs).Err()
}

// Len 当前队列长度（观测用）。
func (q *Queue) Len(ctx context.Context) (int64, error) {
	return q.rdb.SCard(ctx, QueueKey).Result()
}

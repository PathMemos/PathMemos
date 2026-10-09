package ai

import (
	"context"
	"strings"
	"testing"
	"time"

	"papafeiji/backend/internal/config"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAiTurnKeys(t *testing.T) {
	k1, r1 := aiTurnKeys("u1", "hello", "req00000001")
	k2, r2 := aiTurnKeys("u1", "hello", "req00000001")
	if k1 != k2 || r1 != r2 {
		t.Fatalf("keys must be deterministic: %q/%q vs %q/%q", k1, r1, k2, r2)
	}
	// 不同 request_id 必须产生不同键（同一消息的两轮主动发送不被误判为重放）。
	k3, _ := aiTurnKeys("u1", "hello", "req00000002")
	if k1 == k3 {
		t.Fatalf("different request ids must yield different keys")
	}
	// 无 request_id 时回退消息内容哈希：相同消息同键、不同消息不同键。
	m1, _ := aiTurnKeys("u1", "same", "")
	m2, _ := aiTurnKeys("u1", "same", "")
	m3, _ := aiTurnKeys("u1", "other", "")
	if m1 != m2 || m1 == m3 {
		t.Fatalf("fallback keys broken: %q %q %q", m1, m2, m3)
	}
	if !strings.HasPrefix(k1, "ai:turn:") || !strings.HasPrefix(r1, "ai:reply:") {
		t.Fatalf("unexpected key prefixes: %q %q", k1, r1)
	}
}

func TestStreamReplay_Chunking(t *testing.T) {
	full := strings.Repeat("你好世界abcdef", 100)
	var got strings.Builder
	err := streamReplay(full, func(chunk string) error {
		if n := len([]rune(chunk)); n > aiReplayChunkRunes {
			t.Fatalf("chunk too large: %d runes", n)
		}
		got.WriteString(chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("streamReplay: %v", err)
	}
	if got.String() != full {
		t.Fatalf("replay mismatch")
	}
}

func newTestReplayService(t *testing.T) (*Service, *redis.Client, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := &Service{rdb: rdb} // pool 为 nil：回放路径不应触碰 DB
	return svc, rdb, func() {
		rdb.Close()
		mr.Close()
	}
}

func TestChatWithPrompt_ReplayFromCache(t *testing.T) {
	svc, rdb, cleanup := newTestReplayService(t)
	defer cleanup()

	_, replyKey := aiTurnKeys("u1", "你好", "req00000002")
	if err := rdb.Set(context.Background(), replyKey, "缓存回复内容", aiReplyTTL).Err(); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var chunks []string
	got, _, err := svc.chatWithPrompt(context.Background(), "u1", "你好", func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	}, "", &config.SysConfig{}, "req00000002")
	if err != nil {
		t.Fatalf("chatWithPrompt: %v", err)
	}
	if got != "缓存回复内容" {
		t.Fatalf("unexpected reply: %q", got)
	}
	if strings.Join(chunks, "") != "缓存回复内容" {
		t.Fatalf("chunks mismatch: %q", strings.Join(chunks, ""))
	}
	// 回放路径不得创建/占用处理中标记。
	tk, _ := aiTurnKeys("u1", "你好", "req00000002")
	if exists, _ := rdb.Exists(context.Background(), tk).Result(); exists != 0 {
		t.Fatalf("replay path must not create turn marker")
	}
}

func TestChatWithPrompt_WaitsForInFlightTurn(t *testing.T) {
	svc, rdb, cleanup := newTestReplayService(t)
	defer cleanup()

	turnKey, replyKey := aiTurnKeys("u2", "在途消息", "req00000003")
	if err := rdb.Set(context.Background(), turnKey, "1", aiTurnKeyTTL).Err(); err != nil {
		t.Fatalf("seed turn marker: %v", err)
	}
	// 模拟在途请求 400ms 后完成并写入回复缓存。
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = rdb.Set(context.Background(), replyKey, "在途完成回复", aiReplyTTL).Err()
		_ = rdb.Del(context.Background(), turnKey).Err()
	}()

	var chunks []string
	got, _, err := svc.chatWithPrompt(context.Background(), "u2", "在途消息", func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	}, "", &config.SysConfig{}, "req00000003")
	if err != nil {
		t.Fatalf("chatWithPrompt: %v", err)
	}
	if got != "在途完成回复" {
		t.Fatalf("unexpected reply: %q", got)
	}
	if strings.Join(chunks, "") != "在途完成回复" {
		t.Fatalf("chunks mismatch: %q", strings.Join(chunks, ""))
	}
}

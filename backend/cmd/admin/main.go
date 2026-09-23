// admin 提供一个一次性的命令行工具，用于按手机号彻底删除用户及其数据。
// 仅在运维场景下通过 deploy/delete-user.sh 调用，不会作为常驻服务运行。
// Package main provides related functionality.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/jackc/pgx/v5"
	goredis "github.com/redis/go-redis/v9"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/family"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/jobs"
	mw "papafeiji/backend/internal/middleware"
	"papafeiji/backend/internal/redis"
	"papafeiji/backend/internal/user"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "jobs":
			if len(args) == 2 && args[1] == "status" {
				withRedis(func(rdb *goredis.Client) { printJobStatus(context.Background(), rdb) })
				return
			}
		case "job":
			if len(args) == 3 && args[1] == "run" {
				withRedis(func(rdb *goredis.Client) { triggerJob(context.Background(), rdb, args[2]) })
				return
			}
		}
		log.Fatalf("unknown admin command %q; usage: jobs status | job run <name>（无参数时按 TARGET_PHONE 删除用户）", args)
	}
	deleteUserByPhone()
}

// deleteUserByPhone 是既有运维语义：按 TARGET_PHONE 彻底删除用户及其数据。
func deleteUserByPhone() {
	phone := os.Getenv("TARGET_PHONE")
	if phone == "" {
		log.Fatal("TARGET_PHONE is required")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	redisAddr := os.Getenv("REDIS_ADDR")
	if databaseURL == "" || redisAddr == "" {
		log.Fatal("DATABASE_URL and REDIS_ADDR are required")
	}

	ctx := context.Background()

	pgPool, err := db.NewPool(databaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pgPool.Close()
	pool := db.WrapPool(pgPool)

	rdb, err := redis.NewClient(redisAddr)
	if err != nil {
		log.Fatalf("connect redis: %v", err)
	}
	defer rdb.Close() //nolint:errcheck

	lock := db.NewAdvisoryLock(pgPool)
	sessions := mw.NewSessionManager(rdb)
	familyService := family.NewService(pool, rdb, lock, "")

	// 与 server main.go 一致：本地存储路径读 STORAGE_LOCAL_PATH（生产为 /opt/papafeiji/uploads），
	// 否则 NewStorage("") 回落 DefaultUploadDir(/opt/pathmemos/uploads) 会导致删除用户时物理文件残留。
	storage := file.NewStorage(os.Getenv("STORAGE_LOCAL_PATH")).WithBaseURL(strings.TrimSuffix(os.Getenv("OSS_PUBLIC_URL"), "/"))
	if os.Getenv("OSS_ACCESS_KEY_ID") != "" && os.Getenv("OSS_ACCESS_KEY_SECRET") != "" &&
		os.Getenv("OSS_ENDPOINT") != "" && os.Getenv("OSS_BUCKET") != "" {
		ossClient, err := oss.New(
			os.Getenv("OSS_ENDPOINT"),
			os.Getenv("OSS_ACCESS_KEY_ID"),
			os.Getenv("OSS_ACCESS_KEY_SECRET"),
		)
		if err != nil {
			log.Fatalf("init oss client: %v", err)
		}
		// 与 server 端一致：OSS 删除/上传走带超时的 HTTP 客户端，避免运维工具无限挂起。
		ossClient.HTTPClient = config.HTTPClient()
		ossBucket, err := ossClient.Bucket(os.Getenv("OSS_BUCKET"))
		if err != nil {
			log.Fatalf("init oss bucket: %v", err)
		}
		storage.WithOSS(file.NewOSSStore(ossBucket, strings.TrimSuffix(os.Getenv("OSS_PUBLIC_URL"), "/")))
	}

	var userID, currentFamilyID, personalFamilyID string
	err = pool.Pool().QueryRow(ctx,
		`SELECT id, current_family_id, personal_family_id FROM users WHERE phone_number = $1`,
		phone,
	).Scan(&userID, &currentFamilyID, &personalFamilyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fmt.Printf("user with phone %s not found, nothing to delete\n", phone)
			return
		}
		log.Fatalf("find user by phone %s: %v", phone, err)
	}

	fmt.Printf("found user %s, current_family=%s, personal_family=%s\n", userID, currentFamilyID, personalFamilyID)

	// 先删除用户所有 session，再删 DB，避免“DB 已删、session 仍可用”。
	// Redis 等依赖故障时关键安全校验不允许降级放行，删除失败必须中止。
	if err := sessions.DeleteAll(ctx, userID); err != nil {
		log.Fatalf("delete user sessions failed: %v", err)
	}

	cleanup, err := familyService.DeleteAccount(ctx, userID)
	if err != nil {
		log.Fatalf("delete account failed: %v", err)
	}

	fmt.Printf("delete account succeeded, affected=%d, files=%d, family=%s\n", len(cleanup.AffectedUserIDs), len(cleanup.Paths), cleanup.FamilyID)

	// 同步清理给 2 分钟上限：OSS 删除卡住时运维工具不应无限挂起。
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	user.CleanupAfterAccountDeletion(cleanupCtx, pool, nil, sessions, storage, cleanup, userID)

	if cleanup.MarkerDeletable() {
		fmt.Printf("deleted avatar marker %s\n", cleanup.MarkerPath)
	}

	fmt.Println("done")
}

// withRedis 连接 Redis 后执行运维子命令（只读状态/写触发键，不触碰 DB）。
func withRedis(run func(*goredis.Client)) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		log.Fatal("REDIS_ADDR is required")
	}
	rdb, err := redis.NewClient(addr)
	if err != nil {
		log.Fatalf("connect redis: %v", err)
	}
	defer rdb.Close() //nolint:errcheck // 一次性运维 CLI，退出前关闭连接
	run(rdb)
}

// printJobStatus 打印每个后台任务的最近成功/失败时间与连续失败次数（R-03）。
func printJobStatus(ctx context.Context, rdb *goredis.Client) {
	orDash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	fmt.Printf("%-28s %-24s %-24s %s\n", "JOB", "LAST_SUCCESS", "LAST_FAILURE", "FAIL_STREAK")
	for _, name := range jobs.KnownJobNames() {
		var last, fail, streak string
		if v, err := rdb.Get(ctx, jobs.JobLastSuccessKey(name)).Result(); err == nil {
			last = v
		}
		if v, err := rdb.Get(ctx, jobs.JobLastFailureKey(name)).Result(); err == nil {
			fail = v
		}
		if v, err := rdb.Get(ctx, jobs.JobFailStreakKey(name)).Result(); err == nil {
			streak = v
		}
		fmt.Printf("%-28s %-24s %-24s %s\n", name, orDash(last), orDash(fail), orDash(streak))
	}
}

// triggerJob 写入一次性触发键；常驻 app 的 watchJobHealth 在 1 分钟内读取并复用锁+幂等执行（R-03）。
func triggerJob(ctx context.Context, rdb *goredis.Client, name string) {
	known := false
	for _, n := range jobs.KnownJobNames() {
		if n == name {
			known = true
			break
		}
	}
	if !known {
		log.Fatalf("unknown job %q (known: %v)", name, jobs.KnownJobNames())
	}
	if err := rdb.Set(ctx, jobs.JobTriggerKey(name), "1", 0).Err(); err != nil {
		log.Fatalf("set job trigger: %v", err)
	}
	fmt.Printf("trigger set for job %s; the running app picks it up within 1 minute\n", name)
}

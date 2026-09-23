# PP-04 数据库 Schema（L4）

> 层级：L4 数据库 Schema｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：无
> 说明：本文件按当前代码实现整理，描述当前数据库 schema。章节遵循 docs/spec-standards.md 第二节 L4 必含小节（ER 概览 / 字段级表定义 / 数据归属说明 / 数据字典 / 迁移变更流程）。
> **代码内唯一事实源**：backend/migrations/*.up.sql（尤其 000001_baseline.up.sql）与 backend/internal/db/sqlc/models.go。
> 校验基线：迁移最终合计 **23 张表**（baseline 创建 23 张含 sys_configs；000004 新增 client_ops_logs、000006 删除 sys_configs，净 23），models.go 同步为 23 个 struct。

## 1. 概述

- 数据库：PostgreSQL；主键为 text 或 text+date 复合（无自增整数主键）。
- 无软删除列：全部业务表使用**物理删除**（多为 ON DELETE CASCADE / SET NULL），没有 deleted_at / is_deleted 字段。
- 时间列统一 timestamptz，数据库会话时区设为 Asia/Shanghai（db/db.go 连接级 RuntimeParams）。
- 唯一约束与部分唯一索引共同承担幂等（支付回调、VIP 领取、邀请、家庭成员）。
- 迁移文件成对 up/down（spec-standards 第九节阻断级 3）。

## 2. ER 概览（ASCII）

用户是绝大多数表的归属中心；家庭是日记与部分文件共享的协作中心。

    users (id)
      |-- N:1 personal_family_id / current_family_id --> families (id)
      |-- family_members (user_id, family_id) --> families
      |-- 1:N diaries (user_id) 1:N diary_entries (diary_id, created_by)
      |                            |-- diary_entry_images (diary_entry_id) --> files (id)
      |-- 1:N memories (user_id)
      |-- 1:1 user_vips (user_id) ; 1:N user_vip_claims (user_id, vip_id) --> vips
      |-- 1:N orders (user_id nullable) --> vips
      |-- 1:1 api_keys (user_id)
      |-- 1:1 user_invite_codes (user_id)
      |-- 1:N user_invites (user_id = 被邀请人, inviter_id = 邀请人) -- users 自关联
      |-- 1:1 user_avatar_markers (user_id)
      |-- 1:N user_common_addresses (user_id, name)   [无 FK]
      |-- 1:N ai_daily_quota_usage (user_id, quota_date) [无 FK]
      |-- 1:N ai_dialog_logs (user_id)
      |-- 1:N auto_record_trajectories (user_id)
      |-- 0/1:N wx_mp_accounts (user_id nullable, mp_openid, unionid)
      |-- 1:N client_ops_logs (user_id)        [000004 新增]
      |-- 1:N files (created_by nullable)

    families (id)
      |-- family_members
      |-- family_daily_covers (family_id, record_date) --> files (cover_file_id / manual_cover_file_id)

    vips            # 全局商品目录

关键跨表说明：
- diary_entries 的归属靠 created_by（写日记的人），可见性靠 diaries.user_id 与 users.current_family_id（家庭共享）。
- family_daily_covers 归属家庭+日期，不含 user_id。
- diary_entry_images 仅关联 diary_entries 与 files，无 user_id。
- files 通过 created_by 归属；系统文件（file_type='system'）用 metadata.family_id 归属家庭。

## 3. 逐表字段定义

字段来源：backend/migrations/000001_baseline.up.sql（CREATE TABLE）与 000004。类型为 PostgreSQL 类型。NULL 列中 Y 表示可空。

### 3.1 users（用户账号）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | 用户 UUID |
| open_id | text | N | 唯一索引 idx_users_openid | 微信小程序 openid |
| unionid | text | Y | 部分唯一 idx_users_unionid | 微信 unionid |
| phone_number | text | Y | 部分唯一 idx_users_phone_number | 手机号 |
| avatar | text | Y | CHECK length <= 2048 | 头像 URL |
| avatar_file_id | text | Y | FK files(id) ON DELETE SET NULL；部分索引 idx_users_avatar_file_id | 头像文件 |
| nickname | text | Y | | 昵称 |
| user_type | text | N | default 'wechat'；CHECK = 'wechat' | 仅微信用户（当前单值 'wechat'，预留扩展） |
| phone_bind_time | timestamptz | Y | | 最近绑定手机时间（日限判断） |
| auto_record_enabled | boolean | N | default false；索引 idx_users_auto_record_enabled | 自动记录开关 |
| personal_family_id | text | Y | FK families(id) ON DELETE RESTRICT；索引 idx_users_personal_family | 个人家庭 |
| current_family_id | text | Y | FK families(id) ON DELETE RESTRICT；索引 idx_users_current_family | 当前家庭 |
| created_at | timestamptz | N | now() | |
| updated_at | timestamptz | N | now() | |
| session_key | text | Y | | 微信 session_key |
| invited_by | text | Y | FK users(id) ON DELETE SET NULL；索引 idx_users_invited_by | 邀请人 |
| abnormal_subscribe_accepted | boolean | N | default false | 异常提醒订阅是否接受 |
| abnormal_alert_sent_at | timestamptz | Y | | 最近异常提醒发送时间 |
| last_active_at | timestamptz | Y | | 最近活跃（轨迹心跳） |
| lang | text | N | default 'zh' | 语言 |
| image_storage_bytes | bigint | N | default 0；CHECK >= 0 | 图片存储占用（配额） |

### 3.2 families（家庭）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | 家庭 UUID |
| is_personal | boolean | N | default false | 是否个人家庭 |
| created_at | timestamptz | N | now() | |

### 3.3 family_members（家庭成员）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | 关系 UUID |
| family_id | text | N | FK families(id) ON DELETE CASCADE；UNIQUE(family_id,user_id) | |
| user_id | text | N | FK users(id) ON DELETE CASCADE；UNIQUE(user_id) DEFERRABLE INITIALLY DEFERRED | 一个用户同时只属于一个家庭 |
| role | text | N | default 'member'；CHECK owner/member | |
| joined_at | timestamptz | N | now() | |

索引：idx_family_members_family_role (family_id, role)。

### 3.4 diaries（日记，每人每天一条）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N | FK users(id) ON DELETE CASCADE；UNIQUE(user_id, record_date) | |
| record_date | date | N | | 记录日期 |
| created_at | timestamptz | N | now() | |
| updated_at | timestamptz | N | now() | |

### 3.5 diary_entries（日记条目）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| diary_id | text | N | FK diaries(id) ON DELETE CASCADE | |
| created_by | text | N | FK users(id) ON DELETE CASCADE | 条目作者 |
| text | text | Y | | 正文 |
| lat | numeric(10,7) | Y | CHECK -90..90 | |
| lon | numeric(10,7) | Y | CHECK -180..180 | |
| address | text | Y | CHECK length <= 500 | |
| detail_address | text | Y | CHECK length <= 500 | |
| record_time | timestamptz | Y | | |
| sort | integer | N | default 0 | |
| color | text | Y | CHECK 颜色正则 #RGB/#RRGGBB/#RRGGBBAA | |
| created_at | timestamptz | N | now() | |
| updated_at | timestamptz | N | now() | |
| 表级 | | | CHECK (lat IS NULL) = (lon IS NULL) | 经纬度成对 |

索引：idx_diary_entries_auto_last（部分索引 text='（自动记录）'）、created_at、created_by_created_at、created_by_record_time DESC、creator_address（部分索引，非空 address）、diary_created_at DESC、diary_sort_time、updated_at。

### 3.6 diary_entry_images（条目图片关联）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| diary_entry_id | text | N | FK diary_entries(id) ON DELETE CASCADE；UNIQUE(diary_entry_id,file_id) | |
| file_id | text | N | FK files(id)（无级联） | |
| sort_order | integer | N | default 0 | |
| created_at | timestamptz | N | now() | |

索引：idx_diary_entry_images_entry_id、idx_diary_entry_images_file_id。

### 3.7 family_daily_covers（家庭每日封面）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| family_id | text | N | PK 之一 | |
| record_date | date | N | PK 之一 | |
| cover_file_id | text | Y | FK files(id) ON DELETE SET NULL；部分索引 | |
| cover_type | text | N | default 'default'；CHECK image/trajectory/default/manual | |
| updated_at | timestamptz | N | now() | |
| manual_cover_file_id | text | Y | FK files(id) ON DELETE SET NULL；部分索引 | 手动指定封面 |

主键 (family_id, record_date)。`family_id` FK families(id) ON DELETE CASCADE（000009 补齐；迁移内先清孤儿行）。触发器 trg_family_daily_covers_fix_type：BEFORE INSERT OR UPDATE 执行 fix_cover_type_on_null_fk()，manual 且 manual_cover_file_id 为空时降级 cover_type='default'（保留 cover_file_id）；image/trajectory 且 cover_file_id 为空时降级 'default'。

### 3.8 memories（回忆）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | 也是 diaries.id（创建时同 ID upsert） |
| user_id | text | N | FK users(id) ON DELETE CASCADE | |
| record_time | timestamptz | N | | |
| record_date | date | N | | |
| title | text | N | | 应用层限制 1~50 字 |
| content | text | N | | 应用层限制 1~10000 字 |
| created_at | timestamptz | N | now() | |

索引：idx_memories_query_covering (user_id, record_date DESC, record_time DESC) INCLUDE (title, content)。

### 3.9 files（文件元数据）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| created_by | text | Y | FK users(id) ON DELETE SET NULL；索引 idx_files_created_by | 系统文件可为空 |
| path | text | N | CHECK length <= 255；索引 idx_files_path | |
| name | text | N | CHECK length <= 255 | |
| suffix | text | N | CHECK length <= 32 | |
| size_bytes | bigint | N | default 0 | |
| file_type | text | N | default 'image'；CHECK image/system | |
| metadata | jsonb | Y | GIN 索引 idx_files_metadata；部分索引 idx_files_metadata_family_record | 系统文件存 family_id/record_date |
| created_at | timestamptz | N | now() | |
| updated_at | timestamptz | N | now() | |
| storage_type | text | N | default 'local'；CHECK local/oss；索引 idx_files_storage_type | |

其他索引：idx_files_created_at、idx_files_file_type_created_at、idx_files_image_type_id。

### 3.10 vips（VIP 商品目录）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| type | text | N | CHECK month/year/trial/free | |
| name | text | N | | |
| time_limit_mark | text | N | CHECK day/month/year | |
| time_limit_number | integer | N | CHECK > 0 | |
| product_id | text | Y | | 微信虚拟支付商品 ID |
| sort | integer | N | default 0 | |
| is_active | boolean | N | default true；索引 idx_vips_is_active | |
| prices | jsonb | N | default '[]' | |
| created_at | timestamptz | N | now() | |

### 3.11 user_vips（用户 VIP 状态，一人一行）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N | FK users(id) ON DELETE CASCADE；UNIQUE(user_id) | |
| begin_time | timestamptz | N | | |
| expire_time | timestamptz | N | 索引 idx_user_vips_expire_time | |
| created_at | timestamptz | N | now() | |
| 表级 | | | CHECK expire_time > begin_time | |

### 3.12 user_vip_claims（VIP 领取防重）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N→Y | FK users(id) ON DELETE **SET NULL**（000008：注销保留墓碑行）；UNIQUE(user_id,vip_id) | |
| vip_id | text | N | FK vips(id) | |
| open_id | text | Y | 000008 新增：发放主体微信 openid（写入时冗余，历史回填） | 墓碑行（user_id NULL）凭 openid 防"注销重注册重领" |
| created_at | timestamptz | N | now() | |

索引：idx_user_vip_claims_user_created、idx_user_vip_claims_vip_id、**uq_user_vip_claims_open_id_vip_id**（部分唯一 `(open_id, vip_id) WHERE open_id IS NOT NULL`）。

### 3.13 orders（支付订单）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | Y | FK users(id) ON DELETE SET NULL；部分索引 idx_orders_null_user | 注销事务内 `NullifyOrdersByUser` 置空（FK SET NULL）产生无主订单，由关单任务 `CloseOwnerlessPendingOrders` 收敛 |
| vip_id | text | N | FK vips(id)；索引 idx_orders_vip_id | |
| out_trade_no | text | N | UNIQUE orders_out_trade_no_key；CHECK length <= 32 | |
| channel | text | N | CHECK = 'virtual_pay' | 仅微信虚拟支付 |
| state | text | N | default 'pending'；CHECK pending/paid/closed | |
| amount | integer | N | CHECK > 0 | 单位分 |
| prepay_id | text | Y | CHECK length <= 128 | （预留：后端不访问微信下单，当前无写入点，恒 NULL） |
| transaction_id | text | Y | UNIQUE orders_transaction_id_key + 部分唯一 uq_orders_transaction_id_not_null；CHECK length <= 128 | 微信支付单号，幂等键 |
| paid_at | timestamptz | Y | | |
| created_at | timestamptz | N | now() | |
| updated_at | timestamptz | N | now() | |

索引：idx_orders_pending_created_at（部分索引 state='pending'）、idx_orders_state_created_at、idx_orders_user_id_state_created、idx_orders_user_vip_state。

### 3.14 ai_dialog_logs（AI 对话日志）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N | FK users(id) ON DELETE CASCADE | |
| role | text | N | CHECK user/assistant/system | |
| content | text | N | | |
| created_at | timestamptz | N | now() | |

索引：idx_ai_dialog_logs_created_at、idx_ai_dialog_logs_user_created_at。

> 保留策略：仅保留 90 天，由后台任务 `cleanup_ai_logs` 定期删除（按 created_at 最旧优先，无每用户条数上限）。

### 3.15 ai_daily_quota_usage（AI 每日配额用量）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| user_id | text | N | PK 之一 | **无外键** |
| quota_date | date | N | PK 之一 | |
| used | integer | N | default 0 | |
| created_at | timestamptz | N | now() | |
| updated_at | timestamptz | N | now() | |

主键 (user_id, quota_date)；索引 idx_ai_daily_quota_usage_date。无 FK 到 users，删除用户需依赖应用层清理（见 §4.1）。

### 3.16 api_keys（MCP API Key）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N | FK users(id) ON DELETE CASCADE；UNIQUE(user_id) | 一人一 Key |
| key_hash | text | N | 唯一索引 idx_api_keys_key_hash | SHA-256 |
| expires_at | timestamptz | N | 索引 idx_api_keys_expires_at | 认证时不判过期，固定 9999-12-31 |
| created_at | timestamptz | N | now() | |
| api_key | text | N | | 明文存储 |

### 3.17 auto_record_trajectories（GPS 轨迹点）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N | FK users(id) ON DELETE CASCADE | |
| lat | numeric(10,7) | N | CHECK -90..90 | |
| lon | numeric(10,7) | N | CHECK -180..180 | |
| recorded_at | timestamptz | N | default now() | |
| geocode_attempts | integer | N | default 0 | 逆地理重试计数 |
| created_at | timestamptz | N | now() | |

索引：idx_auto_record_trajectories_created_at、geocode_attempts、recorded_at、user_attempts、user_recorded；唯一索引 `uq_auto_record_trajectories_point (user_id, recorded_at, lat, lon)`（000005，轨迹上报幂等）。写入侧 `InsertTrajectories` 使用 `ON CONFLICT DO NOTHING`，与该唯一索引配合实现重复上报幂等。

> 保留策略：仅保留 7 天，由后台任务定期删除。

### 3.18 user_invites（邀请关系）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N→Y | FK users(id) ON DELETE **SET NULL**（000008：注销保留行）；UNIQUE(user_id) | 被邀请人，一人一条 |
| inviter_id | text | Y | FK users(id) ON DELETE SET NULL（000011） | 邀请人；置空保留行（被邀请奖励墓碑与邀请人存续解耦） |
| entry_count | integer | N | default 0 | |
| user_open_id | text | Y | 000008 新增：被邀请人微信 openid（写入时冗余，历史回填） | 被邀请奖励终身一次判定依据 |
| reward_inviter_at | timestamptz | Y | 部分索引 idx_user_invites_reward_inviter_at | 邀请人奖励下发时间 |
| reward_invitee_at | timestamptz | Y | 部分索引 idx_user_invites_pending（reward_invitee_at IS NULL） | 被邀请人奖励下发时间 |
| created_at | timestamptz | N | now() | |

索引：idx_user_invites_inviter_created、**uq_user_invites_user_open_id**（部分唯一 `(user_open_id) WHERE user_open_id IS NOT NULL AND reward_invitee_at IS NOT NULL`，被邀请奖励每微信主体终身一次）。

### 3.19 user_invite_codes（邀请短码）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| user_id | text | N | PK；FK users(id) ON DELETE CASCADE | |
| short_code | text | N | UNIQUE user_invite_codes_short_code_key；索引 idx_user_invite_codes_short_code_lookup | 8 位大写字母数字（去易混字符） |
| created_at | timestamptz | N | now() | |
| expires_at | timestamptz | Y | | |
| used_at | timestamptz | Y | | |

### 3.20 wx_mp_accounts（公众号账号绑定）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | Y | FK users(id) ON DELETE CASCADE；索引 idx_wx_mp_accounts_user_id | 关注先于注册时可为空 |
| mp_openid | text | N | UNIQUE wx_mp_accounts_mp_openid_key | 公众号 openid |
| unionid | text | Y | 部分唯一 idx_wx_mp_accounts_unionid | |
| nickname | text | Y | | |
| avatar | text | Y | CHECK length <= 2048 | |
| subscribed | boolean | N | default true | 关注状态 |
| subscribe_time | timestamptz | Y | | |
| last_interact_time | timestamptz | Y | | |
| created_at | timestamptz | N | now() | |
| updated_at | timestamptz | N | now() | |

### 3.21 user_common_addresses（用户常用地址聚合）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| user_id | text | N | PK 之一 | **无外键** |
| name | text | N | PK 之一 | |
| lat | numeric(10,7) | N | CHECK -90..90 | |
| lon | numeric(10,7) | N | CHECK -180..180 | |
| count | integer | N | default 0；CHECK >= 0 | 出现次数 |
| updated_at | timestamptz | N | now() | |

主键 (user_id, name)。无 FK 到 users（见 §4.1）。

> 写入来源：后台任务 `common_address_summary`（每日 03:00）经 `db.SummarizeUserCommonAddresses` 汇总；`/user/common-addresses/refresh` 同底层。

### 3.22 user_avatar_markers（头像地图标记）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| user_id | text | N | PK；FK users(id) ON DELETE CASCADE | |
| marker_path | text | N | | 标记图片路径 |
| updated_at | timestamptz | N | now() | |
| storage_type | text | N | default 'local'；CHECK local/oss | |

### 3.23 client_ops_logs（客户端操作日志，000004 新增）

| 字段 | 类型 | NULL | 默认 / 约束 | 说明 |
|------|------|------|-------------|------|
| id | text | N | PK | |
| user_id | text | N | FK users(id) ON DELETE CASCADE | |
| device | text | N | default '' | |
| app_version | text | N | default '' | |
| events | jsonb | N | | 事件数组 |
| client_sent_at | timestamptz | N | | 客户端发送时间 |
| created_at | timestamptz | N | now() | |

索引：idx_client_ops_logs_user_created (user_id, created_at DESC)。

> 保留策略：仅保留 30 天，由后台任务 `cleanup_client_ops_logs` 定期删除（任务总数 10）。
>
> 全库保留窗口汇总：ai_dialog_logs 90 天、auto_record_trajectories 7 天、client_ops_logs 30 天；孤儿文件扫描窗口为图片创建超 1 天且无引用、系统文件创建/更新均超 7 天且无封面引用、无 `user_avatar_markers.marker_path` 引用（file.sql ScanOrphanFiles / ScanOldSystemFiles）。

## 4. 数据归属说明

### 4.1 直接以 user_id 归属的表

| 表 | 归属列 | 外键 | 删除用户时 |
|----|--------|------|-----------|
| users | id | — | 主表 |
| diaries | user_id | users(id) CASCADE | 级联删 |
| diary_entries | created_by | users(id) CASCADE | 级联删 |
| memories | user_id | users(id) CASCADE | 级联删 |
| user_vips | user_id | users(id) CASCADE | 级联删 |
| user_vip_claims | user_id（可空） | users(id) SET NULL（000008） | 置空保留墓碑行 |
| api_keys | user_id | users(id) CASCADE | 级联删 |
| auto_record_trajectories | user_id | users(id) CASCADE | 级联删 |
| ai_dialog_logs | user_id | users(id) CASCADE | 级联删 |
| user_invites | user_id（可空）、inviter_id（可空） | users(id) SET NULL（000008/000011） | 双向置空保留行（领取/被邀请奖励墓碑） |
| user_invite_codes | user_id | users(id) CASCADE | 级联删 |
| user_avatar_markers | user_id | users(id) CASCADE | 级联删 |
| wx_mp_accounts | user_id（可空） | users(id) CASCADE | 级联删 |
| client_ops_logs | user_id | users(id) CASCADE | 级联删 |
| family_members | user_id | users(id) CASCADE | 级联删 |
| orders | user_id（可空） | users(id) SET NULL | 置空保留订单 |
| files | created_by（可空） | users(id) SET NULL | 置空保留文件 |
| ai_daily_quota_usage | user_id | **无 FK** | 需应用层清理 |
| user_common_addresses | user_id | **无 FK** | 需应用层清理 |

> `ai_daily_quota_usage` 与 `user_common_addresses` 的 `user_id` 无外键，注销清理依赖应用层（`family.DeleteAccount` / `user.CleanupAfterAccountDeletion`）。

### 4.2 无 user_id 归属表的豁免清单

以下 4 张表**不含 user_id 列**，属规格允许的豁免（归属由外键或全局语义决定）：

| 表 | 豁免理由 | 实际归属 |
|----|---------|---------|
| families | 家庭是共享协作实体，成员通过 family_members 关联 | family_members / users.current_family_id |
| family_daily_covers | 家庭每日封面 | (family_id, record_date)，经 family_members 校验 |
| diary_entry_images | 条目与文件的纯关联表 | diary_entry_id → diary_entries.created_by |
| vips | 全局商品目录 | 无用户归属 |

另有 2 张表用 created_by 而非 user_id 表达归属（仍属可归属，非豁免）：diary_entries、files。

## 5. 数据字典（枚举 / 状态 / 软删除）

### 5.1 枚举（由 CHECK 约束固化）

| 表.字段 | 允许值 | 源 |
|---------|--------|----|
| users.user_type | wechat | baseline CHECK |
| users.lang | 默认 zh（无 CHECK，应用层允许 zh / zh-Hant / en） | user/handler.go:202 |
| family_members.role | owner / member | baseline CHECK |
| diary_entries.color | 正则 ^#([0-9A-Fa-f]{3}\|[0-9A-Fa-f]{6}\|[0-9A-Fa-f]{8})$ | baseline CHECK |
| family_daily_covers.cover_type | image / trajectory / default / manual | baseline CHECK |
| files.file_type | image / system | baseline CHECK |
| files.storage_type | local / oss | baseline CHECK |
| user_avatar_markers.storage_type | local / oss | baseline CHECK |
| ai_dialog_logs.role | user / assistant / system | baseline CHECK |
| vips.type | month / year / trial / free | baseline CHECK |
| vips.time_limit_mark | day / month / year | baseline CHECK |
| orders.channel | virtual_pay（仅此一值） | baseline CHECK |
| orders.state | pending / paid / closed | baseline CHECK |

### 5.2 状态机：orders.state

    [创建订单] --pending--> paid      (支付回调/发货成功, 写 paid_at)
                  |
                  +--> closed         (用户 cancel 或 1 分钟后台任务关闭 24h 未支付)

    closed --(支付回调补记)--> paid    (MarkClosedOrderPaid，补发 VIP)

- 只有 pending 可被 CloseOrder 关闭；已 closed/paid 的并发 cancel 返回 200（payment/handler.go:125-152）。
- 已 closed 订单收到支付回调时由 `MarkClosedOrderPaid` 补记为 paid 并补发（payment/service.go `handleNotify`）。
- 后台关单（24h 未支付）`runOrderClose`：先用 `CloseOwnerlessPendingOrders` 循环关闭 `user_id IS NULL` 的无主 pending 订单（每批 ≤1000），再按 `id ASC` 游标分批关闭有主 pending 订单（jobs/runner.go `runOrderClose`）。
- 创建新订单前 `ClosePendingOrdersByUserAndVIP` 会先关闭同用户同 VIP、创建超 5 分钟的 pending 单（order.sql），防止重复挂单。
- 支付回调以 transaction_id 唯一约束保证幂等（orders_transaction_id_key + 部分唯一索引）。

### 5.3 状态机：家庭 / 成员

- families.is_personal：true=个人家庭（新用户默认）；false=共享家庭。
- users.current_family_id 指向当前家庭；personal_family_id 永远指向个人家庭。
- family_members.user_id 全局唯一（DEFERRABLE），保证同一时刻只在一个家庭；加入新家庭在可延迟约束内先删后插。
- family_daily_covers 由触发器保证 cover_type 与实际封面文件一致（见 3.7）。

### 5.4 软删除

- **无软删除**：所有表均为物理删除，无 deleted_at / is_deleted。
- 删除用户走 family.DeleteAccount 事务 + 后台清理；orders.user_id 与 files.created_by 因 SET NULL 而保留孤儿记录。
- ai_daily_quota_usage、user_common_addresses 无 FK，用户删除后需应用层显式清理（见 §4.1）。
- 注销事务除 FK 级联外还**显式执行** NullifyOrdersByUser、DeleteAPIKeyByUser、DeleteUserInviteCodeByUserID（family/service.go:822-830）——api_keys/邀请码的清理不单靠 CASCADE。

### 5.5 并发控制（DB/Redis 侧一览，详见各 L2 分册）

| 机制 | Key / 对象 | 使用位置 |
|------|-----------|---------|
| PG session 级 advisory lock（迁移互斥） | 常量 `0x6d6967726174696f`（ASCII "migratio"） | internal/migration/migrate.go |
| PG advisory try lock / unlock（业务互斥，无 TTL） | `hashtextextended("lock:family:{id}" / "lock:delete_account:{uid}" / "lock:auto_record:{uid}" / "lock:background:{task}" / "lock:covers:{family}:{date}" 等)` | internal/db/advisory_lock.go + 各域 |
| PG 事务级 advisory lock | `hashtext('inviter_reward:' || inviterID)` | db/sqlc/invite.sql |
| Redis SETNX（去重/节流/幂等标记，非互斥锁） | `wxmp:msgid:{msgID}`（60s）、`lock:covers:refresh:{familyID}:{date}`（30s）、AI turn key | wxmp、diary、ai 各域 |

## 6. 索引与约束要点

- users 其他索引：idx_users_avatar_file_id（avatar_file_id 非空部分索引）、idx_users_personal_family（personal_family_id）。
- 部分索引（含部分唯一索引）：
  - idx_users_phone_number、idx_users_unionid、idx_wx_mp_accounts_unionid（仅非空值唯一）。
  - uq_orders_transaction_id_not_null（transaction_id 非空且非空串时唯一）。
  - idx_orders_null_user（user_id IS NULL）。
  - idx_orders_pending_created_at（state='pending'）。
  - idx_family_daily_covers_cover_file_id / manual_cover_file_id。
  - idx_user_invites_pending（reward_invitee_at IS NULL）。
  - idx_files_metadata_family_record（file_type='system'）。
  - idx_diary_entries_creator_address（address 非空且非空串）。
  - idx_diary_entries_auto_last（text='（自动记录）'）。
- 覆盖索引：idx_memories_query_covering INCLUDE (title, content)。
- 延迟约束：uq_family_members_user_id UNIQUE DEFERRABLE INITIALLY DEFERRED。
- 函数与触发器：fix_cover_type_on_null_fk() + trg_family_daily_covers_fix_type（000002 重定义函数体）。

## 7. 迁移变更记录（000001 ~ 000011）

| 编号 | 文件 | 变更 | down 行为 | 可逆性 |
|------|------|------|-----------|--------|
| 000001 | 000001_baseline.up.sql / .down.sql | 合并基线（2026-08-15）：将历史 000001-000020 squash 为单一 baseline，内容以生产库 pg_dump --schema-only 为准。创建 23 张表（含 sys_configs）、1 个函数 fix_cover_type_on_null_fk、1 个触发器、全部主键/唯一约束/索引/外键；不含 schema_migrations | DROP SCHEMA public CASCADE; CREATE SCHEMA public | 破坏性（清空整库），仅用于全新环境 |
| 000002 | 000002_fix_cover_trigger_preserve_file_id.up.sql / .down.sql | CREATE OR REPLACE FUNCTION fix_cover_type_on_null_fk：manual→default 回退时保留 cover_file_id。注：baseline 的函数体已含相同保留逻辑，全新环境按序执行 000001→000002 时本迁移为 no-op；仅对 squash 前的存量库有实际差异 | 恢复旧函数体（manual→default 时清空 cover_file_id） | 可逆 |
| 000003 | 000003_seed.up.sql / .down.sql | 种子数据：INSERT sys_configs('default')（ai_config model=deepseek-v4-flash、baseUrl=https://api.deepseek.com；sys_config 默认封面/轨迹图标/头像/文件基址；ai_prompt 系统提示词）；INSERT 4 个 vips（vip-trial-0001 trial/day/7、vip-free-0001 free/day/30、vip-month-0001 month/month/1 product_id=month_vip 价格 600/原价 1200、vip-year-0001 year/year/1 product_id=year_vip 价格 6000/原价 12000）。全部 ON CONFLICT DO NOTHING | DELETE 上述 vips 与 sys_configs('default')（有业务数据时慎用） | 可逆但会丢种子 |
| 000004 | 000004_client_ops_logs.up.sql / .down.sql | CREATE TABLE IF NOT EXISTS client_ops_logs + 索引 idx_client_ops_logs_user_created | DROP TABLE IF EXISTS client_ops_logs | 可逆 |
| 000005 | 000005_trajectory_idempotency.up.sql / .down.sql | 轨迹上报幂等：清理历史重复后建唯一索引 uq_auto_record_trajectories_point (user_id, recorded_at, lat, lon) | DROP INDEX IF EXISTS uq_auto_record_trajectories_point | 可逆 |
| 000006 | 000006_drop_sys_configs.up.sql / .down.sql | 删除 `sys_configs` 表：配置改为环境变量（见 `ARCHITECTURE-INVARIANTS.md` §5） | 仅重建表结构（不回填种子；回滚到读 sys_configs 的旧代码前需按 000003 手工回填种子行） | 可逆 |
| 000007 | 000007_enable_pg_stat_statements.up.sql / .down.sql | 创建 `pg_stat_statements` 扩展（供 `scripts/sql-top.sh` 慢 SQL 榜单） | 无表/数据变更；需 postgres command 含 `shared_preload_libraries`（deploy.sh 渲染）才有统计数据，扩展本身无前置 | 可逆 |
| 000008 | 000008_vip_claim_openid_guard.up.sql / .down.sql | 注销循环权益收口（评审定稿）：`user_vip_claims` 加 `open_id`（回填存量）+ 部分唯一索引 `(open_id, vip_id) WHERE open_id IS NOT NULL`；`user_invites` 加 `user_open_id`（回填）+ 部分唯一索引 `(user_open_id) WHERE reward_invitee_at IS NOT NULL`（历史重复标记保留最早一条）；两表 `user_id` FK 由 CASCADE 改 **SET NULL**（注销保留领取/邀请墓碑行，`user_invites.user_id` 同时放开 NOT NULL） | down 恢复 CASCADE + NOT NULL；若墓碑行已存在需先人工清理，否则 SET NOT NULL 失败 | 可逆（墓碑行存在时 down 需人工） |
| 000009 | 000009_family_covers_fk.up.sql / .down.sql | `family_daily_covers.family_id` 补外键（先防御性清孤儿行，`ON DELETE CASCADE`） | down 仅移除约束；CASCADE 已删行不恢复 | 可逆 |
| 000010 | 000010_user_vip_claims_user_id_nullable.up.sql / .down.sql | 补 000008 遗漏：`user_vip_claims.user_id` DROP NOT NULL（否则注销触发 23502、领取墓碑行无法保留；逐文件扫描发现） | down 恢复 NOT NULL；若墓碑行已存在需先人工清理 | 可逆（墓碑行存在时 down 需人工） |
| 000011 | 000011_user_invites_inviter_set_null.up.sql / .down.sql | `user_invites.inviter_id` FK CASCADE→SET NULL（列改可空）：被邀请奖励墓碑行与邀请人存续解耦，邀请人注销不再连带删除、「终身一次」持续成立（第三方评审发现） | down 先人工清理 `inviter_id IS NULL` 行再恢复 NOT NULL + CASCADE | 可逆（需人工清理墓碑行） |

并行撞号规则（spec-standards 第六节）：同号不同名允许（slug 全局唯一），改同一张表需 rebase 确认顺序。

## 8. 迁移变更流程

1. 分配迁移号：取 backend/migrations/ 当前最大 +1，命名 NNN_<slug>.up.sql / .down.sql。
2. down 可逆性：不可逆操作在文件头声明「不可逆：<原因>」。
3. 同步 L4 文档：同次提交回写本文件第 3 节字段定义与第 7 节变更记录。
4. 机械校验：scripts/spec-check.sh 比对 migrations 编号与 L4 文档登记（spec-standards 第九节阻断级 4）。
5. 改 sqlc SQL 后必须 make sqlc-generate + make check-sqlc-sync。
6. 部署顺序：先 migration 后代码。生产回滚**不以 down migration 为常规手段**：新代码有问题时回滚旧代码 + 用部署前备份恢复数据库；结构变更采用 expand → 兼容旧代码 → contract。`down` 脚本手工使用仅限本地/全新环境。唯一例外：deploy.sh **部署失败窗口内**的自动 `migrate goto/down`——它与旧代码/旧配置成对回退，是原子部署回滚的一部分，不作为数据回退手段。

环境相关：DEPLOYMENT_MODE=open 时应用启动自动执行未应用迁移（main.go:107-115）；SaaS 由 deploy/deploy.sh 显式控制迁移时机。注意两套迁移记账**同名不同构、不可混用**：open 启动器自建 `schema_migrations(version TEXT PK, applied_at)`，而 golang-migrate CLI 使用 `schema_migrations(version BIGINT, dirty)`——同一数据库只能用其中一种机制管理迁移（migration/migrate.go:35-45）。



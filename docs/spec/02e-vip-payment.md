# PP-02E VIP 与虚拟支付（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：ADR-0011（支付回调安全模型与金额策略）
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。关键实现位置：`backend/internal/payment`、`backend/internal/vip`、`backend/internal/wechatcrypto`、`backend/internal/db/sqlc/order.sql`、`backend/internal/db/sqlc/vip.sql`、`backend/migrations/000001_baseline.up.sql`、`000003_seed.up.sql` 及小程序前端 `frontend/miniapp/miniprogram/utils/pay.ts`、`utils/vip.ts`、`pages/sub/Vip/`。

## 1. 背景与目标

- **解决什么问题**：为小程序用户提供 VIP 权益的查询、领取与付费购买；以微信「小程序虚拟支付（short_series_goods）」完成收款，并以微信发货回调驱动幂等发货（延长 VIP 到期时间）。
- **服务谁**：微信小程序终端用户（Session 登录）；微信支付服务器（回调）；客服/运营（按手机号补发，见 §8）。
- **本域边界（包含）**：
  - VIP 商品查询（付费档 / 免费档）、VIP 状态查询；
  - 新用户试用 VIP（trial）自动领取、免费 VIP（free）领取与领取查重；
  - 虚拟支付下单签名、订单取消、订单状态查询；
  - 微信虚拟支付发货回调的 GET 服务器验证、安全模式验签解密、幂等发货、VIP 激活；
  - 订单关闭（下单时清理、用户取消、后台超时关单）。
- **明确不属于本域**：
  - 邀请/家庭玩法赠送 VIP 的业务触发（本域只提供 `ExtendVIPDaysWithTx` / `IssueTrialVIPWithTx` 供 `auth`、`family` 调用）；
  - 公众号消息回调（仅与 `wechatcrypto` 共用加解密/签名算法）；
  - AI 配额、文件存储上限等对 VIP 状态的消费方（只读 `vip.InfoProvider`）。
- **代码入口**：
  - 后端：`backend/internal/payment/{handler,service,wechat,order}.go`、`backend/internal/vip/{handler,service,errors}.go`；路由注册 `backend/cmd/server/main.go` L190/L202/L225-227。
  - 迁移：`backend/migrations/000001_baseline.up.sql`（建表）、`000003_seed.up.sql`（VIP 种子）。
  - 前端：`frontend/miniapp/miniprogram/utils/pay.ts`、`utils/vip.ts`、`pages/sub/Vip/Vip.ts`。

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | 微信发货回调采用**安全模式**：加密 body + fail-closed 验签 + 解密后校验 `receive_id` 等于小程序 AppID（`WECHAT_APPID`）；Token 与 AES 密钥用独立环境变量 `WECHAT_VIRTUAL_CALLBACK_TOKEN` / `WECHAT_VIRTUAL_CALLBACK_AES_KEY`（与公众号 `WECHAT_MSG_TOKEN` 独立） | 明文签名不绑定消息体，捕获一组合法三元组即可伪造任意订单的发货通知；receive_id 校验防 Token/AES 复用（资金安全） | 无 |
| D2 | **明文模式禁用**：POST body 无密文包装一律拒绝，返回固定成功 JSON 并告警 | 终止微信重试，同时堵住伪造首次通知 | 无 |
| D3 | 发货**幂等**依赖 DB：`UPDATE ... WHERE state='pending'`（常规）+ `WHERE state='closed'` 补记 + `transaction_id` 唯一约束 | 同一笔订单最多发货一次，无需 Redis 幂等；已关闭订单被支付时不漏发 | 无 |
| D4 | `out_trade_no` 由 `crypto/rand` 生成 32 位不可预测随机串 | 防枚举；回调验签 + DB 幂等（transaction_id 唯一）共同构成防伪造边界（金额不作为闸门，见 D11） | 无 |
| D5 | 订单状态机仅 `pending / paid / closed` 三态，DB CHECK 约束；允许 `closed → paid` 补记（微信已扣款的补发） | 状态收敛，避免中间态；闭合「已关闭但已支付」漏发 | 无 |
| D6 | VIP 激活**叠加而非覆盖**：`expire = GREATEST(now, 现有 expire) + 时长`，首次从 now 起算 | 购买/补发不缩短既有权益 | 无 |
| D7 | `trial` / `free` 每种类型每个微信主体只能领取一次：`user_vip_claims` 上 `(user_id,vip_id)` 唯一 + `(open_id, vip_id)` 部分唯一索引（open_id 冗余发放主体，注销墓碑行防重注册重领；`/vip/free/check` 优先按 openid 判）；付费档可重复购买 | 领取类防重复（含注销循环），付费类允许续购 | 无 |
| D8 | 虚拟支付依据客户端传入 `env` 选择 prod/sandbox appKey（**仅 0/1 合法**：1=沙箱、0=生产，非法值 400，与 VP-6-AC1 一致）；生产受服务端闸门 `PAYMENT_ALLOW_SANDBOX` 保护，未显式开启时 `env=1` 直接 403（见 VP-6-AC6） | 分环境联调，同时防止客户端在生产环境越权切换沙箱 | 无 |
| D9 | 交易/领取在 `db.WithTx` 事务内完成；`user_vips` 读取用 `FOR UPDATE` 行锁 | 防并发重复发货/缩短时长 | 无 |
| D10 | 订单关闭三重机制：下单时关闭同用户同 VIP 且创建超 5 分钟的 pending；用户取消；后台任务关闭创建超 24h 的 pending | 防止陈旧 pending 堆积 | 无 |
| D11 | 回调金额 > 0 即视为真实扣款并照常发货；与标价不一致仅告警对账（平台立减/优惠）；非正数拒绝 | 金额校验不作为漏发闸门，避免用户已扣款静默漏发 | 无 |
| D12 | 前端下单成功后由 `wx.requestVirtualPayment` 拉起收银台，后端 `Request` **不访问微信服务端**，仅本地构造/签名支付参数 | 虚拟支付 SDK 由前端直接调起 | 无 |

## 3. 核心流程（用户故事 + 时序）

### VP-1 VIP 状态查询

**用户故事**：作为小程序用户，我希望随时看到自己是否为 VIP 及到期时间。

| AC | 断言 |
|----|------|
| VP-1-AC1 | `GET /user/vip` 需要 Session；无有效记录时返回 `data.isVip=false, data.expireTime=null` 且 HTTP 200（不报错） |
| VP-1-AC2 | 有记录时 `isVip = expire_time.After(上海当前时间)` |
| VP-1-AC3 | `expireTime` 以上海时区格式化为 `2006-01-02T15:04:05-07:00`（如 `2026-09-14T12:00:00+08:00`） |

**时序**：`user.Handler.GetVIP` → `vip.Service.GetVIPInfo` → `GetUserVIP(user_id)`；`pgx.ErrNoRows` → `IsVIP=false`；其它错误 → 500。

### VP-2 付费 VIP 商品查询

**用户故事**：作为用户，我希望看到可购买的月/年 VIP 档位与价格。

| AC | 断言 |
|----|------|
| VP-2-AC1 | `GET /vip` 需要 Session，返回 `data` 为数组；仅含 `is_active=true` 且 `type IN ('month','year')` 的记录，按 `sort ASC`，上限 100 条 |
| VP-2-AC2 | 每项含 `id/name/type/timeLimit{mark,number}/prices`；`prices` 为 JSON 反序列化结果，空则 `[]` |

**时序**：`vip.Handler.ListPaidVIP` → `ListActivePaidVIPs`（`vip.sql` L1-5）→ `vipToMap`。

### VP-3 新用户试用 VIP 自动领取

**用户故事**：作为新注册用户，我希望自动获得 7 天试用 VIP。

| AC | 断言 |
|----|------|
| VP-3-AC1 | `POST /vip/new-user` 需要 Session；**不读取请求体**，直接以 Session userID 领取硬编码 `trialVIPID = "vip-trial-0001"` |
| VP-3-AC2 | 首次领取成功返回 HTTP 200 `data={}` |
| VP-3-AC3 | 重复领取返回 HTTP 409 + `code=4090` + `biz_code=TRIAL_VIP_ALREADY_CLAIMED` + message `trial vip already claimed` |
| VP-3-AC4 | 其它错误返回 500 `CodeInternalError`；无 `ErrInvalidVIP` 分支（trial 领取不校验入参） |

**时序**：注册事务内 `auth/handler.go` 已调 `vipService.IssueTrialVIPWithTx` 直接发放试用（该发放撞 openid 墓碑时静默跳过、不回滚注册，见 02a A-1）；登录成功后 `utils/auth.ts` 在 `data.newUser` 为真时仍调用 `claimNewUserFreeVip()` → `POST /vip/new-user` → `vip.Service.ClaimTrialVIP` → `db.WithTx` → `activateVIPWithTx`，正常 SaaS 流程因已发放而返回 409 `TRIAL_VIP_ALREADY_CLAIMED`；前端把该码当作成功吞掉（`utils/vip.ts`）。该端点仅在注册未发放（如历史/边界路径）时首次成功返回 200。

### VP-4 免费 VIP 领取与查重

**用户故事**：作为用户，我希望领取运营开放的 30 天免费 VIP，且不能重复领取。

| AC | 断言 |
|----|------|
| VP-4-AC1 | `GET /vip/free` 需要 Session，仅返回 `is_active=true AND type='free'` 的记录（`sort ASC`，上限 100），每项仅 `{id,name}` |
| VP-4-AC2 | `POST /vip/free/claim` body 上限 8KB；`vipId` 为空 → 400 `4000`；商品不存在/非 free/未激活 → 400 `invalid vip` |
| VP-4-AC3 | 已领取返回 409 + `code=4090` + `biz_code=FREE_VIP_ALREADY_CLAIMED`；成功 200 `data={}` |
| VP-4-AC4 | `GET /vip/free/check?vipId=` 返回 `data.claimed`（bool）；`vipId` 为空 → 400；先取 claimant 的 openid，优先按 `open_id` 查 `user_vip_claims`，openid 缺失（异常数据）才回退 `user_id` 维度（`hasVIPClaimWithQ` / `HasVIPClaimByOpenID`，与 D7 口径一致） |

**时序**：`pages/sub/Vip/Vip.ts doClaimFreeVip` → `GET /vip/free` 取第一个档位 → `POST /vip/free/claim{vipId}` → `vip.Service.ClaimFreeVIP` 校验 `type=='free' && is_active` → `activateVIPWithTx`。
> 注意：`utils/vip.ts fetchVipInfo` 用 `NEW_USER_FREE_VIP_ID='vip-free-0001'` 调 `/vip/free/check`，因此 UI 的 `receivedFreeVip` 反映的是 **free** 领取状态，不反映 trial 领取状态。

### VP-5 VIP 商品/权益的默认种子

| AC | 断言 |
|----|------|
| VP-5-AC1 | 全新库执行 `000003_seed.up.sql` 后存在 4 行 `vips`：`vip-trial-0001`(trial,day,7)、`vip-free-0001`(free,day,30)、`vip-month-0001`(month,month,1,product_id=`month_vip`,amount=600)、`vip-year-0001`(year,year,1,product_id=`year_vip`,amount=6000) |

### VP-6 虚拟支付下单（含沙箱 env）

**用户故事**：作为用户，我选择档位后，希望后端生成可供 `wx.requestVirtualPayment` 使用的支付参数。

| AC | 断言 |
|----|------|
| VP-6-AC1 | `POST /payment/virtual/request` 需 Session；body 上限 4096B；`vipId` 为空 → 400 `4000`；`env` 非 0/1 → 400 `env must be 0 or 1` |
| VP-6-AC2 | VIP 不存在/未激活/类型非 month/year/价格匹配失败或 ≤0/时长解析失败 → 400 `invalid vip`（sentinel `payment.ErrInvalidVIP`）；其它错误 → 500 `5001` |
| VP-6-AC3 | 下单事务内先关闭该用户**同 VIP**、状态 pending、`created_at < now()-5min` 的旧订单，再生成 `out_trade_no`（32 位随机）与 UUID 并插入 `orders`（`channel='virtual_pay'`，`state='pending'`，`prepay_id=NULL`） |
| VP-6-AC4 | 响应 HTTP 200 `data` 含 `outTradeNo、offerId、buyQuantity=1、needPay=true、signData、paySig、signature、mode="short_series_goods"`；`prepayId` 因 omitempty 恒不出现 |
| VP-6-AC5 | `paySig = HMAC-SHA256(appKey,"requestVirtualPayment&"+signData)`，`signature = HMAC-SHA256(sessionKey,signData)`；appKey 按 `env` 取（1→sandbox，其余→production）；appKey 未配置或 `users.session_key` 为空 → 错误并尽力关闭刚建订单 |
| VP-6-AC6 | 当前代码**不访问微信服务端**，`Request` 仅本地构造签名；**沙箱闸门**：`env=1` 且 `PaymentAllowSandbox=false` → 403 `sandbox payment is disabled`，仅 `PAYMENT_ALLOW_SANDBOX=1` 放行（`backend/internal/payment/handler.go`） |

**时序**：
1. `payment.Handler.Request` 校验 body/vipId/env，打诊断日志；
2. `createOrder` 读 `GetVIPByID`，校验 `is_active` 与 `type`，`MatchPrice(vipRecord.Prices, type)` 取金额，`vip.ParseVIPDuration` 验时长；
3. 事务：`ClosePendingOrdersByUserAndVIP` → `GenerateOutTradeNo` → `util.NewUUID` → `CreateOrder`；
4. 事务外读 `GetUserByID`、`GetUserSessionKeyByID`，组装 `attach={"userId":..,"vipId":..}`；
5. `wechat.Request` → `BuildSignData`（offerId、buyQuantity、env、currencyType=CNY、productId、goodsPrice、outTradeNo、attach）→ `GenerateSigns`；
6. 失败则 best-effort `CloseOrder`（失败仅记日志）并返回错误。

> **体验版/开发版预期（ADR-0011）**：前端 `getPayEnv` 对 develop/trial 返回 `env=1`，在未开启 `PAYMENT_ALLOW_SANDBOX` 的环境必然 403——这是**预期行为**：trial VIP 由注册自动发放（VP-3，不可购买亦不可重复领取），体验版不承担支付联调职责；沙箱联调使用开启闸门的专用环境。

### VP-7 支付结果轮询

**用户故事**：作为用户，支付完成后我希望页面自动确认为 VIP。

| AC | 断言 |
|----|------|
| VP-7-AC1 | `GET /payment/virtual/status?outTradeNo=` 需 Session；参数为空 → 400 `outTradeNo is required` |
| VP-7-AC2 | 订单不存在 / `user_id` 为空 / 不属于当前用户 → HTTP **404**、`code=4040`、`biz_code=ORDER_NOT_FOUND`、message `order not found` |
| VP-7-AC3 | 真实 DB 错误（非 `pgx.ErrNoRows`）→ HTTP **500** `code=5001` + `slog.Error`，不得伪装成 404 |
| VP-7-AC4 | 命中返回 200 `data.state ∈ {pending,paid,closed}`；前端每 3s 轮询、最多 10 次（共约 30s），`paid`→成功弹窗，`closed`→失败提示，超时 → `vip.payTimeout`；前端仅处理 `paid`/`closed`，与后端状态机一致 |

**时序**：`Vip.ts _pollOrderStatus`（L419-481）；`maxCount=10`，`interval=3000ms`。

### VP-8 取消订单

**用户故事**：作为用户/客户端，我希望放弃支付时订单能被关闭。

| AC | 断言 |
|----|------|
| VP-8-AC1 | `POST /payment/virtual/cancel` 需 Session；body 上限 4096B；`outTradeNo` 为空 → 400 |
| VP-8-AC2 | 仅关闭「当前用户 + out_trade_no + state=pending」的订单（`CloseOrder`）；影响 0 行时回查订单：订单不存在或非本人 → 404 `code=4040` `biz_code=ORDER_NOT_FOUND`；订单已非 pending → 200 `{}`（幂等成功）；仍 pending 但 0 行（并发）→ 200 `{}` |
| VP-8-AC3 | `CloseOrder` 真实 DB 错误 → 500 `5001` |
| VP-8-AC4 | 前端 `utils/pay.ts` 在支付取消/失败/中止路径 best-effort 调用 `/payment/virtual/cancel` |

**时序**：`payment.Handler.Cancel` L99-153。

### VP-9 微信发货回调：验签与解密

**用户故事**：作为系统，我必须只接受微信真实推送的发货通知。

| AC | 断言 |
|----|------|
| VP-9-AC1 | `GET /api/prod/payment/virtualPayNotify`：`signature/timestamp/nonce` 任一为空 → HTTP 403 `text/plain`；签名用 `CheckSignature`（`sha1(sort([token,timestamp,nonce]))`，常量时间比较）不符 → 403；通过 → 200 原样返回 `echostr` |
| VP-9-AC2 | POST 读取 body 上限 64KB（`maxNotifyBodySize=64*1024`，`LimitReader+1`）；超限或读失败 → 固定成功 JSON |
| VP-9-AC3 | 密文提取：先尝试 JSON 包装 `{"encrypt"/"Encrypt"}`，再尝试 XML `<xml><Encrypt>`（`extractEncryptedCiphertext`） |
| VP-9-AC4 | 有密文但缺 `msg_signature` → 告警 `payment_notify_missing_msg_signature` + 固定成功；验签失败 `CheckEncryptedSignature`（`sha1(sort([token,timestamp,nonce,encrypt]))`）→ 告警 `payment_notify_bad_signature` + 固定成功 |
| VP-9-AC5 | 解密用 `DecryptMsgZeroIV`（AES-256-CBC，**IV=16 字节全零**，PKCS7 按 **32 字节** 对齐去填充）；失败 → 告警 `payment_notify_decrypt_failed`（带 ciphertext/msg_signature/timestamp/nonce）+ 固定成功 |
| VP-9-AC6 | 解密后 JSON 解析；失败 → 告警 `alert:payment_parse_failed` + 固定成功 |
| VP-9-AC7 | **无密文（明文模式）→ 告警 `payment_notify_plaintext_rejected` + 固定成功 JSON**，绝不处理 body |
| VP-9-AC8 | 解密后校验 `receive_id` 等于本小程序 AppID（`WECHAT_APPID`/`WechatAppID`）；不匹配 → 告警 `payment_notify_receive_id_mismatch` + HTTP 500 触发微信重试（避免 AppID 配错静默漏发）；未配置 AppID 时跳过（`validReceiveID`） |

### VP-10 幂等发货与 VIP 激活

| AC | 断言 |
|----|------|
| VP-10-AC1 | 仅处理 `event="xpay_goods_deliver_notify"`；`out_trade_no`（兼容 `OutTradeNo`）与 `transaction_id`（优先 `WeChatPayInfo.TransactionId`，回退顶层）为空 → 业务拒绝 |
| VP-10-AC2 | 金额优先取 `GoodsInfo/goodsInfo.ActualPrice/actualPrice`，回退顶层 `ActualPrice/actualPrice/amount`；取不到 → 业务拒绝。回调已验签视为真实扣款：金额 > 0 一律发货，与 `orders.amount` 不一致仅告警 `payment_notify_amount_mismatch`；金额 ≤ 0 → 业务拒绝 |
| VP-10-AC3 | 事务内做 `UPDATE orders SET state='paid',transaction_id=...,paid_at=now() WHERE out_trade_no=$1 AND state='pending'`；`rows>0` 才发货 |
| VP-10-AC4 | `rows==0`：重新读取订单最新状态——`paid` → 幂等成功（200）；`closed` 且用户未删除 → `MarkClosedOrderPaid` 将 closed 补记为 paid（rows>0）并自动发货，告警 `payment_notify_closed_order_reissued`；补记 rows==0（并发已处理）→ 跳过 |
| VP-10-AC5 | `23505`（transaction_id 唯一冲突，不同订单收到同一 transaction_id）→ 按业务拒绝（200 + 告警）；其余 DB 错误原样返回 → 500 重试 |
| VP-10-AC6 | 订单 `user_id` 为空（用户已删除）→ 只更新订单状态，不激活 VIP；事务内若 `rows==0` 且用户已删除 → 走幂等成功 |
| VP-10-AC7 | 激活成功 → `ActivateVIPWithTx`，日志 `payment notify delivered`；VIP 到期从 `GREATEST(now, 现有expire)` 叠加；首次从 now 起算 |
| VP-10-AC8 | 回调响应体固定为 `{"ErrCode":0,"ErrMsg":"success"}`（200）；仅瞬时故障返回 500 `{"ErrCode":-1,"ErrMsg":"internal error"}`，content-type `application/json; charset=utf-8` |

### VP-11 订单关闭（三重）

| AC | 断言 |
|----|------|
| VP-11-AC1 | 下单事务内：`closePendingOrdersByUserAndVIP` 仅关同 user+vip 且创建超 5 分钟的 pending |
| VP-11-AC2 | 用户取消：见 VP-8 |
| VP-11-AC3 | 后台任务 `runOrderClose`：cutoff=`now-24h`。先循环 `CloseOwnerlessPendingOrders`（每批 ≤1000）关闭 `user_id IS NULL` 的无主 pending 订单；再按 `id ASC` 游标分页、每批 1000 条 `ListPendingOrdersBefore`（`state=pending AND user_id IS NOT NULL`），`CloseOrdersBatch`（按位置配对 out_trade_no→user_id，只关 pending）；不足 1000 结束；默认每 1 分钟调度一次（`JOB_INTERVAL_ORDER_CLOSE`），PG advisory lock `lock:background:order_close`（ADR-0005，无 TTL） |

### VP-12 VIP 补发（客服/运营）

**用户故事**：作为客服，我希望按手机号给用户补发 VIP 天数。

| AC | 断言 |
|----|------|
| VP-12-AC1 | 后端能力 `vip.Service.ExtendVIPDaysWithTx`（由 `auth`/`family` 在事务内调用）：`userID` 空或 `days<=0` 显式报错；`base=GREATEST(now,现expire)`，`expire=base.AddDate(0,0,days)`（自然日），`begin_time` 不变；**非幂等**，重复执行重复累加 |
| VP-12-AC2 | 脚本 `scripts/grant_vip_by_phone.sh --phone <手机号> --days <正整数> [--memo]` 在部署目录执行：手机号正则 `^1[3-9][0-9]{9}$`，`docker compose exec -T postgres psql` 查 user_id，SQL 语义同 `ExtendVIPDaysWithTx`（GREATEST 叠加），写日志 `logs/grant-vip.log` |
| VP-12-AC3 | 漏发货补发可用失败日志中的原始参数重放回调 `POST /api/prod/payment/virtualPayNotify`（幂等，订单已 paid 不重复发货） |

### VP-13 前端支付交互（doPay / Vip 页）

| AC | 断言 |
|----|------|
| VP-13-AC1 | `utils/pay.ts getPayEnv()`：iOS 恒返回 0；否则 `envVersion` 为 `develop/trial` → 1，`release` → 0，异常兜底 0 |
| VP-13-AC2 | `doPay` 先 `POST /payment/virtual/request`（带 `vipId, env`），再 `wx.requestVirtualPayment({signData,paySig,signature,mode})`；成功后回调 `onVirtualPaySuccess(outTradeNo)` 并进入轮询 |
| VP-13-AC3 | 支付失败 `errCode===-15011` → iOS 测试不支持提示；`errMsg` 含 `SIG_EMPTY` → 签名空提示；`cancelToken` 已取消 → reject `request:abort` |
| VP-13-AC4 | Vip 页先读 `GET /system/config`：`features.payment===false` 隐藏付费入口，`features.freeVip===false` 隐藏免费入口；私有模式（`getBackendMode()==='private'`）本地兜底两个都关。后端 `features.freeVip` 由环境变量 `FREE_VIP_ENABLED` 控制（默认 `true`，`system/handler.go` 读 `cfg.FreeVipEnabled`）：免费活动下线置 `0` 并重新部署即可，无需发版（SaaS 经 `CFG_FREE_VIP_ENABLED` 注入，见 DEPLOYMENT §2.2/§2.11） |
| VP-13-AC5 | 商品列表在页面上二次过滤掉 `type==='free'||'trial'`，只展示付费档 |

## 4. 数据模型

来源：`backend/migrations/000001_baseline.up.sql`（建表 L207-226/291-307/340-354，约束 L455-539，索引 L681-777，外键 L860-901）、`000003_seed.up.sql` L17-22。

### 4.1 表定义

| 表 | 关键字段 | 说明 |
|----|---------|------|
| `orders` | `id(text PK)`、`user_id(text NULL)`、`vip_id(text NOT NULL)`、`out_trade_no(text)`、`channel(text)`、`state(text)`、`amount(int)`、`prepay_id(text NULL)`、`transaction_id(text NULL)`、`paid_at(timestamptz NULL)`、`created_at/updated_at` | 虚拟支付订单；`user_id` 可被置空表示用户已删除 |
| `vips` | `id(text PK)`、`type(text)`、`name`、`time_limit_mark`、`time_limit_number`、`product_id(text NULL)`、`sort(int)`、`is_active(bool)`、`prices(jsonb)`、`created_at` | VIP 商品定义 |
| `user_vips` | `id(text PK)`、`user_id(text NOT NULL UNIQUE)`、`begin_time`、`expire_time`、`created_at` | 每用户一行；`CHECK(expire_time > begin_time)` |
| `user_vip_claims` | `id(text PK)`、`user_id`、`vip_id`、`open_id`、`created_at` | 领取记录；`UNIQUE(user_id,vip_id)` 防重复；`open_id`（000008 新增并回填）冗余发放主体，防注销重注册重领；`user_id` 可空（000010，注销保留仅含 openid 的墓碑行） |

### 4.2 约束与索引（真实）

| 对象 | 定义 |
|------|------|
| `orders_amount_check` | `amount > 0` |
| `orders_channel_check` | `channel = 'virtual_pay'` |
| `orders_out_trade_no_check` | `length(out_trade_no) <= 32` |
| `orders_prepay_id_check` | `prepay_id IS NULL OR length<=128` |
| `orders_state_check` | `state IN ('pending','paid','closed')` |
| `orders_transaction_id_check` | `transaction_id IS NULL OR length<=128` |
| `orders_out_trade_no_key` | UNIQUE(out_trade_no) |
| `orders_transaction_id_key` | UNIQUE(transaction_id)（PG 允许多个 NULL） |
| `uq_orders_transaction_id_not_null` | 部分唯一索引：`transaction_id IS NOT NULL AND <>''` |
| orders 外键 | `user_id → users(id) ON DELETE SET NULL`；`vip_id → vips(id)` |
| orders 索引 | `idx_orders_null_user`、`idx_orders_pending_created_at`、`idx_orders_state_created_at`、`idx_orders_user_id_state_created`、`idx_orders_user_vip_state`、`idx_orders_vip_id` |
| `vips_type_check` | `type IN ('month','year','trial','free')` |
| `vips_time_limit_mark_check` | `time_limit_mark IN ('day','month','year')` |
| `vips_time_limit_number_check` | `time_limit_number > 0` |
| `user_vips_user_id_key` | UNIQUE(user_id)（每个用户仅一行） |
| `user_vips_check` | `expire_time > begin_time` |
| user_vips 外键 | `user_id → users(id) ON DELETE CASCADE` |
| `user_vip_claims_user_id_vip_id_key` | UNIQUE(user_id,vip_id) |
| `uq_user_vip_claims_open_id_vip_id` | 部分唯一索引：`(open_id, vip_id) WHERE open_id IS NOT NULL`（000008） |
| user_vip_claims 外键 | `user_id → users(id) ON DELETE SET NULL`（000008；可空见 000010）；`vip_id → vips(id)` |
| `idx_user_vips_expire_time` / `idx_user_vip_claims_user_created` / `idx_user_vip_claims_vip_id` / `idx_vips_is_active` | 普通索引 |

### 4.3 数据字典

- **订单状态机**：`pending → paid`（回调发货）／`pending → closed`（取消、下单清理、后台超时）／`closed → paid`（微信已扣款的补发补记）；无 paid→closed 转换。
- **VIP 类型**：`month` / `year`（付费，可购）；`trial`（新用户试用）；`free`（免费活动）。两类领取均写 `user_vip_claims`；付费档激活时也会写 claim，但 claim 冲突对 month/year **不报错**（继续叠加时长）。
- **时间语义**：所有「now」取上海时区（`timeutil.NowShanghai()`）；`ExtendVIPDaysWithTx` 用自然日 `AddDate(0,0,days)`；时长单位 day→AddDate(0,0,n)、month→AddDate(0,n,0)、year→AddDate(n,0,0)。
- **软删除**：本域无软删除。用户注销由 `NullifyOrdersByUser`（`user_id=NULL`）与 users 级联处理。

## 5. API 契约

### 5.1 端点总表

| 方法 | 路径 | 鉴权 | Body 上限 | 源文件 | 说明 |
|------|------|------|-----------|--------|------|
| GET | `/user/vip` | Session | - | `user/handler.go` L235 | VIP 状态 |
| GET | `/vip` | Session | - | `vip/handler.go` L41 | 付费商品 |
| GET | `/vip/free` | Session | - | `vip/handler.go` L59 | 免费活动 |
| POST | `/vip/free/claim` | Session | 8KB | `vip/handler.go` L80 | 领取免费 |
| GET | `/vip/free/check` | Session | - | `vip/handler.go` L112 | 领取查重 |
| POST | `/vip/new-user` | Session | 不读 body | `vip/handler.go` L132 | 新用户试用 |
| POST | `/payment/virtual/request` | Session | 4096B | `payment/handler.go` L60 | 下单签名 |
| POST | `/payment/virtual/cancel` | Session | 4096B | `payment/handler.go` L107 | 取消订单 |
| GET | `/payment/virtual/status` | Session | - | `payment/handler.go` L163 | 订单状态 |
| GET | `/api/prod/payment/virtualPayNotify` | 公开（跳过 WorkerAuth；public 限流 60/min） | - | `payment/handler.go` L56 | 微信服务器验证 |
| POST | `/api/prod/payment/virtualPayNotify` | 同上 | 64KB | `payment/handler.go` L57 | 发货回调 |
| GET | `/system/config` | 公开 | - | `system/handler.go` L23 | features 开关 |

> 鉴权：Session 由 `main.go` L225-227,301-305 的 `NewSessionMiddleware`（SaaS）或 `NewOpenAuthMiddleware`（open）注入；VIP/支付 handler 从 `middleware.UserID(ctx)` 取 userID，鉴权由中间件完成。
> 统一响应（成功）：`{"code":"0000","message":"ok","data":...,"request_id":"..."}`；错误：`{"code":...,"message":...,"biz_code":...,"request_id":"..."}`（`backend/internal/middleware/response.go` L16-25/L65/L102）。

### 5.2 请求/响应字段

**GET /vip** → `data: [{id,name,type,timeLimit:{mark,number},prices:[...]}...]`

**GET /vip/free** → `data: [{id,name}...]`

**GET /vip/free/check** → query `vipId`；`data: {claimed: bool}`

**POST /vip/new-user** → body 忽略；`data: {}`

**POST /payment/virtual/request** 请求 `{"vipId":"...","env":0|1}`；响应：
```json
{"outTradeNo":"<32>","offerId":"<offer>","buyQuantity":1,"needPay":true,
 "signData":"<json>","paySig":"<hmac>","signature":"<hmac>","mode":"short_series_goods"}
```

**POST /payment/virtual/cancel** 请求 `{"outTradeNo":"..."}`；成功 `data: {}`

**GET /payment/virtual/status** → `data: {"state":"pending|paid|closed"}`

**回调**：请求为密文包装（JSON `{"encrypt":...}` 或 XML `<xml><Encrypt>`）；POST 仅消费密文与 `msg_signature`，不消费 `signature` 查询参数；响应固定 `{"ErrCode":0,"ErrMsg":"success"}`，瞬时故障 `{"ErrCode":-1,"ErrMsg":"internal error"}`。

### 5.3 错误码映射（真实）

| 场景 | HTTP | `body.code` | `body.biz_code` | message | 来源 |
|------|------|------------------|----------------------|---------|------|
| 成功 | 200 | `0000` | — | `ok` | `middleware.JSON` |
| body 非法 / vipId 空 / outTradeNo 空 / env 非 0/1 | 400 | `4000` | — | 各校验文案 | `payment/handler.go`、`vip/handler.go` |
| body 超上限（按端点：VIP 领取 8KB、支付下单/取消 4KB） | 413 | `4130` | — | `request body too large` | `middleware/body.go` `JSONBodyError` |
| `env=1` 且 `PAYMENT_ALLOW_SANDBOX` 未开启 | 403 | `4030` | — | `sandbox payment is disabled` | `payment/handler.go` |
| `payment.ErrInvalidVIP`（下单） | 400 | `4000` | — | `invalid vip` | `payment/handler.go` L94-97 |
| `vip.ErrInvalidVIP`（免费领取） | 400 | `4000` | — | `invalid vip` | `vip/handler.go` L100-101 |
| 免费已领 | 409 | `4090` | `FREE_VIP_ALREADY_CLAIMED` | `free vip already claimed` | `vip/handler.go` L98-99 |
| 试用已领 | 409 | `4090` | `TRIAL_VIP_ALREADY_CLAIMED` | `trial vip already claimed` | `vip/handler.go` L138-139 |
| 订单不存在/非本人（cancel、status） | **404** | `4040` | `ORDER_NOT_FOUND` | `order not found` | `payment/handler.go` |
| 其它内部错误（下单、取消、列表、领取、状态查询） | 500 | `5001` | — | `failed to ...` | 各 handler default 分支 |

> 终态：404 响应 `code=4040`、409 响应 `code=4090`（与 HTTP 状态一致的固定枚举，ADR-0008）。

## 6. 关键实现约束

- **锁 / 并发**：`user_vips` 读取用 `FOR UPDATE` 行锁；并发首次激活/补发（`ActivateVIPWithTx` 与 `ExtendVIPDaysWithTx` 的 `ErrNoRows` 分支）统一用 `ClaimUserVIPRow`（`ON CONFLICT DO NOTHING`，占位 `expire=now+1s` 满足 CHECK）再 `FOR UPDATE` 重读串行化，避免 GREATEST 丢档。
- **幂等**：
  - 发货：`orders.state='pending'` 条件更新 + `transaction_id` 唯一；
  - 领取：`user_vip_claims` `(user_id,vip_id)` 唯一 + `UpsertVIPClaim` 的 `rowsAffected==0` 判定；
  - VIP 写入：`UpsertUserVIP` 的 `GREATEST` 防缩短（但不防重复累加，补发/购买本意即累加）。
- **事务与交付原子性**：`createOrder`、`handleNotify`、`ClaimFreeVIP`/`ClaimTrialVIP`、`ExtendVIPDaysWithTx`/`ActivateVIPWithTx`（由调用方/Claim 包装器包进 `db.WithTx`）。**`handleNotify` 的单个 `db.WithTx` 同时包含 订单置 paid（含 closed 补记）与 `ActivateVIPWithTx` 发货**——「paid」严格等价「已交付」，不存在已扣款未发货窗口；回调重试命中 paid 即幂等成功是安全的。；闭包内 DB 错误用 `fmt.Errorf("操作名: %w", err)` 包裹，业务 sentinel（`ErrInvalidVIP`、`ErrFreeVIPAlreadyClaimed`、`ErrTrialVIPAlreadyClaimed`、`errNotifyRejected`）裸返回。
- **业务拒绝 vs 瞬时故障**：`handleNotify` 中带 `errNotifyRejected` 的错误（订单不存在/金额缺失或非正数/事件不支持/缺字段/23505）→ 200 + 告警；其它 → 500 触发微信重试。金额与标价不一致不拒绝（照常发货 + 告警）。
- **告警关键字**：`payment_notify_missing_msg_signature`、`payment_notify_bad_signature`、`payment_notify_decrypt_failed`、`alert:payment_parse_failed`、`payment_notify_plaintext_rejected`、`payment_notify_business_rejected`、`payment_notify_transient_retry`、`payment_notify_amount_missing`、`payment_notify_amount_invalid`、`payment_notify_amount_mismatch`、`payment_notify_closed_order_reissued`、`payment_notify_receive_id_mismatch`（各关键字的实际日志级别见 DEPLOYMENT §10，Warn/Error 混合）。
- **安全**：Token/AES 密钥仅环境变量注入，不落仓库；POST 回调 body 无密文一律拒绝；签名比较用常量时间；nginx 仅启用 `limit_req` 与 TLS。
- **签名输入**：`WechatVirtualPayClient.appID` 在构造时赋值、不参与签名计算；`paySig` 使用 appKey，`signature` 使用用户 `session_key`。
- **阈值**：见下表。

| 常量/阈值 | 值 | 场景/位置 |
|-----------|-----|-----------|
| 回调 body 上限 | 64 KB | `payment/handler.go maxNotifyBodySize` |
| 下单/取消 body 上限 | 4096 B | `middleware.ReadJSONBody(...,4096)` |
| VIP 领取 body 上限 | 8 KB | `vip/handler.go maxVIPRequestBodySize` |
| 下单时清理旧 pending | 创建超过 5 分钟 | `ClosePendingOrdersByUserAndVIP` |
| 后台关单 cutoff | 创建超过 24 小时 | `jobs/runner.go runOrderClose` |
| 后台关单每批 | 1000 | `ListPendingOrdersBefore LIMIT 1000` |
| 后台关单间隔 | 默认 1 分钟 | `JOB_INTERVAL_ORDER_CLOSE` |
| 后台任务锁 | PG advisory lock（无 TTL） | `lock:background:order_close`（ADR-0005） |
| out_trade_no 长度 | 32 | `GenerateOutTradeNo/randomString(32)` |
| 商品列表 LIMIT | 100 | `vip.sql` |
| 前端轮询 | 10 次 × 3 s | `Vip.ts _pollOrderStatus` |

## 7. 前端接入

| 文件 | 职责 |
|------|------|
| `utils/pay.ts` | `getPayEnv()` 决定 `env`；`doPay` 下单 + `wx.requestVirtualPayment`，处理 `-15011` / `SIG_EMPTY` / cancelToken |
| `utils/vip.ts` | `fetchVipInfo`（`GET /user/vip` + `GET /vip/free/check`，60s 内存缓存 + storage）、`claimNewUserFreeVip`（`POST /vip/new-user`，吞 `TRIAL_VIP_ALREADY_CLAIMED`）、`formatVipInfo`（到期取前 10 位） |
| `pages/sub/Vip/Vip.ts` | 商品加载（`GET /vip` 过滤 free/trial）、购买（`doPay` + 轮询 `/payment/virtual/status`）、免费领取（`GET /vip/free` + `POST /vip/free/claim`）、`GET /system/config` 开关、订单创建后按钮防重 |
| `utils/auth.ts` | 登录成功后新用户自动领取 trial（`claimNewUserFreeVip`） |
| `components/SubscribePrompt/` | 是「异常提醒模板消息订阅」弹窗（`POST /subscribe/record`），**与 VIP 无关，也未在 Vip 页引用** |

- 请求 URL 由 `utils/http.ts` 以 `getBaseURL()+path` 拼装，SaaS 默认 `https://pro.papafeiji.cn`，故受保护 VIP/支付端点线上路径为根路径（如 `/payment/virtual/request`）；回调为固定 `/api/prod/payment/virtualPayNotify`。
- 页面入口：`/pages/sub/Vip/Vip`，由 Set / User / index / AIDrawer / NoteEdit / AutoBtn 等入口 `navigateTo` 进入。

## 8. 运维与任务

### 8.1 环境变量（`backend/internal/config/config.go` L207-216）

| 变量 | 必填 | 说明 |
|------|------|------|
| `WECHAT_VIRTUAL_OFFER_ID` | SaaS 必填 | 虚拟支付 offerID |
| `WECHAT_VIRTUAL_APP_KEY_PRODUCTION` | SaaS 必填 | 生产 appKey（`env=0`） |
| `WECHAT_VIRTUAL_APP_KEY_SANDBOX` | SaaS 必填 | 沙箱 appKey（`env=1`） |
| `WECHAT_VIRTUAL_CALLBACK_TOKEN` | SaaS 必填 | 回调验签 Token（与 `WECHAT_MSG_TOKEN` 独立） |
| `WECHAT_VIRTUAL_CALLBACK_AES_KEY` | SaaS 必填 | 回调 EncodingAESKey（43 位） |
| `PAYMENT_ALLOW_SANDBOX` | 否 | 解析为 `Config.PaymentAllowSandbox`（值为 `"1"` 时 true）；仅当为 true 才放行 `env=1`，生产默认关闭（`payment/handler.go` 沙箱闸门） |
| `JOB_INTERVAL_ORDER_CLOSE` | 否 | 后台关单间隔，默认 1 分钟 |
| `DEPLOYMENT_MODE` | 否 | `saas`/`open`；open 不强制支付配置，`/system/config` 返回 `features.payment=false` |

### 8.2 后台任务

- `runOrderClose`：见 VP-11-AC3；锁 `lock:background:order_close`（PG advisory lock，无 TTL），最长执行 5 分钟。

### 8.3 网关与部署

- nginx 独立 location `/api/prod/payment/virtualPayNotify`（`deploy/nginx/default.conf` L86-98），`limit_req zone=api burst=50 nodelay`，代理超时 60s。
- 后端 `main.go` L167 `WorkerAuth` 跳过该回调路径；`RegisterPublic` 注册在 `publicRouter`（IP 限流 60/min）。

### 8.4 排障与补发

- 解密/验签失败日志含 ciphertext/msg_signature/timestamp/nonce；核对容器环境变量与微信后台「虚拟支付回调」一致。
- 漏发货：用失败日志参数重放回调（幂等）；客服补发见 VP-12。

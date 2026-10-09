package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/vip"
	"papafeiji/backend/pkg/timeutil"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

// ---- mocks ----

type mockSessions struct {
	createCalled bool
	createUID    string
}

func (m *mockSessions) Create(_ context.Context, userID string) (string, error) {
	m.createCalled = true
	m.createUID = userID
	return "session-1", nil
}

func (m *mockSessions) Delete(_ context.Context, _ string) error { return nil }

func (m *mockSessions) DeleteAll(_ context.Context, _ string) error { return nil }

type mockAuthVIP struct {
	issueCalls int
}

func (m *mockAuthVIP) GetVIPInfo(_ context.Context, _ string) (vip.Info, error) {
	return vip.Info{IsVIP: false}, nil
}

func (m *mockAuthVIP) IssueTrialVIPWithTx(_ context.Context, _ string, _ *sqlc.Queries) error {
	m.issueCalls++
	return nil
}

func (m *mockAuthVIP) ExtendVIPDaysWithTx(_ context.Context, _ string, _ int, _ *sqlc.Queries) error {
	return nil
}

// rewriteTransport 把微信 API 请求重写到本机 httptest server，避免改动生产代码。
type rewriteTransport struct {
	target *url.URL
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

func newWechatTestServer(t *testing.T) (*WechatClient, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sns/jscode2session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("js_code") {
		case "valid-code":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"openid": "openid-1", "session_key": "sk-1", "unionid": "",
			})
		case "valid-code-union":
			// 带unionid 的登录用例：老用户 openid 命中后补写 unionid（公众号打通）
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"openid": "openid-mp-1", "session_key": "sk-1", "unionid": "unionid-1",
			})
		case "valid-code-nosk":
			// 异常响应：errcode=0 但 session_key 为空——openid 命中分支必须跳过 session_key 更新
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"openid": "openid-1", "session_key": "", "unionid": "",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errcode": 40029, "errmsg": "invalid code"})
		}
	})
	// 供 LoginApp 测试：多端应用 token + donut/code2verifyinfo
	mux.HandleFunc("/cgi-bin/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("appid") != "donut-appid-test" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errcode": 40013, "errmsg": "invalid appid"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "donut-tok-1", "expires_in": 7200})
	})
	mux.HandleFunc("/donut/code2verifyinfo", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("code") {
		case "app-valid-code":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"errcode": 0, "errmsg": "ok",
				"user_info": map[string]interface{}{
					"openapp_info": map[string]interface{}{
						"openid": "openid-app-1", "unionid": "unionid-1",
						"headimgurl": "https://thirdwx.qlogo.cn/app-avatar.png", "nickname": "微信用户",
					},
				},
			})
		case "app-valid-code-noopenid":
			// 异常响应：errcode=0 但 openapp_info.openid 缺失——必须 500 拒绝，不得以 open_id='' 建号
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"errcode": 0, "errmsg": "ok",
				"user_info": map[string]interface{}{
					"openapp_info": map[string]interface{}{
						"openid": "", "unionid": "unionid-1",
					},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errcode": 10001001, "errmsg": "invalid code"})
		}
	})
	// 供 BindPhone 测试：stable_token + 手机号置换
	mux.HandleFunc("/cgi-bin/stable_token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "tok-1", "expires_in": 7200})
	})
	mux.HandleFunc("/wxa/business/getuserphonenumber", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"errcode": 0, "errmsg": "ok",
			"phone_info": map[string]interface{}{"phoneNumber": "13800000000"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	return &WechatClient{
		appID:       "appid-test",
		secret:      "secret-test",
		donutAppID:  "donut-appid-test",
		donutSecret: "donut-secret-test",
		httpClient:  &http.Client{Transport: &rewriteTransport{target: u}},
		rdb:         nil,
	}, srv
}

// userRow 构造 sqlc.GetUserByIDRow 扫描所需的 18 列 mock 行。
func userRow(id, openID string) *pgxmock.Rows {
	now := time.Now()
	return pgxmock.NewRows([]string{
		"id", "open_id", "unionid", "phone_number", "avatar", "avatar_file_id",
		"nickname", "user_type", "phone_bind_time", "auto_record_enabled",
		"personal_family_id", "current_family_id", "invited_by", "lang",
		"created_at", "updated_at", "abnormal_subscribe_accepted", "last_active_at",
	}).AddRow(id, openID, nil, nil, nil, nil,
		pgtype.Text{String: "测试用户", Valid: true}, "wechat", nil, false,
		pgtype.Text{String: "fam-" + id, Valid: true}, pgtype.Text{String: "fam-" + id, Valid: true},
		nil, "zh", now, now, false, now)
}

func loginRequest(code string) *http.Request {
	body := strings.NewReader("{\"code\":\"" + code + "\"}")
	req := httptest.NewRequest(http.MethodPost, "/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeLoginResp(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var resp struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp.Code, resp.Data
}

// ---- phoneModificationLockedToday ----

func TestPhoneModificationLockedToday(t *testing.T) {
	today := timeutil.NowShanghai()
	yesterday := today.AddDate(0, 0, -1)

	base := sqlc.GetUserByIDRow{}

	// 今天绑定过 → true
	todayUser := base
	todayUser.PhoneBindTime = pgtype.Timestamptz{Time: today, Valid: true}
	if !phoneModificationLockedToday(todayUser) {
		t.Fatal("bind today should be locked")
	}

	// 非今天（昨天）绑定 → false
	oldUser := base
	oldUser.PhoneBindTime = pgtype.Timestamptz{Time: yesterday, Valid: true}
	if phoneModificationLockedToday(oldUser) {
		t.Fatal("bind yesterday should NOT be locked")
	}

	// 未绑定（PhoneBindTime 无效）→ false
	if phoneModificationLockedToday(base) {
		t.Fatal("no bind time should NOT be locked")
	}
}

// ---- applyInviteRewardsWithTx ----

// TestApplyInviteRewardsWithTx_ConcurrentUniqueViolation 并发补绑竞态：
// BindInviter 与家庭加入奖励路径同时为同一 invitee 建 user_invites 行时，后到者撞
// user_id 唯一索引（23505）——应幂等跳过奖励返回 nil，而不是 500 回滚（对端事务已发奖）。
func TestApplyInviteRewardsWithTx_ConcurrentUniqueViolation(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	h, _, _ := newLoginHandler(mock, &WechatClient{})

	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("inviter-1").
		WillReturnRows(userRow("inviter-1", "openid-inv"))
	// 事务内幂等预检：此刻尚无邀请行（并发插入发生在预检之后，才走到 INSERT 撞唯一索引）。
	mock.ExpectQuery("FROM user_invites WHERE user_id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("invitee-1").
		WillReturnRows(userRow("invitee-1", "openid-ee"))
	mock.ExpectQuery("INSERT INTO user_invites").
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"})

	if err := h.applyInviteRewardsWithTx(context.Background(), h.pool.Queries(), "invitee-1", "inviter-1"); err != nil {
		t.Fatalf("expected nil on concurrent unique violation, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestApplyInviteRewardsWithTx_ReRegistrationSkipsRewards 注销重注册（openid 墓碑命中）：
// 邀请行照常创建（"我的邀请"列表可见），但整体不发奖——+3/+7 均不发，
// 返回 nil 让注册/绑定照常成功。
func TestApplyInviteRewardsWithTx_ReRegistrationSkipsRewards(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	h, _, _ := newLoginHandler(mock, &WechatClient{})

	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("inviter-1").
		WillReturnRows(userRow("inviter-1", "openid-inv"))
	// 事务内幂等预检：此刻尚无邀请行。
	mock.ExpectQuery("FROM user_invites WHERE user_id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("invitee-1").
		WillReturnRows(userRow("invitee-1", "openid-ee"))
	// 邀请行先建（关系记录进邀请列表）。
	mock.ExpectQuery("INSERT INTO user_invites").
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "user_id", "inviter_id", "entry_count", "reward_inviter_at", "reward_invitee_at", "created_at", "user_open_id"}).
			AddRow("invite-1", "invitee-1", "inviter-1", 0, nil, nil, time.Now(), "openid-ee"))
	// 墓碑命中：openid 曾领过被邀请奖励 → 跳过全部奖励。
	mock.ExpectQuery("FROM user_invites WHERE user_open_id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))

	if err := h.applyInviteRewardsWithTx(context.Background(), h.pool.Queries(), "invitee-1", "inviter-1"); err != nil {
		t.Fatalf("expected nil on re-registration tombstone, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ---- Login flow ----

func newLoginHandler(mock pgxmock.PgxPoolIface, wc *WechatClient) (*Handler, *mockSessions, *mockAuthVIP) {
	sess := &mockSessions{}
	v := &mockAuthVIP{}
	return &Handler{
		pool:       db.NewPoolWithDBTX(mock),
		sessions:   sess,
		wechat:     wc,
		vipService: v,
	}, sess, v
}

// TestLoginApp_UnionidHit wx.weixinAppLogin code → donut/code2verifyinfo 返回
// openapp_info（openid/unionid）→ unionid 命中存量小程序用户 → 登录成功。
func TestLoginApp_UnionidHit(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	wc, _ := newWechatTestServer(t)
	h, sess, _ := newLoginHandler(mock, wc)

	// findOrCreateUser：unionid 命中存量小程序用户
	mock.ExpectQuery("FROM users WHERE unionid = \\$1").
		WithArgs(pgtype.Text{String: "unionid-1", Valid: true}).
		WillReturnRows(userRow("u1", "openid-mp-1"))
	// weixinAppLogin 链路无 session_key（code2verifyinfo 不返回）：跳过 session_key 更新
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(userRow("u1", "openid-mp-1"))
	// LinkWxMPAccountByUnionID + getMPSubscribed
	mock.ExpectExec("UPDATE wx_mp_accounts").
		WithArgs(pgtype.Text{String: "unionid-1", Valid: true}, pgtype.Text{String: "u1", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectQuery("FROM wx_mp_accounts").
		WithArgs(pgtype.Text{String: "u1", Valid: true}).
		WillReturnError(pgx.ErrNoRows)

	rec := httptest.NewRecorder()
	h.LoginApp(rec, loginRequest("app-valid-code"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	code, data := decodeLoginResp(t, rec)
	if code != "0000" {
		t.Fatalf("resp code = %q, want 0000; data=%v", code, data)
	}
	if data["newUser"] != false {
		t.Fatalf("newUser = %v, want false", data["newUser"])
	}
	if !sess.createCalled || sess.createUID != "u1" {
		t.Fatalf("session create not called for u1")
	}
}

// TestLoginApp_InvalidCode code 无效 → 400 invalid wechat code。
func TestLoginApp_InvalidCode(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	wc, _ := newWechatTestServer(t)
	h, _, _ := newLoginHandler(mock, wc)

	rec := httptest.NewRecorder()
	h.LoginApp(rec, loginRequest("bad-code"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestLogin_ValidCode_ExistingUser 有效 code → 命中已有用户 → 更新 session_key → Create 会话 → 返回 session。
func TestLogin_ValidCode_ExistingUser(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// findOrCreateUser: UnionID 为空跳过 GetUserByUnionID；GetUserByOpenID 命中已有用户
	mock.ExpectQuery("FROM users WHERE open_id = \\$1").
		WithArgs("openid-1").
		WillReturnRows(userRow("u1", "openid-1"))
	mock.ExpectExec("UPDATE users SET session_key = \\$2").
		WithArgs("u1", pgtype.Text{String: "sk-1", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(userRow("u1", "openid-1"))
	// Login: UnionID 为空跳过 LinkWxMPAccountByUnionID；GetWxMPAccountByUserID 无记录
	mock.ExpectQuery("FROM wx_mp_accounts").
		WithArgs(pgtype.Text{String: "u1", Valid: true}).
		WillReturnError(pgx.ErrNoRows)

	wc, _ := newWechatTestServer(t)
	h, sess, _ := newLoginHandler(mock, wc)

	rec := httptest.NewRecorder()
	h.Login(rec, loginRequest("valid-code"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	code, data := decodeLoginResp(t, rec)
	if code != "0000" {
		t.Fatalf("resp code = %q, want 0000; data=%v", code, data)
	}
	if data["sessionId"] != "session-1" {
		t.Fatalf("sessionId = %v, want session-1", data["sessionId"])
	}
	if data["newUser"] != false {
		t.Fatalf("newUser = %v, want false", data["newUser"])
	}
	userInfo, _ := data["userInfo"].(map[string]interface{})
	if userInfo == nil || userInfo["id"] != "u1" {
		t.Fatalf("userInfo.id = %v, want u1", userInfo)
	}
	if !sess.createCalled || sess.createUID != "u1" {
		t.Fatalf("sessions.Create called=%v uid=%q, want u1", sess.createCalled, sess.createUID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestLoginApp_MissingOpenid donut errcode=0 但 openapp_info.openid 缺失 → 500
// `wechat app login unavailable` 且不触碰 DB：放行会以 open_id='' 建号，users.open_id
// 唯一索引被空串占位后，后续同类异常响应的主体会命中该行造成账号混淆（02a A-9）。
func TestLoginApp_MissingOpenid(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	wc, _ := newWechatTestServer(t)
	h, _, _ := newLoginHandler(mock, wc)

	rec := httptest.NewRecorder()
	h.LoginApp(rec, loginRequest("app-valid-code-noopenid"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wechat app login unavailable") {
		t.Fatalf("body = %s, want message wechat app login unavailable", rec.Body.String())
	}
	// 不触碰 DB：未设置任何 Expectation，pgxmock 对未预期查询会直接报错。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestLogin_EmptySessionKeySkipsUpdate openid 命中分支的空值守卫：微信异常返回空
// session_key 时跳过 UpdateUserSessionKey，不得用 NULL 覆盖存量 key（与 unionid 分支 /
// 23505 竞态回退分支同款守卫，02a A-1）。
func TestLogin_EmptySessionKeySkipsUpdate(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// openid 命中已有用户；不设置 UPDATE users SET session_key 期望——
	// pgxmock 对未预期查询直接报错，登录成功即证明空 session_key 被跳过。
	mock.ExpectQuery("FROM users WHERE open_id = \\$1").
		WithArgs("openid-1").
		WillReturnRows(userRow("u1", "openid-1"))
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(userRow("u1", "openid-1"))
	mock.ExpectQuery("FROM wx_mp_accounts").
		WithArgs(pgtype.Text{String: "u1", Valid: true}).
		WillReturnError(pgx.ErrNoRows)

	wc, _ := newWechatTestServer(t)
	h, sess, _ := newLoginHandler(mock, wc)

	rec := httptest.NewRecorder()
	h.Login(rec, loginRequest("valid-code-nosk"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !sess.createCalled || sess.createUID != "u1" {
		t.Fatalf("session create not called for u1")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestLogin_ValidCode_NewUser 有效 code → 无已有用户 → tx 内创建用户（家庭/成员/试用 VIP/邀请码）→ 返回 session。
func TestLogin_ValidCode_NewUser(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// findOrCreateUser: GetUserByOpenID 无记录 → 进入创建事务
	mock.ExpectQuery("FROM users WHERE open_id = \\$1").
		WithArgs("openid-1").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO families").
		WithArgs(pgxmock.AnyArg(), true).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fam-new"))
	mock.ExpectQuery("INSERT INTO users").
		WithArgs(pgxmock.AnyArg(), "openid-1", pgtype.Text{}, pgtype.Text{}, pgtype.Text{},
			pgtype.Text{}, pgxmock.AnyArg(), "wechat", pgtype.Timestamptz{}, false,
			pgtype.Text{String: "sk-1", Valid: true}, pgxmock.AnyArg(), pgxmock.AnyArg(), pgtype.Text{}).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "open_id", "unionid", "phone_number", "avatar", "avatar_file_id",
			"nickname", "user_type", "phone_bind_time", "auto_record_enabled",
			"personal_family_id", "current_family_id", "created_at", "updated_at",
			"session_key", "invited_by", "abnormal_subscribe_accepted",
			"abnormal_alert_sent_at", "last_active_at", "lang", "image_storage_bytes",
		}).AddRow("u-new", "openid-1", nil, nil, nil, nil, "测试", "wechat", nil,
			false, "fam-new", "fam-new", time.Now(), time.Now(), "sk-1", nil,
			false, nil, time.Now(), "zh", int64(0)))
	mock.ExpectQuery("INSERT INTO family_members").
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), "owner").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("mem-new"))
	mock.ExpectQuery("INSERT INTO user_invite_codes").
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"user_id", "short_code", "created_at", "expires_at",
		}).AddRow("u-new", "ABCD1234", time.Now(), nil))
	mock.ExpectCommit()
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(userRow("u-new", "openid-1"))
	// Login: GetWxMPAccountByUserID 无记录
	mock.ExpectQuery("FROM wx_mp_accounts").
		WithArgs(pgtype.Text{String: "u-new", Valid: true}).
		WillReturnError(pgx.ErrNoRows)

	wc, _ := newWechatTestServer(t)
	h, sess, v := newLoginHandler(mock, wc)

	rec := httptest.NewRecorder()
	h.Login(rec, loginRequest("valid-code"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	code, data := decodeLoginResp(t, rec)
	if code != "0000" {
		t.Fatalf("resp code = %q, want 0000; data=%v", code, data)
	}
	if data["sessionId"] != "session-1" {
		t.Fatalf("sessionId = %v, want session-1", data["sessionId"])
	}
	if data["newUser"] != true {
		t.Fatalf("newUser = %v, want true", data["newUser"])
	}
	if !sess.createCalled || sess.createUID != "u-new" {
		t.Fatalf("sessions.Create called=%v uid=%q, want u-new", sess.createCalled, sess.createUID)
	}
	if v.issueCalls != 1 {
		t.Fatalf("IssueTrialVIPWithTx calls = %d, want 1", v.issueCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestLogin_InvalidCode 无效 code（errcode=40029）→ 400 invalid wechat code，不触碰 DB。
func TestLogin_InvalidCode(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	wc, _ := newWechatTestServer(t)
	h, _, _ := newLoginHandler(mock, wc)

	rec := httptest.NewRecorder()
	h.Login(rec, loginRequest("bad-code"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid wechat code") {
		t.Fatalf("body = %s, want message invalid wechat code", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ---- 默认头像条件回写（02a A-9）----

// TestWriteDefaultAvatarIfEmpty 注册后异步默认头像回写为条件更新：SQL 仅当
// users.avatar 为空/NULL 时写入，与 LoginApp 同步预填的微信 headimgurl 不再构成
// last-writer-wins 竞态；受影响行数 0（已有头像）不视为错误、不覆盖。
func TestWriteDefaultAvatarIfEmpty(t *testing.T) {
	const avatarURL = "https://cdn.example.com/avatars/u1.png"

	t.Run("avatar empty writes", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("new mock pool: %v", err)
		}
		defer mock.Close()

		h, _, _ := newLoginHandler(mock, &WechatClient{})
		mock.ExpectExec("(?s)UPDATE users SET avatar = \\$2.*avatar IS NULL").
			WithArgs("u1", pgtype.Text{String: avatarURL, Valid: true}, pgtype.Text{}).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))

		if err := h.writeDefaultAvatarIfEmpty(context.Background(), h.pool.Queries(), "u1", avatarURL); err != nil {
			t.Fatalf("write-back failed: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("avatar already set skips overwrite", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("new mock pool: %v", err)
		}
		defer mock.Close()

		h, _, _ := newLoginHandler(mock, &WechatClient{})
		// LoginApp 已写入微信头像：条件更新命中 0 行，默认头像不得覆盖（行数 0 非错误）。
		mock.ExpectExec("(?s)UPDATE users SET avatar = \\$2.*avatar IS NULL").
			WithArgs("u1", pgtype.Text{String: avatarURL, Valid: true}, pgtype.Text{}).
			WillReturnResult(pgxmock.NewResult("UPDATE", 0))

		if err := h.writeDefaultAvatarIfEmpty(context.Background(), h.pool.Queries(), "u1", avatarURL); err != nil {
			t.Fatalf("rows=0 must be a silent skip, got %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

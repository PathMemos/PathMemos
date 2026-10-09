import { login, loginAppWithCode } from '../../utils/request';
import { getErrorMessage, createCancelToken, type CancelToken } from '../../utils/http';
import { setLoggedOut } from '../../utils/storage';
import { opsLog, flushOpsLog } from '../../utils/opslog';
import themeBehavior from '../../behaviors/theme';
import i18nBehavior from '../../behaviors/i18n';

// 登录成功后 _applyLoginResult 可能刚对 newUser/邀请链接发起 redirectTo（Guide/Family），
// 延后等页面栈更新再判断是否需要自行回首页，避免双重跳转竞争。
const GO_HOME_DELAY_MS = 400;

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    wechatLoading: false,
  },

  _isDestroyed: false,
  _cancelToken: null as CancelToken | null,

  onLoad() {
    (this as any)._cancelToken = createCancelToken();
  },

  onShow() {
    (this as any)._isDestroyed = false;
    (this as any)._applyPendingSetData();
    this.updateNavTitle();
  },

  onUnload() {
    (this as any)._isDestroyed = true;
    (this as any)._cancelToken?.cancel();
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('brand.name') });
  },

  async onWechatLogin() {
    if (this.data.wechatLoading) return;
    (this as any)._safeSetData({ wechatLoading: true });
    // 无独立按钮后以全屏 loading 提供登录中反馈（mask 同时防重复点击）。
    wx.showLoading({ title: (this as any).$t('login.loggingIn'), mask: true });
    // 多端 App：wx.weixinAppLogin 直接拉起微信授权（官方多端应用登录，无中间页），
    // code 经 /auth/login/app（服务端 donut/code2verifyinfo）换会话；等待用户在
    // 微信内确认属正常长等待。小程序环境：静默 wx.login → /auth/login。
    const isAppEnv = !!(wx as any).miniapp;
    try {
      if (isAppEnv) {
        const api = (wx as any).weixinAppLogin;
        if (typeof api !== 'function') {
          throw new Error('weixinAppLogin unavailable in app runtime');
        }
        const res: any = await new Promise((resolve, reject) => api({ success: resolve, fail: reject }));
        if ((this as any)._cancelToken?.isCancelled()) {
          wx.hideLoading();
          return;
        }
        await loginAppWithCode(res.code, (this as any)._cancelToken);
      } else {
        await login((this as any)._cancelToken);
      }
      // 用户在登录页的主动登录成功：解除退出/注销停留标记。
      // （静默路径——onLaunch、401 续登、邀请卡片补登——不经过这里，
      // 标记保留，"停留登录页"语义才成立；见 utils/auth.ts notifyLoginSuccess 注释。）
      setLoggedOut(false);
      wx.hideLoading();
      this._goHomeAfterLogin();
    } catch (err) {
      // 先收 loading 再 toast（部分平台 hideLoading 会连带关掉 toast）。
      wx.hideLoading();
      // 登录失败先落本地 opslog（原始 errMsg），登录成功瞬间经 flushOpsLog 补报——
      // App 端授权失败的根因诊断靠它。
      opsLog('login_fail', {
        msg: (err as any)?.errMsg || (err as any)?.message || String(err),
      });
      wx.showToast({ title: getErrorMessage(err, (this as any).$t('login.loginFailed')), icon: 'none' });
    } finally {
      (this as any)._safeSetData({ wechatLoading: false });
    }
  },

  _goHomeAfterLogin() {
    if ((this as any)._isDestroyed) return;
    // 登录成功瞬间补报滞留日志（如未登录期间记录的 login_fail errMsg——
    // /ops/client-log 在鉴权组内，失败当时报不出去，此刻已有 session 可立即上报）。
    flushOpsLog().catch(() => {});
    const go = (useRelaunch: boolean) => {
      const pages = getCurrentPages();
      const current = pages.length > 0 ? pages[pages.length - 1] : null;
      if (!current || current.route !== 'pages/Login/Login') return;
      opsLog('login_step', { step: useRelaunch ? 'redirect_relaunch' : 'redirect_to' });
      const jump = () =>
        useRelaunch
          ? wx.reLaunch({ url: '/pages/index/index' })
          : wx.redirectTo({
              url: '/pages/index/index',
              fail: () => wx.reLaunch({ url: '/pages/index/index' }),
            });
      jump();
    };
    setTimeout(() => {
      if ((this as any)._isDestroyed || (this as any)._cancelToken?.isCancelled()) return;
      go(false);
      // 兜底：800ms 后仍停留在登录页（redirectTo 视觉未生效等端上差异），reLaunch 强制切首页。
      setTimeout(() => go(true), 800);
    }, GO_HOME_DELAY_MS);
  },
});

import '../polyfills/textDecoder';
import { get, post, isSessionExpiredError, type CancelToken } from './http';
import { logger } from './logger';
import {
  getSessionId, setSessionId, clearSessionId,
  setBaseInfo, getBaseInfo,
  getAvatar, needShowXPa, setNeedShowXPa,
  getPendingLinkId, getPendingInviter, clearPendingInviter,
} from './storage';
import { fetchVipInfo, claimNewUserFreeVip } from './vip';


const LOGIN_API = '/auth/login';
const LOGIN_APP_API = '/auth/login/app';


let _isLogining = false;
let _loginFlight: Promise<void> | null = null;
let _loginInitiatorPage: any | null = null;

export const notifyLoginSuccess = () => {
  _isLogining = false;
  _loginFlight = null;
  // 注意：这里不能清退出/注销停留标记——本函数也是后台静默登录（onLaunch、401 续登、
  // 邀请卡片补登）的公共出口，静默登录清标记会让"停留登录页"失效。清标记只在
  // Login 页的用户主动登录成功处（pages/Login/Login.ts onWechatLogin）。
};

export const isLogin = (): boolean => {
  return !!getSessionId();
};

export const getFamilyConfig = async (cancelToken?: CancelToken) => {
  const { data } = await get('/auto-record/config', { cancelToken }, true);
  const enabled = data?.enabled ?? false;
  setBaseInfo({ familyConfig: { autoRecordEnabled: enabled } });
  return { autoRecordEnabled: enabled };
};

const _applyLoginResult = async (data: any, cancelToken?: CancelToken) => {
  if (cancelToken?.isCancelled()) return;
  const userInfo = data?.userInfo || {};
  if (data?.sessionId) {
    setSessionId(data.sessionId);
  }
  setBaseInfo({
    avatar: userInfo.avatarUrl || '',
    userId: userInfo.id || '',
    nickName: userInfo.nickName || '',
  });

  const app = getApp() as any;
  if (app && app.globalData) {
    app.globalData._needRefreshIndexList = true;
  }
  getFamilyConfig().catch((e) => {
    logger.error('获取家庭配置失败', e);
  });

  if (data.newUser) {
    setNeedShowXPa(true);
  }

  if (cancelToken?.isCancelled()) return;

  const pendingLinkId = getPendingLinkId();

  const _isInitiatorPageValid = () => {
    const pages = getCurrentPages();
    const currentPage = pages.length > 0 ? pages[pages.length - 1] : null;
    if (!currentPage || !!(currentPage as any)._isDestroyed || !!(currentPage as any)._isDetached || !!(currentPage as any)._isHidden) {
      return false;
    }
    return _loginInitiatorPage === currentPage;
  };


  if (data.newUser) {
    if (cancelToken?.isCancelled()) return;
    try {
      await claimNewUserFreeVip();
    } catch (err: any) {
      // TRIAL_VIP_ALREADY_CLAIMED 已在 claimNewUserFreeVip 内部收敛为成功，此处不会收到该码。
      logger.error('新用户自动领取免费 VIP 失败', err);
    }
  }


  if (pendingLinkId) {
    const pages = getCurrentPages();
    const currentPage = pages.length > 0 ? pages[pages.length - 1] : null;
    if (currentPage && currentPage.route === 'pages/Family/Family') {
      notifyLoginSuccess();
      return;
    }
    if (!_isInitiatorPageValid()) {
      notifyLoginSuccess();
      return;
    }
    // 受邀新用户：先看 4 页教程再进家庭——pendingLinkId 保留在本地，
    // 教程结束进入家庭页时仍会凭它自动完成加入。
    if (data.newUser && !cancelToken?.isCancelled()) {
      notifyLoginSuccess();
      wx.redirectTo({ url: '/pages/Guide/Guide' });
      return;
    }
    notifyLoginSuccess();
    wx.redirectTo({ url: '/pages/Family/Family' });
    return;
  }
  if (data.newUser) {
    // claim 期间登录发起页可能已销毁/用户已跳走，跳转前补一次取消校验，避免强制重定向。
    if (cancelToken?.isCancelled()) return;
    notifyLoginSuccess();
    wx.redirectTo({ url: '/pages/Guide/Guide' });
    return;
  }
  notifyLoginSuccess();
};

const _doWxLogin = async (cancelToken?: CancelToken) => {
  const res: any = await new Promise((resolve, reject) => {
    wx.login({
      success: resolve,
      fail: reject,
    });
  });

  if (cancelToken?.isCancelled()) return;

  const pendingInviter = getPendingInviter();
  const payload: any = { code: res.code };
  if (pendingInviter) {
    payload.inviter = pendingInviter;
  }
  const { data } = await post(
    LOGIN_API,
    {
      data: payload,
      cancelToken,
    },
    true,
    20000,
    true
  );
  if (pendingInviter) {
    clearPendingInviter();
  }
  await _applyLoginResult(data, cancelToken);
};

// 通用 code 登录：多端 App 环境由 loginAppWithCode 传入 LOGIN_APP_API（wx.weixinAppLogin
// 的 code 走 /auth/login/app）；小程序静默登录走 _doWxLogin，不经此函数。
export const loginWithCode = async (code: string, cancelToken?: CancelToken, api: string = LOGIN_API): Promise<void> => {
  if (_isLogining && _loginFlight) {
    return _loginFlight;
  }
  _isLogining = true;
  const pages = getCurrentPages();
  _loginInitiatorPage = pages.length > 0 ? pages[pages.length - 1] : null;
  _loginFlight = (async () => {
    try {
      if (cancelToken?.isCancelled()) return;
      const pendingInviter = getPendingInviter();
      const payload: any = { code };
      if (pendingInviter) {
        payload.inviter = pendingInviter;
      }
      const { data } = await post(
        api,
        {
          data: payload,
          cancelToken,
        },
        true,
        20000,
        true
      );
      if (pendingInviter) {
        clearPendingInviter();
      }
      await _applyLoginResult(data, cancelToken);
    } catch (error) {
      _isLogining = false;
      _loginFlight = null;
      _loginInitiatorPage = null;
      throw error;
    }
    _isLogining = false;
    _loginFlight = null;
    _loginInitiatorPage = null;
  })();
  return _loginFlight;
};

// 多端 App 微信登录：wx.weixinAppLogin 的 code 经 /auth/login/app
// （服务端 donut/code2verifyinfo）换取会话，响应契约与 /auth/login 一致。
export const loginAppWithCode = async (code: string, cancelToken?: CancelToken): Promise<void> => {
  return loginWithCode(code, cancelToken, LOGIN_APP_API);
};

export const login = async (cancelToken?: CancelToken): Promise<void> => {
  if (getSessionId()) {
    try {
      await get('/user/profile', { cancelToken }, true);
      notifyLoginSuccess();
      fetchVipInfo().catch((e) => {
        logger.error('获取 VIP 信息失败', e);
      });
      return;
    } catch (error: any) {
      // 请求被取消（页面切换 abort）或网络抖动都不能证明会话失效：误清会让有效会话
      // 被登出、当次请求以空 Bearer 发出。仅 401 会话过期哨兵才清会话进入重登。
      if (!isSessionExpiredError(error)) {
        logger.warn('profile 校验未通过（非会话过期），保留现有会话', error);
        return;
      }
      clearSessionId();
    }
  }

  if (_isLogining && _loginFlight) {
    return _loginFlight;
  }
  _isLogining = true;
  const pages = getCurrentPages();
  _loginInitiatorPage = pages.length > 0 ? pages[pages.length - 1] : null;
  _loginFlight = (async () => {
    try {
      // 微信小程序环境：静默 wx.login → POST /auth/login。多端 App 环境的授权登录
      // 在 Login 页走 wx.weixinAppLogin → /auth/login/app。
      await _doWxLogin(cancelToken);
    } catch (error) {
      _isLogining = false;
      _loginFlight = null;
      _loginInitiatorPage = null;
      throw error;
    }
    _isLogining = false;
    _loginFlight = null;
    _loginInitiatorPage = null;
  })();
  return _loginFlight;
};

export {
  getSessionId,
  setSessionId,
  clearSessionId,
  setBaseInfo,
  getBaseInfo,
  getAvatar,
  needShowXPa,
  setNeedShowXPa,
};


import { createCancelToken, getErrorMessage } from '../../../utils/request';
import {
  getSessionId,
  clearUserData,
  getBackendMode,
  getPrivateBackendUrl,
  getPrivateBackendApiKey,
  STORAGE_KEY_MODE,
  STORAGE_KEY_URL,
  STORAGE_KEY_API_KEY,
} from '../../../utils/storage';
import { WORKER_BASE_URL, getHelpBaseURL } from '../../../config/index';
import { openUrl } from '../../../utils/util';
import { logger } from '../../../utils/logger';
import { closeAutoRecord } from '../../../utils/autoRecord';
import { resetVipCache } from '../../../utils/vip';
import themeBehavior from '../../../behaviors/theme';
import i18nBehavior from '../../../behaviors/i18n';
import type { CancelToken } from '../../../utils/http';

function safeSetStorage(key: string, value: string) {
  try {
    wx.setStorageSync(key, value);
  } catch (err) {
    logger.error('storage set failed', { key, err });
  }
}

// Worker /worker/register 错误码（与 api-worker 约定）：4000 地址或 Key 不合法（https/长度），
// 4001 目标非开源版后端，4002 后端不可达，4003 Key 被目标后端拒绝。
// 命中按码映射为 i18n 文案；未命中的 message 原样透出。
function registerFailMessage(err: any, $t: (key: string) => string, fallback: string): string {
  const codeKeyMap: Record<string, string> = {
    '4000': 'backendConfig.invalidUrlOrKey',
    '4001': 'backendConfig.notOpenBackend',
    '4002': 'backendConfig.backendUnreachable',
    '4003': 'backendConfig.apiKeyRejected',
  };
  const key = codeKeyMap[err?.code || ''];
  return key ? $t(key) : getErrorMessage(err, fallback);
}

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    enabled: false,
    backendUrl: '',
    apiKey: '',
    testing: false,
    saving: false,
    currentMode: 'saas',
  },

  _isDestroyed: false,
  _isHidden: false,
  _cancelToken: null as CancelToken | null,

  onLoad() {
    (this as any)._isDestroyed = false;
    const mode = getBackendMode();
    const backendUrl = getPrivateBackendUrl();
    const apiKey = getPrivateBackendApiKey();
    (this as any)._safeSetData({
      enabled: mode === 'private',
      backendUrl,
      apiKey,
      currentMode: mode || 'saas',
    });
  },

  async onShow() {
    (this as any)._isHidden = false;
    (this as any)._applyPendingSetData();
    this.updateNavTitle();
    // 保存成功发生在隐藏态时补执行关键收尾（清 session + 回首页重登），
    // 否则旧 session 会持续请求新后端直到手动重启。
    if ((this as any)._pendingReLogin) {
      (this as any)._pendingReLogin = false;
      // 停自动记录与清 VIP 缓存必须先于清用户态：防旧后端的驻留点经静默重登
      // 写入新后端新账号、60 秒 TTL 内 VIP 门禁命中旧账号状态（与登出/注销对齐）。
      await closeAutoRecord().catch(() => {});
      resetVipCache();
      clearUserData();
      wx.reLaunch({ url: '/pages/index/index' });
    }
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('backendConfig.title') });
  },

  onHide() {
    (this as any)._isHidden = true;
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {}
      this._cancelToken = null;
    }
  },

  onUnload() {
    (this as any)._isDestroyed = true;
    (this as any)._isHidden = true;
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {}
      this._cancelToken = null;
    }
    (this as any).unsubscribeTheme?.();
  },

  onEnabledChange(e: any) {
    const enabled = !!e.detail.value;
    (this as any)._safeSetData({ enabled });
  },

  onBackendUrlInput(e: any) {
    (this as any)._safeSetData({ backendUrl: e.detail.value || '' });
  },

  onApiKeyInput(e: any) {
    (this as any)._safeSetData({ apiKey: e.detail.value || '' });
  },

  copyApiKey() {
    const { apiKey } = this.data;
    if (!apiKey) return;
    wx.setClipboardData({
      data: apiKey,
      success: () => {
        wx.showToast({ title: (this as any).$t('backendConfig.copied'), icon: 'success' });
      },
    });
  },

  openHomepage() {
    // 官方站点链接收敛到 config 集中管理。
    openUrl(getHelpBaseURL());
  },

  async testConnection() {
    if ((this as any)._isDestroyed) return;
    // 与保存互斥（两者共用 _cancelToken，并发会互相取消对方在途请求）。
    if (this.data.testing || this.data.saving) return;
    const { backendUrl, apiKey } = this.data;
    let url = backendUrl.trim();
    if (!url || !apiKey.trim()) {
      wx.showToast({ title: (this as any).$t('backendConfig.urlAndKeyRequired'), icon: 'none' });
      return;
    }
    if (!/^https?:\/\//i.test(url)) {
      url = 'https://' + url;
    }
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {}
    }
    this._cancelToken = createCancelToken();
    (this as any)._safeSetData({ testing: true });
    try {
      const sessionId = getSessionId();
      if (!sessionId) {
        wx.showToast({ title: (this as any).$t('backendConfig.needLogin'), icon: 'none' });
        return;
      }
      // 先临时注册，测试请求才能按 API Key 路由到该私有化后端
      await this._registerPrivateBackend(sessionId, url, apiKey.trim());
      const result: any = await this._directWorkerRequest('/system/config', sessionId, apiKey.trim());
      if ((this as any)._isDestroyed || (this as any)._isHidden) return;
      if (result?.code === '0000' && result?.data?.mode === 'open') {
        // 注册是覆盖式的：若测试的 Key 正是当前生效配置但 URL 不同，测试已把线上路由
        // 改写到测试地址。与失败路径的 _cleanupTestRegistration 对称，这里用已保存的
        // 生效 URL 重新注册一次恢复路由；恢复失败容忍（重新保存即可修复）。
        // 新 Key 测试成功留下的孤儿映射属已知容忍，不处理。
        const activeKey = getPrivateBackendApiKey();
        const activeUrl = getPrivateBackendUrl();
        if (getBackendMode() === 'private' && activeKey === apiKey.trim() && activeUrl && activeUrl !== url && sessionId) {
          this._registerPrivateBackend(sessionId, activeUrl, activeKey).catch(() => {});
        }
        wx.showToast({ title: (this as any).$t('backendConfig.testSuccess'), icon: 'success' });
      } else {
        // 探测到的不是 open 后端：清理刚写入的临时注册，避免 Worker 留下孤儿映射
        this._cleanupTestRegistration(apiKey.trim());
        wx.showModal({
          title: (this as any).$t('backendConfig.testFail'),
          content: result?.message || (this as any).$t('backendConfig.notOpenBackend'),
          showCancel: false,
        });
      }
    } catch (err: any) {
      // 取消/切后台/卸载等路径同样要清理刚写入的临时注册：测试 Key 与生效 Key 相同但
      // URL 不同时，残留映射会把线上请求永久路由到测试地址（与成功路径的恢复逻辑对称）。
      this._cleanupTestRegistration(apiKey.trim());
      if ((this as any)._isDestroyed || (this as any)._isHidden || err?.message === 'request:abort') return;
      wx.showModal({
        title: (this as any).$t('backendConfig.testFail'),
        content: registerFailMessage(err, (this as any).$t.bind(this), (this as any).$t('error.networkFail')),
        showCancel: false,
      });
    } finally {
      // _safeSetData 自带隐藏态暂存/销毁态丢弃，直接调用避免 testing 卡死
      (this as any)._safeSetData({ testing: false });
    }
  },

  // 测试失败后的注册清理：若该 API Key 正是当前生效的私有后端配置，
  // 用已保存的 URL 恢复注册（避免测试新地址失败把正在使用的路由打断）；
  // 否则直接注销临时注册。恢复失败容忍（用户重新保存即可修复）。
  _cleanupTestRegistration(apiKey: string) {
    const activeKey = getPrivateBackendApiKey();
    const activeUrl = getPrivateBackendUrl();
    const sessionId = getSessionId();
    if (getBackendMode() === 'private' && activeKey && activeKey === apiKey && activeUrl && sessionId) {
      this._registerPrivateBackend(sessionId, activeUrl, activeKey).catch(() => {});
    } else {
      this._unregisterPrivateBackend(apiKey).catch(() => {});
    }
  },

  _directWorkerRequest(url: string, sessionId: string, privateApiKey: string): Promise<any> {
    const $t = (this as any).$t.bind(this);
    return new Promise((resolve, reject) => {
      const headers: Record<string, string> = {
        'content-type': 'application/json',
      };
      if (sessionId) {
        headers.Authorization = `Bearer ${sessionId}`;
      }
      if (privateApiKey) {
        headers['X-Private-Api-Key'] = privateApiKey;
      }
      const task = wx.request({
        url: `${WORKER_BASE_URL}${url}`,
        method: 'GET',
        header: headers,
        timeout: 20000,
        success(res: any) {
          resolve(res.data);
        },
        fail(err: any) {
          reject(new Error(err?.errMsg || $t('error.networkFail')));
        },
      });
      if (this._cancelToken) {
        (this._cancelToken as any)._addAbort(() => {
          try { task.abort(); } catch {}
          reject(new Error('request:abort'));
        });
      }
    });
  },

  async save() {
    if ((this as any)._isDestroyed) return;
    // 与测试互斥（两者共用 _cancelToken，并发会互相取消对方在途请求）。
    if (this.data.saving || this.data.testing) return;
    const { enabled, backendUrl, apiKey } = this.data;
    let normalizedUrl = backendUrl.trim();
    if (enabled) {
      if (!normalizedUrl || !apiKey.trim()) {
        wx.showToast({ title: (this as any).$t('backendConfig.urlAndKeyRequired'), icon: 'none' });
        return;
      }
      if (!/^https?:\/\//i.test(normalizedUrl)) {
        normalizedUrl = 'https://' + normalizedUrl;
      }
    }
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {}
    }
    this._cancelToken = createCancelToken();
    (this as any)._safeSetData({ saving: true });
    try {
      if (enabled) {
        const sessionId = getSessionId();
        if (!sessionId) {
          wx.showToast({ title: (this as any).$t('backendConfig.needLogin'), icon: 'none' });
          return;
        }
        await this._registerPrivateBackend(sessionId, normalizedUrl, apiKey.trim());
        // 停自动记录必须先于 mode/URL 写入：写入后 getBaseURL() 即指向新后端，
        // 此刻 closeAutoRecord 的末批上报会 401 触发静默重登新后端（串号/幽灵账号）。
        await closeAutoRecord().catch(() => {});
        resetVipCache();
        safeSetStorage(STORAGE_KEY_URL, normalizedUrl);
        safeSetStorage(STORAGE_KEY_API_KEY, apiKey.trim());
        safeSetStorage(STORAGE_KEY_MODE, 'private');
      } else {
        const apiKey = getPrivateBackendApiKey();
        if (apiKey) {
          // 注销失败（如 Worker 不可达）不阻断切回 SaaS：本地配置照常清理，
          // Worker 侧残留的映射属已知容忍，重新保存私有模式时会被覆盖。
          try {
            await this._unregisterPrivateBackend(apiKey);
          } catch (err) {
            logger.warn('注销私有后端失败，继续切回 SaaS', err);
          }
        }
        // 同上：停记录先于 mode 写入
        await closeAutoRecord().catch(() => {});
        resetVipCache();
        safeSetStorage(STORAGE_KEY_URL, '');
        safeSetStorage(STORAGE_KEY_API_KEY, '');
        safeSetStorage(STORAGE_KEY_MODE, 'saas');
      }
      if ((this as any)._isDestroyed) return;
      // 保存本身已生效（storage + Worker 注册），"清 session + reLaunch"是关键收尾：
      // 页面隐藏态时跳过会导致旧 session 持续打新后端，这里挂起并在 onShow 补执行。
      if ((this as any)._isHidden) {
        (this as any)._pendingReLogin = true;
        return;
      }
      wx.showModal({
        title: (this as any).$t('backendConfig.saveSuccess'),
        content: (this as any).$t('backendConfig.reloginTip'),
        showCancel: false,
        success: async () => {
          // 停自动记录与清 VIP 缓存先于清用户态（同 onShow 补执行路径的对齐说明）。
          await closeAutoRecord().catch(() => {});
          resetVipCache();
          clearUserData();
          wx.reLaunch({ url: '/pages/index/index' });
        },
      });
    } catch (err: any) {
      if ((this as any)._isDestroyed || (this as any)._isHidden || err?.message === 'request:abort') return;
      wx.showModal({
        title: (this as any).$t('backendConfig.saveFail'),
        content: registerFailMessage(err, (this as any).$t.bind(this), (this as any).$t('error.DEFAULT')),
        showCancel: false,
      });
    } finally {
      // _safeSetData 自带隐藏态暂存/销毁态丢弃，直接调用避免 saving 卡死
      (this as any)._safeSetData({ saving: false });
    }
  },

  _registerPrivateBackend(sessionId: string, url: string, apiKey: string): Promise<any> {
    const $t = (this as any).$t.bind(this);
    return new Promise((resolve, reject) => {
      const task = wx.request({
        url: `${WORKER_BASE_URL}/worker/register`,
        method: 'POST',
        header: {
          'content-type': 'application/json',
          Authorization: `Bearer ${sessionId}`,
        },
        data: { url, apiKey },
        timeout: 20000,
        success(res: any) {
          const data = res.data || {};
          if (data.code === '0000') {
            resolve(data);
          } else {
            // 透传 Worker 错误码，上层按码映射为可操作的提示
            const err: any = new Error(data.message || $t('backendConfig.registerFail'));
            err.code = data.code;
            reject(err);
          }
        },
        fail(err: any) {
          reject(new Error(err?.errMsg || $t('error.networkFail')));
        },
      });
      if (this._cancelToken) {
        (this._cancelToken as any)._addAbort(() => {
          try { task.abort(); } catch {}
          reject(new Error('request:abort'));
        });
      }
    });
  },

  _unregisterPrivateBackend(apiKey: string): Promise<any> {
    const $t = (this as any).$t.bind(this);
    return new Promise((resolve, reject) => {
      const sessionId = getSessionId();
      if (!sessionId) {
        resolve({});
        return;
      }
      const task = wx.request({
        url: `${WORKER_BASE_URL}/worker/register`,
        method: 'DELETE',
        header: {
          'content-type': 'application/json',
          Authorization: `Bearer ${sessionId}`,
        },
        data: { apiKey },
        timeout: 20000,
        success(res: any) {
          resolve(res.data || {});
        },
        fail(err: any) {
          reject(new Error(err?.errMsg || $t('error.networkFail')));
        },
      });
      if (this._cancelToken) {
        (this._cancelToken as any)._addAbort(() => {
          try { task.abort(); } catch {}
          reject(new Error('request:abort'));
        });
      }
    });
  },
});

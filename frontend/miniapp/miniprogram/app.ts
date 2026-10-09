
import './polyfills/textDecoder';
import request from './utils/request';
import { tryRestoreAutoRecord, onAppShow, onAppHide, STORAGE_KEY_ENABLED } from './utils/autoRecord';
import { getSystemInfo, clearSystemInfoCache } from './utils/util';
import { logger } from './utils/logger';
import { flushOpsLog } from './utils/opslog';
import { themeManager } from './utils/theme';
import { i18n } from './utils/i18n';
import { getLoggedOut, setPendingInviter } from './utils/storage';

const t = (key: string) => i18n.t(key);

App<IAppOption>({
  globalData: {
    openAutoRecorded: false,
    _needRefreshIndexList: true,
    _lastAutoRecordRestoreTime: 0,
  },
  onLaunch(options?: WechatMiniprogram.App.LaunchShowOption) {
    // 多端 App（Donut）环境标记：wx.miniapp 仅 App 运行时注入（各页面按此隐藏/替换
    // 微信 open-type 能力：getPhoneNumber/chooseAvatar/share 等）。
    (this as any).globalData.isAppEnv = !!(wx as any).miniapp;

    const systemInfo = getSystemInfo();
    (this as any).globalData._systemInfo = systemInfo;
    (this as any).globalData.theme = systemInfo.theme || 'light';

    i18n.init();

    themeManager.init();
    themeManager.onChange((theme) => {
      (this as any).globalData.theme = theme;
    });

    if (!(this as any)._windowResizeRegistered) {
      (this as any)._windowResizeRegistered = true;
      wx.onWindowResize(() => {
        clearSystemInfoCache();
      });
    }

    const enabled = !!wx.getStorageSync(STORAGE_KEY_ENABLED);
    (this as any).globalData.openAutoRecorded = enabled;

    const doLogin = () => {
      request.login()
        .then(() => request.isLogin())
        .then((loggedIn) => {
          if (loggedIn) {
            
            (this as any).globalData._needRefreshIndexList = true;
            tryRestoreAutoRecord().then((ok) => {
              // 恢复成功才进入 30 秒冷却；失败时保持时间戳为 0，
              // 让紧随其后的 onShow 能立即重试。
              if (ok) {
                (this as any).globalData._lastAutoRecordRestoreTime = Date.now();
              }
            }).catch(() => {});
            request.put('/user/lang', { data: { lang: i18n.getLocale() } }).catch(() => {});
          }
        })
        .catch((err: any) => {
          logger.warn('启动登录或恢复自动记录失败', err);
          // 登录失败（微信授权失败/被拒、网络抖动、后端瞬时不可用）时跳转登录页，
          // 由用户手动重试（微信一键登录；多端 App 经官方授权页拉起）。
          (this as any)._redirectToLogin();
          // 登录失败时也要尝试恢复自动记录：
          // tryRestoreAutoRecord 内部会按需触发登录与退避重试，避免登录失败
          // 直接阻断自动记录恢复，导致用户打开小程序后功能静默停摆。
          tryRestoreAutoRecord().catch(() => {});
        });
    };

    const inviter = options?.query?.inviter;
    const scene = options?.query?.scene;

    // 主动退出/注销停留标记：启动不再静默登录——否则 onLaunch 登录会清掉停留标记
    // 重建会话，注销场景更会静默注册新账号，"停留登录页"活不过一次冷启动。
    // inviter/scene 照常解析入 storage，后续在登录页手动登录仍会带上邀请归属。
    const loggedOut = getLoggedOut();
    const startLogin = () => {
      if (!loggedOut) doLogin();
    };

    if (inviter) {
      setPendingInviter(inviter);
      startLogin();
    } else if (scene) {
      this._resolveSceneAndLogin(scene, startLogin);
    } else {
      startLogin();
    }

    this.checkForUpdate();

    // OPS-LOG：打开小程序时上报本地记录的操作日志（失败保留，下次再报）
    flushOpsLog().catch(() => {});
  },

  _redirectToLogin() {
    try {
      const pages = getCurrentPages();
      const current = pages.length > 0 ? pages[pages.length - 1] : null;
      if (current && current.route === 'pages/Login/Login') return;
      wx.redirectTo({ url: '/pages/Login/Login' });
    } catch (e) {
      // 启动极早期页面栈尚未建立时 redirectTo 会抛错，吞掉即可：
      // 未登录的兜底恢复由 onShow 的 tryRestoreAutoRecord 退避重试。
      logger.warn('跳转登录页失败', e);
    }
  },

  _resolveSceneAndLogin(sceneValue: string, doLogin: () => void) {
    try {
      const decoded = decodeURIComponent(sceneValue);
      const shortCode = decoded.toUpperCase().replace(/[^A-Z0-9]/g, '');
      if (shortCode.length < 6) {
        doLogin();
        return;
      }
      request.get('/invite/resolve', { params: { code: shortCode } }, false)
        .then(({ data }: any) => {
          if (data?.userId) {
            setPendingInviter(data.userId);
          }
        })
        .catch((err: any) => {
          logger.warn('场景码解析失败', err);
        })
        .finally(() => {
          doLogin();
        });
    } catch (e) {
      logger.warn('场景码处理失败', e);
      doLogin();
    }
  },
  onShow() {
    const app = this as any;
    (async () => {
      const lastRestoreTime = app.globalData._lastAutoRecordRestoreTime || 0;
      const now = Date.now();
      if (now - lastRestoreTime > 30000) {
        // 未登录时也尝试恢复：tryRestoreAutoRecord 内部带登录与退避重试，
        // 避免登录态丢失时（微信清理 storage/session 过期）自动记录静默停摆。
        // 仅在恢复成功后才更新时间戳进入冷却；失败时保持 0，下次 onShow 立即重试。
        const ok = await tryRestoreAutoRecord();
        if (ok) {
          app.globalData._lastAutoRecordRestoreTime = Date.now();
        }
      }
      // 恢复流程内部可能已完成登录：这里重新取值，避免"恢复期间登录"被跳过 onAppShow/VIP 刷新。
      if (request.isLogin()) {
        onAppShow();
        const vipInfo = (request as any).getVipInfo?.();
        const needRefresh = !vipInfo || (Date.now() - (vipInfo._fetchTime || 0) > 5 * 60 * 1000);
        if (needRefresh) {
          request.fetchVipInfo().catch((err: any) => {
            logger.error('onShow 刷新 VIP 信息失败', err);
          });
        }
      }
    })();
  },
  onHide() {
    onAppHide();
  },
  onError(err: any) {
    const pages = getCurrentPages();
    const route = pages.length > 0 ? pages[pages.length - 1].route : '';
    const msg = typeof err === 'string' ? err : (err?.message || String(err));
    logger.error('未捕获小程序异常', { message: msg, route });
  },
  onUnhandledRejection(res: any) {
    const pages = getCurrentPages();
    const route = pages.length > 0 ? pages[pages.length - 1].route : '';
    const reason = res?.reason || res;
    const msg = typeof reason === 'string' ? reason : (reason?.message || String(reason));
    logger.error('未捕获 Promise 拒绝', { message: msg, route });
  },
  checkForUpdate() {
    const updateManager = wx.getUpdateManager();
    updateManager.onCheckForUpdate((res) => {
      if (res.hasUpdate) {
        updateManager.onUpdateReady(() => {
          wx.showModal({
            title: t('app.updateTitle'),
            content: t('app.updateContent'),
            success: (res) => {
              if (res.confirm) {
                updateManager.applyUpdate();
              }
            },
          });
        });
        updateManager.onUpdateFailed(() => {
          wx.showModal({
            title: t('app.updateFailTitle'),
            content: t('app.updateFailContent'),
            showCancel: false,
          });
        });
      }
    });
  },
});

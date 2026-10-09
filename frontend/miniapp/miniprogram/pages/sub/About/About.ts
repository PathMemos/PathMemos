
import request, { createCancelToken, getBaseInfo } from '../../../utils/request';
import { openUrl } from '../../../utils/util';
import { clearUserData, getBackendMode, setLoggedOut } from '../../../utils/storage';
import { resetVipCache } from '../../../utils/vip';
import { closeAutoRecord } from '../../../utils/autoRecord';
import { logger } from '../../../utils/logger';
import { getHelpBaseURL } from '../../../config/index';
import themeBehavior from '../../../behaviors/theme';
import i18nBehavior from '../../../behaviors/i18n';
import type { CancelToken } from '../../../utils/http';

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    confirmDialog: {
      visible: false,
      title: '',
      content: '',
      cancelText: '',
      confirmText: '',
      confirmType: 'default',
    },
    accountName: '',
    deleteConfirmInput: '',
    canConfirmDelete: false,
    deleting: false,
    isPrivateBackend: false,
  },

  _isDestroyed: false,
  _isHidden: false,

  _destroyCancelToken: undefined as CancelToken | undefined,

  onLoad() {
    (this as any)._isDestroyed = false;
    this._destroyCancelToken = createCancelToken();
    (this as any)._safeSetData({ isPrivateBackend: getBackendMode() === 'private' });
  },

  onShow() {
    (this as any)._isHidden = false;
    (this as any)._applyPendingSetData();
    this.updateNavTitle();
    this.loadAccountName();
  },

  loadAccountName() {
    const baseInfo = getBaseInfo() || {};
    const accountName = baseInfo.nickName || '';
    (this as any)._safeSetData({ accountName, deleteConfirmInput: '', canConfirmDelete: false });
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('about.title') });
  },

  onHide() {
    (this as any)._isHidden = true;
    (this as any)._forceSetData({ 'confirmDialog.visible': false });
    // 账号注销是破坏性操作，即使页面隐藏也应让它继续完成，不在此处取消。
  },

  onUnload() {
    (this as any)._forceSetData({ 'confirmDialog.visible': false });
    (this as any)._isDestroyed = true;
    (this as any)._isHidden = true;
    // 账号注销请求不应因页面卸载而被中断；后续回调已通过 _isDestroyed 做生命周期保护。
    (this as any).unsubscribeTheme?.();
  },

  openByPrivacyPolicy() {
    openUrl(`${getHelpBaseURL()}/tutorial/privacy-policy/`);
  },

  openByServices() {
    openUrl(`${getHelpBaseURL()}/tutorial/model/`);
  },

  destruction() {
    (this as any)._safeSetData({
      deleteConfirmInput: '',
      canConfirmDelete: false,
      confirmDialog: {
        visible: true,
        title: (this as any).$t('about.deleteConfirmTitle'),
        content: (this as any).$t('about.deleteConfirmContent', { name: this.data.accountName }),
        cancelText: (this as any).$t('common.cancel'),
        confirmText: (this as any).$t('about.deleteAccount'),
        confirmType: 'danger',
      },
    });
  },

  onDeleteInput(e: any) {
    const value = (e.detail?.value || '').trim();
    const accountName = ((this as any).data.accountName || '').trim();
    (this as any)._safeSetData({
      deleteConfirmInput: value,
      canConfirmDelete: value === accountName && accountName !== '',
    });
  },

  async onConfirmDialogConfirm() {
    // 防重入：setData 生效窗口内双击会双发 DELETE /auth/account。
    if ((this as any).data.deleting) {
      return;
    }
    const confirmName = ((this as any).data.deleteConfirmInput || '').trim();
    if (!confirmName) {
      return;
    }
    (this as any)._safeSetData({ deleting: true, 'confirmDialog.visible': false });
    // 停自动记录必须先于注销请求：关停前的末批上报依赖有效会话，放在注销之后会
    // 401 触发静默重登——账号已硬删，会以同一微信身份注册"幽灵账号"并领新人 VIP。
    await closeAutoRecord().catch(() => {});
    request.del('/auth/account', { data: { confirmName }, cancelToken: (this as any)._destroyCancelToken }, true)
      .then(async () => {
        // 服务端已删除账号，后续本地清理、提示、跳转均为 best-effort，
        // 任一环节失败也不应回退“已注销”事实或误导用户。
        try {
          // 本地清理必须执行，不能因页面隐藏/销毁而跳过（否则残留 session 与
          // 本地数据）；仅成功 toast 等 UI 反馈在页面不可见时省略。
          resetVipCache();
          clearUserData();
          if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
            wx.showToast({ title: (this as any).$t('about.deleteAccount'), icon: 'success' });
          }
        } catch (cleanupErr) {
          logger.error('注销后清理失败', cleanupErr);
        }
        // 置停留标记并落登录页：注销后不自动重登新账号——登录页手动点击后才创建新账号；
        // 新用户引导（Guide）由登录页 _applyLoginResult 统一路由。
        setLoggedOut(true);
        wx.reLaunch({ url: '/pages/Login/Login' });
      })
      .catch((err: any) => {
        (this as any)._safeSetData({ deleting: false });
        if ((this as any)._isDestroyed || (this as any)._isHidden) return;
        if (err?.message === 'request:abort') return;
        logger.error('注销账号失败', err);
        wx.showToast({ title: (this as any).$t('error.DEFAULT'), icon: 'none' });
      });
  },

  onConfirmDialogCancel() {
    (this as any)._safeSetData({ 'confirmDialog.visible': false });
  },

  toChat() {
    openUrl(`${getHelpBaseURL()}/tutorial/about/`);
  },

  /** 跳转姊妹小程序「爬爬家庭助手」:必须由用户点击触发(微信自动弹确认框);全屏跳转无需 appid 名单声明 */
  toFamilyApp() {
    wx.navigateToMiniProgram({
      appId: 'wx66f181d33f61a691',
      path: 'pages/map/map',
      fail: (res) => {
        if ((res.errMsg || '').includes('cancel')) return; // 用户在确认框点了取消
        wx.showToast({ title: (this as any).$t('error.DEFAULT'), icon: 'none' });
      },
    });
  },

  toGuide() {
    wx.navigateTo({ url: '/pages/sub/UsageGuide/UsageGuide' });
  },
});

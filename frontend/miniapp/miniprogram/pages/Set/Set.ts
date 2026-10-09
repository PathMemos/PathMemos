
import request, { getErrorMessage, resetLoading } from '../../utils/request';
import { logger } from '../../utils/logger';
import { themeManager, ThemeMode } from '../../utils/theme';
import { i18n, LanguageMode } from '../../utils/i18n';
import { getSystemInfo } from '../../utils/util';
import { clearUserData, getBackendMode, setLoggedOut } from '../../utils/storage';
import { closeAutoRecord } from '../../utils/autoRecord';
import { resetVipCache } from '../../utils/vip';
import { getHelpBaseURL } from '../../config/index';
import themeBehavior from '../../behaviors/theme';
import i18nBehavior from '../../behaviors/i18n';

const getThemeOptions = (): { value: ThemeMode; label: string }[] => [
  { value: 'auto', label: i18n.t('set.darkModeAuto') },
  { value: 'light', label: i18n.t('set.darkModeLight') },
  { value: 'dark', label: i18n.t('set.darkModeDark') },
];

const getLanguageOptions = (): { value: LanguageMode; label: string }[] => [
  { value: 'auto', label: i18n.t('set.languageAuto') },
  { value: 'zh', label: i18n.t('set.languageZh') },
  { value: 'en', label: i18n.t('set.languageEn') },
  { value: 'zh-Hant', label: i18n.t('set.languageZhHant') },
];

const getLanguageLabel = (mode: LanguageMode) => getLanguageOptions().find((o) => o.value === mode)?.label || getLanguageOptions()[0].label;

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    userAvatar: request.getAvatar(),
    baseInfo: request.getBaseInfo() || ({} as any),
    showNicknameDrawer: false,
    tempNickname: '',
    showOfficialAccountDialog: false,
    phone: '',
    canModifyToday: true,
    mpSubscribed: false,
    themeMode: themeManager.getMode() as ThemeMode,
    themeOptions: getThemeOptions(),
    themeModeLabel: getThemeOptions().find((o) => o.value === themeManager.getMode())?.label || '',
    languageMode: i18n.getMode(),
    languageOptions: getLanguageOptions(),
    languageLabel: getLanguageLabel(i18n.getMode()),
    showDesktopGuide: false,
    desktopGuideCanOpenSettings: false,
    desktopGuideHintTop: 0,
    desktopGuideHintRight: 0,
    followImgUrl: `${getHelpBaseURL()}/follow.png`,
    isPrivateBackend: false,
    confirmDialog: {
      visible: false,
      title: '',
      content: '',
      cancelText: '',
      confirmText: '',
      confirmType: 'default',
    },
  },

  _isDestroyed: false,
  _isHidden: false,

  onShow() {
    (this as any)._isDestroyed = false;
    // 多端 App：open-type 的微信开放能力（getPhoneNumber/chooseAvatar）不可用，
    // 隐藏手机号绑定/解绑与改头像入口（头像随微信登录资料自动带出）。
    (this as any)._safeSetData({ isAppEnv: !!(getApp() as any)?.globalData?.isAppEnv });
    (this as any)._isHidden = false;
    (this as any)._applyPendingSetData();
    (this as any)._safeSetData({ isPrivateBackend: getBackendMode() === 'private' });
    const userAvatar = request.getAvatar();
    const baseInfo = request.getBaseInfo() || {};
    const currentBase = this.data.baseInfo || {};
    const baseInfoChanged =
      currentBase.userId !== baseInfo.userId ||
      currentBase.nickName !== baseInfo.nickName ||
      currentBase.avatar !== baseInfo.avatar;
    if (this.data.userAvatar !== userAvatar || baseInfoChanged) {
      (this as any)._safeSetData({ userAvatar, baseInfo });
    }

    const mode = themeManager.getMode();
    const themeOptions = getThemeOptions();
    (this as any)._safeSetData({
      themeMode: mode,
      themeOptions,
      themeModeLabel: themeOptions.find((o) => o.value === mode)?.label || '',
      theme: themeManager.getTheme(),
      languageMode: i18n.getMode(),
      languageOptions: getLanguageOptions(),
      languageLabel: getLanguageLabel(i18n.getMode()),
    });

    if ((this as any)._cancelToken) {
      try { (this as any)._cancelToken.cancel(); } catch {}
    }
    (this as any)._cancelToken = request.createCancelToken();
    this.fetch((this as any)._cancelToken);
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('set.title') });
  },

  onLocaleChange() {
    const themeOptions = getThemeOptions();
    const mode = themeManager.getMode();
    (this as any)._safeSetData({
      themeOptions,
      themeModeLabel: themeOptions.find((o) => o.value === mode)?.label || '',
      languageMode: i18n.getMode(),
      languageOptions: getLanguageOptions(),
      languageLabel: getLanguageLabel(i18n.getMode()),
    });
  },

  setNickname() {
    (this as any)._safeSetData({
      showNicknameDrawer: true,
      tempNickname: this.data.baseInfo.nickName || '',
    });
  },

  onNicknameInput(e: any) {
    (this as any)._safeSetData({ tempNickname: e.detail.value });
  },

  async submitNickname() {
    const nickname = (this.data.tempNickname || '').trim();
    if (!nickname) {
      wx.showToast({ title: (this as any).$t('set.nicknameEmpty'), icon: 'none' });
      return;
    }
    if ([...nickname].length > 20) {
      wx.showToast({ title: (this as any).$t('set.nicknameTooLong'), icon: 'none' });
      return;
    }
    // 按 FP063/FP076：昵称修改为普通业务，不做函数级防重入锁。
    if ((this as any)._submitNicknameCancelToken) {
      try { (this as any)._submitNicknameCancelToken.cancel(); } catch {}
    }
    (this as any)._submitNicknameCancelToken = request.createCancelToken();
    try {
      await request.put('/user/nickname', { data: { nickName: nickname }, cancelToken: (this as any)._submitNicknameCancelToken }, true);
      if (this._isDestroyed) return;
      const baseInfo = request.getBaseInfo() || {};
      request.setBaseInfo({ ...baseInfo, nickName: nickname });
      (getApp() as any).globalData._needRefreshIndexList = true;
      wx.showToast({ title: (this as any).$t('set.modifySuccess'), icon: 'success' });
      (this as any)._safeSetData({ 'baseInfo.nickName': nickname, showNicknameDrawer: false });
    } catch (error: any) {
      if ((this as any)._isDestroyed) return;
      if (error?.message === 'request:abort') return;
      wx.showToast({ title: getErrorMessage(error, (this as any).$t('set.updateFail')), icon: 'none' });
    } finally {
      (this as any)._submitNicknameCancelToken = null;
    }
  },

  hiddenNicknameDrawer() {
    (this as any)._safeSetData({ showNicknameDrawer: false });
  },

  async onChooseAvatar(e: any) {
    if ((this as any)._updatingAvatar) return;
    (this as any)._updatingAvatar = true;
    if ((this as any)._avatarCancelToken) {
      try { (this as any)._avatarCancelToken.cancel(); } catch {}
    }
    (this as any)._avatarCancelToken = request.createCancelToken();
    const { avatarUrl } = e.detail;
    try {
      const userAvatar = await request.updateAvatar(avatarUrl, { cancelToken: (this as any)._avatarCancelToken });
      if ((this as any)._isDestroyed) return;
      (this as any)._safeSetData({ userAvatar });
      (getApp() as any).globalData._needRefreshIndexList = true;
      wx.showToast({ title: (this as any).$t('set.updateSuccess'), icon: 'success' });
    } catch (error: any) {
      if ((this as any)._isDestroyed) return;
      if (error?.message === 'request:abort') return;
      if (error?.code === 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED') {
        this.showStorageLimitDialog();
        return;
      }
      if (!error?._handledByModal) {
        wx.showToast({ title: (this as any).$t('set.updateFail'), icon: 'none' });
      }
    } finally {
      (this as any)._updatingAvatar = false;
      (this as any)._avatarCancelToken = null;
    }
  },

  showStorageLimitDialog() {
    (this as any)._safeSetData({
      confirmDialog: {
        visible: true,
        title: (this as any).$t('common.tip'),
        content: (this as any).$t('error.imageStorageLimitExceeded'),
        cancelText: (this as any).$t('common.cancel'),
        confirmText: (this as any).$t('vip.upgradeNow'),
        confirmType: 'default',
      },
    });
  },

  onConfirmDialogConfirm() {
    (this as any)._safeSetData({ 'confirmDialog.visible': false });
    wx.navigateTo({ url: '/pages/sub/Vip/Vip' });
  },

  onConfirmDialogCancel() {
    (this as any)._safeSetData({ 'confirmDialog.visible': false });
  },

  goCommonAddresses() {
    wx.navigateTo({ url: '/pages/CommonAddresses/CommonAddresses' });
  },

  onLogoutTap() {
    const self = this as any;
    wx.showModal({
      title: self.$t('set.logout'),
      content: self.$t('set.logoutConfirm'),
      confirmText: self.$t('set.logout'),
      cancelText: self.$t('common.cancel'),
      confirmColor: '#f6353b',
      success: async (res: any) => {
        if (!res.confirm || self._isDestroyed) return;
        await self._performLogout();
      },
    });
  },

  async _performLogout() {
    // 停自动记录必须最先：关停前的末批上报依赖有效会话，放在登出请求之后会 401
    // 触发静默重登（服务端残留 30 天会话、停留标记被清）。与 About 注销对齐。
    await closeAutoRecord().catch(() => {});
    try {
      await request.post('/auth/logout', { data: {} }, true, 10000, true);
    } catch (e: any) {
      logger.warn('logout server session failed', e);
    }
    // VIP 内存缓存随用户态清理：防 60 秒 TTL 内换账号命中旧账号 isVip/到期时间。
    resetVipCache();
    clearUserData();
    // 置停留标记：首页冷启动不再静默登录，落登录页等手动点击（登录页主动登录时清除）
    setLoggedOut(true);
    // 落登录页给出明确的已退出状态；小程序点微信一键即静默重登，App 走官方授权页。
    wx.reLaunch({ url: '/pages/Login/Login' });
  },

  onThemeTap() {
    const options = getThemeOptions();
    const itemList = options.map((o) => o.label);
    wx.showActionSheet({
      itemList,
      success: (res) => {
        if ((this as any)._isDestroyed) return;
        const option = options[res.tapIndex];
        if (!option) return;
        const mode = option.value;
        themeManager.setMode(mode);
        (this as any)._safeSetData({
          themeMode: mode,
          themeModeLabel: option.label,
        });
      },
      fail: (err: any) => {
        if ((this as any)._isDestroyed) return;
        const errMsg = err?.errMsg || '';
        if (errMsg.includes('cancel')) return;
        wx.showToast({ title: (this as any).$t('error.DEFAULT'), icon: 'none' });
      },
    });
  },

  onLanguageTap() {
    const options = getLanguageOptions();
    const itemList = options.map((o) => o.label);
    wx.showActionSheet({
      itemList,
      success: (res) => {
        if ((this as any)._isDestroyed) return;
        const option = options[res.tapIndex];
        if (!option) return;
        i18n.setMode(option.value);
        request.put('/user/lang', { data: { lang: i18n.getLocale() } }).catch(() => {});
        (this as any)._safeSetData({
          languageMode: option.value,
          languageLabel: option.label,
        });
      },
      fail: (err: any) => {
        if ((this as any)._isDestroyed) return;
        const errMsg = err?.errMsg || '';
        if (errMsg.includes('cancel')) return;
        wx.showToast({ title: (this as any).$t('error.DEFAULT'), icon: 'none' });
      },
    });
  },

  async fetch(cancelToken?: any) {
    try {
      const [, phoneInfo, profileInfo] = await Promise.all([
        request.getFamilyConfig(cancelToken),
        request.get('/auth/phone', { cancelToken }, true),
        request.get('/user/profile', { cancelToken }, true),
      ]);
      (this as any)._safeSetData({
        phone: phoneInfo.data?.phoneNumber || '',
        canModifyToday: phoneInfo.data?.canModifyToday ?? true,
        mpSubscribed: profileInfo.data?.mpSubscribed ?? false,
      });
    } catch (e: any) {
      if (e?.message === 'request:abort' || (this as any)._isDestroyed) return;
      wx.showToast({ title: (this as any).$t('set.loadFail'), icon: 'none' });
    }
  },

  onHide() {
    (this as any)._isHidden = true;
    // 昵称编辑抽屉在 onHide 中保留，避免 wx.chooseAvatar / 复制文本等系统调用
    // 触发 onHide 后返回抽屉消失。非编辑弹窗（如桌面引导）可在 onHide 关闭。
    // 服务号组件保持展示，避免 hide/show 循环后需要用户重新点击触发绑定。
    // 头像/昵称/手机绑定等写操作请求不在 onHide 取消，避免系统调用返回后失败无感知。
    (this as any)._forceSetData({
      showDesktopGuide: false,
    });
    if ((this as any)._cancelToken) {
      try { (this as any)._cancelToken.cancel(); } catch {}
      (this as any)._cancelToken = null;
    }
    resetLoading();
  },

  onUnload() {
    (this as any)._forceSetData({
      showNicknameDrawer: false,
      showOfficialAccountDialog: false,
      showDesktopGuide: false,
    });
    (this as any)._isDestroyed = true;
    (this as any)._isHidden = true;
    // 页面销毁时才取消写操作请求。
    if ((this as any)._avatarCancelToken) {
      try { (this as any)._avatarCancelToken.cancel(); } catch {}
      (this as any)._avatarCancelToken = null;
    }
    if ((this as any)._submitNicknameCancelToken) {
      try { (this as any)._submitNicknameCancelToken.cancel(); } catch {}
      (this as any)._submitNicknameCancelToken = null;
    }
    if ((this as any)._bindPhoneCancelToken) {
      try { (this as any)._bindPhoneCancelToken.cancel(); } catch {}
      (this as any)._bindPhoneCancelToken = null;
    }
    // onHide 中只取消的读请求也在页面销毁时清理一次，避免显式调用 onHide() 导致 token 被 cancel 两次。
    if ((this as any)._cancelToken) {
      try { (this as any)._cancelToken.cancel(); } catch {}
      (this as any)._cancelToken = null;
    }
    resetLoading();
    (this as any).unsubscribeTheme?.();
  },

  tapPhoneCell() {
    if (!this.data.canModifyToday) {
      wx.showToast({ title: (this as any).$t('set.phoneLimit'), icon: 'none', duration: 2000 });
    }
  },

  getPhoneNumber(e: any) {
    if (!this.data.canModifyToday) {
      wx.showToast({ title: (this as any).$t('set.phoneLimit'), icon: 'none', duration: 2000 });
      return;
    }
    if (e.detail.code) {
      this.bindPhone(e.detail.code);
    } else {
      const errMsg = e.detail.errMsg || '';
      if (errMsg.includes('deny') || errMsg.includes('cancel')) {
        wx.showToast({ title: (this as any).$t('set.phoneAuthRequired'), icon: 'none' });
      } else if (errMsg) {
        wx.showToast({ title: (this as any).$t('set.phoneFail'), icon: 'none' });
      }
    }
  },

  onServiceAccountTap() {
    if (this.data.mpSubscribed) {
      wx.showToast({ title: (this as any).$t('set.officialAccountBound'), icon: 'none' });
      return;
    }
    (this as any)._safeSetData({
      showOfficialAccountDialog: true,
    });
  },

  onOfficialAccountDialogClose() {
    (this as any)._safeSetData({ showOfficialAccountDialog: false });
  },

  onOfficialAccountDialogContentTap() {
    // 阻止点击内容区域关闭弹窗
  },

  onOfficialAccountQrcodeTap() {
    wx.previewImage({ urls: [`${getHelpBaseURL()}/follow.png`] });
  },

  onAddToDesktop() {
    const { platform = '' } = getSystemInfo();
    const canAddDirectly = platform === 'android' && wx.canIUse('addToDesktop');

    if (canAddDirectly) {
      (wx as any).addToDesktop({
        success: () => {
          if ((this as any)._isDestroyed) return;
          wx.showToast({ title: (this as any).$t('set.addedToDesktop'), icon: 'success' });
        },
        fail: (err: any) => {
          if ((this as any)._isDestroyed) return;
          logger.error('addToDesktop failed', err);
          this._showDesktopGuide(true);
        },
      });
    } else {
      this._showDesktopGuide(false);
    }
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

  _showDesktopGuide(canOpenSettings: boolean) {
    const { screenWidth = 0 } = getSystemInfo();
    const rect = wx.getMenuButtonBoundingClientRect();

    let hintTop = 160;
    let hintRight = 60;

    if (rect && rect.width > 0 && rect.height > 0) {
      const capsuleTargetX = rect.left + rect.width * 0.25;
      const capsuleTargetY = rect.top + rect.height / 2;
      hintTop = capsuleTargetY + 24;
      hintRight = screenWidth - capsuleTargetX;
    }

    (this as any)._safeSetData({
      showDesktopGuide: true,
      desktopGuideCanOpenSettings: canOpenSettings && wx.canIUse('openAppAuthorizeSetting'),
      desktopGuideHintTop: hintTop,
      desktopGuideHintRight: hintRight,
    });
  },

  _hideDesktopGuide() {
    (this as any)._safeSetData({ showDesktopGuide: false });
  },

  onDesktopGuideClose() {
    this._hideDesktopGuide();
  },

  onDesktopGuideContentTap() {
    // 阻止点击内容区域关闭引导
  },

  onDesktopGuideOpenSettings() {
    this._hideDesktopGuide();
    if (wx.canIUse('openAppAuthorizeSetting')) {
      wx.openAppAuthorizeSetting();
    }
  },

  async bindPhone(phoneCode: string) {
    if ((this as any)._bindPhoneCancelToken) {
      try { (this as any)._bindPhoneCancelToken.cancel(); } catch {}
    }
    (this as any)._bindPhoneCancelToken = request.createCancelToken();
    try {
      await request.post('/auth/phone/bind', { data: { code: phoneCode }, cancelToken: (this as any)._bindPhoneCancelToken }, true);
      if (this._isDestroyed) return;
      (getApp() as any).globalData._needRefreshIndexList = true;
      wx.showToast({ title: (this as any).$t('set.bindSuccess'), icon: 'success' });
      await this.fetch();
    } catch (error: any) {
      if ((this as any)._isDestroyed) return;
      if (error?.message === 'request:abort') return;
      wx.showToast({ title: getErrorMessage(error, (this as any).$t('error.DEFAULT')), icon: 'none' });
    } finally {
      (this as any)._bindPhoneCancelToken = null;
    }
  },
});

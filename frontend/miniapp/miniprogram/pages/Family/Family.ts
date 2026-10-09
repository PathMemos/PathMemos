
import request, { getBaseInfo, needShowXPa, createCancelToken, resetLoading } from '../../utils/request';
import { logger } from '../../utils/logger';
import { getErrorMessage } from '../../utils/http';
import { localizedBizCodeMessage } from '../../utils/errorMessages';
import { getPendingLinkId, setPendingLinkId, clearPendingLinkId } from '../../utils/storage';
import themeBehavior from '../../behaviors/theme';
import i18nBehavior from '../../behaviors/i18n';

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    isLogin: false,
    familyList: [] as any[],
    ownerId: '',
    baseInfo: getBaseInfo(),
    inviteLinkId: '',
    shareReady: false,
    confirmDialog: {
      visible: false,
      title: '',
      content: '',
      cancelText: '',
      confirmText: '',
      confirmType: 'default',
    },
    _confirmDialogTargetUserId: '',
    _confirmDialogIsSelf: false,
  },

  _creatingInviteLink: false,
  _isDestroyed: false,
  _isHidden: false,
  _cancelToken: null as any,

  _createCancelToken(this: any) {
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {  }
    }
    this._cancelToken = createCancelToken();
    return this._cancelToken;
  },

  _isRequestAbortError(err: any): boolean {
    return err?.message === 'request:abort';
  },

  onLoad(option: any) {
    (this as any)._isDestroyed = false;
    this._safeSetData({ shareReady: false });
    if (option.linkId) {
      setPendingLinkId(option.linkId);
    }
  },

  onHide() {
    (this as any)._isHidden = true;
    (this as any)._forceSetData({ 'confirmDialog.visible': false });
    // _cancelToken 同时用于加入/退出家庭等写操作，不在 onHide 取消，避免系统调用返回后操作失败。
    resetLoading();
  },

  onUnload() {
    (this as any)._forceSetData({ 'confirmDialog.visible': false });
    (this as any)._isDestroyed = true;
    (this as any)._isHidden = true;
    if ((this as any)._cancelToken) {
      try { (this as any)._cancelToken.cancel(); } catch {  }
      (this as any)._cancelToken = null;
    }
    resetLoading();
    (this as any).unsubscribeTheme?.();
  },

  async onShow() {
    (this as any)._isDestroyed = false;
    (this as any)._isHidden = false;
    (this as any)._applyPendingSetData();
    (this as any)._safeSetData({ isAppEnv: !!(getApp() as any)?.globalData?.isAppEnv });
    if (!(this as any)._cancelToken) {
      (this as any)._createCancelToken();
    }
    try {
      const linkId = getPendingLinkId();
      let isLogin = await request.isLogin();

      
      if (!isLogin && linkId) {
        try {
          await request.login((this as any)._cancelToken);
          isLogin = await request.isLogin();
        } catch (e) {
          logger.error('家庭页登录失败', e);
        }
        if ((this as any)._isDestroyed || (this as any)._isHidden) return;
        if (!isLogin) {
          (this as any)._safeSetData({ isLogin: false });
          return;
        }
      }

      (this as any)._safeSetData({ isLogin });
      if (!isLogin) return;

      if (linkId) {
        if ((this as any)._isDestroyed || (this as any)._isHidden) return;
        await this.handlePendingInvite(linkId);
        return;
      }

      await this.fetch();
    } catch (e) {
      logger.error('家庭页 onShow 异常', e);
    }
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('family.title') });
  },

  async fetch() {
    wx.showLoading({ title: (this as any).$t('family.loading'), mask: true });
    const cancelToken = (this as any)._cancelToken;
    try {
      const { data } = await request.get('/family', { cancelToken }, true);

      const baseInfo = getBaseInfo();
      const familyInfo = data || {};
      const ownerId = familyInfo.ownerId || '';
      const currentUserId = baseInfo?.userId || '';
      const familyList = (familyInfo.members || []).map((m: any) => ({
        familyMemberId: m.userId,
        familyMemberAvatar: m.avatarUrl,
        familyMemberNickName: m.nickName,
        isSelf: m.userId === currentUserId,
        isOwner: m.userId === ownerId,
      }));

      (this as any)._safeSetData({ familyList: familyList.slice(0, 50), ownerId, baseInfo });

      if (familyList.length && !this.data.inviteLinkId && !this._creatingInviteLink) {
        await this.createInviteLink();
      }
      (this as any)._safeSetData({ shareReady: familyList.length > 0 && !!this.data.inviteLinkId });
    } catch (e: any) {
      logger.error('家庭页加载失败', e);
      if (e?.message === 'request:abort' || (this as any)._isDestroyed || (this as any)._isHidden) return;
      wx.showToast({ title: (this as any).$t('family.loadFail'), icon: 'none' });
    } finally {
      if (!(this as any)._isDestroyed && !(this as any)._isHidden) wx.hideLoading();
    }
  },

  async createInviteLink(): Promise<string> {
    if (this.data.inviteLinkId) return '';
    this._creatingInviteLink = true;
    const cancelToken = (this as any)._cancelToken;
    try {
      const { data } = await request.post('/family/invite-link', { cancelToken }, false);
      if (data?.linkId) {
        (this as any)._safeSetData({ inviteLinkId: data.linkId });
        return data.linkId;
      }
    } catch (e) {
      logger.error('创建邀请链接失败', e);
    } finally {
      this._creatingInviteLink = false;
    }
    return '';
  },

  async handlePendingInvite(linkId: string) {
    if ((this as any)._isDestroyed) return;
    const isLoginNow = await request.isLogin();
    if (!isLoginNow) {
      
      try {
        await request.login((this as any)._cancelToken);
      } catch (e) {
        logger.error('家庭页登录失败', e);
      }
      return;
    }

    if ((this as any)._isDestroyed) return;
    wx.showLoading({ title: (this as any).$t('family.handlingInvite'), mask: true });
    const cancelToken = (this as any)._cancelToken;
    let aborted = false;
    try {
      await request.post('/family/invite-link/join', {
        data: { linkId },
        cancelToken,
      }, true);

      clearPendingLinkId();
      (getApp() as any).globalData._needRefreshIndexList = true;
      if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
        wx.hideLoading();
        wx.showToast({ title: (this as any).$t('family.joined'), icon: 'success' });
      }
    } catch (error: any) {
      if ((this as any)._isRequestAbortError(error)) {
        aborted = true;
        return;
      }
      // 失效链接/家庭已满/已在家庭等业务失败必须清掉 pendingLinkId：否则 auth 层
      // 每次登录见 pendingLinkId 就强制跳转家庭页重试，循环打扰可达链接 TTL 7 天。
      // 纯网络/5xx 属瞬时错误（无业务 code），保留链接等待下次成功。
      if (error?.code) {
        clearPendingLinkId();
      }
      if ((this as any)._isDestroyed || (this as any)._isHidden) return;
      wx.hideLoading();
      // closeTheErrorMessage 抑制了 http 层通用 toast，此处优先按 biz_code 本地化
      //（如被移除冷却 REMOVED_REJOIN_COOLDOWN），再回退后端 message/通用文案
      wx.showToast({ title: localizedBizCodeMessage(error?.data?.biz_code) || error?.data?.message || (this as any).$t('family.actionFail'), icon: 'none' });
    } finally {
      if (!(this as any)._isDestroyed && !(this as any)._isHidden && !aborted) {
        this.fetch().catch(() => {});
      }
    }
  },

  onShareAppMessage() {
    const baseInfo = this.data.baseInfo || {};
    const linkId = this.data.inviteLinkId;
    if (!linkId) {
      return {
        title: (this as any).$t('family.defaultShareTitle', { brand: (this as any).$t('brand.name') }),
        path: '/pages/index/index',
        imageUrl: '/image/family_invite.png',
      };
    }
    return {
      title: (this as any).$t('family.inviteTitle', { name: baseInfo.nickName || (this as any).$t('invite.me') }),
      path: `/pages/Family/Family?linkId=${encodeURIComponent(linkId)}`,
      imageUrl: '/image/family_invite.png',
    };
  },

  // 多端 App 分享：open-type="share" 的微信转发能力在 App 内不可用，改用
  // wx.miniapp.shareMiniProgramMessage 分享小程序卡片（参数与 onShareAppMessage 同构）。
  onAppShare() {
    const self = this as any;
    const api = (wx as any).miniapp?.shareMiniProgramMessage;
    if (typeof api !== 'function') {
      wx.showToast({ title: self.$t('error.DEFAULT'), icon: 'none' });
      return;
    }
    const baseInfo = self.data.baseInfo || {};
    const linkId = self.data.inviteLinkId;
    const payload = linkId
      ? {
          title: self.$t('family.inviteTitle', { name: baseInfo.nickName || self.$t('invite.me') }),
          path: `/pages/Family/Family?linkId=${encodeURIComponent(linkId)}`,
          imageUrl: '/image/family_invite.png',
        }
      : {
          title: self.$t('family.defaultShareTitle', { brand: self.$t('brand.name') }),
          path: '/pages/index/index',
          imageUrl: '/image/family_invite.png',
        };
    api({
      ...payload,
      success: () => wx.showToast({ title: self.$t('invite.sharedOk'), icon: 'success' }),
      fail: (e: any) => wx.showToast({ title: getErrorMessage(e, self.$t('error.DEFAULT')), icon: 'none' }),
    });
  },

  quitFamily(e: any) {
    const targetUserId = e.currentTarget.dataset.id;
    const isSelf = targetUserId === this.data.baseInfo?.userId;
    this._safeSetData({
      _confirmDialogTargetUserId: targetUserId,
      _confirmDialogIsSelf: isSelf,
      confirmDialog: {
        visible: true,
        title: (this as any).$t('common.tip'),
        content: isSelf ? (this as any).$t('family.quitSelf') : (this as any).$t('family.removeMember'),
        cancelText: (this as any).$t('common.cancel'),
        confirmText: (this as any).$t('common.confirm'),
        confirmType: 'danger',
      },
    });
  },

  onConfirmDialogConfirm() {
    const targetUserId = this.data._confirmDialogTargetUserId;
    const isSelf = this.data._confirmDialogIsSelf;
    this._safeSetData({ 'confirmDialog.visible': false });
    this.doQuitFamily(targetUserId, isSelf);
  },

  onConfirmDialogCancel() {
    this._safeSetData({ 'confirmDialog.visible': false });
  },

  async doQuitFamily(targetUserId: string, isSelf: boolean) {

    wx.showLoading({ title: (this as any).$t('family.processing'), mask: true });
    try {
      if (isSelf) {
        await request.post('/family/leave', { cancelToken: (this as any)._cancelToken }, true);
      } else {
        await request.del(`/family/members/${targetUserId}`, { cancelToken: (this as any)._cancelToken }, true);
      }

      if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
        wx.hideLoading();
      }
      (getApp() as any).globalData._needRefreshIndexList = true;
      if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
        await this.fetch();
      }
    } catch (err: any) {
      if ((this as any)._isDestroyed || (this as any)._isHidden || (this as any)._isRequestAbortError(err)) return;
      wx.hideLoading();
      wx.showToast({ title: err?.data?.msg || (this as any).$t('family.actionFail'), icon: 'none' });
    }
  },

  toIndex() {
    const url = needShowXPa() ? '/pages/Guide/Guide' : '/pages/index/index';
    wx.redirectTo({ url });
  },
});

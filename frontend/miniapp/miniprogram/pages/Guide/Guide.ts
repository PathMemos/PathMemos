
import { setNeedShowXPa, getPendingLinkId } from '../../utils/storage';

import { OSS_PUBLIC_URL } from '../../config/index';
import i18n from '../../utils/i18n';
import themeBehavior from '../../behaviors/theme';
import i18nBehavior from '../../behaviors/i18n';

const OSS_TUTORIAL = `${OSS_PUBLIC_URL}/system/tutorial`;

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    step: 0,
    images: [
      `${OSS_TUTORIAL}/guide-1.png`,
      `${OSS_TUTORIAL}/guide-2.png`,
      `${OSS_TUTORIAL}/guide-3.png`,
      `${OSS_TUTORIAL}/guide-4.png`,
    ],
  },

  onLocaleChange() {
    this._syncImages();
  },

  _isDestroyed: false,
  _isHidden: false,

  onShow() {
    (this as any)._isDestroyed = false;
    (this as any)._isHidden = false;
    (this as any)._applyPendingSetData();
    this.updateNavTitle();
    this._syncImages();
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('brand.name') });
  },

  onUnload() {
    (this as any)._isDestroyed = true;
    (this as any)._isHidden = true;
    (this as any).unsubscribeTheme?.();
  },

  onHide() {
    (this as any)._isHidden = true;
  },

  toUser() {
    wx.navigateTo({ url: '/pages/User/User' });
  },

  next() {
    if (this.data.step === 3) {
      setNeedShowXPa(false);
      // 受邀加入的用户（pendingLinkId 未消费）教程结束直达家庭页，落地即见已加入的家庭
      if (getPendingLinkId()) {
        wx.redirectTo({ url: '/pages/Family/Family' });
        return;
      }
      wx.redirectTo({ url: '/pages/index/index' });
    } else {
      (this as any)._safeSetData({ step: this.data.step + 1 });
    }
  },

  _syncImages() {
    const suffix = i18n.getLocale() === 'en' ? '_en' : '';
    (this as any)._safeSetData({
      images: [
        `${OSS_TUTORIAL}/guide-1${suffix}.png`,
        `${OSS_TUTORIAL}/guide-2${suffix}.png`,
        `${OSS_TUTORIAL}/guide-3${suffix}.png`,
        `${OSS_TUTORIAL}/guide-4${suffix}.png`,
      ],
    });
  },

  pre() {
    if (this.data.step > 0) {
      (this as any)._safeSetData({ step: this.data.step - 1 });
    }
  },
});

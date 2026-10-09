import request from '../../utils/request';
import { closeAutoRecord, openAutoRecord } from '../../utils/autoRecord';
import { isAppEnv } from '../../utils/appPermission';
import { logger } from '../../utils/logger';
import i18nBehavior from '../../behaviors/i18n';

Component({
  behaviors: [i18nBehavior],

  lifetimes: {
    detached(this: any) {
      this._isDetached = true;
      this._changing = false;
    },
  },

  pageLifetimes: {
    hide(this: any) {
      this._forceSetData({ showSubscribePrompt: false });
    },
  },

  data:{
    showSubscribePrompt: false,
  },

  properties: {
    mode: {
      type: String,
      value: 'text',
    },
    openAutoRecorded: {
      type: Boolean,
      value: false,
    },
    disableSubscribe: {
      type: Boolean,
      value: false,
    },
  },

  methods: {
    change() {
      if ((this as any)._changing) return;
      (this as any)._changing = true;

      const vipInfo = request.getVipInfo() || {};
      if (!(vipInfo?.isVip > 0)) {
        wx.showModal({
          title: (this as any).$t('autoBtn.vipTipTitle'),
          content: (this as any).$t('autoBtn.vipTip'),
          cancelText: (this as any).$t('common.cancel'),
          confirmText: (this as any).$t('autoBtn.goUpgrade'),
          success: (res) => {
            if (!(this as any)._isAlive()) return;
            if (res.confirm) {
              wx.navigateTo({
                url: '/pages/sub/Vip/Vip',
                fail: () => wx.showToast({ title: (this as any).$t('autoBtn.navigateFail'), icon: 'none' }),
              });
            }
          },
          fail: () => {},
          complete: () => {
            if (!(this as any)._isAlive()) {
              (this as any)._changing = false;
              return;
            }
            (this as any)._changing = false;
          },
        });
        return;
      }

      const currentState = this.data.openAutoRecorded;
      const finish = (nextState?: boolean) => {
        if (!(this as any)._isAlive()) {
          (this as any)._changing = false;
          return;
        }
        if (typeof nextState === 'boolean' && nextState !== currentState) {
          this.triggerEvent('change', { openAutoRecorded: nextState });
        }
        (this as any)._changing = false;
      };

      if (currentState) {
        wx.showModal({
          title: (this as any).$t('autoBtn.closeTitle'),
          content: (this as any).$t('autoBtn.closeDesc'),
          cancelText: (this as any).$t('common.cancel'),
          confirmText: (this as any).$t('autoBtn.closeAction'),
          success: async (res) => {
            if (res.confirm) {
              try {
                await closeAutoRecord();
                finish(false);
              } catch (err) {
                logger.error('关闭自动记录失败', err);
                if (!(this as any)._isAlive()) {
                  finish();
                  return;
                }
                wx.showToast({ title: (this as any).$t('autoBtn.closeFail'), icon: 'none' });
                finish();
              }
            } else {
              finish();
            }
          },
          fail: () => finish(),
        });
      } else {
        wx.showModal({
          title: (this as any).$t('autoBtn.openTitle'),
          content: (this as any).$t('autoBtn.openDesc'),
          cancelText: (this as any).$t('common.cancel'),
          confirmText: (this as any).$t('autoBtn.openAction'),
          success: (res) => {
            if (res.confirm) {
              openAutoRecord()
                .then(() => {
                  if (!(this as any)._isAlive()) {
                    (this as any)._changing = false;
                    return;
                  }
                  // App 端通知已裁撤：不弹订阅授权、不记订阅（02g/06）
                  if (!isAppEnv()) {
                    if ((this as any).properties.disableSubscribe) {
                      (this as any).triggerEvent('subscribeprompt', { visible: true });
                    } else {
                      (this as any)._safeSetData({ showSubscribePrompt: true });
                    }
                  }
                  finish(true);
                })
                .catch((err) => {
                  logger.error('开启自动记录失败', { msg: err?.message, code: err?.data?.code });
                  if (!(this as any)._isAlive()) {
                    finish();
                    return;
                  }
                  if (err?.needRefresh) {
                    wx.showModal({
                      title: (this as any).$t('autoBtn.locationUnavailableTitle'),
                      content: (this as any).$t('autoBtn.locationUnavailableDesc'),
                      showCancel: false,
                    });
                  } else {
                    wx.showToast({ title: (this as any).$t('autoBtn.openFail'), icon: 'none' });
                  }
                  finish();
                });
            } else {
              finish();
            }
          },
          fail: () => finish(),
        });
      }
    },

    onSubscribeConfirm() {
      (this as any)._safeSetData({ showSubscribePrompt: false });
    },

    onSubscribeCancel() {
      (this as any)._safeSetData({ showSubscribePrompt: false });
    },
  },
});

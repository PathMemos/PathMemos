import request from '../../utils/request';
import { logger } from '../../utils/logger';
import { ABNORMAL_TEMPLATE_ID } from '../../config/index';
import i18nBehavior from '../../behaviors/i18n';

Component({
  behaviors: [i18nBehavior],

  properties: {
    visible: {
      type: Boolean,
      value: false,
    },
  },

  lifetimes: {
    attached(this: any) {
      this._cancelToken = request.createCancelToken();
    },
    detached(this: any) {
      this._isDetached = true;
      if (this._cancelToken) {
        try { this._cancelToken.cancel(); } catch {}
        this._cancelToken = null;
      }
    },
  },

  pageLifetimes: {
    hide(this: any) {
      // 订阅记录是写操作，不在 page hide 时取消请求；_cancelToken 在 detached 时清理。
    },
  },

  methods: {
    onConfirm() {
      // 异常提醒订阅仅小程序通道：App 端通知已裁撤，
      // 不接开放平台一次性订阅，无需按环境分发。
      wx.requestSubscribeMessage({
        tmplIds: [ABNORMAL_TEMPLATE_ID],
        success: (res: any) => {
          if (!(this as any)._isAlive()) return;
          const accepted = res[ABNORMAL_TEMPLATE_ID] === 'accept';
          this.recordSubscribe(accepted);
          this.triggerEvent('confirm', { accepted });
        },
        fail: (err: any) => {
          if (!(this as any)._isAlive()) return;
          logger.error('订阅失败', err);
          this.recordSubscribe(false);
          this.triggerEvent('confirm', { accepted: false });
        },
      });
    },

    onCancel() {
      if (!(this as any)._isAlive()) return;
      this.triggerEvent('cancel');
    },

    recordSubscribe(accept: boolean) {
      if (!(this as any)._isAlive()) return;
      request.post('/subscribe/record', {
        data: {
          templateId: ABNORMAL_TEMPLATE_ID,
          scene: 'ABNORMAL_ALERT',
          accept,
        },
        cancelToken: (this as any)._cancelToken,
      }).catch((err: any) => {
        if (!(this as any)._isAlive()) return;
        logger.error('record subscribe failed', err);
      });
    },
  },
});

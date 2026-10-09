import request, { getErrorMessage, createCancelToken } from '../../utils/request';
import touchSwipe from '../../behaviors/touchSwipe';
import i18nBehavior from '../../behaviors/i18n';

const CONTENT_FONT_SIZE = 28;
const CONTENT_MAX_LINES = 3;
// 卡片内固定占位（rpx）：圆点+间距 24 + 时间列 120 + memory-body 左右 padding 32 + 页面左右边距 48。
// CSS 截断（max-height: 48rpx*3）按 .main 实际可用宽度折行，估算必须覆盖这些固定占位，
// 否则字符数启发式偏宽会漏出"CSS 已截断但无展开按钮"的内容。
const CONTENT_RESERVED_RPX = 24 + 120 + 32 + 48;
const SCREEN_WIDTH_RPX = 750;
const CONTENT_AVAILABLE_WIDTH_RPX = SCREEN_WIDTH_RPX - CONTENT_RESERVED_RPX;
// 保守估算每行可容纳字符数：中文全角字符 ≈ 字号宽（28rpx），取小一档避免启发式过宽。
const AVG_CHARS_PER_LINE = Math.floor(CONTENT_AVAILABLE_WIDTH_RPX / CONTENT_FONT_SIZE);

function _shouldShowMore(text: string): boolean {
  if (!text) return false;
  const lines = text.split('\n');
  if (lines.length > CONTENT_MAX_LINES) return true;
  return text.length > AVG_CHARS_PER_LINE * CONTENT_MAX_LINES;
}

Component({
  behaviors: [touchSwipe, i18nBehavior],
  properties: {
    info: Object,
    index: Number,
    isLast: Boolean,
    userId: String,
  },

  data: {
    data: {} as any,
    dotColor: '#543116',
    dotShadow: '#54311640',
    launch: false,
    confirmDialog: {
      visible: false,
      title: '',
      content: '',
      cancelText: '',
      confirmText: '',
      confirmType: 'default',
    },
  },

  lifetimes: {
    attached(this: any) {
      this._isDestroyed = false;
      this._isHidden = false;
      this._lastInfoId = undefined;
      const info = this.data.info;
      if (info) {
        this._lastInfoId = info.id;
        (this as any)._safeSetData({
          data: this.formatData(info, this.data.index),
          dotColor: info?.color || '#543116',
          dotShadow: `${info?.color || '#543116'}40`,
          isTouchLeft: false,
          launch: false,
        });
      }
    },
    detached(this: any) {
      if (this._cancelToken) {
        try { this._cancelToken.cancel(); } catch {}
        this._cancelToken = null;
      }
      this._isDetached = true;
      this._ignoreNextTap = false;
      (this as any)._isDestroyed = true;
    },
  },

  pageLifetimes: {
    hide(this: any) {
      // 删除是写操作，不在 page hide 时取消请求；_cancelToken 在 detached / 删除完成时清理。
      (this as any)._forceSetData({
        'confirmDialog.visible': false,
        isTouchLeft: false,
      });
    },
  },

  observers: {
    info: function (info) {
      const isNewRecord = (this as any)._lastInfoId !== info?.id;
      (this as any)._lastInfoId = info?.id;
      const update: any = {
        data: this.formatData(info, this.data.index),
        dotColor: info?.color || '#543116',
        dotShadow: `${info?.color || '#543116'}40`,
      };
      if (isNewRecord) {
        update.isTouchLeft = false;
        update.launch = false;
      }
      (this as any)._safeSetData(update);
    },
  },

  methods: {
    formatData(data: any, _itemIndex: number) {
      if (!data || !data.recordTime) return { ...data, recordTime: '', showMore: false };
      return {
        ...data,
        recordTime: data.recordTime.split(' ')[1]?.split(':').slice(0, 2).join(':') || '',
        showMore: _shouldShowMore(data.recordText || ''),
    
      };
    },
    bindEdit() {
      if ((this as any)._ignoreNextTap) {
        (this as any)._ignoreNextTap = false;
        return;
      }
      this.triggerEvent('handEdit', this.data.info);
    },
    doLaunch() {
      (this as any)._safeSetData({ launch: !this.data.launch });
    },

    touchStart: function (e: any) {
      (this as any)._swipeTouchStart(e);
      (this as any)._maxSwipeDeltaX = 0;
      (this as any)._ignoreNextTap = false;
    },
    touchMove: function (e: any) {
      (this as any)._swipeTouchMove(e);
      const startX = (this as any)._startX;
      const moveX = e.touches?.[0]?.pageX;
      if (startX !== undefined && moveX !== undefined) {
        const delta = Math.abs(moveX - startX);
        if (delta > ((this as any)._maxSwipeDeltaX || 0)) {
          (this as any)._maxSwipeDeltaX = delta;
        }
      }
    },
    touchEnd: function (e: any) {
      (this as any)._swipeTouchEnd(e);
      if (((this as any)._maxSwipeDeltaX || 0) > 30) {
        (this as any)._ignoreNextTap = true;
      }
      (this as any)._maxSwipeDeltaX = 0;
    },
    del: function () {
      (this as any)._safeSetData({
        confirmDialog: {
          visible: true,
          title: (this as any).$t('memoryItem.deleteTitle'),
          content: (this as any).$t('memoryItem.deleteContent'),
          cancelText: (this as any).$t('common.cancel'),
          confirmText: (this as any).$t('memoryItem.delete'),
          confirmType: 'danger',
        },
      });
    },

    async onConfirmDialogConfirm() {
      if (!(this as any)._isAlive()) return;
      // 按 FP076：记忆删除为普通业务，不做函数级防重入锁。
      const id = this.data.info?.id;
      if (!id) return;
      const cancelToken = createCancelToken();
      (this as any)._cancelToken = cancelToken;
      (this as any)._safeSetData({ 'confirmDialog.visible': false, isTouchLeft: false });
      try {
        await request.del('/diary/details/memory', {
          params: { id },
          cancelToken: (this as any)._cancelToken,
        }, true);
        if (!(this as any)._isAlive()) return;
        this.triggerEvent('del', { id });
      } catch (error: any) {
        if (!(this as any)._isAlive() || error?.message === 'request:abort') return;
        wx.showToast({ title: getErrorMessage(error, (this as any).$t('memoryItem.deleteFail')), icon: 'none' });
      } finally {
        if ((this as any)._cancelToken === cancelToken) {
          (this as any)._cancelToken = null;
        }
      }
    },

    onConfirmDialogCancel() {
      (this as any)._safeSetData({ 'confirmDialog.visible': false });
    },
  },
});

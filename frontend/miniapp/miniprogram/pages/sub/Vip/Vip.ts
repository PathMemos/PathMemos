
import request, { createCancelToken, resetLoading } from '../../../utils/request';
import { isSessionExpiredError, type CancelToken } from '../../../utils/http';
import { doPay } from '../../../utils/pay';
import { formatVipInfo } from '../../../utils/vip';
import { openUrl } from '../../../utils/util';
import { logger } from '../../../utils/logger';
import { i18n } from '../../../utils/i18n';
import { getBackendMode } from '../../../utils/storage';
import { getHelpBaseURL } from '../../../config/index';
import themeBehavior from '../../../behaviors/theme';
import i18nBehavior from '../../../behaviors/i18n';

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    checkValue: false,
    vipInfo: { isVip: 0, receivedFreeVip: false, vipExpireTime: '' } as any,
    commodity: [],
    selectedPlanId: '',
    selectedPlanAmount: 0,
    selectedPlanAmountFixed: '0.00',
    payNowText: '',
    showAgreementModal: false,
    confirmDialog: {
      visible: false,
      title: '',
      content: '',
      cancelText: '',
      confirmText: '',
      confirmType: 'default',
    },
    pendingCommodity: null as any,
    pendingAction: '',
    claiming: false,
    paymentAvailable: true,
    freeVipAvailable: true,
    backendMode: 'saas',
  },

  _cancelToken: null as CancelToken | null,
  _configCancelToken: null as CancelToken | null,
  _payCancelToken: null as CancelToken | null,
  _claimCancelToken: null as CancelToken | null,
  _isDestroyed: false,
  _isHidden: false,

  onLoad() {
    (this as any)._isDestroyed = false;
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {}
    }
    this._cancelToken = createCancelToken();
    // fetchSystemConfig 使用独立 token：onShow 补偿刷新商品时会重建 _cancelToken，
    // 共享 token 会让进行中的系统配置请求被确定性取消。
    this._configCancelToken = createCancelToken();
    // 私有化后端无支付，本地模式兜底：即使 /system/config 请求失败也能显示正确 UI。
    // SaaS 模式保持默认 paymentAvailable=true，网络抖动不影响付费入口。
    if (getBackendMode() === 'private') {
      (this as any)._safeSetData({ paymentAvailable: false, freeVipAvailable: false, backendMode: 'open' });
    }
    this.fetchSystemConfig(this._configCancelToken);
    if (this.data.paymentAvailable) {
      this.fetchCommodity(this._cancelToken);
    }
    request.fetchVipInfo().then((vipInfo) => {
      if ((this as any)._isDestroyed || (this as any)._isHidden) return;
      (this as any)._safeSetData({ vipInfo: formatVipInfo(vipInfo) });
    }).catch((err) => {
      if ((this as any)._isDestroyed || (this as any)._isHidden) return;
      logger.error('Vip 页获取 VIP 信息失败', err);
    });
  },

  onShow() {
    const wasHidden = (this as any)._isHidden;
    this._isHidden = false;
    (this as any)._applyPendingSetData();
    const cachedVipInfo = formatVipInfo(request.getVipInfo());
    const old = this.data.vipInfo;
    if (!old || old.isVip !== cachedVipInfo?.isVip || old.vipExpireTime !== cachedVipInfo?.vipExpireTime || old.receivedFreeVip !== cachedVipInfo?.receivedFreeVip) {
      (this as any)._safeSetData({ vipInfo: cachedVipInfo });
    }
    // 切后台期间可能完成支付但未被轮询感知；回前台后强制刷新 VIP 状态。
    request.fetchVipInfo().then((vipInfo) => {
      if ((this as any)._isDestroyed || (this as any)._isHidden) return;
      (this as any)._safeSetData({ vipInfo: formatVipInfo(vipInfo) });
    }).catch((err) => {
      if ((this as any)._isDestroyed || (this as any)._isHidden) return;
      logger.error('Vip onShow 刷新 VIP 信息失败', err);
    });
    // 仅"切后台返回"时补偿刷新商品列表：首次进入 onLoad 已发起请求，
    // 此时 cancel+重发会白费一次请求（与 Mcp 页 wasHidden 守卫一致）。
    if (wasHidden && this.data.paymentAvailable && (!this.data.commodity || this.data.commodity.length === 0)) {
      if (this._cancelToken) {
        try { this._cancelToken.cancel(); } catch {}
      }
      this._cancelToken = createCancelToken();
      this.fetchCommodity(this._cancelToken);
    }
  },

  onHide() {
    this._isHidden = true;
    (this as any)._showingAgreement = false;
    // 支付流程（wx.requestVirtualPayment 等）会触发 onHide，不能在 onHide 中取消轮询
    // 并复位 _paying，否则返回后支付状态丢失且可能重复发起支付。
    // _finishPaying 仅在支付完成/失败/超时或 onUnload 时调用。
    // _claimCancelToken 同时用于 GET /vip/free 和 POST /vip/free/claim（写操作），
    // 也不要在 onHide 中取消，避免系统调用切后台后返回导致领取失败；仅在 onUnload 中清理。
    (this as any)._forceSetData({
      showAgreementModal: false,
      'confirmDialog.visible': false,
    });
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {}
      this._cancelToken = null;
    }
    if (this._configCancelToken) {
      try { this._configCancelToken.cancel(); } catch {}
      this._configCancelToken = null;
    }
  },

  onUnload() {
    (this as any)._forceSetData({
      showAgreementModal: false,
      'confirmDialog.visible': false,
    });
    (this as any)._isDestroyed = true;
    (this as any)._showingAgreement = false;
    this._finishPaying();
    if (this._cancelToken) {
      try { this._cancelToken.cancel(); } catch {}
      this._cancelToken = null;
    }
    if (this._configCancelToken) {
      try { this._configCancelToken.cancel(); } catch {}
      this._configCancelToken = null;
    }
    if ((this as any)._claimCancelToken) {
      try { (this as any)._claimCancelToken.cancel(); } catch {}
      (this as any)._claimCancelToken = null;
    }
    (this as any).unsubscribeTheme?.();
  },

  _clearPoll() {
    this._clearPollTimer();
    if ((this as any)._pollCancelToken) {
      try { (this as any)._pollCancelToken.cancel(); } catch {}
      (this as any)._pollCancelToken = null;
    }
  },

  _finishPaying() {
    this._clearPoll();
    (this as any)._paying = false;
    resetLoading();
    if ((this as any)._payCancelToken) {
      try { (this as any)._payCancelToken.cancel(); } catch {}
      (this as any)._payCancelToken = null;
    }
  },

  openUser() {
    openUrl(`${getHelpBaseURL()}/tutorial/terms-of-use/`);
  },

  changeCheck() {
    (this as any)._safeSetData({ checkValue: !this.data.checkValue });
  },

  check() {
    if (this.data.checkValue) return true;
    wx.showToast({ title: (this as any).$t('vip.needAgree'), icon: 'none', duration: 2000 });
    return false;
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('vip.title') });
  },

  onLocaleChange() {
    this.updateNavTitle();
    this._updatePayNowText();
  },

  _updatePayNowText() {
    (this as any)._safeSetData({
      payNowText: (this as any).$t('vip.payNow', { amount: this.data.selectedPlanAmountFixed }),
    });
  },

  onSelectPlan(e: any) {
    const { commodity } = e.currentTarget.dataset;
    (this as any)._safeSetData({
      selectedPlanId: commodity.id,
      selectedPlanAmount: commodity.amount,
      selectedPlanAmountFixed: commodity.amountFixed,
      payNowText: (this as any).$t('vip.payNow', { amount: commodity.amountFixed }),
    });
  },

  confirmBuy() {
    if ((this as any)._showingAgreement) return;
    if (!this.data.selectedPlanId) {
      wx.showToast({ title: (this as any).$t('vip.selectPlanTip'), icon: 'none' });
      return;
    }
    const item = this.data.commodity.find((c: any) => c.id === this.data.selectedPlanId);
    if (!item) {
      wx.showToast({ title: (this as any).$t('vip.planLoadingTip'), icon: 'none' });
      return;
    }
    if (this.data.checkValue) {
      this.buy(item);
      return;
    }
    (this as any)._showingAgreement = true;
    (this as any)._safeSetData({ showAgreementModal: true, pendingCommodity: item });
  },

  closeAgreementModal() {
    (this as any)._showingAgreement = false;
    (this as any)._safeSetData({ showAgreementModal: false, pendingCommodity: null, pendingAction: '' });
  },

  onConfirmDialogConfirm() {
    (this as any)._safeSetData({ 'confirmDialog.visible': false });
  },

  onConfirmDialogCancel() {
    (this as any)._safeSetData({ 'confirmDialog.visible': false });
  },

  agreeAndContinue() {
    (this as any)._showingAgreement = false;
    (this as any)._safeSetData({ checkValue: true, showAgreementModal: false });
    if (this.data.pendingAction === 'claimFree') {
      this.doClaimFreeVip();
    } else if (this.data.pendingCommodity) {
      this.buy(this.data.pendingCommodity);
    }
  },

  async buy(commodity: any) {
    if ((this as any)._paying) return;
    if (!commodity || !commodity.id) {
      wx.showToast({ title: (this as any).$t('vip.planInvalid'), icon: 'none' });
      return;
    }
    if (((this.data.vipInfo?.isVip ?? 0) > 0) && commodity.amount === 0) {
      wx.showToast({ title: (this as any).$t('vip.alreadyVip'), icon: 'none' });
      return;
    }
    (this as any)._paying = true;
    if ((this as any)._payCancelToken) {
      try { (this as any)._payCancelToken.cancel(); } catch {}
    }
    (this as any)._payCancelToken = createCancelToken();
    const payCancelToken = (this as any)._payCancelToken;
    if (!(this as any)._isHidden) {
      wx.showLoading({ title: (this as any).$t('vip.paying'), mask: true });
    }
    try {
      await doPay(
        commodity,
        (outTradeNo: string) => {
          if (!(this as any)._isDestroyed) wx.hideLoading();
          if ((this as any)._isDestroyed) {
            this._finishPaying();
            return;
          }
          if (outTradeNo) {
            this._pollOrderStatus(outTradeNo);
          } else {
            // 空 outTradeNo = App 跳转小程序支付：无 App 侧订单，返回后 onShow 刷新 VIP。
            this._finishPaying();
          }
        },
        () => {
          if (!(this as any)._isDestroyed && !(this as any)._isHidden) wx.hideLoading();
        },
        () => {},
        payCancelToken
      );
      
    } catch (e: any) {
      this._finishPaying();
      if ((this as any)._isDestroyed || (this as any)._isHidden || e?.message === 'request:abort') return;
      wx.showToast({ title: e?.message || (this as any).$t('vip.payFail'), icon: 'error', duration: 2000 });
    }
  },

  async fetchSystemConfig(cancelToken?: CancelToken) {
    try {
      const res: any = await request.get('/system/config', { cancelToken }, true);
      const config = res.data || {};
      const paymentAvailable = config.features?.payment !== false;
      const freeVipAvailable = config.features?.freeVip !== false;
      (this as any)._safeSetData({
        paymentAvailable,
        freeVipAvailable,
        backendMode: config.mode || 'saas',
      });
    } catch (err: any) {
      if ((this as any)._isDestroyed || (this as any)._isHidden || err?.message === 'request:abort') return;
      logger.error('Vip 页获取系统配置失败', err);
    }
  },

  async fetchCommodity(cancelToken?: CancelToken) {
    try {
      const { data: commodity } = await request.get('/vip', { cancelToken }, true);
      const list = (commodity || [])
        .filter((item: any) => item.type !== 'free' && item.type !== 'trial')
        .map((item: any) => {
          const price = (item.prices || [])[0] || {};
          const amount = Number(price.amount || 0) / 100;
          return {
            ...item,
            amount,
            amountFixed: amount.toFixed(2),
            amountStr: price.amount ? `¥${(price.amount / 100).toFixed(2)}` : '¥0',
            displayName: item.type === 'year' ? i18n.t('vip.yearName') : i18n.t('vip.monthName'),
          };
        });
      const defaultPlan = list[0];
      const selectedPlanAmountFixed = defaultPlan ? defaultPlan.amountFixed : '0.00';
      (this as any)._safeSetData({
        commodity: list,
        selectedPlanId: defaultPlan ? defaultPlan.id : '',
        selectedPlanAmount: defaultPlan ? defaultPlan.amount : 0,
        selectedPlanAmountFixed,
        payNowText: (this as any).$t('vip.payNow', { amount: selectedPlanAmountFixed }),
      });
    } catch (e: any) {
      if ((this as any)._isDestroyed || (this as any)._isHidden || e?.message === 'request:abort') return;
      wx.showToast({ title: (this as any).$t('vip.loadFail'), icon: 'none' });
    }
  },

  claimFreeVip() {
    if ((this as any)._showingAgreement) return;
    if (this.data.checkValue) {
      this.doClaimFreeVip();
      return;
    }
    (this as any)._showingAgreement = true;
    (this as any)._safeSetData({ showAgreementModal: true, pendingAction: 'claimFree' });
  },

  async doClaimFreeVip() {
    if (this.data.vipInfo?.receivedFreeVip) return;
    // 入口级防重：双击会 cancel 上一次在途领取（写请求），必须挡住。
    if ((this as any)._claiming) return;
    (this as any)._claiming = true;
    (this as any)._safeSetData({ claiming: true });
    if ((this as any)._claimCancelToken) {
      try { (this as any)._claimCancelToken.cancel(); } catch {}
    }
    (this as any)._claimCancelToken = createCancelToken();
    const claimCancelToken = (this as any)._claimCancelToken;
    try {
      const { data: commodity } = await request.get('/vip/free', { cancelToken: claimCancelToken }, true);
      if ((this as any)._isDestroyed) return;
      if (!commodity || !commodity.length) {
        wx.showToast({ title: (this as any).$t('vip.noFreeActivity'), icon: 'none' });
        return;
      }
      await request.post('/vip/free/claim', { data: { vipId: commodity[0].id }, cancelToken: claimCancelToken }, true);
      if ((this as any)._isDestroyed) return;

      let vipInfo;
      try {
        vipInfo = formatVipInfo(await request.fetchVipInfo());
      } catch (fetchErr) {
        logger.error('领取免费会员后刷新 VIP 信息失败', fetchErr);
        vipInfo = formatVipInfo(request.getVipInfo());
      }
      const app = getApp() as any;
      if (app && app.globalData) {
        app.globalData._needRefreshIndexList = true;
      }
      // 领取成功时页面处于存活状态，使用 _safeSetData；隐藏期间由 behavior 暂存，
      // 返回前台后统一 flush，避免弹窗在切后台期间被静默丢弃。
      const claimContent = vipInfo?.vipExpireTime
        ? `${(this as any).$t('vip.claimSuccess', { brand: (this as any).$t('brand.name') })}\n${(this as any).$t('vip.validityTo', { expire: vipInfo.vipExpireTime })}`
        : (this as any).$t('vip.claimSuccess', { brand: (this as any).$t('brand.name') });
      (this as any)._safeSetData({
        confirmDialog: {
          visible: true,
          title: '',
          content: claimContent,
          cancelText: '',
          confirmText: (this as any).$t('common.know'),
          confirmType: 'default',
        },
        vipInfo,
      });
    } catch (e: any) {
      if ((this as any)._isDestroyed || (this as any)._isHidden || e?.message === 'request:abort') return;
      const code = e?.data?.biz_code || '';
      if (code === 'FREE_VIP_ALREADY_CLAIMED') {
        wx.showToast({ title: (this as any).$t('vip.alreadyClaimed'), icon: 'none' });
      } else {
        wx.showToast({ title: e?.data?.msg || (this as any).$t('vip.claimFail'), icon: 'none' });
      }
    } finally {
      (this as any)._claiming = false;
      (this as any)._safeSetData({ claiming: false });
      (this as any)._claimCancelToken = null;
    }
  },

  _clearPollTimer() {
    if ((this as any)._pollTimer) {
      clearTimeout((this as any)._pollTimer);
      (this as any)._pollTimer = null;
    }
  },

  _pollOrderStatus(outTradeNo: string) {
    this._clearPoll();
    (this as any)._pollCancelToken = request.createCancelToken();
    const cancelToken = (this as any)._pollCancelToken;

    let count = 0;
    const maxCount = 10;
    const interval = 3000;

    const doPoll = async () => {
      if ((this as any)._isDestroyed) {
        this._finishPaying();
        return;
      }
      count++;
      try {
        const res = await request.get('/payment/virtual/status', {
          params: { outTradeNo },
          cancelToken,
        }, true);
        const status = (res.data?.state || '').toString().toLowerCase();

        if (status === 'paid') {
          this.paySuccess();
          return;
        }

        if (status === 'closed') {
          this._finishPaying();
          if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
            wx.showToast({ title: (this as any).$t('vip.payStatusFail'), icon: 'none', duration: 2000 });
          }
          return;
        }
      } catch (e: any) {
        if (e?.message === 'request:abort') {
          this._finishPaying();
          return;
        }
        // 401 会话过期必须用哨兵判定：http 层 reject 的是内部哨兵字符串，
        // 用 i18n 文案比较永远为假（文案可能改、语言可能切换），分支成死代码。
        if (isSessionExpiredError(e)) {
          this._finishPaying();
          if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
            wx.showToast({ title: (this as any).$t('vip.loginExpiredTip'), icon: 'none', duration: 2000 });
          }
          return;
        }
        logger.error('轮询异常', e);
      }

      if (count >= maxCount) {
        this._finishPaying();
        if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
          wx.showToast({ title: (this as any).$t('vip.payTimeout'), icon: 'none', duration: 2000 });
        }
        return;
      }

      (this as any)._pollTimer = setTimeout(doPoll, interval);
    };

    doPoll();
  },

  async paySuccess() {
    if (!(this as any)._isHidden) {
      wx.showLoading({ title: (this as any).$t('vip.paying'), mask: true });
    }
    try {
      const vipInfo = formatVipInfo(await request.fetchVipInfo());
      const app = getApp() as any;
      if (app && app.globalData) {
        app.globalData._needRefreshIndexList = true;
      }
      if ((this as any)._isDestroyed) {
        this._finishPaying();
        return;
      }
      // 支付成功时页面处于存活状态，使用 _safeSetData；隐藏期间由 behavior 暂存，
      // 返回前台后统一 flush，避免弹窗在切后台期间被静默丢弃。
      (this as any)._safeSetData({
        vipInfo,
        confirmDialog: {
          visible: true,
          title: '',
          content: `${(this as any).$t('vip.buySuccess', { brand: (this as any).$t('brand.name') })}\n${(this as any).$t('vip.validityTo', { expire: vipInfo.vipExpireTime })}`,
          cancelText: '',
          confirmText: (this as any).$t('common.know'),
          confirmType: 'default',
        },
      });
      this._finishPaying();
    } catch {
      this._finishPaying();
      if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
        wx.showToast({ title: (this as any).$t('vip.fetchVipFail'), icon: 'none', duration: 3000 });
      }
    }
  },
});

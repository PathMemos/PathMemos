import request, { getErrorMessage, resetLoading } from '../../utils/request';
import { logger } from '../../utils/logger';
import themeBehavior from '../../behaviors/theme';
import i18nBehavior from '../../behaviors/i18n';

interface CommonAddress {
  name: string;
  lat: number;
  lon: number;
  count: number;
}

Page({
  behaviors: [themeBehavior, i18nBehavior],

  data: {
    addresses: [] as CommonAddress[],
    loading: false,
    editingName: '',
    tempName: '',
    showEditDrawer: false,
  },

  _isDestroyed: false,
  _isHidden: false,

  onLoad() {
    (this as any)._isDestroyed = false;
    (this as any)._isHidden = false;
  },

  onShow() {
    (this as any)._isDestroyed = false;
    (this as any)._isHidden = false;
    // 防重：快速进出页面会并发重复触发较重的聚类刷新。
    if (!(this as any).data.loading) this.fetchAddresses();
  },

  onHide() {
    (this as any)._isHidden = true;
    resetLoading();
  },

  onUnload() {
    (this as any)._isDestroyed = true;
    (this as any)._isHidden = true;
    resetLoading();
  },

  async fetchAddresses() {
    if ((this as any)._isDestroyed) return;
    (this as any)._safeSetData({ loading: true });
    try {
      const res = await request.post('/user/common-addresses/refresh', {}, true);
      if ((this as any)._isDestroyed) return;
      const addresses = (res.data?.addresses || []) as CommonAddress[];
      addresses.sort((a, b) => b.count - a.count);
      (this as any)._safeSetData({
        addresses,
        loading: false,
      });
    } catch (e: any) {
      if ((this as any)._isDestroyed || e?.message === 'request:abort') return;
      logger.error('fetch common addresses failed', e);
      (this as any)._safeSetData({ loading: false });
      wx.showToast({ title: (this as any).$t('commonAddresses.loadFail'), icon: 'none' });
    } finally {
      // 兜底复位 loading——任何提前返回路径都不让它卡在 true，
      // 否则 onShow 的防重判断会永久跳过后续刷新。
      if (!(this as any)._isDestroyed && (this as any).data.loading) {
        (this as any)._safeSetData({ loading: false });
      }
    }
  },

  onAddressTap(e: any) {
    const index = e.currentTarget.dataset.index;
    const addr = this.data.addresses[index];
    if (!addr) return;
    (this as any)._safeSetData({
      editingName: addr.name,
      tempName: addr.name,
      showEditDrawer: true,
    });
  },

  onNameInput(e: any) {
    (this as any)._safeSetData({ tempName: e.detail.value });
  },

  onPresetTap(e: any) {
    const name = e.currentTarget.dataset.name;
    (this as any)._safeSetData({ tempName: name });
  },

  hiddenEditDrawer() {
    (this as any)._safeSetData({
      showEditDrawer: false,
        editingName: '',
      tempName: '',
    });
  },

  async submitName() {
    // 防重入：双击双发 PUT，第一次成功后 oldName 已不存在，第二次必报
    // "修改失败"但实际已成功，用户被误导。
    if ((this as any)._saving) return;
    const newName = this.data.tempName || '';
    const oldName = this.data.editingName;
    if (!newName) {
      wx.showToast({ title: (this as any).$t('commonAddresses.nameEmpty'), icon: 'none' });
      return;
    }
    if (newName === oldName) {
      this.hiddenEditDrawer();
      return;
    }
    if (newName.length > 100) {
      wx.showToast({ title: (this as any).$t('commonAddresses.nameTooLong'), icon: 'none' });
      return;
    }

    (this as any)._saving = true;
    try {
      await request.put(
        `/user/common-addresses/${encodeURIComponent(oldName)}`,
        { data: { newName } },
        true
      );
      if ((this as any)._isDestroyed) return;
      wx.showToast({ title: (this as any).$t('commonAddresses.modifySuccess'), icon: 'success' });
      this.hiddenEditDrawer();
      this.fetchAddresses();
    } catch (e: any) {
      if ((this as any)._isDestroyed || e?.message === 'request:abort') return;
      wx.showToast({ title: getErrorMessage(e, (this as any).$t('commonAddresses.modifyFail')), icon: 'none' });
    } finally {
      (this as any)._saving = false;
    }
  },
});

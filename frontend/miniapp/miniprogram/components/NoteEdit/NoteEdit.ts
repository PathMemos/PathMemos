
import dayjs from '../../lib/dayjs';
import { safeDayjs, getReverseAddress, flatDistanceMeters, formatTimeLabel, type ReverseAddressResult } from '../../utils/util';
import request, { FILE_TYPE, getErrorMessage, createCancelToken, resetLoading } from '../../utils/request';
import { opsLog, opsLogFail, flushOpsLog } from '../../utils/opslog';
import { isAppEnv, guideToAppAuthorizeSetting } from '../../utils/appPermission';
import i18nBehavior from '../../behaviors/i18n';
import { i18n } from '../../utils/i18n';

function formatDisplayDate(d: dayjs.Dayjs) {
  return i18n.t('noteEdit.dateFormat', {
    year: d.year(),
    month: d.month() + 1,
    day: d.date(),
  });
}

function formatDisplayTime(timeStr: string) {
  if (!timeStr || typeof timeStr !== 'string') return '';
  const [h, m] = timeStr.split(':');
  return formatTimeLabel(parseInt(h, 10), parseInt(m, 10));
}

Component({
  behaviors: [i18nBehavior],

  properties: {
    info: Object,
    hiddenAutoRecord: Boolean,
    openAutoRecord: {
      type: Boolean,
      value: false,
    },
  },

  data: {
    form: {
      date: dayjs().format('YYYY-MM-DD'),
      time: dayjs().format('HH:mm'),
      diaryAddress: '',
      detailAddr: '',
      diaryLat: '',
      diaryLon: '',
      areaCode: '',
      areaName: '',
      recordText: '',
    } as any,
    formatDate: formatDisplayDate(dayjs()),
    formatTime: formatDisplayTime(dayjs().format('HH:mm')),
    calendarVisible: false,
    timePickerVisible: false,
    timeEditVisible: false,
    imgList: [] as any[],
    poiOptions: [] as { id: string; title: string; address: string; distance: number; lat: number; lng: number }[],
    confirmDialog: {
      visible: false,
      title: '',
      content: '',
      cancelText: '',
      confirmText: '',
      confirmType: 'default',
      action: '' as 'location' | 'upgrade' | '',
    },
  },

  observers: {
    info: function (info) {
      if (!info) return;
      const form = this.formatData(info);
      this._safeSetData({
        form,
        imgList: info.recordImages
          ? info.recordImages.map((item: { filePath: string; id: string }) => ({
              ...item,
              path: item.filePath,
              type: FILE_TYPE.UPLOADED,
            }))
          : [],
        formatDate: formatDisplayDate(safeDayjs(form.date) || dayjs()),
        formatTime: formatDisplayTime(form.time || dayjs().format('HH:mm')),
      });
    },
  },

  lifetimes: {
    attached: function () {
      (this as any)._isDestroyed = false;
      (this as any)._locating = false;
      // 新建/无 id 时，若父页面未指定日期，把日期时间重置为当前时刻；
      // 避免 data 初始值只在组件定义时求值一次，导致抽屉多次打开后仍显示页面加载时的时间/格式。
      if (!this.data.info?.id && !this.data.info?.date) {
        const now = dayjs();
        const date = now.format('YYYY-MM-DD');
        const time = now.format('HH:mm');
        (this as any)._safeSetData({
          form: { ...this.data.form, date, time },
          formatDate: formatDisplayDate(now),
          formatTime: formatDisplayTime(time),
        });
      }
      // 只在新建/无定位信息时请求定位。
      if (!this.data.info?.id && !this.data.form.diaryLat) {
        this.requestLocation();
      }
    },
    detached: function () {
      if ((this as any)._locationTimeoutTimer) {
        clearTimeout((this as any)._locationTimeoutTimer);
        (this as any)._locationTimeoutTimer = null;
      }
      if ((this as any)._uploadCancelToken) {
        try { (this as any)._uploadCancelToken.cancel(); } catch {}
        (this as any)._uploadCancelToken = null;
      }
      if ((this as any)._saveCancelToken) {
        try { (this as any)._saveCancelToken.cancel(); } catch {}
        (this as any)._saveCancelToken = null;
      }
      (this as any)._isDestroyed = true;
      resetLoading();
    },
  },

  pageLifetimes: {
    hide() {
      if ((this as any)._locationTimeoutTimer) {
        clearTimeout((this as any)._locationTimeoutTimer);
        (this as any)._locationTimeoutTimer = null;
      }
      // 保存/上传是用户主动发起的写操作，不应在 page hide（包括切后台、系统调用）时取消，
      // 否则返回后保存失败且用户不知情。这些 token 在 detached / 用户主动取消 / 完成时清理。
      (this as any)._forceSetData({
        calendarVisible: false,
        timePickerVisible: false,
        timeEditVisible: false,
        'confirmDialog.visible': false,
      });
      resetLoading();
    },
  },

  methods: {
    onLocaleChange() {
      const form = this.data.form || {};
      this._safeSetData({
        formatDate: formatDisplayDate(safeDayjs(form.date) || dayjs()),
        formatTime: formatDisplayTime(form.time || dayjs().format('HH:mm')),
      });
    },

    onAutoRecordChange(e: any) {
      const next = e.detail?.openAutoRecorded === true;
      (getApp() as any).globalData.openAutoRecorded = next;
      this._safeSetData({ openAutoRecord: next });
    },

    requestLocation() {
      // 多端 App：不前置查询授权态（getAppAuthorizeSetting 在部分真机上不回调、
      // 悬挂导致整个定位流程无任何输出，实测）。官方模式即「调用定位接口
      // 触发系统授权」，系统授权弹窗在 doGetLocation 的 getLocation 时触发；
      // 被拒场景由 doGetLocation 的 fail 回调处理（引导系统设置）。
      if (isAppEnv()) {
        this.doGetLocation();
        return;
      }
      wx.getSetting({
        success: (res) => {
          const locationAuth = res.authSetting['scope.userLocation'];
          if (locationAuth === true || locationAuth === undefined) {
            this.doGetLocation();
          } else if (locationAuth === false) {
            this._showLocationRequiredDialog();
          }
        },
        fail: () => {
          if (!(this as any)._isAlive()) return;
          wx.showToast({ title: (this as any).$t('noteEdit.openSettingsFail'), icon: 'none', duration: 2000 });
        },
      });
    },

    _showLocationRequiredDialog() {
      this._safeSetData({
        confirmDialog: {
          visible: true,
          title: (this as any).$t('noteEdit.locationRequired'),
          content: (this as any).$t('noteEdit.locationRequiredDesc'),
          cancelText: (this as any).$t('common.cancel'),
          confirmText: (this as any).$t('noteEdit.openSettings'),
          confirmType: 'default',
          action: 'location',
        },
      });
    },

    doGetLocation() {
      const self = this as any;
      if (self._isDestroyed || self._isDetached || self._locating) return;
      self._locating = true;
      // 立刻显示占位文案，让用户知道正在获取位置
      self._safeSetData({ 'form.diaryAddress': self.$t('noteEdit.locating') || '获取位置中...' });

      // 诊断包（0.0.42）：隐私同意态是定位 SDK 初始化的前置，先查官方
      // wx.miniapp.getPrivacySetting 并上报，用于判别「未同意隐私协议导致定位被框架禁用」。
      // 查询独立发起、不阻塞主流程（该 API 在部分环境可能不回调，不等待其结果）。
      if (isAppEnv()) {
        try {
          (wx as any).miniapp.getPrivacySetting({
            success: (res: any) => {
              console.log('[location_diag] privacy =', JSON.stringify(res));
              opsLog('location_diag', { step: 'privacy-check', needAuthorization: res?.needAuthorization });
              void flushOpsLog();
            },
            fail: (err: any) => {
              console.log('[location_diag] privacy query fail:', err?.errMsg);
              opsLog('location_diag', { step: 'privacy-check-fail', errMsg: err?.errMsg });
            },
          });
        } catch (e) {
          console.log('[location_diag] privacy query threw:', e);
        }
      }
      console.log('[location_diag] getLocation begin, appEnv =', isAppEnv());

      const LOCATION_TIMEOUT_MS = 15000;
      const locationPromise = new Promise<WechatMiniprogram.GetLocationSuccessCallbackResult>((resolve, reject) => {
        wx.getLocation({
          type: 'gcj02',
          success: resolve,
          fail: (err: any) => {
            console.log('[location_diag] getLocation fail:', JSON.stringify(err));
            opsLog('location_diag', { step: 'get-fail', errCode: err?.errCode, errMsg: err?.errMsg });
            void flushOpsLog();
            reject(err);
          },
        });
      });

      let timeoutTimer: any = null;
      const timeoutPromise = new Promise<never>((_, reject) => {
        timeoutTimer = setTimeout(() => reject(new Error('location timeout')), LOCATION_TIMEOUT_MS);
      });
      self._locationTimeoutTimer = timeoutTimer;

      Promise.race([locationPromise, timeoutPromise])
        .then(async ({ latitude, longitude }: any) => {
          console.log('[location_diag] getLocation ok:', latitude, longitude);
          opsLog('location_diag', { step: 'get-ok', lat: Number(latitude) });
          // 与 catch 分支一致：切后台后才返回定位结果时不再发起逆向解析/常用地址请求。
          if (self._isDestroyed || self._isDetached || self._isHidden) return;
          // 逆向解析和常用地址查询互不依赖，并行请求
          const [result, commonRes] = await Promise.all([
            getReverseAddress(latitude, longitude),
            request.get('/user/common-addresses', {}, true).catch(() => ({ data: { addresses: [] } } as any)),
          ]);
          console.log('[location_diag] reverse result =', JSON.stringify(result)?.slice(0, 200));
          opsLog('location_diag', { step: 'reverse-ok', hasResult: !!result });
          if (self._isDestroyed || self._isDetached || self._isHidden) return;
          if (!result) {
            if (!self._isHidden) {
              wx.showToast({ title: (this as any).$t('noteEdit.locationFailNetwork'), icon: 'none', duration: 2000 });
            }
            return;
          }
          await this._fillLocation(result, (commonRes as any)?.data?.addresses || []);
        }).catch((err: any) => {
          if (self._isDestroyed || self._isDetached || self._isHidden) return;
          // 超时/网络失败与权限拒绝区分文案，避免超时提示"权限被拒"误导用户。
          const timedOut = err?.message === 'location timeout';
          // 诊断：失败原因直接显示在地址栏 + 上报（0.0.42）
          const detail = String(err?.errMsg || err?.message || (timedOut ? 'timeout' : 'unknown')).slice(0, 80);
          console.log('[location_diag] doGetLocation catch:', detail);
          opsLog('location_diag', { step: 'doget-catch', errMsg: detail });
          void flushOpsLog();
          self._safeSetData({ 'form.diaryAddress': `${(this as any).$t('noteEdit.locationFailedPrefix')}[${detail}]` });
          // App 端系统权限被拒（auth deny/denied）：引导去系统设置重新开启。
          if (isAppEnv() && /auth\s*den|denied|permission/i.test(detail)) {
            wx.showModal({
              title: (this as any).$t('noteEdit.locationRequired'),
              content: (this as any).$t('noteEdit.locationRequiredDesc'),
              cancelText: (this as any).$t('common.cancel'),
              confirmText: (this as any).$t('noteEdit.openSettings'),
              success: (m) => {
                if (m.confirm) guideToAppAuthorizeSetting();
              },
            });
            return;
          }
          wx.showToast({ title: (this as any).$t(timedOut ? 'noteEdit.locationFailNetwork' : 'noteEdit.locationFailPermission'), icon: 'none', duration: 2000 });
        }).finally(() => {
          clearTimeout(timeoutTimer);
          if ((this as any)._locationTimeoutTimer === timeoutTimer) {
            (this as any)._locationTimeoutTimer = null;
          }
          self._locating = false;
        });
    },

    async _fillLocation(result: ReverseAddressResult, commonAddresses: any[]) {
      let landmark = result.landmark || result.address;
      const lat = result.location.lat;
      const lon = result.location.lng;

      if (landmark) {
        for (const addr of commonAddresses) {
          if (flatDistanceMeters(lat, lon, addr.lat, addr.lon) <= 300) {
            landmark = addr.name;
            break;
          }
        }
      }

      this._safeSetData({
        form: {
          ...this.data.form,
          diaryAddress: landmark,
          detailAddr: result.address,
          diaryLat: lat,
          diaryLon: lon,
          areaCode: result.areaCode,
          areaName: result.areaName,
        },
        poiOptions: (result.pois || []).slice(0, 5),
      });
    },
    selectPOI(e: any) {
      const index = e.currentTarget.dataset.index;
      const poi = this.data.poiOptions[index];
      if (!poi) return;
      this._safeSetData({
        'form.diaryAddress': poi.title,
        'form.diaryLat': poi.lat,
        'form.diaryLon': poi.lng,
      });
    },

    goCommonAddresses() {
      wx.navigateTo({ url: '/pages/CommonAddresses/CommonAddresses' });
    },
    formatData(data: any) {
      const recordTime = data.recordTime;
      const timeStr = recordTime
        ? (safeDayjs(recordTime) || dayjs()).format('HH:mm')
        : (data.time || dayjs().format('HH:mm'));
      return {
        ...data,
        diaryAddress: data.diaryAddress || '',
        detailAddr: data.detailAddr || '',
        recordText: data.recordText || '',
        date: recordTime
          ? (safeDayjs(recordTime) || dayjs()).format('YYYY-MM-DD')
          : (data.date || dayjs().format('YYYY-MM-DD')),
        time: timeStr,
        diaryLat: data.diaryLat != null ? data.diaryLat : '',
        diaryLon: data.diaryLon != null ? data.diaryLon : '',
        areaCode: data.areaCode || '',
        areaName: data.areaName || '',
      };
    },
    chooseLocation() {
      const self = this as any;
      wx.chooseLocation({
        success: async ({ name, address, latitude, longitude }) => {
          if (self._isDestroyed || self._isDetached) return;
          try {
            const result = await getReverseAddress(latitude, longitude);
            if (self._isDestroyed || self._isDetached) return;
            if (!result) {
              wx.showToast({ title: (this as any).$t('noteEdit.locationFail'), icon: 'error', duration: 2000 });
              return;
            }
            if (self._isDestroyed || self._isDetached) return;
            // 选择地址后更新表单；若回调时页面仍处隐藏态，
            // _safeSetData 会暂存到 pending 并在 show 时统一 flush。
            this._safeSetData({
              form: {
                ...this.data.form,
                diaryAddress: name,
                detailAddr: address,
                diaryLat: latitude,
                diaryLon: longitude,
                areaCode: result.areaCode || this.data.form.areaCode,
                areaName: result.areaName || this.data.form.areaName,
              },
              poiOptions: (result.pois || []).slice(0, 5),
            });
          } catch {
            if (self._isDestroyed || self._isDetached || self._isHidden) return;
            wx.showToast({ title: (this as any).$t('noteEdit.locationRetryFail'), icon: 'none', duration: 2000 });
          }
        },
        fail: (err: any) => {
          // 选点失败诊断：Donut 端若 LBS/权限/Key 异常会走这里，上报原始
          // errMsg 供 client_ops_logs 排查。
          console.log('[location_diag] chooseLocation fail:', JSON.stringify(err));
          opsLog('location_diag', { step: 'choose-fail', errCode: err?.errCode, errMsg: err?.errMsg });
          void flushOpsLog();
          if (self._isDestroyed || self._isDetached || self._isHidden) return;
          if (err?.errMsg?.includes('auth deny') || err?.errMsg?.includes('fail auth')) {
            wx.showToast({ title: (this as any).$t('noteEdit.locationPermissionDenied'), icon: 'none', duration: 2000 });
            return;
          }
          wx.showToast({ title: (this as any).$t('noteEdit.locationFail'), icon: 'none', duration: 2000 });
        },
      });
    },
    openCalendar() {
      (this.selectComponent('#NoteEditRecord') as any)?.blur?.();
      this._safeSetData({ calendarVisible: true });
    },
    onCalendarConfirm(e: { detail: { value: string } }) {
      const date = e.detail.value;
      this._safeSetData({
        'form.date': date,
        formatDate: formatDisplayDate(safeDayjs(date) || dayjs()),
        calendarVisible: false,
      });
    },
    onCalendarClose() {
      this._safeSetData({ calendarVisible: false });
    },
    openTimePicker() {
      (this.selectComponent('#NoteEditRecord') as any)?.blur?.();
      this._safeSetData({ timePickerVisible: true });
    },
    onTimeConfirm(e: { detail: { value: string; fromEdit?: boolean } }) {
      const time = e.detail.value;
      this._safeSetData({
        'form.time': time,
        formatTime: formatDisplayTime(time),
      });
      if (e.detail.fromEdit) {
        this._safeSetData({ timeEditVisible: false, timePickerVisible: false });
      } else {
        this._safeSetData({ timePickerVisible: false });
      }
    },
    onTimeClose() {
      this._safeSetData({ timePickerVisible: false });
    },
    onTimeEditOpen() {
      this._safeSetData({ timeEditVisible: true });
    },
    onTimeEditClose() {
      this._safeSetData({ timeEditVisible: false });
    },
    hidden() {
      (this as any)._locationRequested = false;
      this.triggerEvent('hidden');
    },

    onRecordTextInput(e: any) {
      this._safeSetData({ 'form.recordText': e.detail.value });
    },

    onConfirmDialogConfirm() {
      // 关闭弹窗是用户明确操作，使用 _safeSetData 统一做生命周期守卫。
      const action = this.data.confirmDialog.action;
      (this as any)._safeSetData({ 'confirmDialog.visible': false });

      if (action === 'upgrade') {
        wx.navigateTo({ url: '/pages/sub/Vip/Vip' });
        return;
      }

      if (action === 'location' && isAppEnv()) {
        // 多端 App：wx.openSetting 不可用，跳系统权限设置页；返回后用户重试定位。
        guideToAppAuthorizeSetting();
        return;
      }

      wx.openSetting({
        success: (settingRes) => {
          if (settingRes.authSetting['scope.userLocation']) {
            this.doGetLocation();
          }
        },
        fail: () => {
          if ((this as any)._isDestroyed || (this as any)._isDetached) return;
          wx.showToast({ title: (this as any).$t('noteEdit.openSettingsFail'), icon: 'none', duration: 2000 });
        },
      });
    },

    onConfirmDialogCancel() {
      (this as any)._safeSetData({ 'confirmDialog.visible': false });
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
          action: 'upgrade',
        },
      });
    },

    selectImage() {
      const self = this as any;
      // 上传/保存进行中禁止增删图片：uploadFile 基于提交时的快照，
      // 期间变更会导致新图静默丢 id、已删图复活。
      if (self._submitting || self._uploading) return;
      const MAX_IMAGES = 9;
      const remain = MAX_IMAGES - this.data.imgList.filter((i: any) => i.type !== FILE_TYPE.DELETE).length;
      if (remain <= 0) {
        wx.showToast({ title: (this as any).$t('noteEdit.maxImages', { count: MAX_IMAGES }), icon: 'none' });
        return;
      }
      wx.chooseMedia({
        count: Math.min(remain, 9),
        mediaType: ['image'],
        sizeType: ['compressed'],
        success: (res: any) => {
          if ((this as any)._isDestroyed || (this as any)._isDetached) return;

          const newFiles = res.tempFiles.map((item: { tempFilePath: string }) => ({
            path: item.tempFilePath,
            type: FILE_TYPE.TO_BE_UPLOADED,
          }));
          const total = this.data.imgList.filter((i: any) => i.type !== FILE_TYPE.DELETE).length + newFiles.length;
          if (total > MAX_IMAGES) {
            wx.showToast({ title: (this as any).$t('noteEdit.maxImages', { count: MAX_IMAGES }), icon: 'none' });
            return;
          }
          // 选择图片后更新 imgList；若回调时页面仍处隐藏态，
          // _safeSetData 会暂存到 pending 并在 show 时统一 flush，
          // 保证用户返回后能看到已选图片并正常上传。
          this._safeSetData({
            imgList: [...this.data.imgList, ...newFiles],
          });
        },
        fail: (error) => {
          if (!(this as any)._isAlive()) return;
          const errMsg = error?.errMsg || '';
          if (/cancel/i.test(errMsg)) return;
          if (/auth/i.test(errMsg) || /deny/i.test(errMsg)) {
            wx.showToast({ title: (this as any).$t('noteEdit.albumPermissionRequired'), icon: 'none' });
          } else {
            wx.showToast({ title: (this as any).$t('noteEdit.chooseImageFail'), icon: 'none' });
          }
        },
      });
    },
    delImage(e: { detail: any }) {
      const self = this as any;
      if (self._submitting || self._uploading) return;
      const { index } = e.detail;
      const item = this.data.imgList[index];
      this._safeSetData({
        [`imgList[${index}]`]: { ...item, type: FILE_TYPE.DELETE },
      });
    },
    cancel() {
      if ((this as any)._uploadCancelToken) {
        try { (this as any)._uploadCancelToken.cancel(); } catch {}
        (this as any)._uploadCancelToken = null;
      }
      if ((this as any)._saveCancelToken) {
        try { (this as any)._saveCancelToken.cancel(); } catch {}
        (this as any)._saveCancelToken = null;
      }
      this._safeSetData({ poiOptions: [] });
      this.hidden();
    },
    async submit() {
      const self = this as any;
      if (self._isDestroyed || self._isDetached) return;
      // 按 FP054/FP076：日记编辑为普通业务，不做函数级防重入锁；重复提交由后端兜底。
      // _submitting 仅用于按钮 loading/disabled 展示与提交期间禁止改图。

      const recordText = (this.data.form.recordText || '').trim();
      if ([...recordText].length > 140) {
        wx.showToast({ title: (this as any).$t('noteEdit.textMaxLength'), icon: 'none' });
        return;
      }
      // diaryLat 为数字 0（赤道）不应被误判为缺省；统一用空串/空值判断。
      if (!this.data.form.date || !this.data.form.time || !this.data.form.diaryAddress
        || this.data.form.diaryLat === '' || this.data.form.diaryLat == null) {
        wx.showToast({ title: (this as any).$t('noteEdit.requiredFields'), icon: 'none' });
        return;
      }
      const recordDateTime = safeDayjs([this.data.form.date, this.data.form.time].join(' '));
      if (!recordDateTime) {
        wx.showToast({ title: (this as any).$t('noteEdit.invalidDateTime'), icon: 'none' });
        return;
      }

      self._submitting = true;
      // 快照提升到 try 外，便于部分上传失败时把已成功图片的 id 落回 imgList。
      let imgSnapshot: any[] = [];

      try {
        // 提交时拍定 imgList 快照：上传是异步的，期间若 imgList 被增删（尽管有守卫，
        // 仍防外部路径），重建结果与上传的 imageIds 会错位（新图丢 id/已删图复活）。
        imgSnapshot = this.data.imgList.map((item: any) => ({ ...item }));
        const hasUpload = imgSnapshot.some((item: any) => item.type === FILE_TYPE.TO_BE_UPLOADED);
        const hasDelete = imgSnapshot.some((item: any) => item.type === FILE_TYPE.DELETE);
        const existingUploadedIds: string[] = imgSnapshot
          .filter((item: any) => item.type === FILE_TYPE.UPLOADED && item.id)
          .map((item: any) => item.id);
        let imageIds: string[] = [];
        if (hasUpload || hasDelete) {
          if (self._uploadCancelToken) {
            try { self._uploadCancelToken.cancel(); } catch {}
          }
          self._uploadCancelToken = createCancelToken();
          self._uploading = true;
          imageIds = await request.uploadFile(imgSnapshot, { cancelToken: self._uploadCancelToken });
          self._uploading = false;
          self._uploadCancelToken = null;
          if (self._isDestroyed || self._isDetached) return;
          const newImgList = imgSnapshot;
          const existingUploadedCount = newImgList.filter((item: any) => item.type === FILE_TYPE.UPLOADED && item.id).length;
          const newIds = imageIds.slice(existingUploadedCount);
          let newIdIndex = 0;
          for (let i = 0; i < newImgList.length; i++) {
            if (newImgList[i].type === FILE_TYPE.TO_BE_UPLOADED) {
              newImgList[i] = { ...newImgList[i], type: FILE_TYPE.UPLOADED, id: newIds[newIdIndex++] };
            }
          }
          // 上传完成时组件处于存活状态，使用 _safeSetData 同步 imgList，
          // 由 behavior 在隐藏期间暂存、返回前台后统一 flush。
          // 保留 DELETE 标记（PTextarea 对 DELETE 项不渲染，UI 不变）：
          // 若此处提前过滤，保存失败重试时删除意图永久丢失、被删图片复活。
          (this as any)._safeSetData({ imgList: newImgList });
        } else {
          imageIds = existingUploadedIds;
        }
        const lat = this.data.form.diaryLat === '' ? undefined : Number(this.data.form.diaryLat);
        const lon = this.data.form.diaryLon === '' ? undefined : Number(this.data.form.diaryLon);
        const payload = {
          data: {
            id: this.data.form.id || undefined,
            recordTime: recordDateTime.toISOString(),
            text: recordText,
            imageIds,
            lat,
            lon,
            address: this.data.form.diaryAddress,
            detailAddress: this.data.form.detailAddr,
            areaCode: this.data.form.areaCode,
            areaName: this.data.form.areaName,
          },
        };
        if (self._saveCancelToken) {
          try { self._saveCancelToken.cancel(); } catch {}
        }
        self._saveCancelToken = createCancelToken();
        const isUpdate = !!this.data.form.id;
        opsLog('manual_record_start', {
          isUpdate,
          textLen: (recordText || '').length,
          imageCount: (imageIds || []).length,
          recordTime: recordDateTime.toISOString(),
        });
        const res: any = isUpdate
          ? await request.put('/diary/details', { ...payload, cancelToken: self._saveCancelToken }, true)
          : await request.post('/diary/details', { ...payload, cancelToken: self._saveCancelToken }, true);
        self._saveCancelToken = null;

        if (self._isDestroyed || self._isDetached) return;
        opsLog('manual_record_ok', { isUpdate, hasCard: !!res?.data?.card, recordDate: this.data.form.date });
        // recordDate 优先取后端返回 card 的目标日期（跨天改期时与上海时区口径一致），
        // card 缺失（兜底失败）时回退表单日期；isUpdate 供页面区分「编辑改期」与「新建选日期」。
        const card = res?.data?.card;
        this.triggerEvent('submit', {
          recordDate: (card && card.recordDate) || this.data.form.date,
          card,
          isUpdate,
        });
        this.cancel();
      } catch (error: any) {
        if (!self._isAlive()) return;
        // 部分图片已上传成功但整批失败时，把成功项标记为 UPLOADED，
        // 下次提交只补传失败图，避免重复上传（孤儿文件 + 配额浪费）。
        if (imgSnapshot.length > 0 && imgSnapshot.length === this.data.imgList.length) {
          const patched = this.data.imgList.map((item: any, i: number) => {
            const snap: any = imgSnapshot[i];
            if (item.type === FILE_TYPE.TO_BE_UPLOADED && snap && snap.uploadedId) {
              return { ...item, type: FILE_TYPE.UPLOADED, id: snap.uploadedId };
            }
            return item;
          });
          if (patched.some((item: any, i: number) => item !== this.data.imgList[i])) {
            (this as any)._safeSetData({ imgList: patched });
          }
        }
        if (error?.message === 'request:abort') return;
        if (error?.code === 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED') {
          this.showStorageLimitDialog();
          return;
        }
        opsLogFail('manual_record_fail', error, { isUpdate: !!this.data.form.id });
        if (!error?._handledByModal) {
          wx.showToast({ title: getErrorMessage(error, (this as any).$t('noteEdit.saveFail')), icon: 'none' });
        }
      } finally {
        self._saveCancelToken = null;
        self._uploadCancelToken = null;
        self._submitting = false;
        // 上传失败/取消/超限时 _uploading 只在成功路径（L531）复位，需在此显式复位，
        // 否则 selectImage/delImage 被 _uploading 永久拦截，本页无法再增删图片。
        self._uploading = false;
      }
    },
  },
});

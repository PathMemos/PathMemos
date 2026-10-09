
import dayjs from '../../lib/dayjs';
import request, { createCancelToken, resetLoading } from '../../utils/request';
import { closeAutoRecord, openAutoRecord } from '../../utils/autoRecord';
import { isAppEnv } from '../../utils/appPermission';
import { logger } from '../../utils/logger';
import { getLoggedOut, getSessionId, needShowXPa, setNeedShowXPa, setPendingInviter } from '../../utils/storage';
import type { CancelToken } from '../../utils/http';
import themeBehavior from '../../behaviors/theme';
import i18nBehavior from '../../behaviors/i18n';

const PAGE_SIZE = 15;
const MAX_LIST_SIZE = 200;

function extractFamilyId(virtualID: string): string {
  const m = virtualID?.match(/^family:([^:]+):date:/);
  return m ? m[1] : '';
}

interface DiaryCard {
  id: string;
  recordDate: string;
  coverImg: string;
  [key: string]: any;
}

interface StatsData {
  firstRecordDate: string;
  recordDays: number;
  totalEntries: number;
  weeklyEntries: number;
}

Page({
  behaviors: [themeBehavior, i18nBehavior],
  data: {
    list: [] as DiaryCard[],
    isLogin: false,
    isScrollTop: true,
    loading: false,
    showEditDrawer: false,
    showAIDrawer: false,
    showMemoryCreateDrawer: false,
    showLongpressGuide: false,
    memoryCreateInfo: {},
    showSubscribePrompt: false,
    stats: {
      firstRecordDate: '',
      recordDays: 0,
      totalEntries: 0,
      weeklyEntries: 0,
    } as StatsData,
    openAutoRecorded: false,
    todayDay: '',
    refresherTriggered: false,
    _loadedOnce: false,
  },

  cursorDate: '',
  finishedLoad: false,
  _listCancelToken: null as CancelToken | null,
  _statsCancelToken: null as CancelToken | null,
  _isDestroyed: false,
  _isHidden: false,
  _coverPollTimer: null as any,
  _sceneCancelToken: null as CancelToken | null,
  _loginCancelToken: null as CancelToken | null,
  _loggingIn: false,
  _scenePromise: null as Promise<void> | null,

  onLoad(option: any) {
    (this as any)._isDestroyed = false;
    this._sceneCancelToken = createCancelToken();
    if (option.inviter) {
      setPendingInviter(option.inviter);
    }
    if (option.scene) {
      this._scenePromise = this._handleScene(option.scene);
    }
  },

  async _handleScene(scene: string) {
    try {
      const decoded = decodeURIComponent(scene);
      const shortCode = decoded.toUpperCase().replace(/[^A-Z0-9]/g, '');
      if (shortCode.length >= 6) {
        await this._resolveInviterFromShortCode(shortCode);
      }
    } catch (e) {
      logger.warn('scene parse failed', e);
    }
  },

  async _resolveInviterFromShortCode(shortCode: string) {
    try {
      const { data } = await request.get('/invite/resolve', { params: { code: shortCode }, cancelToken: this._sceneCancelToken || undefined }, false);
      if (data?.userId) {
        setPendingInviter(data.userId);
        // 登录已先于场景码解析完成时（极弱网 1.5s 竞态），补绑邀请人。
        // 后端幂等：已有邀请人/注册超 7 天均静默成功，不会重复奖励。
        if (request.isLogin()) {
          request.post('/auth/inviter', { data: { inviter: data.userId } }, true).catch((e) => {
            logger.warn('late bind inviter failed', e);
          });
        }
      }
    } catch (e: any) {
      if (e?.message === 'request:abort') return;
      logger.warn('resolve inviter from scene failed', e);
    }
  },

  async onShow() {
    (this as any)._isDestroyed = false;
    (this as any)._isHidden = false;
    (this as any)._applyPendingSetData();

    // 主动退出/注销停留标记：不再静默登录，落登录页等手动点击（仅在登录页的主动
    // 登录成功时清除；邀请 scene 在 onLoad 已解析进 storage，手动登录仍带邀请归属）。
    // 有有效会话则不拦——邀请卡片补登等静默登录建立的会话对用户透明。
    if (getLoggedOut() && !getSessionId()) {
      wx.reLaunch({ url: '/pages/Login/Login' });
      return;
    }

    // 新用户教程兜底：受邀注册的各入口路径（家庭卡片冷启动/静默补登）成功后
    // 都会置 needShowXPa，在此统一补看 4 页教程（Guide 结束按 pendingLinkId
    // 直达家庭页，无则回本页），堵住非登录页入口跳过教程的缺口。
    if (needShowXPa()) {
      setNeedShowXPa(false);
      wx.redirectTo({ url: '/pages/Guide/Guide' });
      return;
    }

    // 场景码解析与首屏并行：invite/resolve 弱网下最长 20s，串行等待会造成白屏。
    // 最多等待 1.5s（覆盖绝大多数正常网络）；超时未完成则先渲染首屏，
    // 极弱网下新用户登录可能在解析完成前发生、错过 inviter 归属（可接受，
    // 邀请关系仍可通过家庭页邀请链接补建）。
    if ((this as any)._scenePromise) {
      const scenePromise = (this as any)._scenePromise;
      (this as any)._scenePromise = null;
      await Promise.race([
        scenePromise,
        new Promise((resolve) => setTimeout(resolve, 1500)),
      ]);
    }

    // 登录是写操作，若 reload 或抽屉触发的登录仍在进行，不应取消正在进行的登录请求。
    if (!(this as any)._reloading && !(this as any)._loggingIn) {
      if ((this as any)._loginCancelToken) {
        try { (this as any)._loginCancelToken.cancel(); } catch {}
      }
      (this as any)._loginCancelToken = createCancelToken();
    }
    const enabled = (getApp() as any).globalData.openAutoRecorded === true;
    if (this.data.openAutoRecorded !== enabled) {
      (this as any)._safeSetData({ openAutoRecorded: enabled });
    }

    this.updateTodayText();

    
    const needReload = (getApp() as any).globalData._needRefreshIndexList || (this.data.list.length === 0 && !this.data._loadedOnce);
    if (needReload) {
      try {
        const loaded = await this.reload();
        if (loaded) {
          (getApp() as any).globalData._needRefreshIndexList = false;
          (this as any)._safeSetData({ _loadedOnce: true });
        }
      } catch (e) {
        if ((this as any)._isDestroyed || (this as any)._isHidden) return;
        logger.warn('index reload failed', e);
        wx.showToast({ title: (this as any).$t('home.loadFail'), icon: 'none', duration: 2000 });
      }
    }
    // 从详情页保存后返回：检查是否有待轮询的封面更新
    const pending: any = (getApp() as any).globalData._pendingCoverPoll;
    if (pending && pending.recordDate && pending.cardID) {
      delete (getApp() as any).globalData._pendingCoverPoll;
      (this as any)._pollCoverForDate(pending.recordDate, extractFamilyId(pending.cardID));
    }
  },

  _pollCoverForDate(this: any, recordDate: string, familyId: string) {
    const initialCover = this.data.list.find((c: DiaryCard) => c.recordDate === recordDate)?.coverImg || '';
    let polls = 0;
    if (this._coverPollTimer) clearTimeout(this._coverPollTimer);
    // setTimeout 链式调度，本轮请求完成后（含 catch）再排下一轮，弱网不并发叠加。
    const tick = async () => {
      if (++polls > 12 || this._isDestroyed || this._isHidden) {
        this._coverPollTimer = null;
        return;
      }
      try {
        // 首页封面轮询属后台请求，偶发 401 不踢登录态。
        const res: any = await request.get('/diary/cover-url', { params: { familyId, recordDate } }, true, true);
        const newCover = res?.data?.coverImg;
        // 02b D-12：封面变化（含变空串——API 清空封面场景）即更新并停止轮询；
        // 空串的占位图回退由渲染层承担。与 NoteDetail 同口径。
        if (newCover !== undefined && newCover !== initialCover) {
          const idx = this.data.list.findIndex((c: DiaryCard) => c.recordDate === recordDate);
          if (idx >= 0) {
            this._safeSetData({ [`list[${idx}].coverImg`]: newCover });
          }
          this._coverPollTimer = null;
          return;
        }
      } catch { /* poll failure is tolerated */ }
      this._coverPollTimer = setTimeout(tick, 500);
    };
    this._coverPollTimer = setTimeout(tick, 500);
  },

  onUnload() {
    // 先执行必须持久化的 UI 清理，再标记销毁；否则 _forceSetData 会因 _isDestroyed 直接返回。
    (this as any)._forceSetData({ showAIDrawer: false, showEditDrawer: false, showMemoryCreateDrawer: false, loading: false, showSubscribePrompt: false });
    (this as any)._isDestroyed = true;
    (this as any)._isHidden = true;
    (this as any)._changing = false;
    if (this._listCancelToken) {
      try { this._listCancelToken.cancel(); } catch {}
      this._listCancelToken = null;
    }
    if (this._statsCancelToken) {
      try { this._statsCancelToken.cancel(); } catch {}
      this._statsCancelToken = null;
    }
    if (this._sceneCancelToken) {
      try { this._sceneCancelToken.cancel(); } catch {}
      this._sceneCancelToken = null;
    }
    if ((this as any)._loginCancelToken) {
      try { (this as any)._loginCancelToken.cancel(); } catch {}
      (this as any)._loginCancelToken = null;
    }
    if ((this as any)._scrollThrottleTimer) {
      clearTimeout((this as any)._scrollThrottleTimer);
      (this as any)._scrollThrottleTimer = null;
    }
    if ((this as any)._coverPollTimer) {
      // _pollCoverForDate 为 setTimeout 链式调度，须用 clearTimeout 清理（同一句柄类型）。
      clearTimeout((this as any)._coverPollTimer);
      (this as any)._coverPollTimer = null;
    }
    try { wx.hideLoading(); } catch {}
    resetLoading();
    (this as any).unsubscribeTheme?.();
  },

  onHide() {
    (this as any)._isHidden = true;
    // _changing 是防止自动记录开关被重复触发的异步状态标志，不在 onHide 复位，
    // 避免切后台/系统调用返回后可重复触发 openAutoRecord/closeAutoRecord。
    // 切后台时关闭非编辑类 UI，保留 AI 抽屉以便从日记详情页返回后继续对话；
    // 编辑抽屉（showEditDrawer/showMemoryCreateDrawer）需要保留，因为 wx.chooseLocation /
    // wx.chooseMedia 等系统调用也会触发 onHide，返回后应继续编辑。
    // AI 抽屉的 SSE 连接由 onPageHide() 独立清理，不需要销毁组件本身。
    // 此处用 _forceSetData 确保 hidden 态也能写入。
    (this as any)._forceSetData({ loading: false, showSubscribePrompt: false });
    try {
      (this.selectComponent('#aiDrawer') as any)?.onPageHide?.();
    } catch {}
    if (this._listCancelToken) {
      try { this._listCancelToken.cancel(); } catch {}
      this._listCancelToken = null;
    }
    if (this._statsCancelToken) {
      try { this._statsCancelToken.cancel(); } catch {}
      this._statsCancelToken = null;
    }
    if (this._sceneCancelToken) {
      try { this._sceneCancelToken.cancel(); } catch {}
      this._sceneCancelToken = null;
    }
    // request.login 是写操作（创建 session），不得在 onHide 中取消；页面真正销毁（onUnload）
    // 时再清理，避免系统调用触发 onHide 后返回时登录状态丢失（AGENTS.md §3.4）。
    if ((this as any)._scrollThrottleTimer) {
      clearTimeout((this as any)._scrollThrottleTimer);
      (this as any)._scrollThrottleTimer = null;
    }
    try { wx.hideLoading(); } catch {}
    resetLoading();
  },

  updateTodayText(_stats?: StatsData) {
    const now = dayjs();
    // 仅维护跨天检测所需的 todayDay；todayDateText/weeklyProgress 在 wxml 中无绑定（死状态数据），
    // 已移除，避免无效 setData 流量与硬编码英文月份。
    const todayDay = now.format('DD');
    if (this.data.todayDay !== todayDay) {
      (this as any)._safeSetData({ todayDay });
    }
  },

  updateNavTitle() {
    wx.setNavigationBarTitle({ title: (this as any).$t('brand.name') });
  },

  onLocaleChange() {
    this.updateTodayText();
    this.updateNavTitle();
  },

  async reload(): Promise<boolean> {
    if ((this as any)._reloading || (this as any)._isHidden) return false;
    (this as any)._reloading = true;
    try {
      this.cursorDate = '';
      this.finishedLoad = false;
      (this as any)._safeSetData({ list: [], loading: false });

      const isLogin = await this.ensureLogin((this as any)._loginCancelToken);
      if (!isLogin || (this as any)._isDestroyed) return false;

      await this.fetch({ skipReloadGuard: true });
      if ((this as any)._isDestroyed || (this as any)._isHidden) return false;
      // fetch 被 abort 或失败时 list 仍为空且 finishedLoad 为 false，不能算加载成功
      if (this.data.list.length === 0 && !this.finishedLoad) return false;
      await this.fetchStats();
      return !(this as any)._isDestroyed && !(this as any)._isHidden;
    } catch (e) {
      logger.warn('index reload failed', e);
      return false;
    } finally {
      (this as any)._reloading = false;
    }
  },

  // 下拉刷新：在现有内容基础上原位替换第一页，避免清空列表造成空态闪烁；
  // 与 reload() 的差异在于不清 list、不要求 finishedLoad，刷新失败时保留旧内容。
  async handlePullRefresh() {
    if ((this as any)._reloading || (this as any)._isHidden) {
      (this as any)._safeSetData({ refresherTriggered: false });
      return;
    }
    (this as any)._reloading = true;
    (this as any)._safeSetData({ refresherTriggered: true });
    let prevCursor = this.cursorDate;
    let prevFinished = this.finishedLoad;
    try {
      const isLogin = await this.ensureLogin((this as any)._loginCancelToken);
      if (!isLogin || (this as any)._isDestroyed) return;
      this.cursorDate = '';
      this.finishedLoad = false;
      const ok = await this.fetch({ skipReloadGuard: true, replace: true });
      if ((this as any)._isDestroyed || (this as any)._isHidden) return;
      if (!ok) {
        // fetch 内部吞错（toast 已提示）：恢复旧游标。否则下次 loadMore 从空游标
        // 重拉第一页并 append 到旧列表，造成整页重复。
        this.cursorDate = prevCursor;
        this.finishedLoad = prevFinished;
        return;
      }
      await this.fetchStats();
    } catch (e) {
      // 请求失败时恢复旧游标，避免下次 loadMore 从空游标重拉首页造成列表重复。
      this.cursorDate = prevCursor;
      this.finishedLoad = prevFinished;
      logger.warn('index pull refresh failed', e);
    } finally {
      (this as any)._reloading = false;
      (this as any)._safeSetData({ refresherTriggered: false });
    }
  },

  async onEntrySubmit(e: any) {
    // 编辑/创建日记后后端会刷新封面，前端必须重新拉取列表以获取最新封面（AGENTS.md §3.8）。
    (getApp() as any).globalData._needRefreshIndexList = true;
    await this.reload();
    const recordDate: string = e?.detail?.recordDate;
    const cardID: string = e?.detail?.card?.id;
    if (recordDate && cardID) {
      (this as any)._pollCoverForDate(recordDate, extractFamilyId(cardID));
    }
  },

  _resetListCancelToken() {
    if (this._listCancelToken) {
      try { this._listCancelToken.cancel(); } catch {}
    }
    this._listCancelToken = createCancelToken();
  },

  _resetStatsCancelToken() {
    if (this._statsCancelToken) {
      try { this._statsCancelToken.cancel(); } catch {}
    }
    this._statsCancelToken = createCancelToken();
  },

  _isRequestAbortError(err: any): boolean {
    return err?.message === 'request:abort';
  },

  async ensureLogin(cancelToken?: CancelToken): Promise<boolean> {
    (this as any)._loggingIn = true;
    try {
      let isLogin = await request.isLogin();
      if (isLogin) {
        // 验证本地 token 对应的用户是否仍然有效（账号注销/被删除后本地 token 可能未过期）
        try {
          await request.get('/user/profile', { cancelToken }, true);
        } catch {
          isLogin = await request.isLogin();
        }
      }
      if (!isLogin) {
        try {
          await request.login(cancelToken);
          isLogin = await request.isLogin();
        } catch {
          isLogin = false;
        }
      }
      (this as any)._safeSetData({ isLogin });
      return isLogin;
    } finally {
      (this as any)._loggingIn = false;
    }
  },

  /** 拉取列表。返回是否成功（守卫直返/成功为 true；失败/被取消为 false，供下拉刷新恢复游标）。 */
  async fetch(options?: { skipReloadGuard?: boolean; replace?: boolean }): Promise<boolean> {
    if (this.data.loading || this.finishedLoad || !this.data.isLogin || ((this as any)._reloading && !options?.skipReloadGuard) || (this as any)._isHidden) return true;

    this._resetListCancelToken();
    const cancelToken = this._listCancelToken;
    (this as any)._safeSetData({ loading: true });
    try {
      const params: any = { size: PAGE_SIZE };
      if (this.cursorDate) {
        params.cursorDate = this.cursorDate;
      }
      const { data, nextCursor } = await request.get('/diary/info', { params, cancelToken: cancelToken! }, true);
      const mappedData = data || [];

      this.finishedLoad = !nextCursor;
      this.cursorDate = nextCursor || '';

      if (options?.replace) {
        // 下拉刷新：整体替换第一页（首屏至多 PAGE_SIZE 条，单次 setData 数组远小于阈值）。
        (this as any)._safeSetData({
          list: mappedData,
          loading: false,
        });
        return true;
      }

      const newList = this.data.list.concat(mappedData);
      if (newList.length > MAX_LIST_SIZE) {
        // 达到上限时仍以增量方式仅写入新增分片（截断到上限），避免单次 setData
        // 传入整份 200 条大数组（AGENTS.md §5.5 / 5.4-L018 单次 ≤50 条）。
        this.finishedLoad = true;
        const start = this.data.list.length;
        const updateData: any = {
          loading: false,
        };
        for (let i = 0; start + i < MAX_LIST_SIZE && i < mappedData.length; i++) {
          updateData[`list[${start + i}]`] = mappedData[i];
        }
        (this as any)._safeSetData(updateData);
      } else {
        const start = this.data.list.length;
        const updateData: any = {
          loading: false,
        };
        mappedData.forEach((item: DiaryCard, i: number) => {
          updateData[`list[${start + i}]`] = item;
        });
        (this as any)._safeSetData(updateData);
      }
      return true;
    } catch (err: any) {
      if (this._isRequestAbortError(err)) {
        (this as any)._safeSetData({ loading: false });
        return false;
      }
      if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
        wx.showToast({ title: (this as any).$t('home.loadFail'), icon: 'none' });
      }
      (this as any)._safeSetData({ loading: false });
      return false;
    }
  },

  async fetchStats() {
    if ((this as any)._isDestroyed || (this as any)._isHidden || !this.data.isLogin) return;
    this._resetStatsCancelToken();
    const cancelToken = this._statsCancelToken;
    try {
      const { data: stats } = await request.get('/diary/stats', { cancelToken: cancelToken! });
      const enabled = (getApp() as any).globalData.openAutoRecorded === true;
      const newStats = {
        firstRecordDate: stats?.firstRecordDate || '',
        recordDays: stats?.recordDays || 0,
        totalEntries: stats?.totalEntries || 0,
        weeklyEntries: stats?.weeklyEntries || 0,
      };
      (this as any)._safeSetData({
        stats: newStats,
        openAutoRecorded: enabled,
      });
      this.updateTodayText(newStats);
    } catch (err: any) {
      if (this._isRequestAbortError(err)) return;
      logger.warn('fetch stats failed', err);
    }
  },

  toggleAutoRecord() {
    if ((this as any)._changing) return;
    (this as any)._changing = true;
    const vipInfo = request.getVipInfo() || {};
    if (!(vipInfo?.isVip > 0)) {
      wx.showModal({
        title: (this as any).$t('common.tip'),
        content: (this as any).$t('home.autoRecordVipTip'),
        cancelText: (this as any).$t('common.cancel'),
        confirmText: (this as any).$t('home.confirm'),
        success: (res) => {
          if (res.confirm) {
            wx.navigateTo({
              url: '/pages/sub/Vip/Vip',
              fail: () => wx.showToast({ title: (this as any).$t('home.navigateFail'), icon: 'none' }),
            });
          }
        },
        fail: () => {
          (this as any)._changing = false;
        },
        complete: () => {
          (this as any)._changing = false;
        },
      });
      return;
    }

    const finish = () => {
      (this as any)._changing = false;
      const enabled = (getApp() as any).globalData.openAutoRecorded === true;
      (this as any)._safeSetData({ openAutoRecorded: enabled });
    };

    if (this.data.openAutoRecorded) {
      wx.showModal({
        title: (this as any).$t('home.closeAutoRecord'),
        content: (this as any).$t('home.closeAutoRecordTip'),
        success: async (res) => {
          if (res.confirm) {
            try {
              await closeAutoRecord();
            } catch {
              if ((this as any)._isDestroyed || (this as any)._isHidden) return;
              wx.showToast({ title: (this as any).$t('home.closeFail'), icon: 'none' });
            }
          }
          finish();
        },
        fail: finish,
      });
    } else {
      wx.showModal({
        title: (this as any).$t('home.openAutoRecord'),
        content: (this as any).$t('home.openAutoRecordTip'),
        success: async (res) => {
          if (res.confirm) {
            try {
              await openAutoRecord();
              // 在 VIP 验证、后台定位授权和自动记录开启成功后，再引导订阅（仅小程序；
              // App 端通知已裁撤——不弹订阅授权、不记订阅，02g/06）
              if (!isAppEnv()) {
                (this as any)._safeSetData({ showSubscribePrompt: true });
              }
            } catch {
              if (!(this as any)._isDestroyed && !(this as any)._isHidden) {
                wx.showToast({ title: (this as any).$t('home.openFail'), icon: 'none' });
              }
            }
          }
          finish();
        },
        fail: finish,
      });
    }
  },

  async openEditDrawer() {
    if (this.data.showEditDrawer) return;
    const shown = wx.getStorageSync('memory_longpress_guide_shown');
    if (!shown) {
      this._showLongpressGuide();
      return;
    }
    await this.ensureLogin((this as any)._loginCancelToken);
    if ((this as any)._isDestroyed || !this.data.isLogin) return;
    (this as any)._safeSetData({ showEditDrawer: true });
  },

  _showLongpressGuide() {
    (this as any)._safeSetData({ showLongpressGuide: true });
  },

  onLongpressGuideClose() {
    wx.setStorageSync('memory_longpress_guide_shown', true);
    (this as any)._safeSetData({ showLongpressGuide: false });
  },

  onLongpressGuideContentTap() {
    // 点击引导内容区域不做任何操作，防止误触关闭
  },

  closeEditDrawer() {
    (this as any)._safeSetData({ showEditDrawer: false });
  },

  async openAIDrawer() {
    if (this.data.showAIDrawer) return;
    await this.ensureLogin((this as any)._loginCancelToken);
    if ((this as any)._isDestroyed || !this.data.isLogin) return;
    (this as any)._safeSetData({ showAIDrawer: true });
  },

  closeAIDrawer() {
    (this as any)._safeSetData({ showAIDrawer: false });
  },

  async openMemoryCreateDrawer() {
    if (this.data.showMemoryCreateDrawer) return;
    await this.ensureLogin((this as any)._loginCancelToken);
    if ((this as any)._isDestroyed || !this.data.isLogin) return;
    (this as any)._safeSetData({
      showMemoryCreateDrawer: true,
      memoryCreateInfo: {},
    });
  },

  closeMemoryCreateDrawer() {
    (this as any)._safeSetData({ showMemoryCreateDrawer: false });
  },

  onMemoryCreateSubmit() {
    this.reload();
  },

  handleScroll(e: any) {
    if ((this as any)._scrollThrottleTimer) return;
    (this as any)._scrollThrottleTimer = setTimeout(() => {
      (this as any)._scrollThrottleTimer = null;
    }, 16);
    const { scrollTop } = e.detail;
    const isScrollTop = scrollTop < 10;
    if (this.data.isScrollTop !== isScrollTop) {
      (this as any)._safeSetData({ isScrollTop });
    }
  },

  handleScrollToUpper() {
    (this as any)._safeSetData({ isScrollTop: true });
  },

  onSubscribeConfirm() {
    (this as any)._safeSetData({ showSubscribePrompt: false });
  },

  onSubscribeCancel() {
    (this as any)._safeSetData({ showSubscribePrompt: false });
  },

  handleLoadMore() {
    if ((this as any)._reloading) return;
    this.fetch();
  },

  goToUser() {
    wx.navigateTo({
      url: '/pages/User/User',
      fail: () => wx.showToast({ title: (this as any).$t('home.navigateFail'), icon: 'none' }),
    });
  },
});

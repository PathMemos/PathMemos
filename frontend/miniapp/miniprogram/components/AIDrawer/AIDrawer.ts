import request, { createCancelToken } from '../../utils/request';
import { getSSEBaseURL } from '../../config/index';
import { getSystemInfo, safeDayjs } from '../../utils/util';
import { eventSource } from '../../utils/eventSource';
import { getBackendMode, getPrivateBackendApiKey } from '../../utils/storage';
import { markdownToHtml } from '../../utils/markdown';
import { logger } from '../../utils/logger';
import type { CancelToken } from '../../utils/http';
import i18nBehavior from '../../behaviors/i18n';
import { i18n } from '../../utils/i18n';

const MESSAGE_TYPES = {
  PPFJ: 'PPFJ',
  USER: 'USER',
};

const MAX_MESSAGE_COUNT = 50;

function randomArr(arr: string[], length: number) {
  const copy = [...arr];
  const newArr = [] as string[];
  for (let i = 0; i < length && copy.length > 0; i++) {
    const index = Math.floor(Math.random() * copy.length);
    newArr.push(copy.splice(index, 1)[0]);
  }
  return newArr.reverse();
}

Component({
  behaviors: [i18nBehavior],

  properties: {},

  data: {

    bottom: 0,
    userAvatar: '',
    bottomSafeHeight: '0',
    inputValue: '',
    loading: false,
    connecting: false,
    exampleList: [] as any,
    list: [] as any,
    scrollIntoId: '',
    showQuotaDialog: false,
  },

  // @ts-ignore
  _aiText: '',
  _sseAbort: null as (() => void) | null,
  _lastPrompt: '',
  _aiTextDirty: false,
  _aiFlushTimer: null as any,
  _sending: false,
  // 发送代次。每次 send 自增，旧 SSE 的回调据此失效，避免旧流 abort 时同步复位 _sending。
  _sendSeq: 0,
  _diaryCardsCancelToken: null as CancelToken | null,
  _loginCancelToken: null as CancelToken | null,

  lifetimes: {
    attached: function () {
      const self = this as any;
      self._isDestroyed = false;
      self._isHidden = false;
      self._sending = false;
      const { bottomSafeHeight } = getSystemInfo();
      this._safeSetData({
        userAvatar: request.getAvatar(),
        bottomSafeHeight: `${Math.max(bottomSafeHeight, 30)}px`,
        exampleList: this._buildExamples(),
      });
    },
    detached: function () {
      // behavior detached 已置 _isDetached=true 并清空 pending；_forceSetData 会因此短路，
      // 未落盘 AI 文本属于已接受风险（容忍丢弃，见 frontend-pages.md FP003 / 已接受风险）。
      this._abortSSE();
      (this as any)._isDestroyed = true;
      if ((this as any)._diaryCardsCancelToken) {
        try { (this as any)._diaryCardsCancelToken.cancel(); } catch {}
        (this as any)._diaryCardsCancelToken = null;
      }
      if ((this as any)._loginCancelToken) {
        try { (this as any)._loginCancelToken.cancel(); } catch {}
        (this as any)._loginCancelToken = null;
      }
    },
  },

  pageLifetimes: {
    hide() {
      // onHide 中中止 SSE、清除定时器并关闭非编辑类弹窗。_sending 不由 onHide 直接复位，
      // 而是由 SSE abort 触发的 onclose/onerror 回调在操作终止时复位（AGENTS.md §3.4/§3.5）。
      this._abortSSE();
      this._forceSetData({ loading: false, connecting: false, showQuotaDialog: false });
      if ((this as any)._diaryCardsCancelToken) {
        try { (this as any)._diaryCardsCancelToken.cancel(); } catch {}
        (this as any)._diaryCardsCancelToken = null;
      }
      // 登录是写操作，不得在 onHide 中取消；组件 detached 时再清理。
    },
    show() {
      (this as any)._applyPendingSetData();
      if ((this as any)._aiTextDirty) {
        this._flushAIText();
      }
      if (this.data.list && this.data.list.length > 0 && !this.data.connecting) {
        this._safeSetData({ connecting: true });
      }
    },
  },

  methods: {
    onLocaleChange() {
      this._safeSetData({ exampleList: this._buildExamples() });
    },

    _buildExamples() {
      const examples = (i18n.getMessages().aiDrawer.exampleList || []) as string[];
      const first = i18n.t('aiDrawer.exampleFirst');
      return [first, ...randomArr(examples, 4)];
    },

    // 中止 SSE 并清理定时器。eventSource.abort() 会触发 onclose/onerror，由回调负责
    // 复位 _sending 与 loading 状态；本函数不再直接操作 _sending（AGENTS.md §3.4/§3.5）。
    _abortSSE() {
      if ((this as any)._aiFlushTimer) {
        clearTimeout((this as any)._aiFlushTimer);
        (this as any)._aiFlushTimer = null;
      }
      // 中止前先把未 flush 的 AI 文本写入 data.list；正常存活路径走 _safeSetData，
      // detached 等销毁路径已在调用前强制 flush。
      if ((this as any)._aiTextDirty) {
        this._flushAIText();
      }
      if ((this as any)._sseAbort) {
        try {
          (this as any)._sseAbort();
        } catch (e) {
          logger.warn('abort sse error', e);
        }
        (this as any)._sseAbort = null;
      }
    },

    _trimOldMessages(list: any[]) {
      if (list.length <= MAX_MESSAGE_COUNT) return list;
      return list.slice(list.length - MAX_MESSAGE_COUNT);
    },

    _flushAIText() {
      const self = this as any;
      self._aiTextDirty = false;
      const list = this.data.list as any[];
      const lastIndex = list.length - 1;
      if (lastIndex >= 0 && list[lastIndex].type === MESSAGE_TYPES.PPFJ) {
        const data = { [`list[${lastIndex}].txt`]: self._aiText };
        this._safeSetData(data);
      }
    },

    _scheduleAITextFlush() {
      const self = this as any;
      if (self._aiFlushTimer) return;
      self._aiFlushTimer = setTimeout(() => {
        self._aiFlushTimer = null;
        if (!self._isAlive()) return;
        if (self._aiTextDirty) {
          this._flushAIText();
        }
      }, 120);
    },

    hidden() {
      // 用户主动关闭 AI 抽屉，发送流程结束，复位 _sending。
      this._abortSSE();
      if ((this as any)._diaryCardsCancelToken) {
        try { (this as any)._diaryCardsCancelToken.cancel(); } catch {}
        (this as any)._diaryCardsCancelToken = null;
      }
      if ((this as any)._loginCancelToken) {
        try { (this as any)._loginCancelToken.cancel(); } catch {}
        (this as any)._loginCancelToken = null;
      }
      // 收起抽屉时不主动清空消息列表，保留对话上下文；仅关闭 SSE/清理状态。
      this._safeSetData({ connecting: false, loading: false, showQuotaDialog: false });
      this.triggerEvent('hidden');
    },

    // 父页面 onHide 调用：中止 SSE、清定时器、关闭非编辑类弹窗。_sending 由 SSE 回调复位，
    // 与 pageLifetimes.hide 语义一致，用于 Page 实例无法触发组件 pageLifetimes 的场景。
    onPageHide() {
      this._abortSSE();
      this._forceSetData({ loading: false, connecting: false, showQuotaDialog: false });
      if ((this as any)._diaryCardsCancelToken) {
        try { (this as any)._diaryCardsCancelToken.cancel(); } catch {}
        (this as any)._diaryCardsCancelToken = null;
      }
    },

    bindinput(e: any) {
      this._safeSetData({ inputValue: e.detail.value });
    },

    submit(e: any) {
      const txt = (e?.currentTarget?.dataset?.example || '') || this.data.inputValue;
      this.sendPrompt(txt);
    },

    async sendPrompt(prompt: string) {
      const self = this as any;
      try {
        if (self._sending || !prompt?.trim()) return;

        // 与后端 maxMessageCodePoints 统一为 2500，避免两端口径不一致。
        if ([...prompt].length > 2500) {
          wx.showToast({ title: (this as any).$t('aiDrawer.maxLength', { count: 2500 }), icon: 'none' });
          return;
        }

        self._lastPrompt = prompt;
        self._sending = true;

        const userMsg = {
          type: MESSAGE_TYPES.USER,
          txt: prompt,
          _msgId: `${Date.now()}_${Math.random().toString(36).slice(2, 8)}`,
        };
        const newList = this._trimOldMessages([...this.data.list, userMsg]);

        this._safeSetData({
          connecting: true,
          inputValue: '',
          list: newList,
          scrollIntoId: 'msg-bottom',
        });
        // 不要把 send 放在 setData callback 中：页面隐藏时 callback 被丢弃，_sending 会永远
        // 为 true，返回后无法再次发送（AGENTS.md §3.4/§3.5）。
        if (self._isDestroyed || self._isDetached) {
          self._sending = false;
          return;
        }
        try {
          await this.send(prompt);
        } catch (err) {
          logger.error('AI send failed', err);
          if (!self._isDestroyed && !self._isDetached && !self._isHidden) {
            wx.showToast({ title: (this as any).$t('aiDrawer.sendFail'), icon: 'none' });
          }
          self._sending = false;
        }
      } catch (err) {
        logger.error('AI sendPrompt threw', err);
        self._sending = false;
        this._abortSSE();
        if (self._isDestroyed || self._isDetached || self._isHidden) {
          return;
        }
        wx.showToast({ title: (this as any).$t('aiDrawer.sendFail'), icon: 'none' });
      }
    },

    _extractDates(text: string): string[]{
      const matches = text.match(/\$\$(\d{8})\$\$/g) || [];
      const dateSet = new Set<string>();
      matches.forEach((m) => {
        const d = m.slice(2, 10);
        const y = parseInt(d.slice(0, 4), 10);
        const mo = parseInt(d.slice(4, 6), 10);
        const day = parseInt(d.slice(6, 8), 10);
        if (y < 2000 || y > 2100 || mo < 1 || mo > 12 || day < 1 || day > 31) return;
        dateSet.add(`${d.slice(0, 4)}-${d.slice(4, 6)}-${d.slice(6, 8)}`);
      });
      return Array.from(dateSet);
    },

    _getWeekDayName(dateStr: string): string {
      const d = safeDayjs(dateStr);
      return d ? i18n.t(`aiDrawer.weekday${d.day()}`) : '';
    },

    async _fetchDiaryCards(dates: string[], cancelToken?: CancelToken): Promise<any[]> {
      try {
        // AI 对话期间的后台卡片请求，偶发 401 不踢登录态（由 catch 静默处理）。
        const res = await request.post('/diary/info/dates', { data: { dates }, cancelToken }, false, 20000, true);
        return (res.data || [])
          .filter((item: any) => item && item.recordDate)
          .map((item: any) => {
            const recordDate = item.recordDate;
            const [, month, day] = recordDate.split('-');
            const dateNameFormat = i18n.t('aiDrawer.dateFormat', {
              year: recordDate.slice(2, 4),
              month: parseInt(month, 10),
              day: parseInt(day, 10),
            });
            return {
              id: item.id,
              myDiaryInfoId: item.myDiaryInfoId || item.id,
              recordDate,
              dateName: dateNameFormat,
              dateNameFormat,
              weekDayName: this._getWeekDayName(recordDate),
              coverImg: item.coverImg || '',
            };
          });
      } catch {
        return [];
      }
    },

    _fetchDiaryCardsForMessage(dates: string[], targetIndex: number) {
      const list = this.data.list as any[];
      const targetItem = list[targetIndex];
      if (!targetItem || targetItem.type !== MESSAGE_TYPES.PPFJ) return;
      // 先记录目标消息的 _msgId，回填完成时按 _msgId 重新定位当前索引，
      // 避免请求期间列表被裁剪（trim/新消息插入）导致卡片挂到错误消息上。
      const targetMsgId = targetItem._msgId;
      if ((this as any)._diaryCardsCancelToken) {
        try { (this as any)._diaryCardsCancelToken.cancel(); } catch {}
      }
      const cancelToken = createCancelToken();
      (this as any)._diaryCardsCancelToken = cancelToken;
      this._fetchDiaryCards(dates, cancelToken).then((cards: any[]) => {
        if (!this._isAlive() || cancelToken.isCancelled()) return;
        const currentList = this.data.list as any[];
        const idx = currentList.findIndex((item: any) => item._msgId === targetMsgId);
        if (idx < 0) return;
        if (currentList[idx]?.type !== MESSAGE_TYPES.PPFJ) return;
        this._safeSetData({ [`list[${idx}].diaryCard`]: cards.slice(0, MAX_MESSAGE_COUNT) });
      });
    },

    _finalizeAIResponse() {
      // @ts-ignore
      const cleanText = this._aiText.replace(/\$\$[^$]*\$\$/g, '');
      const dates = this._extractDates(this._aiText);

      const list = this.data.list as any[];
      const lastIndex = list.length - 1;
      if (lastIndex < 0) {
        (this as any)._sending = false;
        this._safeSetData({ loading: false });
        return;
      }

      const newItem = {
        ...list[lastIndex],
        txt: cleanText,
        html: markdownToHtml(cleanText),
        diaryCard: [],
      };

      // @ts-ignore
      this._aiText = '';
      (this as any)._aiTextDirty = false;
      (this as any)._sending = false;

      this._safeSetData({
        [`list[${lastIndex}]`]: newItem,
        loading: false,
        scrollIntoId: 'msg-bottom',
      });

      if (dates.length > 0) {
        (this as any)._fetchDiaryCardsForMessage(dates, lastIndex);
      }
    },

    async send(prompt?: string) {
      const self = this as any;
      // 切后台属于正常生命周期，SSE 允许在后台继续；只拦截已销毁/已 detached 实例。
      if (self._isDestroyed || self._isDetached) {
        self._sending = false;
        return;
      }
      self._aiTextDirty = false;
      // 先递增发送代次并置发送锁，再中止旧 SSE。
      // 旧 SSE 的 abort 会同步触发其 onclose/onerror，代次不匹配时旧回调直接返回，
      // 不会把 _sending 复位导致新流在锁失效下运行（慢网/连点并发发送）。
      const sendSeq = ((self._sendSeq as number) || 0) + 1;
      self._sendSeq = sendSeq;
      self._sending = true;
      this._safeSetData({ loading: true });
      this._abortSSE();
      // @ts-ignore
      this._aiText = '';

      const message = prompt !== undefined ? prompt : this.data.inputValue;

      const sseBaseURL = getSSEBaseURL();
      if (!sseBaseURL) {
        if (this._isDestroyed || this._isDetached || this._isHidden) {
          self._sending = false;
          return;
        }
        wx.showModal({ title: (this as any).$t('aiDrawer.configError'), content: (this as any).$t('aiDrawer.sseEmpty'), showCancel: false });
        self._sending = false;
        this._safeSetData({ loading: false });
        return;
      }

      if ((this as any)._loginCancelToken) {
        try { (this as any)._loginCancelToken.cancel(); } catch {}
      }
      (this as any)._loginCancelToken = createCancelToken();
      try {
        await request.login((this as any)._loginCancelToken);
      } catch (e) {
        logger.error('AI login failed', e);
        (this as any)._loginCancelToken = null;
        if (self._isDestroyed || self._isDetached || self._isHidden) {
          self._sending = false;
          return;
        }
        wx.showToast({ title: (this as any).$t('aiDrawer.loginFail'), icon: 'none' });
        self._sending = false;
        this._safeSetData({ loading: false });
        return;
      }
      (this as any)._loginCancelToken = null;

      if (self._isDestroyed || self._isDetached) {
        self._sending = false;
        this._safeSetData({ loading: false });
        return;
      }

      const sessionId = request.getSessionId();
      const sseHeaders: Record<string, string> = {
        Authorization: sessionId ? `Bearer ${sessionId}` : '',
      };
      if (getBackendMode() === 'private') {
        const apiKey = getPrivateBackendApiKey();
        if (apiKey) {
          sseHeaders['X-Private-Api-Key'] = apiKey;
        }
      }

      // 本轮消息的幂等标识。eventSource 断线重连会复用同一 data 对象重发，
      // 后端据此精确识别"同一轮消息的重连"而非"重复发了两条相同消息"。
      const requestId = `${Date.now()}_${Math.random().toString(36).slice(2, 10)}`;
      const sse = eventSource({
        url: `${sseBaseURL}/ai/chat`,
        header: sseHeaders,
        data: { message, request_id: requestId },
        onreconnect: () => {
          if (this._isDestroyed || this._isDetached) return;
          if (sendSeq !== self._sendSeq) return;
          // 自动重连前清空半截输出：新流会重新 onopen 建立气泡，避免新旧内容拼接。
          self._aiText = '';
          self._aiTextDirty = false;
          if ((this as any)._aiFlushTimer) {
            clearTimeout((this as any)._aiFlushTimer);
            (this as any)._aiFlushTimer = null;
          }
          const list = this.data.list as any[];
          const lastIndex = list.length - 1;
          if (lastIndex >= 0 && list[lastIndex].type === MESSAGE_TYPES.PPFJ && !list[lastIndex].html) {
            this._safeSetData({ list: list.slice(0, lastIndex), loading: true });
          }
        },
        onopen: () => {
          if (this._isDestroyed || this._isDetached) return;
          if (sendSeq !== self._sendSeq) return;
          const aiMsg = { type: MESSAGE_TYPES.PPFJ, txt: '', html: '', diaryCard: [], _msgId: `${Date.now()}_${Math.random().toString(36).slice(2, 8)}` };
          const newList = this._trimOldMessages([...this.data.list, aiMsg]);
          this._safeSetData({ list: newList, scrollIntoId: 'msg-bottom' });
        },
        onmessage: (res: any) => {
          if (this._isDestroyed || this._isDetached) return;
          if (sendSeq !== self._sendSeq) return;
          const { data, done } = res;
          const safeData = data == null ? '' : String(data);
          self._aiText += safeData;

          if (!done) {
            self._aiTextDirty = true;
            this._scheduleAITextFlush();
          } else {
            if ((this as any)._aiFlushTimer) {
              clearTimeout((this as any)._aiFlushTimer);
              (this as any)._aiFlushTimer = null;
            }
            self._finalizeAIResponse();
          }
        },
        onnotice: (res: any) => {
          if (this._isDestroyed || this._isDetached) return;
          if (sendSeq !== self._sendSeq) return;
          // 服务端超时截断标记（done 前送达）：把不完整提示追加到本轮回文末尾。
          try {
            const notice = JSON.parse(res?.data || '');
            if (notice?.type === 'truncated') {
              self._aiText += (this as any).$t('aiDrawer.truncatedSuffix');
              self._aiTextDirty = true;
              this._scheduleAITextFlush();
            }
          } catch {
            // 无法解析的提示事件忽略——正文流不受影响。
          }
        },
        onclose: () => {
          if (this._isDestroyed || this._isDetached) return;
          if (sendSeq !== self._sendSeq) return;
          const list = this.data.list as any[];
          const lastIndex = list.length - 1;
          const last = list[lastIndex];
          if ((this as any)._aiFlushTimer) {
            clearTimeout((this as any)._aiFlushTimer);
            (this as any)._aiFlushTimer = null;
          }
          if (last && last.type === MESSAGE_TYPES.PPFJ && !last.html) {
            this._finalizeAIResponse();
          } else {
            (this as any)._sending = false;
            this._safeSetData({ loading: false });
          }
        },
        onerror: (err: any) => {
          if (this._isDestroyed || this._isDetached) return;
          if (sendSeq !== self._sendSeq) return;
          if ((this as any)._aiFlushTimer) {
            clearTimeout((this as any)._aiFlushTimer);
            (this as any)._aiFlushTimer = null;
          }
          if (err?.bizCode === 'AI_DAILY_QUOTA_EXCEEDED') {
            this._safeSetData({ showQuotaDialog: true });
          } else if (!this._isHidden) {
            // 页面处于隐藏态时不弹 Toast，避免切后台/系统调用返回后闪现错误提示。
            wx.showToast({ title: err?.message || (this as any).$t('aiDrawer.chatFail'), icon: 'none' });
          }
          (this as any)._sending = false;
          const list = this.data.list as any[];
          const lastIndex = list.length - 1;
          if (lastIndex >= 0 && list[lastIndex].type === MESSAGE_TYPES.PPFJ) {
            this._safeSetData({ [`list[${lastIndex}].error`]: true, loading: false });
          } else {
            this._safeSetData({ loading: false });
          }
        },
      });

      (this as any)._sseAbort = sse.abort;
      if (self._isDestroyed || self._isDetached) {
        this._abortSSE();
        this._safeSetData({ loading: false });
        return;
      }
    },

    bindfocus(e: any) {
      this._safeSetData({ bottom: e.detail.height, scrollIntoId: '' }, () => {
        this._safeSetData({ scrollIntoId: 'msg-bottom' });
      });
    },
    bindblur() {
      this._safeSetData({ bottom: 0 });
    },

    gotoDetail(e: any) {
      const info = e.currentTarget.dataset.info;
      const baseInfo = {
        id: info.id,
        coverImg: info.coverImg || '',
        dateName: info.dateName,
        recordDate: info.recordDate,
      };
      wx.navigateTo({
        url: `/pages/NoteDetail/NoteDetail?baseInfo=${encodeURIComponent(JSON.stringify(baseInfo))}`,
        fail: () => wx.showToast({ title: (this as any).$t('aiDrawer.navigateFail'), icon: 'none' }),
      });
    },

    retryLast() {
      const self = this as any;
      const prompt = self._lastPrompt;
      if (!prompt || self._sending) return;
      let list = this.data.list as any[];
      const lastIndex = list.length - 1;
      if (lastIndex >= 0 && list[lastIndex].type === MESSAGE_TYPES.PPFJ && list[lastIndex].error) {
        list = list.slice(0, -1);
      }
      // 不要把 send 放在 setData callback 中：页面隐藏时 callback 被丢弃，retry 会丢失
      //（AGENTS.md §3.4/§3.5）。先更新列表，再直接发送。
      this._safeSetData({ list });
      this.sendPrompt(prompt);
    },

    onQuotaDialogConfirm() {
      this._safeSetData({ showQuotaDialog: false });
      wx.navigateTo({ url: '/pages/sub/Vip/Vip' });
    },

    onQuotaDialogClose() {
      this._safeSetData({ showQuotaDialog: false });
    },
  },
});

import '../polyfills/textDecoder';
import { logger } from './logger';
import { i18n } from './i18n';

function _byteLength(str: string): number {
  let len = 0;
  for (let i = 0; i < str.length; i++) {
    const c = str.charCodeAt(i);
    if (c <= 0x7f) {
      len += 1;
    } else if (c <= 0x7ff) {
      len += 2;
    } else if (c >= 0xd800 && c <= 0xdbff) {
      // surrogate pair
      len += 4;
      i++;
    } else {
      len += 3;
    }
  }
  return len;
}

const _safeCallback = (fn?: Function, ...args: any[]) => {
  if (!fn) return;
  try { fn(...args); } catch (e) { logger.error('[eventSource] callback error', e); }
};

interface EventSourceParams {
  url: string;
  method?: string;
  header?: Record<string, string>;
  data?: any;
  onopen?: () => void;
  onmessage?: (data: any) => void;
  // 服务端非终止性提示事件（event: notice，如 AI 回答被超时截断）；不进入 onmessage 正文流。
  onnotice?: (data: any) => void;
  onclose?: () => void;
  onerror?: (err: Error) => void;
  // 网络类失败自动重连前回调：调用方借此清空半截输出，避免新流与旧内容拼接。
  onreconnect?: () => void;
}

export function eventSource(params: EventSourceParams) {
  const { url, method = 'POST', header = {}, data = {}, onopen, onmessage, onnotice, onclose, onerror, onreconnect } = params;
  if (!url) {
    throw new Error(i18n.t('error.urlEmpty'));
  }

  let aborted = false;
  let ended = false;
  let errorFired = false;
  let reconnectAttempted = false;
  let buffer = '';
  let requestTask: WechatMiniprogram.RequestTask;
  let decoder = new TextDecoder('utf-8');
  const MAX_BUFFER_SIZE = 64 * 1024; // 64KB，与服务端 SSE 消息体大小限制对齐

  const onHeadersReceived = (res: any) => {
    if (aborted) return;
    if (res.statusCode === 200) {
      _safeCallback(onopen);
    } else {
      errorFired = true;
      abort();
      _safeCallback(onerror, new Error(`HTTP ${res.statusCode}`));
    }
  };

  const onChunkReceived = (res: any) => {
    if (aborted) return;
    buffer += decoder.decode(res.data || new ArrayBuffer(0), { stream: true });
    if (_byteLength(buffer) > MAX_BUFFER_SIZE) {
      logger.error('[eventSource] SSE buffer exceeded max size');
      errorFired = true;
      abort();
      _safeCallback(onerror, new Error('SSE buffer exceeded max size'));
      return;
    }
    processBuffer();
  };

  function startRequest() {
    requestTask = wx.request({
      url,
      method: method as WechatMiniprogram.RequestOption['method'],
      data,
      header,
      enableChunked: true,
      responseType: 'arraybuffer',
      timeout: 200000,
      success() {
        if (aborted) return;
        ended = true;
        const tail = decoder.decode(new ArrayBuffer(0), { stream: false });
        if (tail) {
          buffer += tail;
        }
        if (buffer.trim()) processBuffer();
        // 流正常结束：末尾可能缺少空行终止符，残留 buffer 即最后一条完整消息，需按消息处理，避免丢末段。
        const remainder = buffer.trim();
        buffer = '';
        if (remainder) processMessage(remainder);
        _safeCallback(onclose);
        cleanupListeners();
      },
      fail(err: any) {
        if (aborted) return;
        // 网络类失败（断网/服务端断开/超时）做一次性自动重连：核心 AI 对话不应因
        // 一次瞬时抖动就丢弃整流；业务错误（HTTP 非 200、SSE error 事件）仍直接终止。
        if (!reconnectAttempted && !errorFired && !ended) {
          reconnectAttempted = true;
          buffer = '';
          // 重连必须换新 TextDecoder：流式解码器保留多字节半字符状态，
          // 旧流末尾的半个 UTF-8 字符会污染新流开头的解码结果（buffer 清不掉）。
          decoder = new TextDecoder('utf-8');
          logger.warn('[eventSource] transport fail, auto reconnect once', err?.errMsg || err);
          _safeCallback(onreconnect);
          startRequest();
          return;
        }
        logger.error('[eventSource] wx.request fail', err);
        errorFired = true;
        _safeCallback(onerror, new Error(err?.errMsg || i18n.t('error.networkFail')));
        abort();
      },
    });
    // 低版本基础库可能无这两个回调 API，做存在性守卫防止崩溃。
    requestTask.onHeadersReceived?.(onHeadersReceived);
    requestTask.onChunkReceived?.(onChunkReceived);
  }

  startRequest();

  function processBuffer() {

    // 兼容 CRLF 分隔（服务端若按规范输出 \r\n\r\n 也能正确切分）。
    for (;;) {
      const m = /(\r?\n){2}/.exec(buffer);
      if (!m || m.index === undefined) break;
      const raw = buffer.substring(0, m.index).trim();
      buffer = buffer.substring(m.index + m[0].length);
      processMessage(raw);
    }
  }

  function processMessage(raw: string) {
    if (!raw) return;
    const lines = raw.split('\n');
    let eventName = '';
    const dataLines: string[] = [];
    for (const line of lines) {
      const t = line.trim();
      if (t.startsWith('event:')) {
        eventName = t.substring(6).trim();
      } else if (t.startsWith('data:')) {
        // SSE 规范：data: 后可选一个前导空格应被忽略；保留其余原样内容。
        let dataValue = t.substring(5);
        if (dataValue.startsWith(' ')) {
          dataValue = dataValue.substring(1);
        }
        dataLines.push(dataValue);
      }
    }
    const dataText = dataLines.join('\n');
    if (eventName === 'error') {
      let error = new Error(dataText || i18n.t('error.aiServiceError'));
      try {
        const parsed = JSON.parse(dataText);
        // 后端 SSE error 体为 {code, biz_code, message}（ADR-0008），兼容历史 bizCode 写法。
        const bizCode = parsed?.biz_code || parsed?.bizCode;
        if (bizCode) {
          (error as any).bizCode = bizCode;
        }
        if (parsed && parsed.code) {
          (error as any).code = parsed.code;
        }
        if (parsed && parsed.message) {
          error.message = parsed.message;
        }
      } catch {
        
      }
      errorFired = true;
      _safeCallback(onerror, error);
      abort();
      return;
    }
    if (eventName === 'notice') {
      // 非终止性提示（如截断标记）：单独分发，不混入正文消息流。
      _safeCallback(onnotice, { data: dataText });
      return;
    }
    if (onmessage) {
      _safeCallback(onmessage, { data: dataText, done: eventName === 'done' });
    }
  }

  function cleanupListeners() {
    // 与注册处（onHeadersReceived?./onChunkReceived?.）一致加存在性守卫：
    // 低版本基础库无 off* 回调时直接调用会抛 TypeError，冒泡到页面 onUnload 等。
    requestTask.offHeadersReceived?.(onHeadersReceived);
    requestTask.offChunkReceived?.(onChunkReceived);
  }

  function abort() {
    if (aborted || ended) return;
    aborted = true;
    cleanupListeners();
    try {
      requestTask.abort();
    } catch (e) {
      logger.warn('[eventSource] requestTask.abort error', e);
    }
    // 外部/生命周期调用 abort() 时不会触发 success/fail 回调，需要在这里触发 onclose，
    // 让调用方（如 AIDrawer）在 SSE 被强制中止时能正确复位发送锁等状态。
    // 错误路径已设置 errorFired 并调用 onerror，避免 onclose/onerror 双发。
    if (!errorFired && onclose) {
      _safeCallback(onclose);
    }
  }

  return { abort };
}

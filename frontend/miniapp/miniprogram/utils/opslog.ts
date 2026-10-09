// 客户端操作日志：本地持久化记录用户在 App 内的关键操作事件（手动记录/自动记录等），
// 在打开小程序时批量上报到后端 /ops/client-log，用于排查“App 端操作了但服务器未收到”
// 的数据问题（如记录丢失）。detail 只保留非敏感摘要（计数/标识/错误码），不包含用户正文。
import request from './request';
import { getSessionId } from './storage';

export interface OpsEvent {
  t: string; // 客户端本地 ISO 时间
  type: string; // manual_record_start / manual_record_ok / manual_record_fail / auto_upload / auto_upload_fail / auto_entry_ok / auto_entry_fail
  detail?: Record<string, any>;
}

const STORAGE_KEY = 'ops_log_queue';
const MAX_QUEUE = 200;
const MAX_BATCH = 50;

const _readQueue = (): OpsEvent[] => {
  try {
    const v = wx.getStorageSync(STORAGE_KEY);
    return Array.isArray(v) ? (v as OpsEvent[]) : [];
  } catch {
    return [];
  }
};

const _writeQueue = (q: OpsEvent[]) => {
  try {
    wx.setStorageSync(STORAGE_KEY, q);
  } catch {
    // 本地存储失败（空间满等）时静默丢弃，不阻塞业务
  }
};

const _safeMsg = (msg: any): string | undefined =>
  typeof msg === 'string' && msg !== '' ? msg.slice(0, 200) : undefined;

/** 记录一条客户端操作事件（只保留最近 MAX_QUEUE 条） */
export const opsLog = (type: string, detail?: Record<string, any>) => {
  const q = _readQueue();
  q.push({ t: new Date().toISOString(), type, detail: detail ?? undefined });
  _writeQueue(q.slice(-MAX_QUEUE));
};

/** 记录一次请求失败（网络/超时/非 2xx 等） */
export const opsLogFail = (type: string, error: any, extra?: Record<string, any>) => {
  opsLog(type, {
    code: typeof error?.code === 'string' ? error.code : undefined,
    msg: _safeMsg(error?.message),
    ...(extra ?? {}),
  });
};

/** 批量上报本地队列；成功才清空对应批次，失败保留等待下次打开再报 */
export const flushOpsLog = async (): Promise<void> => {
  // 上报端点需要登录态：未登录（冷启动登录尚在途）时请求必然 401，徒增一条失败
  // 请求与控制台噪音；事件留在队列，由登录成功路径的 flushOpsLog 或下次启动补报。
  if (!getSessionId()) return;
  const q = _readQueue();
  if (q.length === 0) return;
  const batch = q.slice(0, MAX_BATCH);
  try {
    await request.post(
      '/ops/client-log',
      { data: { events: batch } },
      true, // closeTheErrorMessage：静默，不弹错误提示
      5000,
      true, // skipAuthExpire：上报失败不影响登录流程
    );
    _writeQueue(q.slice(batch.length));
  } catch {
    // 静默失败：保留本地队列，下次打开小程序再报
  }
};

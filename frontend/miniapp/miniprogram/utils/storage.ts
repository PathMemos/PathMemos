

const SESSION_ID_KEY = 'papafeiji:sessionId';

export const getSessionId = () => {
  try {
    return wx.getStorageSync(SESSION_ID_KEY);
  } catch {
    return '';
  }
};

export const setSessionId = (sessionId: string) => {
  try { wx.setStorageSync(SESSION_ID_KEY, sessionId); } catch {}
};

export const clearSessionId = () => {
  try { wx.removeStorageSync(SESSION_ID_KEY); } catch {}
};

const LOGGED_OUT_KEY = 'papafeiji:logged_out';

// 主动退出/注销停留标记：置位后首页不再静默登录，落在登录页等用户手动点击
// （否则注销后回到首页即静默重登新账号，邀请链路无法测试）。
// 仅 Login 页用户主动登录成功时清除（静默登录路径不清，否则停留语义失效）。
// 注意：不列入 clearUserData——标记本身要在清掉用户态后存活。
export const setLoggedOut = (loggedOut: boolean) => {
  try { wx.setStorageSync(LOGGED_OUT_KEY, loggedOut ? '1' : ''); } catch {}
};

export const getLoggedOut = () => {
  try { return wx.getStorageSync(LOGGED_OUT_KEY) === '1'; } catch { return false; }
};

export const clearUserData = () => {
  try { wx.removeStorageSync(SESSION_ID_KEY); } catch {}
  try { wx.removeStorageSync('papafeiji:baseInfo'); } catch {}
  try { wx.removeStorageSync('papafeiji:vipInfo'); } catch {}
  try { wx.removeStorageSync('papafeiji:needShowXPa'); } catch {}
  try { wx.removeStorageSync('papafeiji:autoRecordEnabled'); } catch {}
  try { wx.removeStorageSync('papafeiji:autoRecordStayPoints'); } catch {}
  try { wx.removeStorageSync('memory_longpress_guide_shown'); } catch {}
  // 客户端操作日志队列随用户态清理：注销/切换账号后旧账号事件
  // （含 login_fail）不得在新会话下批量上报造成归因错乱。
  try { wx.removeStorageSync('ops_log_queue'); } catch {}
  clearPendingLinkId();
  clearPendingInviter();
};

export const setBaseInfo = (baseInfo: any) => {
  const existing = getBaseInfo();
  const merged = { ...existing, ...baseInfo };
  for (const key of Object.keys(baseInfo)) {
    if (baseInfo[key] && typeof baseInfo[key] === 'object' && !Array.isArray(baseInfo[key]) && existing && existing[key] && typeof existing[key] === 'object') {
      merged[key] = { ...existing[key], ...baseInfo[key] };
    }
  }
  try { wx.setStorageSync('papafeiji:baseInfo', JSON.stringify(merged)); } catch {}
};

export const getBaseInfo = () => {
  let baseInfo = '';
  try {
    baseInfo = wx.getStorageSync('papafeiji:baseInfo');
  } catch {
    return null;
  }
  if (!baseInfo) return null;
  try {
    return JSON.parse(baseInfo);
  } catch {
    try { wx.removeStorageSync('papafeiji:baseInfo'); } catch {}
    return null;
  }
};

export const getAvatar = () => {
  const { avatar } = getBaseInfo() || {};
  return avatar;
};

export const needShowXPa = () => {
  try {
    return wx.getStorageSync('papafeiji:needShowXPa');
  } catch {
    return false;
  }
};

export const setNeedShowXPa = (needShowXPa: boolean) => {
  try { wx.setStorageSync('papafeiji:needShowXPa', needShowXPa); } catch {}
};

export const STORAGE_KEY_MODE = 'backend_mode';
export const STORAGE_KEY_URL = 'private_backend_url';
export const STORAGE_KEY_API_KEY = 'private_backend_api_key';

export const getBackendMode = (): string => {
  try {
    return wx.getStorageSync(STORAGE_KEY_MODE) || 'saas';
  } catch {
    return 'saas';
  }
};

export const getPrivateBackendUrl = (): string => {
  try {
    return wx.getStorageSync(STORAGE_KEY_URL) || '';
  } catch {
    return '';
  }
};

export const getPrivateBackendApiKey = (): string => {
  try {
    return wx.getStorageSync(STORAGE_KEY_API_KEY) || '';
  } catch {
    return '';
  }
};

const PENDING_LINK_KEY = 'pendingLinkId';


const PENDING_LINK_MAX_AGE_MS = 7 * 24 * 3600 * 1000;

export const getPendingLinkId = (): string | null => {
  try {
    const raw = wx.getStorageSync(PENDING_LINK_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    // 校验有效期：残留的旧 linkId 会让每次进入家庭页都自动发起已失效的邀请请求。
    if (parsed.ts && Date.now() - parsed.ts > PENDING_LINK_MAX_AGE_MS) {
      try { wx.removeStorageSync(PENDING_LINK_KEY); } catch {}
      return null;
    }
    return parsed.value || null;
  } catch {
    try { wx.removeStorageSync(PENDING_LINK_KEY); } catch {}
    return null;
  }
};

export const setPendingLinkId = (linkId: string) => {
  try { wx.setStorageSync(PENDING_LINK_KEY, JSON.stringify({ value: linkId, ts: Date.now() })); } catch {}
};

export const clearPendingLinkId = () => {
  try { wx.removeStorageSync(PENDING_LINK_KEY); } catch {}
};

const PENDING_INVITER_KEY = 'pendingInviter';
const PENDING_INVITER_MAX_AGE_MS = 7 * 24 * 3600 * 1000;


export const getPendingInviter = (): string | null => {
  try {
    const raw = wx.getStorageSync(PENDING_INVITER_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    // 校验有效期（与 pendingLinkId 同构）：残留的旧 inviter 会让任意久之后
    // 的手动登录都绑定当年那次扫描的邀请人，奖励归属错乱。
    if (parsed.ts && Date.now() - parsed.ts > PENDING_INVITER_MAX_AGE_MS) {
      try { wx.removeStorageSync(PENDING_INVITER_KEY); } catch {}
      return null;
    }
    return parsed.value || null;
  } catch {
    try { wx.removeStorageSync(PENDING_INVITER_KEY); } catch {}
    return null;
  }
};

export const setPendingInviter = (inviter: string) => {
  try { wx.setStorageSync(PENDING_INVITER_KEY, JSON.stringify({ value: inviter, ts: Date.now() })); } catch {}
};

export const clearPendingInviter = () => {
  try { wx.removeStorageSync(PENDING_INVITER_KEY); } catch {}
};

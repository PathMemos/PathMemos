import { get, post } from './http';
import { NEW_USER_FREE_VIP_ID } from '../config/index';
import { logger } from './logger';


export const formatVipInfo = (vipInfo: any) => {
  if (!vipInfo) return vipInfo;
  return {
    ...vipInfo,
    vipExpireTime: vipInfo.vipExpireTime ? vipInfo.vipExpireTime.substring(0, 10) : '',
  };
};

let _fetchVipPromise: Promise<any> | null = null;
const VIP_CACHE_TTL_MS = 60 * 1000;

let _vipInfoCache: any = null;
let _vipInfoCacheAt = 0;
let _vipGeneration = 0;

// 退出/注销/切后端时调用：内存缓存、在途请求与 storage 副本随用户态一起清理——
// 否则 60 秒 TTL 内换账号会命中旧账号的 isVip/到期时间（VIP 门禁被旧状态放行），
// 在途请求的写回也会把旧账号数据带进新账号视角。
export const resetVipCache = () => {
  _vipGeneration += 1;
  _fetchVipPromise = null;
  _vipInfoCache = null;
  _vipInfoCacheAt = 0;
  try {
    wx.removeStorageSync('papafeiji:vipInfo');
  } catch {
  }
};

const _setVipCache = (info: any) => {
  _vipInfoCache = info;
  _vipInfoCacheAt = Date.now();
  try {
    wx.setStorageSync('papafeiji:vipInfo', JSON.stringify(info));
  } catch {
  }
};

export const fetchVipInfo = async () => {
  
  if (_fetchVipPromise) {
    return _fetchVipPromise;
  }

  _fetchVipPromise = (async () => {
    const gen = _vipGeneration;
    // 两个查询无依赖，并行以省一个串行 RTT（处于启动/回前台关键路径）；
    // free/check 失败不阻断（receivedFreeVip 降级 false），/user/vip 失败整体上抛（语义同前）。
    const [vipRes, freeRes] = await Promise.all([
      get('/user/vip', {}, true),
      get('/vip/free/check', { params: { vipId: NEW_USER_FREE_VIP_ID } }, true).catch((e: any) => {
        logger.error('查询免费 VIP 领取状态失败', e);
        return null;
      }),
    ]);
    const vipInfo = vipRes?.data;
    const info = {
      receivedFreeVip: !!((freeRes as any)?.data?.claimed),
      isVip: vipInfo?.isVip ?? 0,
      vipExpireTime: vipInfo?.expireTime ?? '',
      _fetchTime: Date.now(),
    };
    // 用户态已切换（resetVipCache）：丢弃旧账号结果，不写缓存与 storage。
    if (gen === _vipGeneration) {
      _setVipCache(info);
    }
    return info;
  })();

  try {
    return await _fetchVipPromise;
  } finally {
    _fetchVipPromise = null;
  }
};

export const claimNewUserFreeVip = async () => {
  try {
    await post('/vip/new-user', {}, true);
    
    await fetchVipInfo();
  } catch (error: any) {
    const code = error?.data?.biz_code || '';
    if (code === 'TRIAL_VIP_ALREADY_CLAIMED') {
      await fetchVipInfo();
      return;
    }
    logger.error('领取新用户免费 VIP 失败', error);
    throw error;
  }
};

export const getVipInfo = () => {
  const now = Date.now();
  if (_vipInfoCache && (now - _vipInfoCacheAt) < VIP_CACHE_TTL_MS) {
    return _vipInfoCache;
  }
  const vipInfo = wx.getStorageSync('papafeiji:vipInfo');
  try {
    _vipInfoCache = JSON.parse(vipInfo);
    _vipInfoCacheAt = now;
    return _vipInfoCache;
  } catch {
    wx.removeStorageSync('papafeiji:vipInfo');
    _vipInfoCache = null;
    _vipInfoCacheAt = 0;
    return null;
  }
};

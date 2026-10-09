import request, { createCancelToken } from './request';
import { getVipInfo, fetchVipInfo } from './vip';
import { isIOS } from './util';
import { logger } from './logger';
import { opsLog, opsLogFail, flushOpsLog } from './opslog';
import { isAppEnv } from './appPermission';
import i18n from './i18n';


const CENTROID_CALC_INTERVAL_MS = 30_000;     
const BACKGROUND_CENTROID_CALC_INTERVAL_MS = 120_000; // 后台降低计算频率，省电
const CENTROID_WINDOW_TIME_MS = 60_000;       
// 后台定位回调频率低且不稳定（Android 微信静止时 1-5 分钟/次），60 秒窗口凑不齐
// CENTROID_MIN_POINTS 个点导致后台驻留检测失效；后台窗口放宽到 10 分钟，
// 且后台最少点数降为 2（两点质心即可，配合 300m 静止阈值仍可靠）。
const BACKGROUND_CENTROID_WINDOW_TIME_MS = 600_000;
const CENTROID_MIN_POINTS = 3;                
const BACKGROUND_CENTROID_MIN_POINTS = 2;
const CENTROID_MAX_POINTS = 200;              
const BACKGROUND_CENTROID_MAX_POINTS = 50;    // 后台保留更少点，降低内存与计算
const STATIONARY_THRESHOLD_METERS = 300;
const STATIONARY_THRESHOLD_METERS_SQ = STATIONARY_THRESHOLD_METERS * STATIONARY_THRESHOLD_METERS;
const STATIONARY_EXIT_CONFIRM_COUNT = 2;
const BACKGROUND_STATIONARY_CHECK_INTERVAL_MS = 60_000; // 后台已保存驻留点后，每分钟检查是否离开
const STAY_POINT_MATURE_MS = 10 * 60 * 1000;  

const STORAGE_MAX_SIZE = 50;                  
const COORD_PRECISION = 4;                    
const getAccuracyFilter = (): number => isIOS() ? 3000 : 500;  


const VIP_CHECK_INTERVAL_MS = 5 * 60 * 1000; // VIP 状态前台运行时 5 分钟缓存，及时反映订阅/退订变化

const IOS_RESUME_GET_LOCATION_DELAY_MS = 300;
const FALLBACK_INTERVAL_MS = 300_000; // 后台系统定位回调丢失时，5 分钟兜底调 getLocation，避免静止用户误告警

interface LocationPoint {
  latitude: number;
  longitude: number;
  accuracy: number;
  timestamp: number;
}

interface StayPoint {
  lat: number;
  lon: number;
  recordedAt: string;
}

const STORAGE_KEY_ENABLED = 'papafeiji:autoRecordEnabled';      
export { STORAGE_KEY_ENABLED };
const STORAGE_KEY_STAY_POINTS = 'papafeiji:autoRecordStayPoints'; 


let _window: LocationPoint[] = [];
let _lastCentroidCalcTime = 0;
let _state: 'stationary' | 'moving' = 'moving';
let _stationaryStartTime: number | null = null;
let _stationarySaved = false;
let _stableCentroid: { lat: number; lon: number } | null = null;
let _exitConfirmCount = 0;
let _lastStationaryCheckTime = 0; // 后台静止已保存时，控制离开检测频率
let _stayPointQueue: StayPoint[] = [];


let _enabledInMemory: boolean | null = null;


let _lastVipCheckTime = 0;
// 三态：null=尚无判定（未知态，禁止关闭自动记录）；true/false=已确认。
let _lastVipResult: boolean | null = null;
// 未知态 fetch 节流时间戳（与判定缓存 _lastVipCheckTime 分离，避免未知被当非 VIP）。
let _lastVipFetchAt = 0;


let _opening = false;
let _restoring = false;
let _closingGeneration = 0;
let _lifecycleCancelToken: any = null;
let _activeCancelToken: any = null;
let _justFromSettings = false;


let _appBackgroundedAt = 0;


let _reportBatchSeq = 0;
let _lastSystemCallbackAt = 0;
let _reportFailureCount = 0;
let _lastReportFailureTime = 0;


const ACTIVE_TOUCH_INTERVAL_MS = 30 * 60 * 1000;
let _lastActiveTouchTime = 0;
let _fallbackTimer: any = null;
let _fallbackPending = false;
let _iosResumeTimer: any = null;

// 定位监听注册状态（进程内存态）：小程序被杀后台后重新进入时该值为 false，
// 用于判断是否需要重建 startLocationUpdateBackground + onLocationChange。
let _listenerRegistered = false;

const _locationListening = (): boolean => _listenerRegistered;


const _nextReportBatchSeq = (): number => {
  _reportBatchSeq++;
  return _reportBatchSeq;
};


const _roundCoord = (v: number): number => {
  return Math.round(v * Math.pow(10, COORD_PRECISION)) / Math.pow(10, COORD_PRECISION);
};


// 返回平方距离（米²），避免每次比较都调用 Math.sqrt
const _distanceMetersSq = (lat1: number, lon1: number, lat2: number, lon2: number): number => {
  const avgLat = ((lat1 + lat2) * 0.5) * Math.PI / 180;
  const dLat = lat2 - lat1;
  const dLon = lon2 - lon1;
  const metersPerDegLat = 111320;
  const metersPerDegLon = 111320 * Math.cos(avgLat);
  return (dLat * metersPerDegLat) ** 2 + (dLon * metersPerDegLon) ** 2;
};


const _calcCentroid = (points: LocationPoint[]): { lat: number; lon: number } => {
  let latSum = 0;
  let lonSum = 0;
  for (const p of points) {
    latSum += p.latitude;
    lonSum += p.longitude;
  }
  return {
    lat: latSum / points.length,
    lon: lonSum / points.length,
  };
};


const _isBackground = (): boolean => _appBackgroundedAt > 0;

const _getCentroidCalcInterval = (): number =>
  _isBackground() ? BACKGROUND_CENTROID_CALC_INTERVAL_MS : CENTROID_CALC_INTERVAL_MS;

const _getCentroidWindowTime = (): number =>
  _isBackground() ? BACKGROUND_CENTROID_WINDOW_TIME_MS : CENTROID_WINDOW_TIME_MS;

const _getCentroidMinPoints = (): number =>
  _isBackground() ? BACKGROUND_CENTROID_MIN_POINTS : CENTROID_MIN_POINTS;

const _getCentroidMaxPoints = (): number =>
  _isBackground() ? BACKGROUND_CENTROID_MAX_POINTS : CENTROID_MAX_POINTS;

const _updateVipCache = (isVip: boolean) => {
  _lastVipResult = isVip;
  _lastVipCheckTime = Date.now();
};

// 返回三态：true=确认 VIP、false=确认非 VIP、null=未知（VIP 信息尚未获取，
// 如 storage 被微信清理/首次运行）。未知时禁止关闭自动记录，避免误杀。
const _isVipFromCache = (): boolean | null => {
  if (_lastVipResult !== null && _lastVipCheckTime > 0 && Date.now() - _lastVipCheckTime < VIP_CHECK_INTERVAL_MS) {
    return _lastVipResult;
  }
  const vipInfo = getVipInfo();
  if (!vipInfo) {
    // 未知态：不写 _lastVipCheckTime / _lastVipResult，保持 null 让调用方跳过；
    // fetch 节流由调用方用独立的 _lastVipFetchAt 控制。
    return null;
  }
  const isVip = vipInfo.isVip > 0;
  _updateVipCache(isVip);
  return isVip;
};

const _pushStayPoint = (lat: number, lon: number, timestamp: number) => {
  const point: StayPoint = {
    lat: _roundCoord(lat),
    lon: _roundCoord(lon),
    recordedAt: new Date(timestamp).toISOString(),
  };
  _stayPointQueue.push(point);
  
  _serialFlushAndReport().catch(() => {});
};


let _flushPromise: Promise<void> | null = null;

let _reportPromise: Promise<void> | null = null;
// _reporting 单飞标志：restore/close 路径会直接调用 _tryReportStorage，与 push 路径的
// _serialFlushAndReport 可能并发；用进程内标志短路，避免两个上报循环同时 load/report/clear。
let _reporting = false;

const _shouldSkipReportDueToBackoff = (): boolean => {
  if (_reportFailureCount === 0) return false;
  // 指数退避：30s、60s、120s、240s... 最大 5 分钟
  const backoffMs = Math.min(30_000 * Math.pow(2, _reportFailureCount - 1), 5 * 60 * 1000);
  return Date.now() - _lastReportFailureTime < backoffMs;
};

const _serialFlushAndReport = async (cancelToken?: any, force = false): Promise<void> => {
  // 已有上报在进行时，不能直接返回旧 promise——期间新入队的点会被排除在本轮之外。
  // 先等旧一轮结束，再用最新 storage 重跑一轮（force=true 时忽略退避，用于关闭前的最后一批）。
  if (_reportPromise) {
    try { await _reportPromise; } catch {}
  }
  _reportPromise = (async () => {
    await _flushToStorage();
    if (!force && _shouldSkipReportDueToBackoff()) {
      return;
    }
    try {
      await _tryReportStorage(cancelToken);
  } catch {
    }
  })();
  try {
    await _reportPromise;
  } finally {
    _reportPromise = null;
  }
};


const _flushToStorage = async (): Promise<void> => {
  if (_flushPromise) {
    return _flushPromise;
  }
  if (_stayPointQueue.length === 0) {
    return;
  }
  _flushPromise = (async () => {
    let pointsToFlush: StayPoint[] = [];
    try {
      pointsToFlush = _stayPointQueue.splice(0);
      const existing = await _loadFromStorage(true);
      const combined = existing.concat(pointsToFlush);
      if (combined.length > STORAGE_MAX_SIZE) {
        const dropped = combined.length - STORAGE_MAX_SIZE;
        combined.splice(0, dropped);
        // 头部淘汰的是最旧的点；同步前移「已上报前缀」计数，
        // 否则计数大于实际已上报前缀，_tryReportStorage 会跳过未上报的新点并最终误清。
        _reportedLeadingCount = Math.max(0, _reportedLeadingCount - dropped);
      }
      await new Promise<void>((resolve, reject) => {
        wx.setStorage({
          key: STORAGE_KEY_STAY_POINTS,
          data: combined,
          success: () => resolve(),
          fail: reject,
        });
      });
    } catch {
      // 写存储失败时，把尚未持久化的点放回队列头部，避免静默丢失
      if (pointsToFlush.length > 0) {
        _stayPointQueue.unshift(...pointsToFlush);
      }
    }
  })();
  try {
    await _flushPromise;
  } finally {
    _flushPromise = null;
  }
};


const _loadFromStorage = async (throwOnError = false): Promise<StayPoint[]> => {
  try {
    return await new Promise<StayPoint[]>((resolve, reject) => {
      wx.getStorage({
        key: STORAGE_KEY_STAY_POINTS,
        success: (res) => resolve((res.data as StayPoint[]) || []),
        fail: (err) => {
          // 只有"数据不存在"才视为空；其他读取错误需要让上层感知，避免用空数据覆盖已有数据
          if (err?.errMsg?.includes('data not found')) {
            resolve([]);
          } else if (throwOnError) {
            reject(err);
          } else {
            resolve([]);
          }
        },
      });
    });
  } catch (e) {
    if (throwOnError) throw e;
    return [];
  }
};


const _isRecordingEnabled = (): boolean => {
  if (_enabledInMemory != null) return _enabledInMemory;
  try {
    _enabledInMemory = !!wx.getStorageSync(STORAGE_KEY_ENABLED);
    return _enabledInMemory;
  } catch {
    return false;
  }
};


const _setRecordingState = (enabled: boolean) => {
  _enabledInMemory = enabled;
  try {
    wx.setStorageSync(STORAGE_KEY_ENABLED, enabled);
  } catch {
  }
  try {
    const app = getApp() as any;
    if (app && app.globalData) {
      app.globalData.openAutoRecorded = enabled;
    }
  } catch {
    
  }
};


const _syncBackendConfig = async (enabled: boolean, cancelToken?: any) => {
  await request.put('/auto-record/config', { data: { enabled }, cancelToken }, true, 5000);
};


const _isTokenExpiredError = (error: any): boolean => {
  // 03-api §3：会话失效 = HTTP 401 + code '4010' + biz_code SESSION_INVALID；
  // skipAuthExpire 请求不走哨兵（http.ts 不清 session），哨兵分支仅覆盖未跳过的调用方。
  const code = error?.data?.code || '';
  const bizCode = error?.data?.biz_code || '';
  return code === '4010' || bizCode === 'SESSION_INVALID' ||
         error?.message === '__ppfj_session_expired__';
};


const _stopLocationUpdateBackground = () => {
  // 优先调用 stopLocationUpdateBackground 停止由 startLocationUpdateBackground 启动的后台监听；
  // 部分基础库可能不存在该 API，此时回退到 stopLocationUpdate。
  try {
    if (typeof (wx as any).stopLocationUpdateBackground === 'function') {
      (wx as any).stopLocationUpdateBackground();
    }
  } catch {}
  if (typeof wx.stopLocationUpdate === 'function') {
    wx.stopLocationUpdate();
  }
  _listenerRegistered = false;
};


const _postTrajectoriesWithSilentRefresh = async (data: any, cancelToken?: any): Promise<any> => {
  opsLog('auto_upload', { count: Array.isArray(data?.points) ? data.points.length : 0 });
  try {
    return await request.post('/auto-record/trajectories', { data, cancelToken }, true, 10000, true);
  } catch (error: any) {
    if (!_isTokenExpiredError(error)) {
      opsLogFail('auto_upload_fail', error, { count: Array.isArray(data?.points) ? data.points.length : 0 });
      throw error;
    }
    await request.login(cancelToken);
    try {
      return await request.post('/auto-record/trajectories', { data, cancelToken }, true, 10000, true);
    } catch (retryError: any) {
      opsLogFail('auto_upload_fail', retryError, { count: Array.isArray(data?.points) ? data.points.length : 0, retried: true });
      throw retryError;
    }
  }
};


const _startFallbackTimer = () => {
  if (!_isRecordingEnabled() || _fallbackTimer != null) return;
  const intervalMs = FALLBACK_INTERVAL_MS;
  const tick = () => {
    if (!_isRecordingEnabled()) return;
    if (!_isBackground()) return;
    if (_fallbackPending) return;
    let desired = intervalMs;
    if (_stationarySaved && _state === 'stationary') {
      const stationaryMs = Date.now() - (_stationaryStartTime || 0);
      desired = stationaryMs > 30 * 60 * 1000 ? 5 * 60 * 1000 : 60_000;
    }
    if (Date.now() - _lastSystemCallbackAt < desired - 5000) {
      return;
    }
    _fallbackPending = true;
    _getCurrentLocation(LOCATION_TIMEOUT_MS)
      .then((res: any) => {
        _fallbackPending = false;
        res.__fromPlatformFallback = true;
        onLocationChange(res);
      })
      .catch(() => {
        _fallbackPending = false;
      });
  };
  _fallbackTimer = setInterval(tick, intervalMs);
};

const _restartFallbackTimer = () => {
  _stopFallbackTimer();
  if (_isRecordingEnabled()) {
    _startFallbackTimer();
  }
};

const _stopFallbackTimer = () => {
  if (_fallbackTimer != null) {
    clearInterval(_fallbackTimer);
    _fallbackTimer = null;
  }
  _fallbackPending = false;
};


const _reportStayPoints = async (points: StayPoint[], cancelToken?: any): Promise<boolean> => {
  if (points.length === 0) return true;
  const batchSeq = _nextReportBatchSeq();
  try {
    const payload: any = { points, batchSeq };
    await _postTrajectoriesWithSilentRefresh(payload, cancelToken);
    _reportFailureCount = 0;
    _lastReportFailureTime = 0;
    return true;
  } catch (error: any) {
    _reportFailureCount++;
    _lastReportFailureTime = Date.now();
    if (_isTokenExpiredError(error)) {
      // token 过期后停止监听，但保留本地 storage 开关；安排退避重试，
      // 重新登录成功后 tryRestoreAutoRecord 自动恢复（AGENTS.md §3.7）。
      _resetState(true);
      _scheduleRestoreRetry();
      return false;
    }
    return false;
  }
};


// _reportedLeadingCount 记录 storage 中"已上报但清理失败"的前缀数量：
// 上报成功而清空失败时，同批数据仍留在 storage，退避重试若整批重报会造成后端重复轨迹
// （后端无按批去重约束）。按数量跳过已上报前缀，只重试清空。
let _reportedLeadingCount = 0;

const _tryReportStorage = async (cancelToken?: any): Promise<boolean> => {
  if (_reporting) return true;
  _reporting = true;
  try {
    const points = await _loadFromStorage();
    if (points.length === 0) return true;
    const toReport = points.slice(Math.min(_reportedLeadingCount, points.length));
    if (toReport.length === 0) {
      // 全部已上报：只重试清空 storage，不再重报。
      return _clearReportedStorage();
    }
    const ok = await _reportStayPoints(toReport, cancelToken);
    if (!ok) return false;
    try {
      await new Promise<void>((resolve, reject) => {
        wx.setStorage({ key: STORAGE_KEY_STAY_POINTS, data: [], success: () => resolve(), fail: reject });
      });
      _reportedLeadingCount = 0;
    } catch (e) {
      logger.error('autoRecord clear storage failed after report', e);
      // 本批已成功上报：标记为已上报前缀，后续只清空不再重报。
      _reportedLeadingCount = points.length;
      _reportFailureCount++;
      _lastReportFailureTime = Date.now();
    }
    return true;
  } finally {
    _reporting = false;
  }
};

const _clearReportedStorage = async (): Promise<boolean> => {
  try {
    await new Promise<void>((resolve, reject) => {
      wx.setStorage({ key: STORAGE_KEY_STAY_POINTS, data: [], success: () => resolve(), fail: reject });
    });
    _reportedLeadingCount = 0;
  } catch (e) {
    logger.error('autoRecord clear reported storage failed', e);
  }
  return true;
};

const LOCATION_TIMEOUT_MS = 15000;

interface _GetCurrentLocationOptions {
  highAccuracyExpireTime?: number;
  highAccuracy?: boolean;
}

const _getCurrentLocation = (
  timeoutMs: number = LOCATION_TIMEOUT_MS,
  opts: _GetCurrentLocationOptions = {}
): Promise<{ latitude: number; longitude: number }> => {
  return new Promise((resolve, reject) => {
    let settled = false;
    const timeoutTimer = setTimeout(() => {
      if (settled) return;
      settled = true;
      opsLog('location_diag', { step: 'get-fail', errMsg: 'getLocation: timeout' });
      opsLogFail('location_get_fail', { errMsg: 'getLocation: timeout' });
      void flushOpsLog();
      reject({ errMsg: 'getLocation: timeout' });
    }, timeoutMs);
    wx.getLocation({
      type: 'gcj02',
      isHighAccuracy: opts.highAccuracy ?? true,
      highAccuracyExpireTime: opts.highAccuracyExpireTime ?? 5000,
      success: (res) => {
        if (settled) return;
        settled = true;
        clearTimeout(timeoutTimer);
        resolve(res);
      },
      fail: (err) => {
        if (settled) return;
        settled = true;
        clearTimeout(timeoutTimer);
        const we = err as any;
        opsLog('location_diag', { step: 'get-fail', errCode: we?.errCode, errMsg: we?.errMsg });
        opsLogFail('location_get_fail', err);
        void flushOpsLog();
        reject(err);
      },
    });
  });
};


const _saveFirstRecord = async (): Promise<void> => {
  try {
    const loc = await _getCurrentLocation(10000, { highAccuracy: false, highAccuracyExpireTime: 2000 });
    const res: any = await request.post('/diary/details/auto', {
      data: {
        lat: _roundCoord(loc.latitude),
        lon: _roundCoord(loc.longitude),
      },
    }, true, 10000, true);
    opsLog('auto_entry_ok', { hasId: !!res?.data?.id });
    if (res?.data?.id) {
      _refreshIndexList();
    }
  } catch (error: any) {
    opsLogFail('auto_entry_fail', error);
    throw error;
  }
};


const _refreshIndexList = () => {
  const app = getApp() as any;
  if (app && app.globalData) {
    app.globalData._needRefreshIndexList = true;
  }
};


export const onLocationChange = (res: any) => {
  const now = Date.now();
  if (!res.__fromPlatformFallback) {
    _lastSystemCallbackAt = now;
  }

  try {
    if (!_isRecordingEnabled()) {
      return;
    }

    // 心跳：每 30 分钟上报一次存活信号，跟随 onLocationChange 触发，不增加独立定时器耗电。
    // 用于小程序订阅消息和服务号消息的异常告警检测。
    // 注意：心跳必须放在 VIP 检查之前——VIP 缓存缺失/查询失败时仍要报活，
    // 否则网络抖动期间后端误判"用户失联"触发异常告警。
    if (now - _lastActiveTouchTime >= ACTIVE_TOUCH_INTERVAL_MS) {
      _lastActiveTouchTime = now;
      _touchActive();
    }

    const vipState = _isVipFromCache();
    if (vipState === null) {
      // VIP 状态未知（缓存缺失）：节流异步刷新确认，本次回调跳过，不关闭自动记录。
      // 避免 storage 被清理后把用户已开启的自动记录误杀；fetch 失败时保持未知，绝不回落为非 VIP。
      const now = Date.now();
      if (now - _lastVipFetchAt >= VIP_CHECK_INTERVAL_MS) {
        _lastVipFetchAt = now;
        fetchVipInfo().then((info) => {
          _updateVipCache(info?.isVip > 0);
          if (!(info?.isVip > 0)) {
            closeAutoRecord().catch(() => {});
          }
        }).catch(() => {});
      }
      return;
    }
    if (!vipState) {
      closeAutoRecord().catch(() => {});
      return;
    }

    const accuracy = typeof res.accuracy === 'number' ? res.accuracy : 0;
    if (accuracy > getAccuracyFilter()) {
      return;
    }

    const lat = res.latitude;
    const lon = res.longitude;
    if (lat == null || lon == null) {
      return;
    }

    const rawTs = res.timestamp;
    const pointTimestampRaw = typeof rawTs === 'number' && rawTs > 0
      ? rawTs
      : rawTs
        ? new Date(rawTs).getTime()
        : Date.now();
    const pointTimestamp = Number.isFinite(pointTimestampRaw) ? pointTimestampRaw : Date.now();

    const isBackgroundStationarySaved =
      _isBackground() && _state === 'stationary' && _stationarySaved && _stableCentroid != null;

    // 后台且已保存驻留点的静止状态：不维护完整窗口，只保留最后一个点用于离开检测
    if (!isBackgroundStationarySaved) {
      _window.push({ latitude: lat, longitude: lon, accuracy, timestamp: pointTimestamp });

      const cutoff = pointTimestamp - _getCentroidWindowTime();
      while (_window.length > 0 && _window[0].timestamp < cutoff) {
        _window.shift();
      }

      const maxPoints = _getCentroidMaxPoints();
      while (_window.length > maxPoints) {
        _window.shift();
      }
    }

    // 后台且已保存驻留点的静止状态：只做轻量离开检测，大幅降低计算
    const stableCentroid = _stableCentroid;
    if (isBackgroundStationarySaved && stableCentroid) {
      if (now - _lastStationaryCheckTime < BACKGROUND_STATIONARY_CHECK_INTERVAL_MS) {
        return;
      }
      _lastStationaryCheckTime = now;
      const distSq = _distanceMetersSq(lat, lon, stableCentroid.lat, stableCentroid.lon);
      if (distSq < STATIONARY_THRESHOLD_METERS_SQ) {
        _exitConfirmCount = 0;
        return;
      }
      _exitConfirmCount++;
      if (_exitConfirmCount < STATIONARY_EXIT_CONFIRM_COUNT) {
        return;
      }
      _state = 'moving';
      _stationaryStartTime = null;
      _stationarySaved = false;
      _stableCentroid = { lat, lon };
      _exitConfirmCount = 0;
      // 恢复窗口计算，把当前离开点作为窗口起点，避免从头积累
      _window.length = 0;
      _window.push({ latitude: lat, longitude: lon, accuracy, timestamp: pointTimestamp });
      _lastCentroidCalcTime = now;
      return;
    }

    if (now - _lastCentroidCalcTime < _getCentroidCalcInterval() || _window.length < _getCentroidMinPoints()) {
      return;
    }

    _lastCentroidCalcTime = now;
    const centroid = _calcCentroid(_window);
    const stable = _stableCentroid || centroid;
    const distSq = _distanceMetersSq(centroid.lat, centroid.lon, stable.lat, stable.lon);
    const isStationary = distSq < STATIONARY_THRESHOLD_METERS_SQ;

    if (isStationary) {
      _exitConfirmCount = 0;
      if (_state === 'moving') {
        _state = 'stationary';
        _stationaryStartTime = now;
        _stationarySaved = false;
        _stableCentroid = centroid;
      }
      if (_stationaryStartTime == null) {
        _stationaryStartTime = now;
      }
      if (_stableCentroid == null) {
        _stableCentroid = centroid;
      }

      const stationaryDuration = now - _stationaryStartTime;
      if (!_stationarySaved && stationaryDuration >= STAY_POINT_MATURE_MS) {
        _pushStayPoint(_stableCentroid.lat, _stableCentroid.lon, now);
        _stationarySaved = true;
      }
    } else {
      if (_state === 'stationary') {
        _exitConfirmCount++;
        if (_exitConfirmCount < STATIONARY_EXIT_CONFIRM_COUNT) {
          return;
        }
        _state = 'moving';
        _stationaryStartTime = null;
        _stationarySaved = false;
        _stableCentroid = centroid;
        _exitConfirmCount = 0;
      } else {
        
        _stableCentroid = centroid;
      }
    }

  } catch (e) {
    logger.error('autoRecord onLocationChange error', e);
  }
};


const _iosResumeGetLocation = () => {
  if (!isIOS() || !_isRecordingEnabled()) return;
  if (_iosResumeTimer) clearTimeout(_iosResumeTimer);
  _iosResumeTimer = setTimeout(() => {
    _iosResumeTimer = null;
    if (_fallbackPending) return;
    _fallbackPending = true;
    _getCurrentLocation(LOCATION_TIMEOUT_MS, { highAccuracyExpireTime: 4000 })
      .then((res) => {
        _fallbackPending = false;
        onLocationChange(res);
      })
      .catch(() => {
        _fallbackPending = false;
      });
  }, IOS_RESUME_GET_LOCATION_DELAY_MS);
};


const _startLocationUpdate = (): Promise<void> => {
  return new Promise((resolve, reject) => {
    wx.startLocationUpdateBackground({
      success: () => {
        wx.offLocationChange(onLocationChange);
        wx.onLocationChange(onLocationChange);
        _listenerRegistered = true;
        _startFallbackTimer();
        resolve();
      },
      fail: (err) => {
        // 多端 App 定位排障：失败原因（隐私未同意/权限拒绝/系统服务关闭）落到
        // client_ops_logs，服务端可直接查询。
        const we = err as any;
        opsLog('location_diag', { step: 'start-background-fail', errCode: we?.errCode, errMsg: we?.errMsg });
        opsLogFail('location_start_fail', err);
        void flushOpsLog();
        reject(err);
      },
    });
  });
};

// 多端 App 的系统定位授权前置。
// 多端 App 脱离微信权限体系：wx.authorize/getSetting/openSetting 均不可用，
// 系统权限由框架在「调用对应能力的接口」时向 Android/iOS 申请。
// 关键：Android 10+ 不允许一步到位申请后台定位——未获前台定位授权时直接
// startLocationUpdateBackground 会被系统静默拒绝。因此 App 环境开启自动记录前
// 先跑一次前台 wx.getLocation 主动触发系统授权弹窗。
// 注意：不做 getAppAuthorizeSetting 前置决策查询——实测该 API 在部分
// 真机上既不回调 success 也不回调 fail（悬挂），不能作为流程门卫；仅独立记录
// 其结果用于诊断，无论结果如何都继续走前台定位。
const _ensureAppLocationAuthorized = async (): Promise<void> => {
  const appEnv = !!(wx as any).miniapp;
  opsLog('location_diag', { step: 'ensure-begin', appEnv });
  if (!appEnv) return; // 小程序环境走微信 scope 授权体系
  try {
    (wx as any).getAppAuthorizeSetting({
      success: (res: any) => {
        opsLog('location_diag', { step: 'auth-state', authorized: res?.locationAuthorized });
        void flushOpsLog();
      },
      fail: (err: any) => {
        opsLog('location_diag', { step: 'getAppAuthorizeSetting-fail', errMsg: (err as any)?.errMsg });
      },
    });
  } catch (e: any) {
    opsLog('location_diag', { step: 'getAppAuthorizeSetting-throw', errMsg: String(e?.message || e).slice(0, 100) });
  }
  // 未授权时系统弹窗在此触发，已授权时无感通过；失败（含用户 denied）直接抛出——
  // _doOpenAutoRecord 捕获后中止开启并反向关闭后端开关（02c AR-1.1）。
  try {
    await _getCurrentLocation(LOCATION_TIMEOUT_MS, { highAccuracy: false, highAccuracyExpireTime: 2000 });
    opsLog('location_diag', { step: 'foreground-getLocation-ok' });
  } catch (e: any) {
    opsLog('location_diag', { step: 'foreground-getLocation-fail', errMsg: (e as any)?.errMsg });
    void flushOpsLog();
    throw e;
  }
};


const _doOpenAutoRecord = async (onDone: (ok: boolean, err?: any) => void, startGeneration: number) => {
  if (_lifecycleCancelToken) {
    try { _lifecycleCancelToken.cancel(); } catch {}
  }
  const token = createCancelToken();
  _lifecycleCancelToken = token;
  const _checkAborted = () => {
    if (!_opening || _closingGeneration !== startGeneration) {
      throw new Error('openAutoRecord aborted by close');
    }
  };

  try {
    try { await _syncBackendConfig(true, token); } catch (e) { logger.error('autoRecord step1 syncBackendConfig failed', e); throw e; }
    _checkAborted();
    try { await _ensureAppLocationAuthorized(); } catch (e) { logger.error('autoRecord step1.5 app location permission failed', e); throw e; }
    _checkAborted();
    try { await _startLocationUpdate(); } catch (e) { logger.error('autoRecord step2 startLocationUpdate failed', e); throw e; }
    _checkAborted();

    _setRecordingState(true);
    _clearRestoreRetry();
    _restoreRetryDelay = 60_000;
    onDone(true);

    _saveFirstRecord().catch(() => {});
  } catch {
    // 仅业务失败才反向关闭后端开关；取消/超时/关闭触发的失败交由超时处理器或 closeAutoRecord
    // 统一裁决，避免与"后端已开启"的恢复逻辑竞态导致"本地 on + 后端 off"不一致。
    if (!token.isCancelled()) {
      try {
        await _syncBackendConfig(false);
      } catch {
      }
    }
    _resetState();
    if (_justFromSettings) {
      _justFromSettings = false;
      onDone(false, { needRefresh: true });
      return;
    }
    onDone(false);
  } finally {
    if (_lifecycleCancelToken === token) {
      _lifecycleCancelToken = null;
    }
  }
};


export const openAutoRecord = (): Promise<boolean> => {
  if (_opening) {
    return Promise.reject(new Error('openAutoRecord in progress'));
  }
  _opening = true;
  _justFromSettings = false;
  const startGeneration = ++_closingGeneration;
  return new Promise((resolve, reject) => {
    let timeoutFired = false;
    const timeout = setTimeout(async () => {
      timeoutFired = true;
      _opening = false;
      // 超时后立即取消进行中的请求并停止定位，避免后台偷偷运行
      if (_lifecycleCancelToken) {
        try { _lifecycleCancelToken.cancel(); } catch {}
      }
      // 超时可能发生在后端已接受 enabled=true（响应丢失/挂起）之后，
      // 先查后端真实状态再决定本地开关，避免"本地置 off / 后端仍 on"的不一致窗口。
      // 查询失败或后端未开启时按关闭处理。
      let backendEnabled = false;
      try {
        const query = request.get('/auto-record/config', {}, true);
        backendEnabled = await Promise.race([
          query.then((res: any) => res?.data?.enabled === true),
          new Promise<boolean>((r2) => setTimeout(() => r2(false), 5000)),
        ]);
      } catch (e) {
        logger.warn('autoRecord open timeout: 查询后端配置失败，按关闭处理', e);
      }
      if (backendEnabled) {
        // 后端已开启：保留本地开启态，不反向关闭后端开关。
        // 立即尝试重建后台定位监听（否则存在"界面显示已开启但监听未建立"的窗口）。
        _setRecordingState(true);
        logger.warn('autoRecord open timed out but backend enabled, keep local state on and restore immediately');
        tryRestoreAutoRecord().catch(() => {});
      } else {
        _resetState();
      }
      reject(new Error('openAutoRecord timeout'));
    }, 30000);

    const finish = (ok: boolean, err?: any) => {
      clearTimeout(timeout);
      if (timeoutFired) {
        return;
      }
      _opening = false;
      if (ok) {
        resolve(true);
      } else {
        reject(err || new Error('openAutoRecord failed'));
      }
    };

    if (!(getVipInfo()?.isVip > 0)) {
      finish(false);
      return;
    }

    // 多端 App：wx.getSetting 不可用（API 总览 否[4]），原流程会静默 finish(false)。
    // 后台定位授权由系统权限承担，_ensureAppLocationAuthorized 已前置处理
    // （查授权态/引导设置/前台 getLocation 触发系统弹窗），直接进入开启流程。
    if (isAppEnv()) {
      _doOpenAutoRecord(finish, startGeneration);
      return;
    }

    wx.getSetting({
      success(res) {
        const bgAuth = (res.authSetting as any)['scope.userLocationBackground'];
        if (bgAuth === true) {
          _doOpenAutoRecord(finish, startGeneration);
        } else {
          _showLocationSettingModal(finish, startGeneration);
        }
      },
      fail: () => {
        finish(false);
      },
    });
  });
};


function _showLocationSettingModal(onDone: (ok: boolean, err?: any) => void, startGeneration: number) {
  wx.showModal({
    title: i18n.t('autoRecord.bgLocationTitle'),
    content: i18n.t('autoRecord.bgLocationDesc'),
    confirmText: i18n.t('autoRecord.goEnable'),
    cancelText: i18n.t('common.cancel'),
    success: (modalRes) => {
      if (modalRes.confirm) {
        let settingTimedOut = false;
        const openTimeout = setTimeout(() => {
          settingTimedOut = true;
          _opening = false;
          onDone(false);
        }, 15000);
        wx.openSetting({
          success: (settingRes) => {
            clearTimeout(openTimeout);
            if (settingTimedOut) return;
            if (!_opening) return;
            if ((settingRes.authSetting as any)['scope.userLocationBackground']) {
              _justFromSettings = true;
              _doOpenAutoRecord(onDone, startGeneration);
            } else {
              _opening = false;
              onDone(false);
            }
          },
          fail: () => {
            clearTimeout(openTimeout);
            if (settingTimedOut) return;
            if (!_opening) return;
            _opening = false;
            onDone(false);
          },
        });
      } else {
        _opening = false;
        onDone(false);
      }
    },
    fail: () => {
      _opening = false;
      onDone(false);
    },
  });
}


const _resetState = (preserveStorage = false) => {
  // 上报进度计数同步清零：上一账号「上报成功但清空失败」的残留会让新账号驻留点
  // 被 slice 跳过前 N 点不上报（登出/注销均经 closeAutoRecord → _resetState）
  _reportedLeadingCount = 0;
  if (_iosResumeTimer != null) {
    clearTimeout(_iosResumeTimer);
    _iosResumeTimer = null;
  }
  if (_activeCancelToken) {
    try { _activeCancelToken.cancel(); } catch {}
    _activeCancelToken = null;
  }
  _appBackgroundedAt = 0;
  // VIP 查询等瞬时失败场景需要保留本地 storage 开关，网络恢复后可自动恢复（AGENTS.md §3.7）。
  if (!preserveStorage) {
    _setRecordingState(false);
  } else {
    _enabledInMemory = null;
  }
  _stopFallbackTimer();
  wx.offLocationChange(onLocationChange);
  _stopLocationUpdateBackground();
  _window.length = 0;
  // 注意：不在这里清空 _stayPointQueue；关闭流程中 flush 失败时，点会被加回队列，
  // 需要保留到下次 flush。flush 成功时队列已被 splice(0) 清空。
  _stableCentroid = null;
  _lastCentroidCalcTime = 0;
  _state = 'moving';
  _stationaryStartTime = null;
  _stationarySaved = false;
  _exitConfirmCount = 0;
  _reportBatchSeq = 0;
  _lastSystemCallbackAt = 0;
  _lastStationaryCheckTime = 0;
  _reportFailureCount = 0;
  _lastReportFailureTime = 0;
  _lastActiveTouchTime = 0;
  _lastVipResult = null;
  _lastVipCheckTime = 0;
  _lastVipFetchAt = 0;
  _fallbackPending = false;
};


export const closeAutoRecord = async (): Promise<void> => {
  _closingGeneration++;
  _clearRestoreRetry();
  if (_lifecycleCancelToken) {
    try { _lifecycleCancelToken.cancel(); } catch {}
    _lifecycleCancelToken = null;
  }
  if (_activeCancelToken) {
    try { _activeCancelToken.cancel(); } catch {}
    _activeCancelToken = null;
  }
  const closeToken = createCancelToken();
  try {
    if (!_isRecordingEnabled()) {
      _resetState();
      return;
    }

    // 先上报积压驻留点再关闭后端开关：若先关开关，后端
    // ListPendingAutoRecordUsers 不再处理该用户，最后一批轨迹
    // 会被静默丢弃，导致关闭前最后停留的地点不生成日记。
    try {
      // 关闭前强制上报最后一批（忽略退避），避免开关关闭后这批轨迹被后端清理。
      await _serialFlushAndReport(closeToken, true);
    } catch {
    }

    try {
      await _syncBackendConfig(false);
    } catch {
    }
  } catch {
  } finally {
    try {
      _resetState();
    } catch {
    }
  }
};


const _touchActive = () => {
  if (_activeCancelToken) {
    try { _activeCancelToken.cancel(); } catch {}
  }
  const token = createCancelToken();
  _activeCancelToken = token;
  request.put('/auto-record/active', { cancelToken: token }, true, 5000)
    .catch(() => {})
    .finally(() => {
      if (_activeCancelToken === token) {
        _activeCancelToken = null;
      }
    });
};

let _restoreRetryTimer: any = null;
let _restoreRetryDelay = 60_000;
const RESTORE_RETRY_MAX_DELAY = 5 * 60 * 1000;

const _clearRestoreRetry = () => {
  if (_restoreRetryTimer != null) {
    clearTimeout(_restoreRetryTimer);
    _restoreRetryTimer = null;
  }
};

// 恢复失败后指数退避重试，最多等 5 分钟；成功后由 tryRestoreAutoRecord 重置。
// 保证用户打开小程序后即使瞬时网络失败/后端抖动，自动记录也能自行恢复，
// 不依赖用户再次手动打开（AGENTS.md §3.7）。
const _scheduleRestoreRetry = () => {
  _clearRestoreRetry();
  const delay = _restoreRetryDelay;
  _restoreRetryDelay = Math.min(_restoreRetryDelay * 2, RESTORE_RETRY_MAX_DELAY);
  _restoreRetryTimer = setTimeout(() => {
    _restoreRetryTimer = null;
    tryRestoreAutoRecord().catch(() => {});
  }, delay);
};

export const tryRestoreAutoRecord = async (): Promise<boolean> => {
  if (_opening) {
    return false;
  }
  if (!_isRecordingEnabled()) {
    _clearRestoreRetry();
    _resetState();
    return false;
  }
  if (_restoring) {
    return false;
  }
  _restoring = true;
  const startGeneration = _closingGeneration;
  if (_lifecycleCancelToken) {
    try { _lifecycleCancelToken.cancel(); } catch {}
  }
  const token = createCancelToken();
  _lifecycleCancelToken = token;
  // 看门狗：恢复流程意外挂起（如小程序进后台 JS 冻结）时强制复位 _restoring，
  // 避免下次进入小程序时恢复被单飞标志永久阻断。60 秒覆盖网络请求 20s 超时×2 + 缓冲。
  const _restoreWatchdog = setTimeout(() => {
    if (_restoring && _lifecycleCancelToken === token) {
      try { token.cancel(); } catch {}
      _restoring = false;
      _scheduleRestoreRetry();
    }
  }, 60_000);
  try {
    // 1. 本地先行恢复定位监听（不依赖网络）：小程序被杀后台后重新进入时，
    //    监听必须立即重建；网络校验（VIP/config）异步后置，失败再停止。
    if (_isRecordingEnabled() && _closingGeneration === startGeneration) {
      await new Promise<void>((resolve) => {
        wx.offLocationChange(onLocationChange);
        wx.startLocationUpdateBackground({
          success: () => {
            if (_closingGeneration !== startGeneration || !_isRecordingEnabled()) {
              resolve();
              return;
            }
            _listenerRegistered = true;
            _setRecordingState(true);
            wx.onLocationChange(onLocationChange);
            _startFallbackTimer();
            resolve();
          },
          fail: () => {
            // 权限不足/系统限制：保留开关并退避重试（用户重新授权后可恢复）
            resolve();
          },
        });
      });
    }

    // 2. 网络校验：上报积压轨迹、VIP 校验、后端开关校验
    // 恢复路径不得绕过 _serialFlushAndReport 的单飞链直调 _tryReportStorage——
    // 否则会与 push/close 路径的 _flushToStorage（读-改-写）并发交错，导致驻留点静默丢失或重复上报。
    await _serialFlushAndReport(token);

    let vipInfo;
    try {
      vipInfo = await fetchVipInfo();
    } catch (error) {
      // VIP 查询失败时不清空本地 storage 开关，但停止监听并清空内存状态，
      // 避免 _enabledInMemory 与实际后台监听不一致（AGENTS.md §3.7）。
      // 安排退避重试，网络恢复后自动恢复监听。
      logger.warn('fetch vip failed during restore', error);
      _resetState(true);
      _scheduleRestoreRetry();
      return false;
    }
    if (_closingGeneration !== startGeneration) {
      // 期间用户已完成 open/close：现场归新的裁决者——这里拆监听/写开关会把
      // "界面 off/后端 on"且监听死亡定格到下次冷启动。直接返回，不做任何清理。
      return false;
    }
    const isVip = vipInfo?.isVip > 0;
    _updateVipCache(isVip);
    if (!isVip) {
      _clearRestoreRetry();
      await closeAutoRecord();
      return false;
    }

    const { data } = await request.get('/auto-record/config', { cancelToken: token });
    if (_closingGeneration !== startGeneration) {
      // 同上：现场归新的 open/close 裁决，不拆场。
      return false;
    }
    const enabled = data?.enabled ?? false;

    if (!enabled) {
      // 后端已关闭自动记录，必须确保本地后台定位停止，避免定位残留
      _clearRestoreRetry();
      _resetState();
      return false;
    }
    // 后端确认开启：监听已在步骤 1 恢复；兜底再确保一次（步骤 1 可能因
    // generation 变化而跳过）。
    if (!_locationListening()) {
      await new Promise<void>((resolve) => {
        wx.startLocationUpdateBackground({
          success: () => {
            if (_closingGeneration === startGeneration && _isRecordingEnabled()) {
              _listenerRegistered = true;
              _setRecordingState(true);
              wx.onLocationChange(onLocationChange);
              _startFallbackTimer();
            }
            resolve();
          },
          fail: () => {
            resolve();
          },
        });
      });
    }
    // 监听仍未建立（权限被系统收回且短期无法恢复）：保留开关并安排退避重试，
    // 不能返回 true 清除重试定时器，否则自动记录静默停摆。
    if (!_locationListening()) {
      _resetState(true);
      _scheduleRestoreRetry();
      return false;
    }
    _clearRestoreRetry();
    _restoreRetryDelay = 60_000;
    return true;
  } catch {
    // 恢复过程失败（网络抖动/后端瞬时不可用）时保留本地开关并安排退避重试，
    // 不再 closeAutoRecord（避免把用户已开启的后端开关反向关掉）。
    _resetState(true);
    _scheduleRestoreRetry();
    return false;
  } finally {
    clearTimeout(_restoreWatchdog);
    if (_lifecycleCancelToken === token) {
      _lifecycleCancelToken = null;
    }
    _restoring = false;
    if (_isRecordingEnabled()) {
      _lastActiveTouchTime = Date.now();
      _touchActive();
    }
  }
};


export const onAppShow = async () => {
  _appBackgroundedAt = 0;

  if (!_isRecordingEnabled()) return;

  // 监听兜底：开启状态但监听已丢失（微信回收进程/系统收回权限后重新进入），
  // 主动触发一次恢复重建 startLocationUpdateBackground + onLocationChange。
  if (!_locationListening()) {
    tryRestoreAutoRecord().catch(() => {});
  }

  try {
    const vipInfo = await fetchVipInfo();
    if (!(vipInfo?.isVip > 0)) {
      await closeAutoRecord();
      return;
    }
  } catch (e) {
    // VIP 查询失败不阻断 iOS 兜底恢复，避免切回前台后 fallback 定时器丢失。
    logger.warn('fetch vip failed on app show', e);
  }

  _iosResumeGetLocation();
  _restartFallbackTimer();

  await _serialFlushAndReport();

  _lastActiveTouchTime = Date.now();
  _touchActive();
};


export const onAppHide = () => {
  if (_iosResumeTimer != null) {
    clearTimeout(_iosResumeTimer);
    _iosResumeTimer = null;
  }
  _appBackgroundedAt = Date.now();
  if (!_isRecordingEnabled()) return;
  _serialFlushAndReport().catch(() => {});
};

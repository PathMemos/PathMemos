import { logger, sanitizeUrlForLog } from './logger';
import { getSessionId, clearSessionId, setBaseInfo, getBackendMode, getPrivateBackendApiKey } from './storage';
import { getBaseURL } from '../config/index';
import { runWithConcurrency } from './concurrency';
import { i18n } from './i18n';
import { localizedBizCodeMessage } from './errorMessages';

// Internal sentinel used by auto-record and other modules to detect session expiration.
// User-facing copy is always rendered through i18n.t('error.sessionExpired').
const SESSION_EXPIRED_SENTINEL = '__ppfj_session_expired__';

export { getBaseURL };

export function getErrorMessage(error: any, defaultMsg: string = i18n.t('error.DEFAULT')): string {
  if (typeof error === 'string') return error;
  if (error instanceof Error) {
    // 内部哨兵（如会话过期）不得原样展示给用户。
    if (error.message === SESSION_EXPIRED_SENTINEL) {
      return i18n.t('error.sessionExpired');
    }
    return error.message;
  }
  return error?.msg || error?.data?.msg || error?.message || defaultMsg;
}

export interface CancelToken {
  cancel: () => void;
  isCancelled: () => boolean;
}

interface _InternalCancelToken extends CancelToken {
  _addAbort: (fn: () => void) => () => void;
}

export function createCancelToken(): CancelToken {
  let cancelled = false;
  const abortFns: Array<() => void> = [];
  const token: _InternalCancelToken = {
    cancel: () => {
      if (cancelled) return;
      cancelled = true;
      abortFns.forEach((fn) => {
        try { fn(); } catch (e) { logger.warn('cancel abort error', e); }
      });
      abortFns.length = 0;
    },
    isCancelled: () => cancelled,
    _addAbort: (fn: () => void) => {
      if (cancelled) {
        try { fn(); } catch (e) { logger.warn('cancel abort error', e); }
        return () => {};
      }
      abortFns.push(fn);
      return () => {
        const idx = abortFns.indexOf(fn);
        if (idx !== -1) abortFns.splice(idx, 1);
      };
    },
  };
  return token;
}

interface _RequestOptions {
  url: string;
  data?: any;
  method: 'GET' | 'POST' | 'PUT' | 'DELETE';
  header: Record<string, string>;
  timeout: number;
  closeTheErrorMessage?: boolean;
  // 401 时不走标准会话过期处理（清 session + 哨兵），由调用方自行处理鉴权刷新（如 autoRecord 静默续登）。
  skipAuthExpire?: boolean;
  cancelToken?: CancelToken;
}

function _request(options: _RequestOptions): Promise<any> {
  return new Promise((resolve, reject) => {
    let aborted = false;
    let removeAbort: (() => void) | undefined;

    const settle = (fn: () => void) => {
      try { fn(); } finally {
        removeAbort?.();
      }
    };

    const requestTask = wx.request({
      url: options.url,
      data: options.data,
      method: options.method,
      header: options.header,
      timeout: options.timeout,
      success(res: any) {
        if (aborted) return;
        settle(() => {
          const { data, statusCode } = res;

          if (statusCode === 401 && !options.skipAuthExpire) {
            clearSessionId();
            if (!options.closeTheErrorMessage) {
              _showSessionExpiredToast();
            }
            reject(new Error(SESSION_EXPIRED_SENTINEL));
            return;
          }

          if (statusCode && statusCode >= 500) {
            let body: string;
            try {
              body = typeof data === 'string' ? data : JSON.stringify(data);
            } catch {
              body = String(data);
            }
            // Worker 层返回的私有后端路由错误，给出可操作的提示。
            // 仅私有模式使用私有后端文案：SaaS 后端宕机时 Worker 也会对 SaaS 目标
            // 返回 5020/5030，此时落到通用服务端错误文案，避免误导。
            const workerCode = (data as any)?.code;
            const message = (workerCode === '5020' || workerCode === '5030') && getBackendMode() === 'private'
              ? i18n.t('error.privateBackendUnreachable')
              : i18n.t('error.serverError', { statusCode });
            const err = new Error(message);
            (err as any).statusCode = statusCode;
            (err as any).responseBody = body;
            reject(err);
            return;
          }

          if (data?.code !== '0000') {
            _handleResponseError(data, options.closeTheErrorMessage);
            const err = new Error(data?.message || i18n.t('error.DEFAULT'));
            (err as any).code = data?.code;
            (err as any).responseData = data;
            (err as any).data = data;
            reject(err);
            return;
          }

          resolve(data);
        });
      },
      fail(errMsg) {
        if (aborted) return;
        settle(() => {
          const isAbort = /abort/i.test(errMsg?.errMsg || '') || errMsg?.errno === 600004;
          if (isAbort) {
            logger.warn(sanitizeUrlForLog(options.url), errMsg);
            return reject(new Error('request:abort'));
          }
          logger.error(sanitizeUrlForLog(options.url), errMsg);
          reject(new Error(errMsg?.errMsg || i18n.t('error.networkFail')));
        });
      },
    });

    if (options.cancelToken) {
      removeAbort = (options.cancelToken as _InternalCancelToken)._addAbort(() => {
        aborted = true;
        try { requestTask.abort(); } catch (e) { logger.warn('abort request error', e); }
        reject(new Error('request:abort'));
      });
    }
  });
}

let _sessionToastAt = 0;
let _errorToastAt = 0;
const _toastThrottleMs = 200;

function _showSessionExpiredToast() {
  if (_isCurrentPageDestroyed() || _isCurrentPageHidden()) return;
  const now = Date.now();
  if (now - _sessionToastAt < _toastThrottleMs) return;
  _sessionToastAt = now;
  wx.showToast({ title: i18n.t('error.sessionExpired'), icon: 'none', duration: 2000 });
}

let _loadingCount = 0;

function _showLoading(title: string) {
  _loadingCount++;
  if (_loadingCount === 1) {
    try {
      wx.showLoading({ title, mask: true });
    } catch (e) {
      logger.error('showLoading failed', e);
    }
  }
}

function _hideLoading() {
  if (_loadingCount > 0) {
    _loadingCount--;
    if (_loadingCount === 0) {
      wx.hideLoading();
    }
  }
}

export function resetLoading() {
  _loadingCount = 0;
  wx.hideLoading();
}

function _isCurrentPageDestroyed(): boolean {
  const pages = getCurrentPages();
  const cur = pages[pages.length - 1] as any;
  return !!cur && (cur._isDestroyed || cur._isDetached);
}

function _isCurrentPageHidden(): boolean {
  const pages = getCurrentPages();
  const cur = pages[pages.length - 1] as any;
  return !!cur && cur._isHidden;
}

const _handleResponseError = (resData: any, closeTheErrorMessage = false): void => {
  if (closeTheErrorMessage) return;
  if (_isCurrentPageDestroyed() || _isCurrentPageHidden()) return;
  const now = Date.now();
  if (now - _errorToastAt < _toastThrottleMs) return;
  _errorToastAt = now;
  if (!resData || typeof resData === 'string') {
    wx.showToast({ title: i18n.t('error.serviceUnavailable'), icon: 'none', duration: 2000 });
    return;
  }
  if (resData.code === '4031' && getBackendMode() === 'private') {
    wx.showToast({ title: i18n.t('error.privateBackendNotRegistered'), icon: 'none', duration: 2000 });
    return;
  }
  // C1：后端 message 为英文硬编码，优先按 biz_code 显示本地化文案，未登记码回退后端 message。
  const bizMessage = localizedBizCodeMessage(resData.biz_code || resData.bizCode);
  wx.showToast({
    title: bizMessage || resData.message || resData.msg || i18n.t('error.DEFAULT'),
    icon: 'none',
    duration: 2000,
  });
};

function _buildUrl(url: string, params?: { [key: string]: any }): string {
  let _url = `${getBaseURL()}${url}`;
  if (params) {
    let paramCount = 0;
    Object.keys(params).forEach((key: any) => {
      const val = params[key];
      if (val == null) return;
      _url = `${_url}${paramCount === 0 ? '?' : '&'}${encodeURIComponent(key)}=${encodeURIComponent(val)}`;
      paramCount++;
    });
  }
  return _url;
}

function _authHeader(): Record<string, string> {
  const sessionId = getSessionId();
  const headers: Record<string, string> = {
    'Accept-Language': i18n.getLocale(),
  };
  if (sessionId) {
    headers.Authorization = `Bearer ${sessionId}`;
  }
  if (getBackendMode() === 'private') {
    const apiKey = getPrivateBackendApiKey();
    if (apiKey) {
      headers['X-Private-Api-Key'] = apiKey;
    }
  }
  return headers;
}

export const get = (
  url: string,
  { params, cancelToken }: { data?: object; params?: { [key: string]: any }; cancelToken?: CancelToken } = {},
  closeTheErrorMessage = false,
  skipAuthExpire = false
): Promise<{ code: string; count?: number; data: any; extra?: any; msg: string; nextCursor?: string }> => {
  const _url = _buildUrl(url, params);
  return _request({
    url: _url,
    method: 'GET',
    header: _authHeader(),
    timeout: 20000,
    closeTheErrorMessage,
    cancelToken,
    skipAuthExpire,
  });
};

export const post = (
  url: string,
  { data, params, cancelToken }: { data?: object; params?: { [key: string]: any }; cancelToken?: CancelToken } = {},
  closeTheErrorMessage = false,
  timeout = 20000,
  skipAuthExpire = false
): Promise<{ code: string; count?: number; data: any; extra?: any; msg: string; nextCursor?: string }> => {
  const _url = _buildUrl(url, params);
  return _request({
    url: _url,
    data: { ...data },
    method: 'POST',
    header: { 'content-type': 'application/json', ..._authHeader() },
    timeout,
    closeTheErrorMessage,
    cancelToken,
    skipAuthExpire,
  });
};

export const put = (
  url: string,
  { data, params, cancelToken }: { data?: object; params?: { [key: string]: any }; cancelToken?: CancelToken } = {},
  closeTheErrorMessage = false,
  timeout = 20000
): Promise<{ code: string; count?: number; data: any; extra?: any; msg: string; nextCursor?: string }> => {
  const _url = _buildUrl(url, params);
  return _request({
    url: _url,
    data: { ...data },
    method: 'PUT',
    header: { 'content-type': 'application/json', ..._authHeader() },
    timeout,
    closeTheErrorMessage,
    cancelToken,
  });
};

export const del = (
  url: string,
  { data, params, cancelToken }: { data?: object; params?: { [key: string]: any }; cancelToken?: CancelToken } = {},
  closeTheErrorMessage = false
): Promise<{ code: string; count?: number; data: any; extra?: any; msg: string; nextCursor?: string }> => {
  const _url = _buildUrl(url, params);
  return _request({
    url: _url,
    data: { ...data },
    method: 'DELETE',
    header: { 'content-type': 'application/json', ..._authHeader() },
    timeout: 20000,
    closeTheErrorMessage,
    cancelToken,
  });
};

export const FILE_TYPE = {
  UPLOADED: 0,
  TO_BE_UPLOADED: 1,
  DELETE: 2,
};

const _MAX_FILE_SIZE = 10 * 1024 * 1024;

const _uploadOne = async (path: string, url: string, cancelToken?: CancelToken): Promise<string> => {
  const filePath = await _compressImage(path);
  const result = await _uploadWithGuard(url, filePath, _authHeader(), cancelToken);
  return result.fileId;
};

const _uploadWithGuard = (
  url: string,
  filePath: string,
  header: Record<string, string>,
  cancelToken?: CancelToken,
): Promise<{ fileId: string; url: string }> => {
  return new Promise<{ fileId: string; url: string }>((resolve, reject) => {
    let aborted = false;
    let removeAbort: (() => void) | undefined;

    const settle = (fn: () => void) => {
      try { fn(); } finally {
        removeAbort?.();
      }
    };

    const uploadTask = wx.uploadFile({
      url,
      filePath,
      header,
      name: 'files',
      timeout: 30000,
      success(res) {
        if (aborted) return;
        settle(() => {
          let parsed: any;
          let parseError = false;
          try {
            parsed = JSON.parse(res.data);
          } catch {
            parseError = true;
          }

          const code = parsed?.code;
          // 后端 envelope 使用 biz_code（middleware/response.go），兼容历史 bizCode 写法。
          const bizCode = parsed?.biz_code || parsed?.bizCode;
          const effectiveCode = bizCode || code;
          const msg = parsed?.message || parsed?.msg;

          if (res.statusCode && res.statusCode >= 400) {
            logger.error('wx.uploadFile HTTP error', { statusCode: res.statusCode, data: res.data });
            const err = new Error(msg || i18n.t('error.uploadFail')) as any;
            err.code = effectiveCode;
            return reject(err);
          }

          if (parseError) {
            logger.error('wx.uploadFile response parse fail', { data: res.data });
            return reject(new Error(i18n.t('error.uploadResponseParseFail')));
          }

          if (code !== '0000') {
            _handleResponseError({ code, bizCode, msg }, true);
            const err = new Error(msg || i18n.t('error.uploadFail')) as any;
            err.code = effectiveCode;
            return reject(err);
          }

          const files = parsed?.data?.files;
          if (!Array.isArray(files) || !files.length || !files[0].fileId) {
            return reject(new Error(i18n.t('error.uploadFail')));
          }
          resolve({ fileId: files[0].fileId, url: files[0].url || '' });
        });
      },
      fail(errMsg) {
        if (aborted) return;
        settle(() => {
          logger.error('wx.uploadFile fail', errMsg);
          reject(new Error(errMsg?.errMsg || i18n.t('error.networkFail')));
        });
      },
    });

    if (cancelToken) {
      removeAbort = (cancelToken as _InternalCancelToken)._addAbort(() => {
        aborted = true;
        try { uploadTask.abort(); } catch (e) { logger.warn('abort upload error', e); }
        reject(new Error('request:abort'));
      });
    }
  });
};

export const uploadFile = async (
  fileLists: { path: string; type: number; id?: string }[],
  { pid, cancelToken }: { pid?: string; cancelToken?: CancelToken } = {},
): Promise<string[]> => {
  _showLoading(i18n.t('common.loading'));
  const url = `${getBaseURL()}/file/upload?type=recordImg`;
  const imageIds: string[] = [];
  if (pid) {
    imageIds.push(pid);
  }
  try {
    // PPJ-B08：记录每个待上传项在 fileLists 中的下标，成功结果可回写调用方对象，
    // 使部分失败后的重试只补传失败项，而不是整批重传（孤儿文件 + 配额浪费）。
    const uploadTargets: { index: number; path: string }[] = [];
    for (let i = 0; i < fileLists.length; i++) {
      const { path, type, id } = fileLists[i];
      if (type === FILE_TYPE.TO_BE_UPLOADED) {
        uploadTargets.push({ index: i, path });
      } else if (type === FILE_TYPE.UPLOADED && id) {
        imageIds.push(id);
      }
    }

    if (uploadTargets.length > 0) {
      const uploadTasks = uploadTargets.map((t) => () => _uploadOne(t.path, url, cancelToken));
      const results = await runWithConcurrency(uploadTasks, 3) as (string | Error)[];
      const successIds: string[] = [];
      const failedPaths: string[] = [];
      let storageLimitError: Error | null = null;
      let allAborted = uploadTargets.length > 0;
      for (let i = 0; i < results.length; i++) {
        const r = results[i];
        if (r instanceof Error) {
          if ((r as any).code === 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED') {
            storageLimitError = r;
          }
          if (r.message !== 'request:abort') {
            allAborted = false;
          }
          failedPaths.push(uploadTargets[i].path);
          logger.error('uploadFile: 单个文件上传失败', r);
        } else {
          successIds.push(r);
          (fileLists[uploadTargets[i].index] as any).uploadedId = r;
          allAborted = false;
        }
      }
      imageIds.push(...successIds);
      if (failedPaths.length > 0) {
        logger.error('uploadFile: 部分文件上传失败', { failedCount: failedPaths.length, total: uploadTargets.length });
        if (allAborted) {
          throw new Error('request:abort');
        }
        if (storageLimitError) {
          const err = new Error(storageLimitError.message) as any;
          err.code = 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED';
          throw err;
        }
        throw new Error(i18n.t('error.imagesUploadFail', { count: failedPaths.length }));
      }
    }
  } catch (error: any) {
    if (error?.code === 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED') {
      throw Object.assign(new Error(i18n.t('error.imageStorageLimitExceeded')), {
        code: 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED',
      });
    }
    const errMsg = getErrorMessage(error, i18n.t('error.DEFAULT'));
    logger.error('uploadFile catch', errMsg);
    if (errMsg !== 'request:abort' && !_isCurrentPageDestroyed() && !_isCurrentPageHidden()) {
      wx.showModal({
        content: errMsg,
        showCancel: false,
      });
    }
    throw Object.assign(new Error(errMsg), { _handledByModal: true });
  } finally {
    _hideLoading();
  }

  return imageIds;
};

export const updateAvatar = async (path: string, { cancelToken }: { cancelToken?: CancelToken } = {}) => {
  _showLoading(i18n.t('common.loading'));
  const url = `${getBaseURL()}/file/upload?type=avatar`;
  try {
    const filePath = await _compressImage(path);
    const { fileId, url: fileUrl } = await _uploadWithGuard(url, filePath, _authHeader(), cancelToken);
    await put('/user/avatar', { data: { fileId }, cancelToken });
    try {
      setBaseInfo({ avatar: fileUrl });
    } catch {}
    _hideLoading();
    return fileUrl;
  } catch (error: any) {
    _hideLoading();
    if (error?.code === 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED') {
      throw Object.assign(new Error(i18n.t('error.imageStorageLimitExceeded')), {
        code: 'USER_IMAGE_STORAGE_LIMIT_EXCEEDED',
      });
    }
    const msg = getErrorMessage(error, i18n.t('error.DEFAULT'));
    if (msg !== 'request:abort' && !_isCurrentPageDestroyed() && !_isCurrentPageHidden()) {
      wx.showModal({ content: msg, showCancel: false });
    }
    throw Object.assign(new Error(msg), { _handledByModal: true });
  }
};

const _compressImage = (path: string): Promise<string> => {
  return new Promise<string>((resolve, reject) => {
    const fs = wx.getFileSystemManager();
    fs.getFileInfo({
      filePath: path,
      success: ({ size }) => {
        if (size > _MAX_FILE_SIZE) {
          reject(new Error(i18n.t('error.imageSizeLimit')));
          return;
        }
        const quality = size > 1024 * 1024 ? 50 : size > 200 * 1024 ? 65 : 80;
        wx.compressImage({
          src: path,
          quality,
          success: ({ tempFilePath }) => {
            fs.getFileInfo({
              filePath: tempFilePath,
              success: ({ size: newSize }) => {
                if (newSize > _MAX_FILE_SIZE) {
                  reject(new Error(i18n.t('error.imageSizeLimit')));
                  return;
                }
                resolve(tempFilePath);
              },
              fail: () => resolve(tempFilePath),
            });
          },
          fail: (err) => {
            logger.error('compressImage fail', err);
            reject(new Error(i18n.t('error.compressImageFail')));
          },
        });
      },
      fail: () => reject(new Error(i18n.t('error.readFileFail'))),
    });
  });
};

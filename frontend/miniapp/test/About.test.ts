/**
 * About 注销本地清理：页面隐藏/销毁时也须执行 closeAutoRecord/clearUserData，
 * 仅成功 toast 受可见性约束；成功后 reLaunch 首页。
 */
import { beforeEach, describe, expect, it, jest } from '@jest/globals';

jest.mock('../miniprogram/utils/request', () => ({
  __esModule: true,
  default: { del: jest.fn() },
  createCancelToken: jest.fn(() => ({ cancel: jest.fn() })),
  getBaseInfo: jest.fn(() => ({})),
}));
jest.mock('../miniprogram/utils/storage', () => ({
  clearUserData: jest.fn(),
  getBackendMode: jest.fn(() => 'saas'),
  setLoggedOut: jest.fn(),
}));
jest.mock('../miniprogram/utils/autoRecord', () => ({
  closeAutoRecord: jest.fn(async () => undefined),
}));

import request from '../miniprogram/utils/request';
import { clearUserData, setLoggedOut } from '../miniprogram/utils/storage';
import { closeAutoRecord } from '../miniprogram/utils/autoRecord';
import '../miniprogram/pages/sub/About/About';

function getPageDef(): any {
  const defs = (globalThis as any).__componentDefs as any[];
  return defs[defs.length - 1];
}

function makePage(def: any, hidden: boolean): any {
  const ctx: any = {};
  const skip = new Set([
    'behaviors', 'data', 'lifetimes', 'pageLifetimes', 'observers', 'options', 'externalClasses',
  ]);
  for (const key of Object.keys(def)) {
    if (!skip.has(key)) ctx[key] = def[key];
  }
  ctx.data = JSON.parse(JSON.stringify(def.data || {}));
  ctx.data.confirmDialog = { visible: true };
  ctx.data.deleteConfirmInput = '测试用户';
  ctx.data.deleting = false;
  ctx._safeSetData = (patch: any) => { Object.assign(ctx.data, patch); };
  ctx._forceSetData = (patch: any) => { Object.assign(ctx.data, patch); };
  ctx._applyPendingSetData = () => {};
  ctx.$t = (key: string) => key;
  ctx._isDestroyed = false;
  ctx._isHidden = hidden;
  return ctx;
}

async function flush(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('About 注销清理', () => {
  beforeEach(() => {
    (request.del as any).mockReset();
    (request.del as any).mockResolvedValue({});
    (closeAutoRecord as any).mockClear();
    (clearUserData as any).mockClear();
  });

  it('隐藏态仍执行本地清理，但不弹成功 toast', async () => {
    const ctx = makePage(getPageDef(), true);
    ctx.onConfirmDialogConfirm();
    await flush();

    expect(closeAutoRecord).toHaveBeenCalled();
    expect(clearUserData).toHaveBeenCalled();
    expect(setLoggedOut).toHaveBeenCalledWith(true);
    expect((globalThis as any).wx.showToast).not.toHaveBeenCalled();
    // 注销后落登录停留页（不再回首页静默重登新账号）
    expect((globalThis as any).wx.reLaunch).toHaveBeenCalledWith({ url: '/pages/Login/Login' });
  });

  it('可见态执行清理并弹成功 toast', async () => {
    const ctx = makePage(getPageDef(), false);
    ctx.onConfirmDialogConfirm();
    await flush();

    expect(clearUserData).toHaveBeenCalled();
    expect(setLoggedOut).toHaveBeenCalledWith(true);
    expect((globalThis as any).wx.showToast).toHaveBeenCalled();
    expect((globalThis as any).wx.reLaunch).toHaveBeenCalledWith({ url: '/pages/Login/Login' });
  });

  it('空确认名时不发起请求', () => {
    const ctx = makePage(getPageDef(), false);
    ctx.data.deleteConfirmInput = '   ';
    ctx.onConfirmDialogConfirm();
    expect(request.del).not.toHaveBeenCalled();
  });
});

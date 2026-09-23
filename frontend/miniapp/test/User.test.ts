/**
 * 个人中心（User）onShow 登录取消回归：
 * onShow 触发的 request.login 被 request:abort 取消时（切页 / onShow 取消上一次登录），
 * 不得弹出「操作失败，请稍后重试」；真实登录失败仍必须提示。
 */
import { beforeEach, describe, expect, it, jest } from '@jest/globals';

jest.mock('../miniprogram/utils/request', () => ({
  __esModule: true,
  default: {
    login: jest.fn(),
    getVipInfo: jest.fn(() => ({})),
  },
  createCancelToken: jest.fn(() => ({ cancel: jest.fn(), isCancelled: () => false })),
}));
jest.mock('../miniprogram/utils/vip', () => ({
  formatVipInfo: jest.fn(() => ({ isVip: false, vipExpireTime: '', receivedFreeVip: false })),
}));
jest.mock('../miniprogram/utils/logger', () => ({
  logger: { error: jest.fn(), warn: jest.fn(), info: jest.fn() },
}));
jest.mock('../miniprogram/utils/storage', () => ({
  getBackendMode: jest.fn(() => 'saas'),
}));
jest.mock('../miniprogram/behaviors/theme', () => ({ __esModule: true, default: {} }));
jest.mock('../miniprogram/behaviors/i18n', () => ({ __esModule: true, default: {} }));

import request from '../miniprogram/utils/request';
import '../miniprogram/pages/User/User';

function getPageDef(): any {
  const defs = (globalThis as any).__componentDefs as any[];
  return defs[defs.length - 1];
}

function makePage(): any {
  const def = getPageDef();
  const ctx: any = {};
  const skip = new Set(['behaviors', 'data', 'lifetimes', 'pageLifetimes', 'observers', 'options', 'externalClasses']);
  for (const key of Object.keys(def)) {
    if (!skip.has(key)) ctx[key] = def[key];
  }
  ctx.data = JSON.parse(JSON.stringify(def.data || {}));
  ctx._safeSetData = (patch: any) => { Object.assign(ctx.data, patch); };
  ctx._applyPendingSetData = () => {};
  ctx.$t = (key: string) => key;
  return ctx;
}

describe('User 个人中心 onShow 登录', () => {
  beforeEach(() => {
    (request.login as any).mockReset();
    (globalThis as any).wx.showToast.mockReset();
  });

  it('登录被 request:abort 取消时不弹「操作失败」', async () => {
    (request.login as any).mockRejectedValue(new Error('request:abort'));
    const ctx = makePage();
    await ctx.onShow();
    expect((globalThis as any).wx.showToast).not.toHaveBeenCalled();
  });

  it('真实登录失败仍弹「操作失败」', async () => {
    (request.login as any).mockRejectedValue(new Error('network fail'));
    const ctx = makePage();
    await ctx.onShow();
    expect((globalThis as any).wx.showToast).toHaveBeenCalledTimes(1);
    expect((globalThis as any).wx.showToast).toHaveBeenCalledWith(
      expect.objectContaining({ title: 'error.DEFAULT' })
    );
  });
});

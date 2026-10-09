/**
 * Invite 保存/分享邀请图（App 端契约，2026-09-30 第一步 B 类修复）：
 * - 多端 App 环境（wx.miniapp 存在）必须走 wx.miniapp.shareImageMessage，
 *   不得再调用小程序专用的 showShareImageMenu（App 端 SDK 不支持，原为死按钮）；
 * - 新增 i18n key（noteEdit.locationFailedPrefix / autoRecord.*）三语齐备。
 */
import { beforeEach, describe, expect, it, jest } from '@jest/globals';

jest.mock('../miniprogram/utils/request', () => ({
  __esModule: true,
  default: { get: jest.fn(), post: jest.fn() },
  createCancelToken: jest.fn(() => ({ cancel: jest.fn() })),
  getBaseInfo: jest.fn(() => ({})),
  resetLoading: jest.fn(),
}));
jest.mock('../miniprogram/utils/storage', () => ({
  getBackendMode: jest.fn(() => 'saas'),
}));

import zh from '../miniprogram/utils/i18n/locales/zh';
import en from '../miniprogram/utils/i18n/locales/en';
import zhHant from '../miniprogram/utils/i18n/locales/zh-Hant';
import '../miniprogram/pages/Invite/Invite';

function getPageDef(): any {
  const defs = (globalThis as any).__componentDefs as any[];
  return defs[defs.length - 1];
}

function makePage(def: any): any {
  const ctx: any = {};
  const skip = new Set([
    'behaviors', 'data', 'lifetimes', 'pageLifetimes', 'observers', 'options', 'externalClasses',
  ]);
  for (const key of Object.keys(def)) {
    if (!skip.has(key)) ctx[key] = def[key];
  }
  ctx.data = JSON.parse(JSON.stringify(def.data || {}));
  ctx.data.shareImageUrl = 'https://example.com/invite.png';
  ctx.data.shareReady = true;
  ctx.$t = (key: string) => key;
  ctx._safeSetData = (patch: any) => { Object.assign(ctx.data, patch); };
  ctx._isDestroyed = false;
  ctx._isHidden = false;
  // 命中缓存路径，跳过 downloadFile。
  ctx._shareImageCache = { url: 'https://example.com/invite.png', path: '/tmp/cached.png' };
  return ctx;
}

describe('Invite 保存/分享邀请图（App 分支）', () => {
  beforeEach(() => {
    const wxAny = (globalThis as any).wx;
    wxAny.getFileSystemManager = () => ({
      access: ({ path, success }: any) => success({ path }),
    });
    wxAny.showLoading = jest.fn();
    wxAny.hideLoading = jest.fn();
  });

  it('App 端（wx.miniapp 存在）走 shareImageMessage，不调 showShareImageMenu', async () => {
    const wxAny = (globalThis as any).wx;
    let sheetResolve: (n: number) => void = () => {};
    wxAny.showActionSheet = jest.fn(({ success }: any) => success({ tapIndex: 0 }));
    wxAny.shareImageMessage = undefined;
    wxAny.miniapp = {
      shareImageMessage: jest.fn(({ success }: any) => success({})),
    };
    wxAny.showToast = jest.fn();
    wxAny.showShareImageMenu = jest.fn();
    void sheetResolve;

    const page = makePage(getPageDef());
    await page.saveShareImage.call(page);
    await Promise.resolve();

    expect(wxAny.showShareImageMenu).not.toHaveBeenCalled();
    expect(wxAny.miniapp.shareImageMessage).toHaveBeenCalledTimes(1);
    const arg = wxAny.miniapp.shareImageMessage.mock.calls[0][0];
    expect(arg.imagePath).toBe('/tmp/cached.png');
    expect(arg.thumbPath).toBe('/tmp/cached.png');
    expect(arg.scene).toBe(0);
  });

  it('小程序端（无 wx.miniapp）仍走 showShareImageMenu', async () => {
    const wxAny = (globalThis as any).wx;
    wxAny.miniapp = undefined;
    wxAny.showShareImageMenu = jest.fn();

    const page = makePage(getPageDef());
    await page.saveShareImage.call(page);
    await Promise.resolve();

    expect(wxAny.showShareImageMenu).toHaveBeenCalledTimes(1);
    expect(wxAny.showShareImageMenu.mock.calls[0][0].path).toBe('/tmp/cached.png');
  });

  it('用户在场景选择弹窗取消时不发起分享', async () => {
    const wxAny = (globalThis as any).wx;
    wxAny.showActionSheet = jest.fn(({ fail }: any) => fail({ errMsg: 'cancel' }));
    wxAny.miniapp = { shareImageMessage: jest.fn() };
    wxAny.showShareImageMenu = jest.fn();

    const page = makePage(getPageDef());
    await page.saveShareImage.call(page);
    await Promise.resolve();

    expect(wxAny.miniapp.shareImageMessage).not.toHaveBeenCalled();
  });
});

describe('新增 i18n key 三语齐备', () => {
  const locales: Array<[string, any]> = [
    ['zh', zh],
    ['en', en],
    ['zh-Hant', zhHant],
  ];

  it.each(['noteEdit.locationFailedPrefix', 'autoRecord.bgLocationTitle', 'autoRecord.bgLocationDesc', 'autoRecord.goEnable'])(
    '%s 在三语均有非空文案',
    (key: string) => {
      for (const [name, locale] of locales) {
        const value = key.split('.').reduce((obj: any, k: string) => (obj ? obj[k] : undefined), locale);
        expect(typeof value).toBe('string');
        expect((value as string).length).toBeGreaterThan(0);
        void name;
      }
    },
  );
});

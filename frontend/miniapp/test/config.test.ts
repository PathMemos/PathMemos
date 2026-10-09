/**
 * miniprogram/config/index.ts 的 getBaseURL 模式判定单元测试。
 *
 * 事实源（当前实现）：
 * - envVersion=develop 默认 → SAAS_BASE_URL（开发者工具联调线上；Storage 写
 *   'dev_use_local_backend'=true 才回落 DEV_BASE_URL 本地联调）；
 * - envVersion=release 且 storage 'backend_mode'='private' → WORKER_BASE_URL（Cloudflare Worker 路由私有后端起）；
 * - 其余情况 → SaaS 源站 https://pro.papafeiji.cn。
 *
 * 注意：isDevelop 在模块加载期由 wx.getAccountInfoSync() 求值一次，
 * 因此每个用例先改环境再 `jest.resetModules()` + 动态 import，得到干净的模块实例。
 */

import { beforeEach, describe, expect, it, jest } from '@jest/globals';

const SAAS_BASE_URL = 'https://pro.papafeiji.cn';
const WORKER_BASE_URL = 'https://api.pathmemos.com';
const DEV_BASE_URL = 'http://localhost:8080';
const STORAGE_KEY_MODE = 'backend_mode';
const DEV_LOCAL_KEY = 'dev_use_local_backend';

function wx(): any {
  return (globalThis as any).wx;
}

async function loadConfig(envVersion: string, mode?: string, devLocal?: boolean) {
  const w = wx();
  w.getAccountInfoSync = () => ({ miniProgram: { appId: 'wxtest', envVersion } });
  if (mode === undefined) {
    w.removeStorageSync(STORAGE_KEY_MODE);
  } else {
    w.setStorageSync(STORAGE_KEY_MODE, mode);
  }
  if (devLocal === undefined) {
    w.removeStorageSync(DEV_LOCAL_KEY);
  } else {
    w.setStorageSync(DEV_LOCAL_KEY, devLocal);
  }
  jest.resetModules();
  return await import('../miniprogram/config/index');
}

describe('config.getBaseURL 模式判定', () => {
  beforeEach(() => {
    wx().getAccountInfoSync = () => ({ miniProgram: { appId: 'wxtest', envVersion: 'release' } });
  });

  it('develop 默认走 SaaS 源站（开发者工具联调线上）', async () => {
    const config = await loadConfig('develop');
    expect(config.getBaseURL()).toBe(SAAS_BASE_URL);
    expect(config.getSSEBaseURL()).toBe(SAAS_BASE_URL);
  });

  it('develop + dev_use_local_backend=true 回落本地 DEV_BASE_URL（即使 mode=private）', async () => {
    const config = await loadConfig('develop', 'private', true);
    expect(config.getBaseURL()).toBe(DEV_BASE_URL);
    expect(config.getSSEBaseURL()).toBe(DEV_BASE_URL);
  });

  it('release + mode=private 走 WORKER_BASE_URL', async () => {
    const config = await loadConfig('release', 'private');
    expect(config.getBaseURL()).toBe(WORKER_BASE_URL);
    expect(config.getSSEBaseURL()).toBe(WORKER_BASE_URL);
  });

  it('release + mode=saas 走 SaaS 源站', async () => {
    const config = await loadConfig('release', 'saas');
    expect(config.getBaseURL()).toBe(SAAS_BASE_URL);
  });

  it('release + 无 mode 缺省走 SaaS 源站', async () => {
    const config = await loadConfig('release');
    expect(config.getBaseURL()).toBe(SAAS_BASE_URL);
  });

  it('storage 读取抛错时降级为 SaaS 源站（守卫不崩溃）', async () => {
    const w = wx();
    const originalGetStorageSync = w.getStorageSync;
    w.getAccountInfoSync = () => ({ miniProgram: { appId: 'wxtest', envVersion: 'release' } });
    w.getStorageSync = () => { throw new Error('storage unavailable'); };
    try {
      jest.resetModules();
      const config = await import('../miniprogram/config/index');
      expect(config.getBaseURL()).toBe(SAAS_BASE_URL);
    } finally {
      w.getStorageSync = originalGetStorageSync;
    }
  });

  it('getSSEBaseURL 与 getBaseURL 始终一致', async () => {
    const config = await loadConfig('release', 'private');
    expect(config.getSSEBaseURL()).toBe(config.getBaseURL());
  });

  it('模块级 BASE_URL 反映首次求值时的模式（兼容旧代码语义）', async () => {
    const config = await loadConfig('release', 'private');
    expect(config.BASE_URL).toBe(WORKER_BASE_URL);
    expect(config.SSE_BASE_URL).toBe(WORKER_BASE_URL);
  });

  it('getHelpBaseURL 指向 SaaS 帮助站', async () => {
    const config = await loadConfig('release');
    expect(config.getHelpBaseURL()).toBe('https://papafeiji.cn');
  });
});

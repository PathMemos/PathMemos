


import { STORAGE_KEY_MODE } from '../utils/storage';

const SAAS_BASE_URL = 'https://pro.papafeiji.cn';
export const WORKER_BASE_URL = 'https://api.pathmemos.com';
const DEV_BASE_URL = 'http://localhost:8080';

// 模块加载期调用 wx 能力需带守卫：无 wx 环境（Node 单测等）import 不应崩溃。
let envVersion = 'release';
try {
  envVersion = (wx as any).getAccountInfoSync().miniProgram.envVersion;
} catch (_err) {
  // 默认按 release 处理
}
const isDevelop = envVersion === 'develop';

// 阶段二：用户可在设置页切换为 'private'，经 Cloudflare Worker 路由到私有化后端。
// SaaS 版本默认直连源站，避免 Cloudflare Worker 带来的延迟。
// 开发版（含微信开发者工具）默认同样直连线上 SaaS——工具联调生产接口是主路径；
// 本地后端联调需显式在 Storage 写入 dev_use_local_backend = true 才回落 localhost。
export function getBaseURL(): string {
  if (isDevelop) {
    try {
      const devLocal = wx.getStorageSync('dev_use_local_backend');
      if (devLocal === true || devLocal === 'true') {
        return DEV_BASE_URL;
      }
    } catch (_err) {
      // ignore
    }
  }
  try {
    const mode = wx.getStorageSync(STORAGE_KEY_MODE);
    if (mode === 'private') {
      return WORKER_BASE_URL;
    }
  } catch (_err) {
    // ignore
  }
  return SAAS_BASE_URL;
}

export function getSSEBaseURL(): string {
  return getBaseURL();
}

// 兼容旧代码：模块加载时计算一次，动态切换请使用 getBaseURL()/getSSEBaseURL()
// 注意：不要在新代码中直接使用这两个常量，否则用户切换后端模式后不会更新。
export const BASE_URL = getBaseURL();
export const SSE_BASE_URL = getSSEBaseURL();




export const OSS_PUBLIC_URL = 'https://ppfj-images.oss-cn-hangzhou.aliyuncs.com';




export const NEW_USER_FREE_VIP_ID = 'vip-free-0001';

export const ABNORMAL_TEMPLATE_ID = 'i7mcEEMDbhYU1oAC1-E0G0xvIBCmTl6f9c3jOq11m3g';


const SAAS_HELP_BASE = 'https://papafeiji.cn';

// 教程/帮助/关注图等静态内容统一托管在 SaaS 站点（papafeiji.cn + 公共 OSS），
// 开源版不单独维护一份，私有模式同样指向 SaaS 地址。
export function getHelpBaseURL(): string {
  return SAAS_HELP_BASE;
}

// 分享导出引导笔记链接（运营可整体替换，避免页面内硬编码死链）。
export const EXPORT_GUIDE_URL = 'https://www.xiaohongshu.com/explore/66362ab7000000001e038b66';

// 多端 App 权限体系兼容层。
// 依据官方《多端应用中 wx.getSetting 兼容处理》：多端 App 脱离微信权限体系，
// wx.getSetting / wx.openSetting / wx.authorize 均不可用（API 总览标注否[4]），
// 授权态用 wx.getAppAuthorizeSetting 查询、wx.openAppAuthorizeSetting 引导系统设置。

export const isAppEnv = (): boolean => !!(wx as any).miniapp;

export type AppLocationAuth = 'authorized' | 'not determined' | 'denied' | 'unknown';

/** 查询系统定位授权态；仅 App 环境有意义，查询失败或小程序环境返回 'unknown' */
export const getAppLocationAuthorized = (): Promise<AppLocationAuth> =>
  new Promise((resolve) => {
    if (!isAppEnv()) return resolve('unknown');
    (wx as any).getAppAuthorizeSetting({
      success: (res: any) => resolve(res?.locationAuthorized ?? 'unknown'),
      fail: () => resolve('unknown'),
    });
  });

/** 打开系统权限设置页（App 专用；小程序环境不应调用） */
export const guideToAppAuthorizeSetting = (): void => {
  (wx as any).openAppAuthorizeSetting({});
};

import request from './request';
import { logger } from './logger';
import { isIOS } from './util';
import { i18n } from './i18n';
import { opsLogFail, flushOpsLog } from './opslog';
import type { CancelToken } from './http';

// 小程序原始 ID（与 app.miniapp.json adapteByMiniprogram.userName 同源）：
// App 环境跳转微信小程序完成支付的目标。
const MINI_PROGRAM_USER_NAME = 'gh_b8e6cc7fb7aa';

// 多端 App 环境判定（与 Login.ts 同口径）：Donut 运行时注入 wx.miniapp。
const isAppEnv = (): boolean => !!(wx as any).miniapp;

// 支付被取消/失败后 best-effort 关闭本地 pending 订单，避免订单滞留到
// 5 分钟（换单清理）或 24 小时（后台任务）才回收；后端 cancel 幂等。
const cancelOrder = (outTradeNo?: string): void => {
  if (!outTradeNo) return;
  request.post('/payment/virtual/cancel', { data: { outTradeNo } }, true).catch((e: any) => {
    logger.error('取消虚拟支付订单失败', e);
  });
};

// 支付环境选择（仅开发版 env=1 沙箱联调；体验版/正式版一律 env=0 实付）：
// 体验版购买与正式版完全一致（真扣款、真 VIP），沙箱「免费领 VIP」通道不对体验版开放。
const getPayEnv = (): number => {
  try {
    const env = wx.getAccountInfoSync().miniProgram.envVersion;

    if (isIOS()) return 0;
    return env === 'develop' ? 1 : 0;
  } catch {
    return 0;
  }
};

export const doPay = async (
  commodity: any,
  onVirtualPaySuccess: (outTradeNo: string) => void,
  onPayFail: () => void,
  onOrderCreated?: (outTradeNo: string) => void,
  cancelToken?: CancelToken
): Promise<void> => {
  // App（多端）环境：虚拟支付的用户态签名依赖「当前有效 session_key」（jscode2session），
  // 而多端登录链路拿不到它——wx.login 在 App 中不适用、donut code2Verifyinfo 不返回
  // session_key，requestVirtualPayment 在客户端瞬拒（连支付面板都不拉起）。
  // 所有者裁决：跳转微信小程序 VIP 页完成支付（正式版），返回 App 后 Vip 页 onShow
  // 的 fetchVipInfo 刷新权益；拉起失败 opsLogFail('pay_jump_fail') 即时 flush。
  if (isAppEnv()) {
    await new Promise<void>((resolve, reject) => {
      (wx as any).miniapp.launchMiniProgram({
        userName: MINI_PROGRAM_USER_NAME,
        path: 'pages/sub/Vip/Vip',
        miniprogramType: 0, // 正式版：支付在正式版小程序内完成
        success: () => {
          resolve();
          // 空 outTradeNo 约定「App 跳转小程序支付」：Vip 页不轮询订单，权益经 onShow 刷新。
          try { onVirtualPaySuccess && onVirtualPaySuccess(''); } catch {}
        },
        fail: (res: any) => {
          logger.error('跳转小程序支付失败', res);
          opsLogFail('pay_jump_fail', res);
          void flushOpsLog();
          onPayFail && onPayFail();
          reject(new Error(i18n.t('error.payFail')));
        },
      });
    });
    return;
  }

  let data: any;
  try {
    const res = await request.post('/payment/virtual/request', {
      data: {
        vipId: commodity.id,
        env: getPayEnv(),
      },
      cancelToken,
    }, true);
    data = res.data;
  } catch (e: any) {
    logger.error('创建虚拟支付订单失败', e);
    onPayFail && onPayFail();
    throw new Error(e?.message || i18n.t('vip.orderCreateFail'));
  }

  if (!data) {
    onPayFail && onPayFail();
    throw new Error(i18n.t('vip.orderCreateFail'));
  }

  const { signData, paySig, signature, mode, outTradeNo } = data;

  // 支付参数字段是否合法由后端与微信 SDK 保证，前端不再做重复硬校验。
  if (onOrderCreated && outTradeNo) {
    onOrderCreated(outTradeNo);
  }

  await new Promise<void>((resolve, reject) => {
    if (cancelToken?.isCancelled()) {
      cancelOrder(outTradeNo);
      reject(new Error('request:abort'));
      return;
    }
    wx.requestVirtualPayment({
      signData,
      paySig,
      signature,
      mode,
      success() {
        // 支付已成功，即使 cancelToken 已被取消也不应视为 abort；
        // 否则可能导致 VIP 状态不刷新（FU09/FU10）。
        resolve();
        try { onVirtualPaySuccess && onVirtualPaySuccess(outTradeNo); } catch {}
      },
      fail(res: any) {
        cancelOrder(outTradeNo);
        if (cancelToken?.isCancelled()) {
          reject(new Error('request:abort'));
          return;
        }
        logger.error('虚拟支付失败', res);
        let errMsg = i18n.t('error.payFail');
        if (res.errCode === -15011) {
          errMsg = i18n.t('error.iosTestNotSupported');
        } else if (res.errMsg?.includes('SIG_EMPTY')) {
          errMsg = i18n.t('error.paySigEmpty');
        }
        // 统一 onPayFail 契约：订单创建与支付任一环节失败都回调 onPayFail（隐藏 loading 等），
        // 具体错误信息仍通过 reject 抛出供调用方 toast。
        onPayFail && onPayFail();
        reject(new Error(errMsg));
      },
    });
  });
};

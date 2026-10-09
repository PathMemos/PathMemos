// 多端 App 分享适配：微信 OpenSDK 对 shareImageMessage 有两条硬约束——
// ① 缩略图 thumbData ≤ 64KB，超限报 sendOpenReq:fail:check args fail, thumbData.size: N；
// ② 分享到朋友圈（scene=1）要求图片位于用户路径，代码包/临时路径会被拒。
// 本文件统一消化这两条平台差异，App 端分享链路（NoteDetail/Invite）均走这里。
const THUMB_LIMIT = 60 * 1024; // 官方 64KB 上限留 4KB 余量

const fileSize = (path: string): Promise<number> =>
  new Promise((resolve, reject) => {
    wx.getFileSystemManager().getFileInfo({
      filePath: path,
      success: ({ size }) => resolve(size),
      fail: reject,
    });
  });

const compressOnce = (src: string, quality: number): Promise<string> =>
  new Promise((resolve, reject) => {
    wx.compressImage({
      src,
      quality,
      success: ({ tempFilePath }) => resolve(tempFilePath),
      fail: reject,
    });
  });

// 迭代压缩，返回 ≤64KB 的缩略图路径；已达标则原样返回，压缩链路失败时回退原图
// （交由分享层按既有 fail 分支报错，与历史行为一致）。
export async function ensureThumbUnderLimit(src: string): Promise<string> {
  try {
    if ((await fileSize(src)) <= THUMB_LIMIT) return src;
  } catch {
    return src;
  }
  for (const quality of [80, 60, 45, 30, 20]) {
    let out = '';
    try {
      out = await compressOnce(src, quality);
      if ((await fileSize(out)) <= THUMB_LIMIT) return out;
    } catch {
      break;
    }
  }
  return src;
}

// 复制到用户路径（USER_DATA_PATH）并覆盖同名旧文件，规避朋友圈临时路径限制。
export async function toUserPath(path: string, name: string): Promise<string> {
  const dest = `${wx.env.USER_DATA_PATH}/${name}`;
  const fs = wx.getFileSystemManager();
  await new Promise<void>((resolve) => {
    fs.unlink({ filePath: dest, success: () => resolve(), fail: () => resolve() });
  });
  await new Promise<void>((resolve, reject) => {
    fs.copyFile({ srcPath: path, destPath: dest, success: () => resolve(), fail: reject });
  });
  return dest;
}

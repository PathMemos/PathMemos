/**
 * uploadFile 部分失败语义：
 * 整批失败时，已成功项必须把 fileId 回写到输入对象（uploadedId），
 * 使调用方（NoteEdit）能把成功项落为 UPLOADED，重试只补传失败项。
 */
import { beforeEach, describe, expect, it } from '@jest/globals';
import { FILE_TYPE, uploadFile } from '../miniprogram/utils/http';

function installUploadMocks(): void {
  const w = (globalThis as any).wx;
  w.getFileSystemManager = () => ({
    getFileInfo: ({ success }: any) => success({ size: 1000 }),
  });
  w.compressImage = ({ src, success }: any) => success({ tempFilePath: src });
  w.uploadFile = (opts: any) => {
    if (opts.filePath === 'ok-path') {
      setTimeout(() => opts.success({
        statusCode: 200,
        data: JSON.stringify({ code: '0000', data: { files: [{ fileId: 'id-ok', url: 'u' }] } }),
      }), 0);
    } else {
      setTimeout(() => opts.fail({ errMsg: 'network fail' }), 0);
    }
    return { abort() {} };
  };
}

describe('uploadFile 部分失败', () => {
  beforeEach(() => {
    installUploadMocks();
  });

  it('整批失败时把成功项 id 回写到输入对象，失败项不回写', async () => {
    const list: any[] = [
      { path: 'ok-path', type: FILE_TYPE.TO_BE_UPLOADED },
      { path: 'bad-path', type: FILE_TYPE.TO_BE_UPLOADED },
    ];
    await expect(uploadFile(list)).rejects.toThrow();
    expect(list[0].uploadedId).toBe('id-ok');
    expect(list[1].uploadedId).toBeUndefined();
  });

  it('全部成功时返回全部 fileId 且不抛错', async () => {
    const w = (globalThis as any).wx;
    w.uploadFile = (opts: any) => {
      setTimeout(() => opts.success({
        statusCode: 200,
        data: JSON.stringify({ code: '0000', data: { files: [{ fileId: 'id-' + opts.filePath, url: 'u' }] } }),
      }), 0);
      return { abort() {} };
    };
    const ids = await uploadFile([
      { path: 'a', type: FILE_TYPE.TO_BE_UPLOADED },
      { path: 'b', type: FILE_TYPE.TO_BE_UPLOADED },
    ]);
    expect(ids.sort()).toEqual(['id-a', 'id-b']);
  });
});

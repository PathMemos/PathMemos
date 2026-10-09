

export {
  getBaseURL,
  get,
  post,
  put,
  del,
  FILE_TYPE,
  uploadFile,
  updateAvatar,
  getErrorMessage,
  createCancelToken,
  resetLoading,
} from './http';

export {
  getSessionId,
  setSessionId,
  clearSessionId,
  getBaseInfo,
  setBaseInfo,
  getAvatar,
  isLogin,
  login,
  loginAppWithCode,
  loginWithCode,
  needShowXPa,
  setNeedShowXPa,
  getFamilyConfig,
} from './auth';

export {
  fetchVipInfo,
  getVipInfo,
  claimNewUserFreeVip,
} from './vip';

import * as http from './http';
import * as auth from './auth';
import * as vip from './vip';

export default {
  ...http,
  ...auth,
  ...vip,
};

// utils/errorMessages.ts
// 后端错误 message 为英文硬编码（03-api §3 / pkg/errors/codes.go），直接 toast 对非英文
// 用户不友好。这里按 biz_code 映射为三语本地化文案（locales 的 error.biz* 键）；
// 未登记的 biz_code 返回空串，调用方回退后端 message，不阻断。
import i18n from './i18n';

// 与 backend/pkg/errors/codes.go 的 biz_code 枚举保持同源。
const BIZ_CODE_MESSAGE_KEYS: Record<string, string> = {
  SESSION_INVALID: 'error.bizSessionInvalid',
  PHONE_ALREADY_BOUND: 'error.bizPhoneAlreadyBound',
  FAMILY_NOT_FOUND: 'error.bizFamilyNotFound',
  TARGET_IS_PERSONAL_FAMILY: 'error.bizTargetIsPersonalFamily',
  OWNER_CANNOT_LEAVE_FAMILY: 'error.bizOwnerCannotLeaveFamily',
  FAMILY_FULL: 'error.bizFamilyFull',
  CANNOT_REMOVE_SELF: 'error.bizCannotRemoveSelf',
  CANNOT_REMOVE_OWNER: 'error.bizCannotRemoveOwner',
  NOT_VIP: 'error.bizNotVip',
  FREE_VIP_ALREADY_CLAIMED: 'error.bizFreeVipAlreadyClaimed',
  TRIAL_VIP_ALREADY_CLAIMED: 'error.bizTrialVipAlreadyClaimed',
  ORDER_NOT_FOUND: 'error.bizOrderNotFound',
  ALREADY_IN_FAMILY: 'error.bizAlreadyInFamily',
  OPERATION_IN_PROGRESS: 'error.bizOperationInProgress',
  INVALID_FILE_TYPE: 'error.bizInvalidFileType',
  FILE_SIZE_EXCEEDED: 'error.bizFileSizeExceeded',
  USER_IMAGE_STORAGE_LIMIT_EXCEEDED: 'error.bizUserImageStorageLimitExceeded',
  INVALID_COLOR_FORMAT: 'error.bizInvalidColorFormat',
  TEXT_TOO_LONG: 'error.bizTextTooLong',
  INVALID_COORDINATES: 'error.bizInvalidCoordinates',
  AI_DAILY_QUOTA_EXCEEDED: 'error.bizAiDailyQuotaExceeded',
  RATE_LIMITED: 'error.bizRateLimited',
};

export function localizedBizCodeMessage(bizCode?: string | null): string {
  if (!bizCode) return '';
  const key = BIZ_CODE_MESSAGE_KEYS[bizCode];
  return key ? i18n.t(key) : '';
}

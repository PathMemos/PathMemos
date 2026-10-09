// Package errors defines business and HTTP error codes for the application.
package errors

import "net/http"

// code 固定枚举：语义标识符一律放 biz_code（见 docs/spec/03-api.md §3）。
const (
	CodeSuccess               = "0000"
	CodeBadRequest            = "4000"
	CodeUnauthorized          = "4010"
	CodeForbidden             = "4030"
	CodeNotFound              = "4040"
	CodeConflict              = "4090"
	CodeRequestEntityTooLarge = "4130"
	CodeTooManyRequests       = "4290"
	CodeInternalError         = "5001"
)

const (
	BizSessionInvalid                = "SESSION_INVALID"
	BizPhoneAlreadyBound             = "PHONE_ALREADY_BOUND"
	BizFamilyNotFound                = "FAMILY_NOT_FOUND"
	BizTargetIsPersonalFamily        = "TARGET_IS_PERSONAL_FAMILY"
	BizOwnerCannotLeaveFamily        = "OWNER_CANNOT_LEAVE_FAMILY"
	BizFamilyFull                    = "FAMILY_FULL"
	BizCannotRemoveSelf              = "CANNOT_REMOVE_SELF"
	BizCannotRemoveOwner             = "CANNOT_REMOVE_OWNER"
	BizNotVip                        = "NOT_VIP"
	BizFreeVipAlreadyClaimed         = "FREE_VIP_ALREADY_CLAIMED"
	BizTrialVipAlreadyClaimed        = "TRIAL_VIP_ALREADY_CLAIMED"
	BizOrderNotFound                 = "ORDER_NOT_FOUND"
	BizAlreadyInFamily               = "ALREADY_IN_FAMILY"
	BizOperationInProgress           = "OPERATION_IN_PROGRESS"
	BizInvalidFileType               = "INVALID_FILE_TYPE"
	BizFileSizeExceeded              = "FILE_SIZE_EXCEEDED"
	BizUserImageStorageLimitExceeded = "USER_IMAGE_STORAGE_LIMIT_EXCEEDED"
	BizInvalidColorFormat            = "INVALID_COLOR_FORMAT"
	BizTextTooLong                   = "TEXT_TOO_LONG"
	BizInvalidCoordinates            = "INVALID_COORDINATES"
	BizAIDailyQuotaExceeded          = "AI_DAILY_QUOTA_EXCEEDED"
	BizRateLimited                   = "RATE_LIMITED"
	BizRemovedRejoinCooldown         = "REMOVED_REJOIN_COOLDOWN"
)

// HTTPStatus 是 biz_code 到 HTTP 状态的唯一映射。
func HTTPStatus(bizCode string) int {
	switch bizCode {
	case BizFamilyNotFound, BizOrderNotFound:
		return http.StatusNotFound
	case BizSessionInvalid:
		// 凭证过期语义统一映射 401。
		return http.StatusUnauthorized
	case BizTargetIsPersonalFamily, BizOwnerCannotLeaveFamily, BizCannotRemoveSelf, BizCannotRemoveOwner, BizNotVip, BizRemovedRejoinCooldown:
		return http.StatusForbidden
	case BizPhoneAlreadyBound, BizAlreadyInFamily, BizFamilyFull, BizFreeVipAlreadyClaimed, BizTrialVipAlreadyClaimed:
		return http.StatusConflict
	case BizOperationInProgress, BizRateLimited, BizAIDailyQuotaExceeded:
		return http.StatusTooManyRequests
	default:
		return http.StatusBadRequest
	}
}

// CodeForBiz 返回 biz_code 对应的固定 code 枚举；与 HTTPStatus 同源，保证
// 「同一 biz_code 全端点同一 HTTP 状态 + 同一 code」。
func CodeForBiz(bizCode string) string {
	switch bizCode {
	case BizSessionInvalid:
		return CodeUnauthorized
	case BizFamilyNotFound, BizOrderNotFound:
		return CodeNotFound
	case BizTargetIsPersonalFamily, BizOwnerCannotLeaveFamily, BizCannotRemoveSelf, BizCannotRemoveOwner, BizNotVip, BizRemovedRejoinCooldown:
		return CodeForbidden
	case BizPhoneAlreadyBound, BizAlreadyInFamily, BizFamilyFull, BizFreeVipAlreadyClaimed, BizTrialVipAlreadyClaimed:
		return CodeConflict
	case BizOperationInProgress, BizRateLimited, BizAIDailyQuotaExceeded:
		return CodeTooManyRequests
	default:
		return CodeBadRequest
	}
}

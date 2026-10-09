package util

import "github.com/jackc/pgx/v5/pgtype"

func ToString(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func ToInterface(t pgtype.Text) interface{} {
	if !t.Valid || t.String == "" {
		return nil
	}
	return t.String
}

// AvatarURLOrDefault 返回头像 URL：有值返回原值；无值但有默认头像模板时返回模板+userID；否则 nil。
// 统一 family/diary/invite 三处的重复实现。
func AvatarURLOrDefault(avatar pgtype.Text, userID, defaultAvatarURL string) interface{} {
	if !avatar.Valid || avatar.String == "" {
		if defaultAvatarURL == "" {
			return nil
		}
		return defaultAvatarURL + userID
	}
	return avatar.String
}

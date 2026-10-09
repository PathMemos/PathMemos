// Package wechatcrypto 提供微信消息推送的签名校验与 AES 加解密（公众号/支付回调共用标准算法）。
// 独立成包供 wxmp（公众号回调）与 payment（虚拟支付回调）复用，避免各包重复实现导致算法漂移。
package wechatcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CheckSignature 校验微信服务器签名（GET 服务器验证与明文推送）：sha1(sort([token, timestamp, nonce]))。
func CheckSignature(token, signature, timestamp, nonce string) bool {
	if signature == "" || timestamp == "" || nonce == "" || token == "" {
		return false
	}
	arr := []string{token, timestamp, nonce}
	sort.Strings(arr)
	h := sha1.New()
	_, _ = h.Write([]byte(strings.Join(arr, "")))
	expected := hex.EncodeToString(h.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) == 1
}

// CheckEncryptedSignature 校验安全模式（加密推送）的 msg_signature：sha1(sort([token, timestamp, nonce, encrypt]))。
func CheckEncryptedSignature(token, timestamp, nonce, encrypt, msgSignature string) bool {
	if token == "" || timestamp == "" || nonce == "" || encrypt == "" || msgSignature == "" {
		return false
	}
	arr := []string{token, timestamp, nonce, encrypt}
	sort.Strings(arr)
	h := sha1.New()
	_, _ = h.Write([]byte(strings.Join(arr, "")))
	expected := hex.EncodeToString(h.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expected), []byte(msgSignature)) == 1
}

// EncryptedXML 表示微信安全模式推送的 XML 包装。
type EncryptedXML struct {
	ToUserName string `xml:"ToUserName"`
	Encrypt    string `xml:"Encrypt"`
}

func aesKeyFromEncodingAESKey(key string) ([]byte, error) {
	if len(key) != 43 {
		return nil, fmt.Errorf("encoding aes key length must be 43, got %d", len(key))
	}
	k, err := base64.StdEncoding.DecodeString(key + "=")
	if err != nil {
		return nil, fmt.Errorf("decode encoding aes key: %w", err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("decoded aes key length must be 32, got %d", len(k))
	}
	return k, nil
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if blockSize <= 0 || len(data)%blockSize != 0 || len(data) == 0 {
		return nil, errors.New("invalid padding data")
	}
	padding := int(data[len(data)-1])
	if padding > blockSize || padding == 0 {
		return nil, errors.New("invalid padding size")
	}
	for i := 0; i < padding; i++ {
		if data[len(data)-1-i] != byte(padding) {
			return nil, errors.New("invalid padding")
		}
	}
	return data[:len(data)-padding], nil
}

// DecryptMsg 解密微信安全模式推送的密文（base64），返回明文内容与尾部附加的接收方 ID。
// 明文结构：random(16) + msg_len(4, 大端) + msg + receive_id。
// 使用 IV = 密钥前 16 字节（公众号消息加解密标准）。
func DecryptMsg(cipherMsg, encodingAESKey string) (string, string, error) {
	return decryptMsg(cipherMsg, encodingAESKey, false)
}

// DecryptMsgZeroIV 与 DecryptMsg 相同，但使用 IV = 16 字节全零。
// 微信「虚拟支付发货回调」的安全模式实测使用全零 IV（与公众号消息加解密不同），
// 用错 IV 会连第一块都解错，后续 unpad 校验失败（"invalid padding size"）。
func DecryptMsgZeroIV(cipherMsg, encodingAESKey string) (string, string, error) {
	return decryptMsg(cipherMsg, encodingAESKey, true)
}

func decryptMsg(cipherMsg, encodingAESKey string, zeroIV bool) (string, string, error) {
	aesKey, err := aesKeyFromEncodingAESKey(encodingAESKey)
	if err != nil {
		return "", "", err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(cipherMsg)
	if err != nil {
		return "", "", fmt.Errorf("decode cipher msg: %w", err)
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return "", "", errors.New("ciphertext length is not a multiple of block size")
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", "", fmt.Errorf("create aes cipher: %w", err)
	}
	iv := aesKey[:aes.BlockSize]
	if zeroIV {
		iv = make([]byte, aes.BlockSize)
	}
	plainData := make([]byte, len(ciphertext))
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(plainData, ciphertext)

	// 微信消息加解密的填充按 32 字节对齐（腾讯官方加解密实现如此，并非 AES 块大小的 16）。
	// 例如明文长度 % 32 == 3 时填充 29 个 0x1d 字节；按 16 字节 PKCS7 校验会把
	// 填充长度 29 误判为 "invalid padding size"（线上真实回调踩坑）。
	const wechatPadBlockSize = 32
	plainData, err = pkcs7Unpad(plainData, wechatPadBlockSize)
	if err != nil {
		return "", "", fmt.Errorf("unpad: %w", err)
	}

	if len(plainData) < 16+4 {
		return "", "", errors.New("decrypted data too short")
	}
	msgLen := binary.BigEndian.Uint32(plainData[16:20])
	if 20+int(msgLen) > len(plainData) {
		return "", "", errors.New("invalid message length")
	}
	msg := string(plainData[20 : 20+msgLen])
	receiveID := string(plainData[20+msgLen:])
	return msg, receiveID, nil
}

// ParseEncryptedXML 解析微信安全模式推送的 XML 包装，返回密文字段。
func ParseEncryptedXML(body []byte) (EncryptedXML, error) {
	var enc EncryptedXML
	if err := xml.Unmarshal(body, &enc); err != nil {
		return enc, err
	}
	return enc, nil
}

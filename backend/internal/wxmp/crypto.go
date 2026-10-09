// Package wxmp provides related functionality.
package wxmp

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ReplyTextXML builds a passive text reply XML.
func ReplyTextXML(toUser, fromUser, content string) string {
	content = strings.ReplaceAll(content, "]]>", "]]]]><![CDATA[>")
	return fmt.Sprintf(`<xml>
<ToUserName><![CDATA[%s]]></ToUserName>
<FromUserName><![CDATA[%s]]></FromUserName>
<CreateTime>%d</CreateTime>
<MsgType><![CDATA[text]]></MsgType>
<Content><![CDATA[%s]]></Content>
</xml>`, toUser, fromUser, time.Now().Unix(), content)
}

// MessageXML represents the fields we care about in a WeChat server push.
type MessageXML struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	Recognition  string `xml:"Recognition"`
	Event        string `xml:"Event"`
	MsgID        string `xml:"MsgId"`
}

// ParseMessageXML parses WeChat XML body into MessageXML.
func ParseMessageXML(xmlBody string) (MessageXML, error) {
	var msg MessageXML
	if err := xml.Unmarshal([]byte(xmlBody), &msg); err != nil {
		return msg, err
	}
	return msg, nil
}

// aesKeyFromEncodingAESKey decodes a 43-character WeChat EncodingAESKey into a 32-byte AES key.
func aesKeyFromEncodingAESKey(key string) ([]byte, error) {
	if len(key) != 43 {
		return nil, fmt.Errorf("encoding aes key length must be 43")
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

// wechatPadBlockSize 微信消息加解密使用的填充对齐长度。
// 腾讯官方加解密实现按 32 字节对齐填充（并非 AES 块大小的 16）；
// 按 16 对齐生成的密文微信服务器无法解密（线上踩坑）。
const wechatPadBlockSize = 32

// pkcs7Pad pads data to a multiple of blockSize using PKCS7.
func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(data, padtext...)
}

// EncryptMsg encrypts a plaintext message using WeChat AES-256-CBC + PKCS7.
// Returns the base64-encoded ciphertext.
func EncryptMsg(plainMsg, appID, encodingAESKey string) (string, error) {
	aesKey, err := aesKeyFromEncodingAESKey(encodingAESKey)
	if err != nil {
		return "", err
	}

	random := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", fmt.Errorf("generate random: %w", err)
	}

	msgLen := make([]byte, 4)
	binary.BigEndian.PutUint32(msgLen, uint32(len(plainMsg)))

	plainData := make([]byte, 0, 16+4+len(plainMsg)+len(appID))
	plainData = append(plainData, random...)
	plainData = append(plainData, msgLen...)
	plainData = append(plainData, []byte(plainMsg)...)
	plainData = append(plainData, []byte(appID)...)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", fmt.Errorf("create aes cipher: %w", err)
	}
	// 填充按微信规范 32 字节对齐（与公众号官方加解密实现一致），
	// 与 AES 块大小（16）无关。
	plainData = pkcs7Pad(plainData, wechatPadBlockSize)
	ciphertext := make([]byte, len(plainData))
	mode := cipher.NewCBCEncrypter(block, aesKey[:aes.BlockSize])
	mode.CryptBlocks(ciphertext, plainData)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// EncryptedXML represents the WeChat encrypted message wrapper.
type EncryptedXML struct {
	ToUserName string `xml:"ToUserName"`
	Encrypt    string `xml:"Encrypt"`
}

// EncryptReplyXML wraps a plaintext reply XML into WeChat encrypted response format.
func EncryptReplyXML(toUserName, plainXML, appID, encodingAESKey, token, timestamp, nonce string) (string, error) {
	cipherText, err := EncryptMsg(plainXML, appID, encodingAESKey)
	if err != nil {
		return "", err
	}
	signature := sha1Sign(token, timestamp, nonce, cipherText)
	return fmt.Sprintf(`<xml>
<Encrypt><![CDATA[%s]]></Encrypt>
<MsgSignature><![CDATA[%s]]></MsgSignature>
<TimeStamp>%s</TimeStamp>
<Nonce><![CDATA[%s]]></Nonce>
</xml>`, cipherText, signature, timestamp, nonce), nil
}

// 签名校验与解密已统一收敛到 wechatcrypto 包：
// CheckSignature -> wechatcrypto.CheckSignature
// CheckEncryptedSignature -> wechatcrypto.CheckEncryptedSignature
// DecryptMsg -> wechatcrypto.DecryptMsg
func sha1Sign(token, timestamp, nonce, encrypt string) string {
	arr := []string{token, timestamp, nonce, encrypt}
	sort.Strings(arr)
	h := sha1.New()
	_, _ = h.Write([]byte(strings.Join(arr, "")))
	return hex.EncodeToString(h.Sum(nil))
}

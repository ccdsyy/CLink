package mailbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	gcmNonceSize = 12 // AES-GCM 推荐 IV
	hkdfSalt     = "clink-v1"
	hkdfInfo     = "clink-signal"
)

// DeriveKey 由房间码派生 AES-256 密钥（HKDF-SHA256）。
// 设计含义：知道房间码 = 是房间成员；服务器全程接触不到密钥。
func DeriveKey(roomCode string) ([]byte, error) {
	r := io.LimitReader(hkdf.New(sha256.New, []byte(roomCode), []byte(hkdfSalt), []byte(hkdfInfo)), 32)
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("密钥派生失败: %w", err)
	}
	return key, nil
}

// Seal 加密：输出 b64( iv ‖ AES-GCM(ciphertext ‖ tag) )，
// 与 Clipzy 官方 WebCrypto 信封格式字节序一致（已实测互操作）。
func Seal(key, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcmNonceSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, iv, plaintext, nil) // ct ‖ tag
	buf := make([]byte, 0, gcmNonceSize+len(sealed))
	buf = append(buf, iv...)
	buf = append(buf, sealed...)
	return base64.StdEncoding.EncodeToString(buf), nil
}

// Open 解密 Seal 输出的信封。密钥错误 / 内容被篡改 → 立即失败（GCM 认证标签）。
func Open(key []byte, sealedB64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealedB64)
	if err != nil {
		return nil, fmt.Errorf("信封不是合法 base64: %w", err)
	}
	if len(raw) < gcmNonceSize+16 {
		return nil, fmt.Errorf("信封长度异常")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv, ct := raw[:gcmNonceSize], raw[gcmNonceSize:]
	return aead.Open(nil, iv, ct, nil)
}

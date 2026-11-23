package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// digest 把若干字符串按明确的字段边界生成 SHA256 十六进制标识。
// 调用方决定哪些字段参与；本函数只负责编码和哈希。
func digest(parts ...string) string {
	// A JSON array distinguishes component boundaries even if a field contains
	// delimiters. String arrays always encode successfully.
	payload, _ := json.Marshal(parts)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

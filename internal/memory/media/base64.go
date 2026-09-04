package media

import "encoding/base64"

// base64 编解码单独抽出来，让 media.go 的 import 块只留业务依赖。
// 用 StdEncoding：data URL 规范用的是标准表（含 + / =），不是 URL-safe 表。

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

package webui

import (
	"os"
	"strings"

	"net/http"
	"path/filepath"
)

// 附件面：/files/ 中转移目录与 /uploads/ 用户上传目录的下载。

// handleFiles 服务 /files/<name>：仅限 webui_files 中转目录内的文件，
// 防路径穿越（name 必须是纯文件名），Content-Type 按扩展名白名单映射。
func (h *Handler) handleFiles(w http.ResponseWriter, r *http.Request) {
	if webFilesDir == "" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/files/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	fp := filepath.Join(webFilesDir, name)
	f, err := os.Open(fp)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	ct := contentTypeByExt(strings.ToLower(filepath.Ext(name)))
	w.Header().Set("Content-Type", ct)
	// 图片内联展示；其他类型 attachment 下载。X-Content-Type-Options 防 MIME sniff。
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") {
		w.Header().Set("Content-Disposition", "inline; filename="+name)
	} else {
		w.Header().Set("Content-Disposition", "attachment; filename="+name)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func contentTypeByExt(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	case ".json":
		return "application/json"
	case ".txt", ".log", ".md":
		return "text/plain; charset=utf-8"
	default:
		// 未知类型强制二进制流 + nosniff，绝不内联执行
		return "application/octet-stream"
	}
}

package nlp

import (
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// ModelSource 模型来源：本地路径或远程 URL
type ModelSource struct {
	Path string // 本地路径（优先）
	URL  string // 远程下载地址
}

// EnsureModel 确保模型文件存在，返回最终路径
func EnsureModel(dstDir string, src ModelSource, filename string) (string, error) {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return "", fmt.Errorf("create dir %s: %w", dstDir, err)
	}

	dst := filepath.Join(dstDir, filename)

	// 1. 本地路径优先
	if src.Path != "" {
		if _, err := os.Stat(src.Path); err == nil {
			if err := copyFile(src.Path, dst); err != nil {
				return "", fmt.Errorf("copy from %s: %w", src.Path, err)
			}
			log.Printf("[nlp] model ready (local): %s", dst)
			return dst, nil
		}
		log.Printf("[nlp] local path %s not found, trying remote...", src.Path)
	}

	// 2. 远程下载
	if src.URL != "" {
		if _, err := os.Stat(dst); err == nil {
			return dst, nil // 已存在
		}
		log.Printf("[nlp] downloading model from %s ...", src.URL)
		if err := downloadFile(dst, src.URL); err != nil {
			return "", fmt.Errorf("download from %s: %w", src.URL, err)
		}
		return dst, nil
	}

	return "", fmt.Errorf("model not found: no local path or remote URL")
}

func downloadFile(dst, url string) error {
	tmp := dst + ".download." + fmt.Sprintf("%x", md5.Sum([]byte(url)))

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http status %s", resp.Status)
	}

	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp %s: %w", tmp, err)
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}

	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("[nlp] downloaded %d bytes to %s", written, dst)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

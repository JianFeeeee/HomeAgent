package skillmgr

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// packSkill 把 skill 目录打包为 .skm（tar.gz）。返回打包的文件数。
// 包内路径统一为 <skillName>/<相对路径>，解包端按首段目录还原。
func packSkill(srcDir, outPath string) (int, error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		return 0, fmt.Errorf("stat skill dir: %w", err)
	}
	if !info.IsDir() {
		// 单 .md 文件 skill：包装成只含一个文件的包
		return packSingleFile(srcDir, outPath)
	}

	skillName := filepath.Base(srcDir)
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return 0, err
	}

	f, err := os.Create(outPath)
	if err != nil {
		return 0, fmt.Errorf("create pack: %w", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	count := 0
	err = filepath.Walk(srcDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// 跳过隐藏文件与 node_modules
		rel, rerr := filepath.Rel(srcDir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") || part == "node_modules" {
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join(skillName, rel))
		if fi.IsDir() {
			hdr.Name += "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil // 跳过符号链接等非常规文件，避免路径逃逸
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("pack walk: %w", err)
	}
	return count, nil
}

func packSingleFile(mdPath, outPath string) (int, error) {
	skillName := strings.TrimSuffix(filepath.Base(mdPath), ".md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return 0, err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	hdr := &tar.Header{
		Name: filepath.ToSlash(filepath.Join(skillName, SkillFileName)),
		Mode: 0644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	if _, err := tw.Write(data); err != nil {
		return 0, err
	}
	return 1, nil
}

// unpackSkill 解压 .skm 到临时目录后整体 rename 为 dstParent。
// 校验包内所有条目必须位于同一首段目录下，防止 TarSlip 路径逃逸。
// 返回解出的文件数。
func unpackSkill(packPath, dstParent string) (int, error) {
	f, err := os.Open(packPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("not a gzip pack: %w", err)
	}
	defer gz.Close()

	tw := tar.NewReader(gz)
	var root string // 包内唯一根目录名
	count := 0

	tmpDir, err := os.MkdirTemp(filepath.Dir(dstParent), ".skillmgr-unpack-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmpDir)

	for {
		hdr, err := tw.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, fmt.Errorf("read tar: %w", err)
		}

		// 清洗路径：拒绝绝对路径与 .. 逃逸
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return count, fmt.Errorf("unsafe entry in pack: %q", hdr.Name)
		}
		parts := strings.Split(name, string(filepath.Separator))
		if len(parts) < 2 {
			return count, fmt.Errorf("entry outside skill root dir: %q", hdr.Name)
		}
		if root == "" {
			root = parts[0]
		} else if parts[0] != root {
			return count, fmt.Errorf("multiple root dirs in pack: %q vs %q", root, parts[0])
		}

		target := filepath.Join(tmpDir, name)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0755); err != nil {
				return count, err
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // 跳过符号链接等
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return count, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0755)
		if err != nil {
			return count, err
		}
		_, err = io.Copy(out, tw)
		out.Close()
		if err != nil {
			return count, err
		}
		count++
	}

	if root == "" {
		return 0, fmt.Errorf("empty pack")
	}
	// 校验解包结果是合法 skill（有 SKILL.md 或 skill.json）
	unpackedRoot := filepath.Join(tmpDir, root)
	if !fileExists(filepath.Join(unpackedRoot, SkillFileName)) && !fileExists(filepath.Join(unpackedRoot, MetaFileName)) {
		return count, fmt.Errorf("pack has no %s or %s at its root", SkillFileName, MetaFileName)
	}
	// rename 到期望位置（包内根名与期望名不同时先改根目录名再落位）
	src := unpackedRoot
	if filepath.Base(dstParent) != root {
		src = filepath.Join(tmpDir, ".renamed")
		if err := os.Rename(unpackedRoot, src); err != nil {
			return count, err
		}
	}
	if err := os.Rename(src, dstParent); err != nil {
		return count, err
	}
	return count, nil
}

// copyDir 递归复制目录（跳过隐藏文件与 node_modules）。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return os.MkdirAll(dst, 0755)
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") || part == "node_modules" {
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		out.Close()
		return err
	})
}

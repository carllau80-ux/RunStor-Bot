// Package fileio 提供两件与持久化有关的小工具: 有界读取与原子写入。
//
// 抽出来的原因: 这两件事在四个包 (知识库、巡检历史、对话历史、运行态配置) 里各写过
// 一遍, 而每一遍都漏了同一件事 —— 目录 fsync, 以及读取没有上限。共用一份实现,
// 修一处就都修上了。
package fileio

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ReadBounded 读取文件, 并在**读之前**按大小上限拦一次。
//
// 为什么不能"先读进来再判断大小": 那次读取本身就已经把内存吃掉了 —— 一个 8 GB 的
// 文件会让服务直接 OOM, 而它本该只是"这份文档超限, 跳过"。
// LimitReader 是第二道保险: stat 与 read 之间文件仍可能变大 (TOCTOU)。
//
// limit <= 0 表示不限制 (只用于确实需要读全文且已经由调用方限制过的场景)。
func ReadBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s 是一个目录", path)
	}
	if limit > 0 && info.Size() > limit {
		return nil, fmt.Errorf("文件 %s 大小 %d 字节, 超过上限 %d 字节",
			filepath.Base(path), info.Size(), limit)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	reader := io.Reader(file)
	if limit > 0 {
		// 多读一个字节: 读满 limit+1 就说明它在 stat 之后又变大了
		reader = io.LimitReader(file, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("文件 %s 在读的过程中超过上限 %d 字节",
			filepath.Base(path), limit)
	}
	return data, nil
}

// WriteAtomic 原子替换写入: 临时文件 → fsync → rename → fsync 目录。
//
// 四步都有理由:
//   - 临时文件 + rename: 中途崩了也不会留下半个文件, 读到的要么是旧内容要么是新内容;
//   - 写前 chmod: 避免"权限暂时过宽"的窗口 (密钥文件尤其在意);
//   - 临时文件 fsync: 保证内容真的落盘了才 rename, 否则掉电后可能得到一个空文件;
//   - 目录 fsync: rename 本身也是目录项的一次修改, 不同步的话掉电后新文件可能"没出现过"。
//
// 目录 fsync 的失败**不当作写失败**: 数据已经落盘, 只是崩溃一致性弱一点; 为它报错会让
// 一次成功的保存被显示成失败, 反而误导。容忍 EINVAL/ENOTSUP (有些文件系统不支持)。
func WriteAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建目录失败 (%s): %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	if err := temp.Chmod(perm); err != nil {
		_ = temp.Close()
		return fmt.Errorf("设置权限失败: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("写入失败: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("内容落盘失败: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("替换文件失败 (%s): %w", path, err)
	}

	syncDir(dir)
	return nil
}

// syncDir 同步目录项, 让 rename 的结果在掉电后仍然存在。
func syncDir(dir string) {
	handle, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return // 该文件系统不支持目录同步
		}
	}
}

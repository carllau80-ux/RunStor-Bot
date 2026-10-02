package fileio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomicReplacesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	if err := WriteAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := WriteAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(data) != "second" {
		t.Errorf("内容不符: %q", data)
	}
}

// 权限必须是最终权限: 密钥文件不能有"权限暂时过宽"的窗口。
func TestWriteAtomicAppliesPermBeforeRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := WriteAtomic(path, []byte("k"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("权限应为 0600, 实际 %04o", mode)
	}
}

// 目录不存在时自动创建, 且不留临时文件残渣。
func TestWriteAtomicCreatesDirAndCleansUp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deeper")
	path := filepath.Join(dir, "data.json")
	if err := WriteAtomic(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("残留了临时文件: %s", entry.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "data.json" {
		t.Errorf("目录内容不符: %v", entries)
	}
}

func TestReadBoundedAcceptsSmallFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.md")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	data, err := ReadBounded(path, 1024)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("内容不符: %q", data)
	}
}

// 超大文件必须在**读之前**就被拦住 —— 否则那次读取已经把内存吃光了。
func TestReadBoundedRejectsOversizedFileBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	_, err := ReadBounded(path, 100)
	if err == nil {
		t.Fatal("超过上限应报错")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("报错应说明超限: %v", err)
	}
	for _, fragment := range []string{"huge.md", "4096", "100"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("报错应包含 %q 便于排查: %v", fragment, err)
		}
	}
}

func TestReadBoundedRejectsDirectory(t *testing.T) {
	if _, err := ReadBounded(t.TempDir(), 1024); err == nil {
		t.Error("目录应被拒绝")
	}
}

func TestReadBoundedWithoutLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := ReadBounded(path, 0); err != nil {
		t.Errorf("limit=0 表示不限制: %v", err)
	}
}

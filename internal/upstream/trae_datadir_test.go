package upstream

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTraeDataDirResolution 锁定数据目录的回落顺序。
//
// 回归背景：容器里没有 %APPDATA%，早先只认它，导致扫码登录拿到 JWT 后
// 「写入 Trae 账号文件失败: 未配置 trae.data_dir 且无法读取 APPDATA 环境变量」。
func TestTraeDataDirResolution(t *testing.T) {
	// t.Setenv 自动还原，避免污染其它用例。
	t.Run("显式配置优先", func(t *testing.T) {
		t.Setenv("TRAE_DATA_DIR", filepath.Join(t.TempDir(), "from-env"))
		got, err := traeDataDir("/explicit/dir")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/explicit/dir" {
			t.Fatalf("got %q, want /explicit/dir", got)
		}
	})

	t.Run("环境变量次之", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "from-env")
		t.Setenv("TRAE_DATA_DIR", want)
		got, err := traeDataDir("  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("没有 APPDATA 也不报错", func(t *testing.T) {
		t.Setenv("APPDATA", "")
		t.Setenv("TRAE_DATA_DIR", "")
		t.Setenv("XDG_DATA_HOME", "")
		got, err := traeDataDir("")
		if err != nil {
			t.Fatalf("容器/非 Windows 场景不应报错: %v", err)
		}
		if strings.TrimSpace(got) == "" {
			t.Fatal("应推导出非空目录")
		}
		if runtime.GOOS == "darwin" && !strings.Contains(got, "Application Support") {
			t.Fatalf("macOS 应回落到 Application Support，得到 %q", got)
		}
		if runtime.GOOS == "linux" && !strings.Contains(got, ".local/share") {
			t.Fatalf("linux 应回落到 ~/.local/share，得到 %q", got)
		}
	})

	t.Run("APPDATA 存在时仍走 TraeWorkAssistant", func(t *testing.T) {
		appData := t.TempDir()
		t.Setenv("APPDATA", appData)
		t.Setenv("TRAE_DATA_DIR", "")
		got, err := traeDataDir("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(appData, "TraeWorkAssistant")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// TestSaveTraeAccountWithoutAppData 端到端确认：没有 APPDATA 时账号能落盘并被读回。
func TestSaveTraeAccountWithoutAppData(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", "")
	t.Setenv("TRAE_DATA_DIR", dir)

	added, err := SaveTraeAccount("", TraeAccount{
		Name:   "测试号",
		UserID: "u-1",
		JWT:    "Cloud-IDE-JWT header.payload.sig",
	})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if !added {
		t.Fatal("首次写入应返回 added=true")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(traeAccountsFile))); err != nil {
		t.Fatalf("账号文件未落盘: %v", err)
	}

	list, err := LoadTraeAccounts("")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(list) != 1 || list[0].UserID != "u-1" {
		t.Fatalf("读回结果不符: %+v", list)
	}
}

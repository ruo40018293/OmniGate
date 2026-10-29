package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 改名（wild-work/workbuddy-api → OmniGate）时最容易出的两类事故，用测试钉住：
//  1. 只换目录名不迁移 → 老用户升级后「账号全没了」；
//  2. 迁移失败却返回新建的空目录 → 同上。

func macSupportDir(home string, name string) string {
	return filepath.Join(home, "Library", "Application Support", name)
}

// writeFile 在 dir 下写一个文件（自动建目录），模拟旧版留下的数据。
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func TestMacDataDirCreatesNewWhenFresh(t *testing.T) {
	home := t.TempDir()
	got, err := macDataDir(home)
	if err != nil {
		t.Fatalf("macDataDir 出错: %v", err)
	}
	want := macSupportDir(home, "OmniGate")
	if got != want {
		t.Fatalf("新装应落到 %q，实际 %q", want, got)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("目录应已创建: err=%v", err)
	}
}

func TestMacDataDirMigratesLegacyDir(t *testing.T) {
	home := t.TempDir()
	legacy := macSupportDir(home, "WildWork")
	writeFile(t, legacy, "config.json", `{"listen":":7863"}`)
	writeFile(t, filepath.Join(legacy, "auths"), "workbuddy-1.json", `{"auth":{}}`)

	got, err := macDataDir(home)
	if err != nil {
		t.Fatalf("macDataDir 出错: %v", err)
	}
	want := macSupportDir(home, "OmniGate")
	if got != want {
		t.Fatalf("应迁移到 %q，实际 %q", want, got)
	}

	// 数据必须真的跟着搬走（不是新建一个空壳）
	raw, err := os.ReadFile(filepath.Join(want, "config.json"))
	if err != nil {
		t.Fatalf("迁移后读不到 config.json: %v", err)
	}
	if string(raw) != `{"listen":":7863"}` {
		t.Fatalf("config.json 内容不一致: %q", string(raw))
	}
	if _, err := os.Stat(filepath.Join(want, "auths", "workbuddy-1.json")); err != nil {
		t.Fatalf("子目录未迁移: %v", err)
	}

	// rename 语义：旧目录不该继续存在（否则下次启动又会被判断为「有旧数据」）
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("旧目录应已搬走，err=%v", err)
	}
}

func TestMacDataDirKeepsLegacyUntouchedWhenNewExists(t *testing.T) {
	home := t.TempDir()
	legacy := macSupportDir(home, "WildWork")
	writeFile(t, legacy, "legacy-only.json", `keep`)
	writeFile(t, macSupportDir(home, "OmniGate"), "config.json", `{"listen":":7899"}`)

	got, err := macDataDir(home)
	if err != nil {
		t.Fatalf("macDataDir 出错: %v", err)
	}
	if want := macSupportDir(home, "OmniGate"); got != want {
		t.Fatalf("新目录已存在时应以它为准，实际 %q", got)
	}
	// 不得删除用户手工保留的旧目录
	if _, err := os.Stat(filepath.Join(legacy, "legacy-only.json")); err != nil {
		t.Fatalf("不应改动旧目录: %v", err)
	}
}

func TestDataHomeEnvPrefersOmniGate(t *testing.T) {
	t.Setenv("OMNIGATE_HOME", "/new-home")
	t.Setenv("WILDWORK_HOME", "/old-home")
	if got := dataHomeEnv(); got != "/new-home" {
		t.Fatalf("应优先 OMNIGATE_HOME，实际 %q", got)
	}

	t.Setenv("OMNIGATE_HOME", "")
	if got := dataHomeEnv(); got != "/old-home" {
		t.Fatalf("旧名 WILDWORK_HOME 应作为兜底，实际 %q", got)
	}

	t.Setenv("WILDWORK_HOME", "")
	if got := dataHomeEnv(); got != "" {
		t.Fatalf("两个都没设时应返回空串，实际 %q", got)
	}
}

// workDir 必须真的用上这两个变量（否则上面的优先级只是死代码）。
func TestWorkDirHonoursDataHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OMNIGATE_HOME", dir)
	t.Setenv("WILDWORK_HOME", "")
	if got := workDir(); got != dir {
		t.Fatalf("workDir 应返回 OMNIGATE_HOME=%q，实际 %q", dir, got)
	}

	legacy := t.TempDir()
	t.Setenv("OMNIGATE_HOME", "")
	t.Setenv("WILDWORK_HOME", legacy)
	if got := workDir(); got != legacy {
		t.Fatalf("workDir 应回退到 WILDWORK_HOME=%q，实际 %q", legacy, got)
	}
}

package config

import (
	"strings"
	"testing"
)

// fakeKV 是 PersonaKV 的最小实现（模拟插件侧 SettingsAPI）。
type fakeKV struct{ m map[string]string }

func (f *fakeKV) GetCore(k string) (interface{}, error) { return f.m[k], nil }
func (f *fakeKV) SetCore(k string, v interface{}) error {
	f.m[k] = v.(string)
	return nil
}

// 三选一语义 + 「只问一次」标记：这是首启向导与 persona_set 工具共用的同一份实现。
func TestSetPersonaKV(t *testing.T) {
	t.Run("custom 写入内容并要求重启", func(t *testing.T) {
		kv := &fakeKV{m: map[string]string{}}
		restart, err := SetPersonaKV(kv, PersonaModeCustom, "你是测试人格")
		if err != nil || !restart {
			t.Fatalf("custom 应成功且需重启: restart=%v err=%v", restart, err)
		}
		if kv.m[PersonaPromptKey] != "你是测试人格" || kv.m[PersonaInitMarkerKey] != "1" {
			t.Fatalf("落库不对: %+v", kv.m)
		}
		if !PersonaInitializedKV(kv) {
			t.Fatal("打过标记应报告已确认")
		}
	})

	t.Run("default 写默认模板且无需重启", func(t *testing.T) {
		kv := &fakeKV{m: map[string]string{}}
		restart, err := SetPersonaKV(kv, PersonaModeDefault, "")
		if err != nil || restart {
			t.Fatalf("default 不应需重启: restart=%v err=%v", restart, err)
		}
		if kv.m[PersonaPromptKey] != DefaultPersonaPrompt {
			t.Fatal("default 应写入内置默认模板")
		}
	})

	t.Run("later 保持人格并打标记", func(t *testing.T) {
		kv := &fakeKV{m: map[string]string{PersonaPromptKey: "原有人格"}}
		if _, err := SetPersonaKV(kv, PersonaModeLater, ""); err != nil {
			t.Fatal(err)
		}
		if kv.m[PersonaPromptKey] != "原有人格" {
			t.Fatal("later 不应改动人格")
		}
		if kv.m[PersonaInitMarkerKey] != "1" {
			t.Fatal("later 也必须打标记（否则每次启动都问）")
		}
	})

	t.Run("非法输入必须拒绝且不打标记", func(t *testing.T) {
		for _, c := range []struct{ mode, content string }{
			{PersonaModeCustom, "   "}, // 空内容
			{"nope", ""},               // 未知 mode
		} {
			kv := &fakeKV{m: map[string]string{}}
			if _, err := SetPersonaKV(kv, c.mode, c.content); err == nil {
				t.Fatalf("mode=%q content=%q 应报错", c.mode, c.content)
			}
			if kv.m[PersonaInitMarkerKey] == "1" {
				t.Fatalf("mode=%q 被拒时不得打标记（否则向导会被跳过）", c.mode)
			}
		}
	})

	t.Run("CurrentPersonaKV 未设置时回落默认", func(t *testing.T) {
		kv := &fakeKV{m: map[string]string{}}
		if got := CurrentPersonaKV(kv); got != DefaultPersonaPrompt {
			t.Fatal("未设置应回落默认模板")
		}
	})
}

// 内核直连注册表的入口必须与 KV 版行为一致（同一份实现的两个薄入口）。
func TestSetPersonaRegistryEntry(t *testing.T) {
	dir := t.TempDir()
	reg := NewConfigRegistry(dir + "/config.db")
	reg.SeedDefaults(dir)
	defer reg.Close()

	if PersonaInitialized(reg) {
		t.Fatal("全新实例不应已确认人格")
	}
	if _, err := SetPersona(reg, PersonaModeCustom, "内核侧人格"); err != nil {
		t.Fatal(err)
	}
	if !PersonaInitialized(reg) {
		t.Fatal("内核侧落库后应报告已确认")
	}
	if got := reg.GetString(PersonaPromptKey, ""); !strings.Contains(got, "内核侧人格") {
		t.Fatalf("注册表里没有人格内容: %q", got)
	}
}

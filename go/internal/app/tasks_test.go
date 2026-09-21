package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTaskStateIsCreatedPrivatelyBesideSettingsAndBindsAPIRealm(t *testing.T) {
	cfg := fixtureConfig(t)
	a, err := New(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	file := filepath.Join(filepath.Dir(cfg.WebSettingsDir), "tasks.json")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state=%v err=%v", info, err)
	}
	before, err := a.taskIdentity()
	if err != nil {
		t.Fatal(err)
	}
	a.Config.BaseURL = "https://different.example"
	after, err := a.taskIdentity()
	if err != nil || after == before {
		t.Fatal("API endpoint was not included in task binding")
	}
}

func TestInvalidTaskStateFailsAssemblyWithoutOverwriting(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.TasksFile = filepath.Join(filepath.Dir(cfg.WebSettingsDir), "tasks.json")
	const invalid = `{"version":999,"records":[]}`
	if err := os.WriteFile(cfg.TasksFile, []byte(invalid), 0600); err != nil {
		t.Fatal(err)
	}
	if a, err := New(cfg, "test"); err == nil {
		a.Close()
		t.Fatal("unknown task state format accepted")
	}
	data, err := os.ReadFile(cfg.TasksFile)
	if err != nil || string(data) != invalid {
		t.Fatalf("state changed: %q %v", data, err)
	}
}

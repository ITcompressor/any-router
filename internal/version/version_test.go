package version

import (
	"fmt"
	"testing"
)

func TestStringWithoutMetadata(t *testing.T) {
	// дефолты явно: тест не должен зависеть от ldflags сборщика
	commit, buildTime := Commit, BuildTime
	Commit, BuildTime = "dev", "unknown"
	defer func() { Commit, BuildTime = commit, buildTime }()

	if got := String(); got != Version {
		t.Errorf("без метаданных: %q", got)
	}
	if got := Short(); got != Version {
		t.Errorf("short без метаданных: %q", got)
	}
}

func TestStringWithMetadata(t *testing.T) {
	commit, buildTime := Commit, BuildTime
	Commit, BuildTime = "abc1234", "2026-01-02T03:04:05Z"
	defer func() { Commit, BuildTime = commit, buildTime }()

	if got := String(); got != fmt.Sprintf("%s (abc1234, 2026-01-02T03:04:05Z)", Version) {
		t.Errorf("с метаданными: %q", got)
	}
	if got := Short(); got != Version+" (abc1234)" {
		t.Errorf("short: %q", got)
	}
}
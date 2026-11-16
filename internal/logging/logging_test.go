package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": LevelDebug, "DEBUG": LevelDebug,
		"info": LevelInfo, "": LevelInfo, "что-то странное": LevelInfo,
		"warn": LevelWarn, "WARN": LevelWarn,
		"error": LevelError, "ERROR": LevelError,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %s, хочу %s", in, got, want)
		}
	}
}

func TestLoggerLevels(t *testing.T) {
	var buf bytes.Buffer
	l := NewWriter(&buf, "warn")
	l.Debugf("скрыто") // ниже уровня
	l.Infof("скрыто")  // ниже уровня
	l.Warnf("видно %d", 1)
	l.Errorf("видно тоже")
	out := buf.String()
	if strings.Contains(out, "скрыто") {
		t.Errorf("сообщения ниже уровня просочились: %q", out)
	}
	if !strings.Contains(out, "видно 1") {
		t.Errorf("WARN-строка потерялась: %q", out)
	}
	if !strings.Contains(out, "видно тоже") {
		t.Errorf("ERROR-строка потерялась: %q", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// "YYYY-MM-DDTHH:MM:SSZ" — ровно 20 символов, затем пробел.
		if len(line) < 21 || line[20] != ' ' {
			t.Errorf("строка без метки времени в начале: %q", line)
		}
	}
}

package control

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/sirupsen/logrus"
)

func newControlJSONLogger(output io.Writer) *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(output)
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.JSONFormatter{DisableTimestamp: true})
	return logger
}

func decodeControlJSONLogs(
	t *testing.T,
	output []byte,
) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func controlEventsNamed(
	events []map[string]any,
	name string,
) []map[string]any {
	var matching []map[string]any
	for _, event := range events {
		if event["event"] == name {
			matching = append(matching, event)
		}
	}
	return matching
}

func assertControlLogExcludes(
	t *testing.T,
	output string,
	forbidden ...string,
) {
	t.Helper()
	for _, value := range forbidden {
		if bytes.Contains([]byte(output), []byte(value)) {
			t.Errorf("log output contains forbidden value %q: %s", value, output)
		}
	}
}

type controlPanicLogHook struct{}

func (controlPanicLogHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (controlPanicLogHook) Fire(*logrus.Entry) error {
	panic("control log hook must be isolated")
}

package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAgentRunLogReturnsNewestLinesAndCursor(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "run.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, size, hasMore, nextBefore, _, err := readAgentRunLog(dataDir, path, nil, 2)
	if err != nil || strings.Join(lines, ",") != "four,five" || size != 24 || !hasMore || nextBefore == nil {
		t.Fatalf("lines=%q size=%d hasMore=%v nextBefore=%v err=%v", lines, size, hasMore, nextBefore, err)
	}
	lines, _, hasMore, nextBefore, _, err = readAgentRunLog(dataDir, path, nextBefore, 2)
	if err != nil || strings.Join(lines, ",") != "two,three" || !hasMore || nextBefore == nil {
		t.Fatalf("lines=%q hasMore=%v nextBefore=%v err=%v", lines, hasMore, nextBefore, err)
	}
	lines, _, hasMore, nextBefore, _, err = readAgentRunLog(dataDir, path, nextBefore, 2)
	if err != nil || strings.Join(lines, ",") != "one" || hasMore || nextBefore != nil {
		t.Fatalf("lines=%q hasMore=%v nextBefore=%v err=%v", lines, hasMore, nextBefore, err)
	}
}

func TestReadAgentRunLogDoesNotSplitLongEntries(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "run.log")
	long := strings.Repeat("x", 300<<10)
	if err := os.WriteFile(path, []byte("before\n"+long+"\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _, hasMore, _, _, err := readAgentRunLog(dataDir, path, nil, 2)
	if err != nil || strings.Join(lines, ",") != long+",after" || !hasMore {
		t.Fatalf("line sizes=%v hasMore=%v err=%v", []int{len(lines[0]), len(lines[1])}, hasMore, err)
	}
}

func TestReadAgentRunLogRejectsOutsidePath(t *testing.T) {
	_, _, _, _, _, err := readAgentRunLog(t.TempDir(), filepath.Join(t.TempDir(), "outside.log"), nil, 50)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err=%v", err)
	}
}

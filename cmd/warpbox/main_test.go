package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRunItemsHook_WorkingDir(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	workDir := t.TempDir()

	// Write a script that creates a sentinel file in the working directory.
	script := filepath.Join(workDir, "test.sh")
	sentinel := filepath.Join(workDir, "hook_ran")
	scriptContent := []byte("#!/bin/sh\ntouch hook_ran\n")
	if err := os.WriteFile(script, scriptContent, 0755); err != nil {
		t.Fatal(err)
	}

	// The hook command uses a relative path. With workDir set, it should
	// resolve the script relative to workDir and execute successfully.
	runItemsHook("sh test.sh", 30, []string{"dummy"}, workDir)

	if _, err := os.Stat(sentinel); os.IsNotExist(err) {
		t.Error("script did not run in the expected working directory; sentinel file not found")
	}
}

func TestRunItemsHook_WorkingDirWithoutItems(t *testing.T) {
	// When items is empty, the hook should return early without executing.
	// This should not panic or hang.
	runItemsHook("sh anything.sh", 30, nil, t.TempDir())
}

func TestRunItemsHook_AbsolutePath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "abs.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch abs_hook_ran\n"), 0755); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	sentinel := filepath.Join(workDir, "abs_hook_ran")

	// Using an absolute path for the script, and a different workDir.
	// The script resolves (absolute path), creates sentinel file in workDir.
	runItemsHook("sh "+script, 30, []string{"dummy"}, workDir)

	if _, err := os.Stat(sentinel); os.IsNotExist(err) {
		t.Error("absolute path script did not execute; sentinel file not found")
	}
}

func TestRunItemsHook_ErrorDoesNotPanic(t *testing.T) {
	// Running a non-existent script should log an error but not panic.
	dir := t.TempDir()
	runItemsHook("sh nonexistent.sh", 5, []string{"dummy"}, dir)
}

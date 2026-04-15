package main

import (
	"strings"
	"testing"
)

func TestNewKeeperRequiresMaxOpenSQLiteEnv(t *testing.T) {
	t.Setenv("DB_PATH", t.TempDir()+"/test.db")
	t.Setenv("MAX_OPEN_SQLITE", "")
	t.Setenv("MAX_IDLE_SQLITE", "1")

	_, err := NewKeeper()
	if err == nil {
		t.Fatal("expected error when MAX_OPEN_SQLITE is missing")
	}
	if !strings.Contains(err.Error(), "MAX_OPEN_SQLITE is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewKeeperRequiresMaxIdleSQLiteEnv(t *testing.T) {
	t.Setenv("DB_PATH", t.TempDir()+"/test.db")
	t.Setenv("MAX_OPEN_SQLITE", "1")
	t.Setenv("MAX_IDLE_SQLITE", "")

	_, err := NewKeeper()
	if err == nil {
		t.Fatal("expected error when MAX_IDLE_SQLITE is missing")
	}
	if !strings.Contains(err.Error(), "MAX_IDLE_SQLITE is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewKeeperRejectsIdleGreaterThanOpen(t *testing.T) {
	t.Setenv("DB_PATH", t.TempDir()+"/test.db")
	t.Setenv("MAX_OPEN_SQLITE", "1")
	t.Setenv("MAX_IDLE_SQLITE", "2")

	_, err := NewKeeper()
	if err == nil {
		t.Fatal("expected error when MAX_IDLE_SQLITE > MAX_OPEN_SQLITE")
	}
	if !strings.Contains(err.Error(), "cannot be greater than") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewKeeperInitializesWithRequiredSQLiteEnv(t *testing.T) {
	t.Setenv("DB_PATH", t.TempDir()+"/test.db")
	t.Setenv("MAX_OPEN_SQLITE", "1")
	t.Setenv("MAX_IDLE_SQLITE", "1")

	k, err := NewKeeper()
	if err != nil {
		t.Fatalf("expected keeper to initialize, got error: %v", err)
	}
	if k == nil {
		t.Fatal("expected non-nil keeper")
	}

	vinyls := k.AllVinyl()
	if vinyls == nil {
		t.Fatal("expected non-nil vinyl slice")
	}
}

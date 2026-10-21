package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var migrationFilePattern = regexp.MustCompile(`^([0-9]{3})_([a-z0-9_]+)\.(up|down)\.sql$`)

func TestMigrationsDirectoryIntegrity(t *testing.T) {
	migrationsDir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir %s: %v", migrationsDir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("no files found in %s", migrationsDir)
	}

	upByPrefix := make(map[int]string)
	downByPrefix := make(map[int]string)
	upBase := make(map[string]bool)
	downBase := make(map[string]bool)

	highest := -1

	for _, entry := range entries {
		if entry.IsDir() {
			t.Errorf("unexpected directory in migrations: %s", entry.Name())
			continue
		}
		name := entry.Name()
		m := migrationFilePattern.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("migration file %q does not match NNN_name.(up|down).sql", name)
			continue
		}
		prefixStr, slug, direction := m[1], m[2], m[3]
		prefix, err := strconv.Atoi(prefixStr)
		if err != nil {
			t.Errorf("invalid numeric prefix in %q: %v", name, err)
			continue
		}
		if prefix > highest {
			highest = prefix
		}

		base := prefixStr + "_" + slug
		switch direction {
		case "up":
			if prev, dup := upByPrefix[prefix]; dup {
				t.Errorf("duplicate .up.sql numeric prefix %03d: %q and %q", prefix, prev, name)
			}
			upByPrefix[prefix] = name
			upBase[base] = true
		case "down":
			if prev, dup := downByPrefix[prefix]; dup {
				t.Errorf("duplicate .down.sql numeric prefix %03d: %q and %q", prefix, prev, name)
			}
			downByPrefix[prefix] = name
			downBase[base] = true
		}
	}

	for base := range upBase {
		if !downBase[base] {
			t.Errorf("missing .down.sql for %s.up.sql", base)
		}
	}
	for base := range downBase {
		if !upBase[base] {
			t.Errorf("missing .up.sql for %s.down.sql", base)
		}
	}

	if highest < 64 {
		t.Fatalf("highest migration prefix = %d, want >= 64", highest)
	}

	next := nextMigrationNumber(upByPrefix)
	if next <= highest {
		t.Fatalf("next migration number %d must be > highest prefix %d", next, highest)
	}
	if next != highest+1 {
		t.Fatalf("next migration number = %d, want %d", next, highest+1)
	}
}

func nextMigrationNumber(upByPrefix map[int]string) int {
	if len(upByPrefix) == 0 {
		return 1
	}
	prefixes := make([]int, 0, len(upByPrefix))
	for p := range upByPrefix {
		prefixes = append(prefixes, p)
	}
	sort.Ints(prefixes)
	return prefixes[len(prefixes)-1] + 1
}

func TestMakefileMigrateCreateUsesHighestPrefix(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available in PATH")
	}

	migrationsDir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	highest := -1
	tmpBackend := t.TempDir()
	tmpMigrations := filepath.Join(tmpBackend, "migrations")
	if err := os.MkdirAll(tmpMigrations, 0o755); err != nil {
		t.Fatalf("mkdir temp migrations: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(entry.Name())
		if m != nil && m[3] == "up" {
			if p, err := strconv.Atoi(m[1]); err == nil && p > highest {
				highest = p
			}
		}
		if err := os.WriteFile(filepath.Join(tmpMigrations, entry.Name()), nil, 0o644); err != nil {
			t.Fatalf("seed temp migration %s: %v", entry.Name(), err)
		}
	}

	makefilePath, err := filepath.Abs(filepath.Join("..", "..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("resolve Makefile path: %v", err)
	}

	cmd := exec.Command("make", "-f", makefilePath, "migrate-create", "BACKEND_DIR="+tmpBackend)
	cmd.Stdin = strings.NewReader("verify_prefix\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make migrate-create failed: %v\noutput: %s", err, out)
	}

	wantPrefix := fmt.Sprintf("%03d", highest+1)
	for _, ext := range []string{"up.sql", "down.sql"} {
		wantFile := filepath.Join(tmpMigrations, fmt.Sprintf("%s_verify_prefix.%s", wantPrefix, ext))
		if _, err := os.Stat(wantFile); err != nil {
			t.Fatalf("expected migrate-create to produce %s (highest=%03d), but stat failed: %v\nmake output: %s",
				filepath.Base(wantFile), highest, err, out)
		}
	}
}

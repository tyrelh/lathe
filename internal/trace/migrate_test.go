package trace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// An existing database one migration behind, opened by many connections
// and processes at once, is upgraded exactly once: each opener reads the
// version under the write lock, so none repeats a migration another already
// committed, and every opener succeeds.
func TestConcurrentUpgrade(t *testing.T) {
	for round := range 3 {
		dataRoot := t.TempDir()
		db, err := Open(dataRoot)
		if err != nil {
			t.Fatal(err)
		}
		// Back to the schema before the last migration.
		for _, column := range []string{"kind", "source_iteration", "guidance", "resolved_request", "restored", "preservation"} {
			if _, err := db.sql.Exec("ALTER TABLE iterations DROP COLUMN " + column); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.sql.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)-1)); err != nil {
			t.Fatal(err)
		}
		db.Close()

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				db, err := Open(dataRoot)
				if err != nil {
					errs <- fmt.Errorf("round %d connection %d: %w", round, i, err)
					return
				}
				db.Close()
			}()
		}
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				cmd := exec.Command(os.Args[0], "-test.run=^TestOpenHelper$")
				cmd.Env = append(os.Environ(), "LATHE_TRACE_OPEN="+dataRoot)
				if out, err := cmd.CombinedOutput(); err != nil {
					errs <- fmt.Errorf("round %d process %d: %v: %s", round, i, err, out)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}

		db, err = Open(dataRoot)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := db.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
			t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
		}
		rows, err := db.sql.Query(`SELECT kind, source_iteration, guidance, resolved_request, restored, preservation FROM iterations LIMIT 1`)
		if err != nil {
			t.Fatalf("upgraded schema: %v", err)
		}
		rows.Close()
		db.Close()
	}
}

// A fresh database carries every migration: worktree reporting columns on
// iterations, an execution mode on attempts that defaults to the legacy
// checkout, and the launches table.
func TestMigrationsAddWorktreesAndLaunches(t *testing.T) {
	db := open(t)
	var version int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	for _, q := range []string{
		`SELECT worktree_path, worktree_state, worktree_error, patch_path, recovery_ref FROM iterations LIMIT 1`,
		`SELECT mode FROM attempts LIMIT 1`,
		`SELECT launch_id, state, host, pid, pgid, endpoint, token FROM launches LIMIT 1`,
	} {
		rows, err := db.sql.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		rows.Close()
	}
	id := submit(t, db, "scout", "/repo")
	attempt, _ := dispatch(t, db, id)
	if a, err := db.GetAttempt(attempt); err != nil || a.Mode != ModeCheckout {
		t.Fatalf("attempt = %+v, %v; want the legacy checkout mode", a, err)
	}
}

// Launch records follow intent, registration and drain, bound to the attempt
// that owns the run: a settled run admits nothing more.
func TestLaunchLifecycle(t *testing.T) {
	db := open(t)
	id := submit(t, db, "build", "/repo")
	attempt, _ := dispatch(t, db, id)
	if err := db.LaunchIntent(id, attempt, "l1", []string{"git", "status"}); err != nil {
		t.Fatal(err)
	}
	if err := db.LaunchRegistered("l1", "host", 10, 10, "127.0.0.1:9", "tok"); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenLaunches(id); len(open) != 1 || open[0].State != LaunchRegistered || open[0].Token != "tok" {
		t.Fatalf("open = %+v", open)
	}
	if err := db.LaunchDrainedAt("l1"); err != nil {
		t.Fatal(err)
	}
	if err := db.LaunchIntent(id, attempt, "l2", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkLost(id, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := db.LaunchRegistered("l2", "host", 11, 11, "x", "y"); err != ErrLaunchRevoked {
		t.Fatalf("a lost run's intent registered: %v", err)
	}
	if err := db.LaunchIntent(id, attempt, "l3", nil); err == nil {
		t.Fatal("a lost run recorded a new intent")
	}
	if cancelled, err := db.CancelLaunch("l2"); !cancelled || err != nil {
		t.Fatalf("cancel = %v, %v", cancelled, err)
	}
	if open, _ := db.OpenLaunches(id); len(open) != 0 {
		t.Fatalf("open = %+v", open)
	}
}

// The data directory and database are owner-only, including an existing
// directory and database created with broader permissions; another user's
// directory is refused.
func TestStorageIsPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lathe")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dbFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Event("r", "", "log", "x", nil); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = Open(root); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		root:                               0o700,
		filepath.Join(root, dbFile):        0o600,
		filepath.Join(root, dbFile+"-wal"): 0o600,
		filepath.Join(root, dbFile+"-shm"): 0o600,
	} {
		fi, err := os.Stat(path)
		if os.IsNotExist(err) && path != root && path != filepath.Join(root, dbFile) {
			continue
		}
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v; want %v", path, fi.Mode().Perm(), err, want)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(root, link)
	if err := Private(link); err == nil {
		t.Fatal("a symlinked data directory was accepted")
	}
}

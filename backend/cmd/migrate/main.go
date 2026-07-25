package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
)

func main() {
	direction := flag.String("direction", "up", "migration direction: up or down")
	steps := flag.Int("steps", 0, "number of steps (0 = all)")
	// A bare `-direction=down` reverts every migration and drops the whole
	// schema. That is routine in development and catastrophic in production,
	// and the two were one missing flag apart.
	confirmDown := flag.Bool("i-know-this-drops-everything", false,
		"required for a full `-direction=down` with no -steps: reverts EVERY migration and destroys all data")
	flag.Parse()

	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal("load config: ", err)
	}

	migrationsPath := os.Getenv("MIGRATIONS_PATH")
	if migrationsPath == "" {
		migrationsPath = "file://migrations"
	}

	m, err := migrate.New(migrationsPath, cfg.DB.DSN())
	if err != nil {
		log.Fatal("create migrator: ", err)
	}
	defer m.Close()

	switch *direction {
	case "up":
		if *steps > 0 {
			err = m.Steps(*steps)
		} else {
			err = m.Up()
		}
	case "down":
		switch {
		case *steps > 0:
			err = m.Steps(-*steps)
		case !*confirmDown:
			log.Fatal("refusing to run a full down migration: it reverts every migration and drops the entire schema.\n" +
				"Use -steps=N to roll back N migrations, or pass -i-know-this-drops-everything to proceed.")
		default:
			err = m.Down()
		}
	default:
		log.Fatalf("unknown direction: %s", *direction)
	}

	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		log.Fatal("migration failed: ", err)
	}

	reportVersion(m)
}

// reportVersion prints the schema version, distinguishing "no migration has ever
// run" from "migration 0 is applied".
//
// The error from m.Version() used to be discarded, so an entirely unmigrated
// database printed `version=0 dirty=false` — byte-identical to the output after
// 000_extensions had been applied. That difference is exactly what you are
// looking at when you are deciding whether a deploy actually migrated.
func reportVersion(m *migrate.Migrate) {
	version, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		fmt.Println("migration complete: version=none (no migrations applied)")
	case err != nil:
		// The migration itself succeeded, but a version we cannot read is not a
		// clean run — exit non-zero so CI notices.
		log.Fatal("migration ran but reading the schema version failed: ", err)
	default:
		fmt.Printf("migration complete: version=%d dirty=%v\n", version, dirty)
		if dirty {
			log.Fatalf("schema is DIRTY at version %d: a previous migration failed part-way. "+
				"Repair the database by hand, then force the version to %d.", version, version)
		}
	}
}

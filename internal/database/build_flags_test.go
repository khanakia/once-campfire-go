package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memstatusFlag compiles SQLite (inside mattn/go-sqlite3, through CGO_CFLAGS) without memory
// statistics. With them on, every SQLite allocation takes one process-wide mutex; under concurrent
// page loads that mutex was the main contention, and turning it off cut search and sidebar CPU per
// request by roughly a third. cgo directives cannot reach another module's C code, so the flag
// lives in each build entry point, and this test keeps them from drifting.
const memstatusFlag = "-DSQLITE_DEFAULT_MEMSTATUS=0"

func TestBuildEntryPointsDisableSQLiteMemstatus(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, file := range []string{"bin/build", "bin/check", "Dockerfile"} {
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			// The default CGO_CFLAGS is "-O2 -g"; setting it replaces that, so keep optimisation.
			if strings.Contains(line, "CGO_CFLAGS=") && strings.Contains(line, memstatusFlag) && strings.Contains(line, "-O2") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not build SQLite with CGO_CFLAGS containing -O2 and %s", file, memstatusFlag)
		}
	}
}

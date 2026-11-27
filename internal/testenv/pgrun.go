package testenv

import (
	"fmt"
	"os"
	"strings"

	"github.com/msidorkevich/mill/internal/pgrun"
)

// pgrunEnv makes a test binary that imports testenv act as the process-group helper.
const pgrunEnv = "MILL_TESTENV_PGRUN"

// init turns the process into the pgrun helper when pgrunEnv is "1": it makes itself the
// leader of a new process group and execs the command in its arguments in place. The
// variable is unset first so the command and its children do not inherit it.
func init() {
	if os.Getenv(pgrunEnv) != "1" {
		return
	}
	if err := os.Unsetenv(pgrunEnv); err != nil {
		fmt.Fprintf(os.Stderr, "testenv pgrun helper: unset %s: %v\n", pgrunEnv, err)
		os.Exit(127)
	}
	err := pgrun.Exec(os.Args[1:])
	fmt.Fprintf(os.Stderr, "testenv pgrun helper: %v\n", err)
	os.Exit(127)
}

// PgrunArgv returns a command prefix that runs the command after it as the leader of a new
// process group, in place, so its pid is the group id: the running test binary, started as
// the pgrun helper (env MILL_TESTENV_PGRUN=1 <exe> --).
func PgrunArgv() []string {
	exe, err := os.Executable()
	if err != nil {
		panic(fmt.Sprintf("testenv: find the test binary to use as the pgrun helper: %v", err))
	}
	return []string{"env", pgrunEnv + "=1", exe, "--"}
}

// PgrunPrefix returns PgrunArgv quoted for a POSIX shell, ready to put before a command in
// a script.
func PgrunPrefix() string {
	argv := PgrunArgv()
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = Quote(a)
	}
	return strings.Join(quoted, " ")
}

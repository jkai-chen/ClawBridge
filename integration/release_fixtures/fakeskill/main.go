// Command fakeskill stands in for clawbridge.exe in the Windows installer
// probe, so the installer's real skill step runs an actual executable. Each
// run appends "<executable>\t<arguments>" to CB_PROBE_SKILL_LOG and exits with
// CB_PROBE_SKILL_EXIT (default 0).
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	if path := os.Getenv("CB_PROBE_SKILL_LOG"); path != "" {
		exe, err := os.Executable()
		if err != nil {
			os.Exit(90)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(91)
		}
		fmt.Fprintf(f, "%s\t%s\n", exe, strings.Join(os.Args[1:], " "))
		if f.Close() != nil {
			os.Exit(92)
		}
	}
	code, _ := strconv.Atoi(os.Getenv("CB_PROBE_SKILL_EXIT"))
	os.Exit(code)
}

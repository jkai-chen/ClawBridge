// Command fakeclawbridge stands in for `clawbridge gui-backend --stdio` in
// the WSL launch job, until the real command is on main. It is built for
// Linux in CI only and is not shipped. It reads the nonce line, prints the
// preamble, then answers each content-length request: hello with a fixed
// protocol 1 reply, anything else with an empty result.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var idRe = regexp.MustCompile(`"id"\s*:\s*(\d+)`)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "gui-backend" || os.Args[2] != "--stdio" {
		fmt.Fprintln(os.Stderr, "fakeclawbridge: expected: gui-backend --stdio")
		os.Exit(2)
	}
	r := bufio.NewReader(os.Stdin)
	nonce, err := r.ReadString('\n')
	if err != nil {
		os.Exit(1)
	}
	fmt.Printf("CLAWBRIDGE-GUI-BACKEND/1 %s\n", strings.TrimSpace(nonce))
	for {
		length := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
				length, _ = strconv.Atoi(v)
			}
		}
		if length < 0 {
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		id := "0"
		if m := idRe.FindSubmatch(body); m != nil {
			id = string(m[1])
		}
		result := `{}`
		if strings.Contains(string(body), `"method":"hello"`) {
			result = `{"protocol":1,"client_version":"0.0.0-fake","mode":"local","platform":"wsl"}`
		}
		out := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
		fmt.Printf("Content-Length: %d\r\n\r\n%s", len(out), out)
	}
}

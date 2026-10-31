package main

import (
	"fmt"
	"os"
	"time"
)

// runSelfCheck is CLAWBRIDGE_GUI_SELFCHECK=1: the app loads the built dist
// with the real Wails runtime and no backend, waits for the page's
// first-render signal, stays three more seconds so late violations are
// caught, prints the verdict and exits. It exits 0 only if the page rendered
// and the webview reported no CSP or Trusted Types violation.
func runSelfCheck(s *Shell) {
	rendered := false
	select {
	case rendered = <-s.ready:
	case <-time.After(30 * time.Second):
	}
	if rendered {
		time.Sleep(3 * time.Second)
	}
	v := s.violationList()
	fmt.Printf("selfcheck: rendered=%t violations=%d\n", rendered, len(v))
	for _, x := range v {
		fmt.Printf("selfcheck: violation: %s\n", x)
	}
	if rendered && len(v) == 0 {
		os.Exit(0)
	}
	os.Exit(1)
}

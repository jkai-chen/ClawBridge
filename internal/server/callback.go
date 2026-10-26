package server

import (
	"io"
	"net/http"
)

// LinearCallbackPath is the redirect URL of the ClawBridge Linear app
// (live/ONBOARDING-DESIGN.md §5, ON-M2). The server uses client credentials,
// so the page exchanges nothing: it ignores its query string, never logs it,
// and only tells the user to go back to the terminal.
const LinearCallbackPath = "/linear/callback"

const linearCallbackPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>ClawBridge</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1rem">
<h1>ClawBridge app installed</h1>
<p>Return to your terminal to continue.</p>
</body>
</html>
`

func linearCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, 405, "method_not_allowed", "GET required")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, linearCallbackPage)
	}
}

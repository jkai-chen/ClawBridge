package linearconn

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A waiter outlasts a holder's whole token refresh, and one that still times
// out names the holder instead of inviting removal of a live lock.
func TestCredentialLockWaitsOutARefreshAndNamesItsHolder(t *testing.T) {
	if lockWait <= refreshTimeout {
		t.Fatalf("lock wait %s does not outlast a %s refresh", lockWait, refreshTimeout)
	}
	m := testManager(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{}`), nil }))
	original := lockWait
	t.Cleanup(func() { lockWait = original })
	lockWait = 2 * time.Second
	release, err := m.diskLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(150 * time.Millisecond); release() }()
	second, err := m.diskLock(context.Background())
	if err != nil {
		t.Fatal("waiter gave up while the holder was still within its refresh", err)
	}
	lockWait = 100 * time.Millisecond
	_, err = m.diskLock(context.Background())
	second()
	var busy *Error
	if !errors.As(err, &busy) || busy.Code() != "profile busy" || !strings.Contains(busy.Detail, "ClawBridge process "+strconv.Itoa(os.Getpid())) || !strings.Contains(busy.Detail, "only if that process is no longer running") || strings.Contains(err.Error(), "inspect retained lock") {
		t.Fatalf("busy lock not explained: %v", err)
	}
}

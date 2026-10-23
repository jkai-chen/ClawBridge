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

// The wait can run out anywhere in the retry loop, not only inside its sleep:
// on CI the retry timer and the deadline fired together and the loop's next
// attempt returned the bare deadline. Expire the wait exactly at the top of an
// attempt: a live caller still gets the busy error naming the holder, and a
// cancelled caller gets its own cancellation.
func TestCredentialLockWaitEndingAtAnAttemptIsStillExplained(t *testing.T) {
	m := testManager(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{}`), nil }))
	original := lockWait
	t.Cleanup(func() { lockWait, lockAttemptHook = original, nil })
	lockWait = 2 * time.Second
	release, err := m.diskLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for name, cancelCaller := range map[string]bool{"live caller": false, "cancelled caller": true} {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		lockAttemptHook = func(wait context.Context) {
			if attempts++; attempts == 2 {
				if cancelCaller {
					cancel()
				}
				<-wait.Done()
			}
		}
		lockWait = 200 * time.Millisecond
		_, err = m.diskLock(ctx)
		cancel()
		var busy *Error
		if cancelCaller {
			if !errors.Is(err, context.Canceled) || errors.As(err, &busy) {
				t.Fatalf("%s: %v", name, err)
			}
			continue
		}
		if attempts != 2 || !errors.As(err, &busy) || busy.Code() != "profile busy" || !strings.Contains(busy.Detail, "ClawBridge process "+strconv.Itoa(os.Getpid())) {
			t.Fatalf("%s: busy lock not explained after %d attempts: %v", name, attempts, err)
		}
	}
}

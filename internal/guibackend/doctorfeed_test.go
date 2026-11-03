package guibackend

import (
	"context"
	"testing"
	"time"

	"clawbridge/internal/doctor"
)

func TestNeedsCycleRunsDoctorOnceInNineMinutes(t *testing.T) {
	clock := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	runs := 0
	feed := NewDoctorFeed(func(ctx context.Context) (doctor.Report, error) {
		runs++
		return syntheticDoctor(ctx)
	}, func() time.Time { return clock })
	cfg := needsCfg(syntheticExec(), "mara@example.test")
	cfg.Doctor = feed.Cached(DoctorBackgroundAge)
	for i := 0; i < 18; i++ { // 9 minutes of 30 s ticks
		r := Compose(context.Background(), cfg, clock)
		if kindOf(r, KindDoctor).Count != 2 {
			t.Fatalf("tick %d: doctor kind %+v", i, kindOf(r, KindDoctor))
		}
		clock = clock.Add(30 * time.Second)
	}
	if runs != 1 {
		t.Fatalf("doctor ran %d times in 9 minutes", runs)
	}
	clock = clock.Add(time.Minute) // past 10 minutes since the first run
	Compose(context.Background(), cfg, clock)
	if runs != 2 {
		t.Fatalf("doctor did not refresh after 10 minutes: %d", runs)
	}
}

func TestDoctorChecksFeedsNeedsAndPageOpenIsFresh(t *testing.T) {
	clock := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	runs := 0
	feed := NewDoctorFeed(func(ctx context.Context) (doctor.Report, error) { runs++; return syntheticDoctor(ctx) }, func() time.Time { return clock })
	if _, err := feed.Run(context.Background()); err != nil { // doctor.checks
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Minute)
	bg := feed.Cached(DoctorBackgroundAge)
	if _, err := bg(context.Background()); err != nil || runs != 1 {
		t.Fatalf("the background cycle did not reuse doctor.checks' report: %d runs, %v", runs, err)
	}
	page := feed.Cached(DoctorPageAge) // a page open wants a report under 30 s old
	if _, err := page(context.Background()); err != nil || runs != 2 {
		t.Fatalf("page open did not refresh: %d runs, %v", runs, err)
	}
	if _, err := page(context.Background()); err != nil || runs != 2 {
		t.Fatalf("a second open at once ran doctor again: %d", runs)
	}
}

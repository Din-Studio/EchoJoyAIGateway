package cluster

import (
	"testing"
	"time"
)

func TestRPMStatsMergeInstancesIntoClusterWideMinutes(t *testing.T) {
	_, client := newTestClient(t)
	clock := &rpmClock{now: time.Date(2026, time.September, 24, 12, 0, 50, 0, time.UTC)}
	limiters := []*AccessKeyRPM{NewAccessKeyRPM(client), NewAccessKeyRPM(client)}
	for _, limiter := range limiters {
		limiter.now = clock.current
	}
	stats := NewRPMStats(client)
	stats.now = clock.current

	// Two instances share one window: the third request in a minute is rejected
	// and every arrival is counted.
	for index := range 3 {
		decision, err := limiters[index%2].Allow(t.Context(), 5, 2)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Allowed != (index < 2) {
			t.Fatalf("request %d: Allowed = %v", index, decision.Allowed)
		}
	}
	// The rolling window carries the 3 requests into the next minute.
	clock.advance(20 * time.Second)
	if _, err := limiters[1].Allow(t.Context(), 5, 0); err != nil {
		t.Fatal(err)
	}

	report, err := stats.Report(t.Context(), RPMAccessKey, 5)
	if err != nil {
		t.Fatal(err)
	}
	minute := clock.current().Truncate(time.Minute).UnixMilli()
	want := []RPMPoint{
		{BucketStartMS: minute - 60_000, Peak: 3, RPMCounter: RPMCounter{Requests: 3, Rejected: 1}},
		{BucketStartMS: minute, Peak: 4, RPMCounter: RPMCounter{Requests: 1}},
	}
	if len(report.Points) != len(want) || report.Points[0] != want[0] || report.Points[1] != want[1] {
		t.Fatalf("points = %+v, want %+v", report.Points, want)
	}
	if report.Requests != 4 || report.Rejected != 1 || report.Peak == nil || *report.Peak != 4 {
		t.Fatalf("totals = %+v", report)
	}
	if report.Current == nil || *report.Current != (RPMCounter{Requests: 4, Rejected: 1}) {
		t.Fatalf("current = %+v", report.Current)
	}

	clock.advance(time.Minute)
	report, err = stats.Report(t.Context(), RPMAccessKey, 5)
	if err != nil || report.Current == nil || *report.Current != (RPMCounter{}) || len(report.Points) != 2 {
		t.Fatalf("after the window: %+v, %v", report, err)
	}
}

func TestRPMStatsCredentialPeaksKeepOneHour(t *testing.T) {
	_, client := newTestClient(t)
	clock := &rpmClock{now: time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)}
	stats := NewRPMStats(client)
	stats.now = clock.current

	for range 5 {
		stats.RecordCredential(t.Context(), 7)
	}
	clock.advance(30 * time.Minute)
	stats.RecordCredential(t.Context(), 8)
	// AccessKey statistics are a separate subject with the same ID.
	if _, err := NewAccessKeyRPM(client).Allow(t.Context(), 8, 0); err != nil {
		t.Fatal(err)
	}

	peaks, err := stats.Peaks(t.Context(), RPMCredential, []uint{7, 8, 9})
	if err != nil || len(peaks) != 2 || peaks[7] != 5 || peaks[8] != 1 {
		t.Fatalf("peaks = %v, %v", peaks, err)
	}
	clock.advance(31 * time.Minute)
	peaks, err = stats.Peaks(t.Context(), RPMCredential, []uint{7, 8})
	if err != nil || len(peaks) != 1 || peaks[8] != 1 {
		t.Fatalf("peaks after an hour = %v, %v", peaks, err)
	}
	report, err := stats.Report(t.Context(), RPMCredential, 9)
	if err != nil || report.Peak != nil || report.Current != nil || len(report.Points) != 0 {
		t.Fatalf("unobserved credential = %+v, %v", report, err)
	}
}

func TestRPMStatsRecordingNeverFails(t *testing.T) {
	server, client := newTestClient(t)
	stats := NewRPMStats(client)
	server.Close()
	stats.RecordCredential(t.Context(), 1)
	if _, err := stats.Report(t.Context(), RPMCredential, 1); err == nil {
		t.Fatal("Report() with Redis down error = nil")
	}
}

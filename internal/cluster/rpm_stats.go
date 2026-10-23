package cluster

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

// RPMKind selects whose request rate is observed.
type RPMKind int

const (
	RPMAccessKey RPMKind = iota
	RPMCredential
)

const (
	rpmHistory     = time.Hour
	rpmBucketWidth = time.Minute
)

// rpmObserveLua defines observe(), shared by every script that counts one
// request. Per subject it keeps two Hashes:
//   - seconds: field <unix second> = requests, <unix second>r = rejected; only
//     the last 60 s are kept, they form the cluster-wide rolling RPM.
//   - minutes: field <minute start ms> = "requests:rejected:peak", where peak
//     is the largest rolling RPM seen in that minute; one hour is kept.
//
// Arguments: seconds key, minutes key, unix second, minute start ms, rejected.
const rpmObserveLua = `
local function observe(second_key, minute_key, second, minute, rejected)
  local now = tonumber(second)
  redis.call('HINCRBY', second_key, second, 1)
  if rejected then
    redis.call('HINCRBY', second_key, second .. 'r', 1)
  end
  local rolling = 0
  local fields = redis.call('HGETALL', second_key)
  for index = 1, #fields, 2 do
    local field = fields[index]
    local at = tonumber(field)
    local counted = at ~= nil
    if not counted then
      at = tonumber(string.sub(field, 1, -2))
    end
    if at <= now - 60 then
      redis.call('HDEL', second_key, field)
    elseif counted then
      rolling = rolling + tonumber(fields[index + 1])
    end
  end
  redis.call('PEXPIRE', second_key, 61000)
  local requests, rejects, peak = 0, 0, 0
  local bucket = redis.call('HGET', minute_key, minute)
  if bucket then
    local q, r, p = string.match(bucket, '^(%d+):(%d+):(%d+)$')
    requests, rejects, peak = tonumber(q), tonumber(r), tonumber(p)
  else
    local oldest = tonumber(minute) - 3600000
    for _, field in ipairs(redis.call('HKEYS', minute_key)) do
      if tonumber(field) < oldest then
        redis.call('HDEL', minute_key, field)
      end
    end
  end
  requests = requests + 1
  if rejected then
    rejects = rejects + 1
  end
  if rolling > peak then
    peak = rolling
  end
  redis.call('HSET', minute_key, minute, requests .. ':' .. rejects .. ':' .. peak)
  redis.call('PEXPIRE', minute_key, 3660000)
end
`

// rpmObserveScript counts one admitted request. KEYS: seconds Hash, minutes
// Hash. ARGV: unix second, minute start ms.
var rpmObserveScript = redis.NewScript(rpmObserveLua + `
observe(KEYS[1], KEYS[2], ARGV[1], ARGV[2], false)
return 1
`)

func (kind RPMKind) keys(client *Client, id uint) []string {
	tag := accessKeyHashTag(id)
	if kind == RPMCredential {
		tag = credentialHashTag(id)
	}
	return []string{client.Key(tag, "rpm_stats", "seconds"), client.Key(tag, "rpm_stats", "minutes")}
}

func (kind RPMKind) String() string {
	if kind == RPMCredential {
		return "credential"
	}
	return "access_key"
}

func rpmObserveArgs(now time.Time) []any {
	second := now.Unix()
	return []any{strconv.FormatInt(second, 10), strconv.FormatInt(second/60*60_000, 10)}
}

// observeRPM counts one request outside any admission decision.
func observeRPM(ctx context.Context, client *Client, kind RPMKind, id uint, now time.Time) {
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	if err := rpmObserveScript.Run(callCtx, client, kind.keys(client, id), rpmObserveArgs(now)...).Err(); err != nil {
		// Statistics never gate traffic: a lost observation only logs.
		logrus.WithError(err).WithFields(logrus.Fields{
			"event": "rpm_stats.record_failed", "kind": kind.String(), "id": id,
		}).Warn("shared RPM statistics are unavailable")
	}
}

type RPMCounter struct {
	Requests int64 `json:"requests"`
	Rejected int64 `json:"rejected"`
}

type RPMPoint struct {
	BucketStartMS int64 `json:"bucket_start_ms"`
	Peak          int64 `json:"peak_rpm"`
	RPMCounter
}

// RPMReport is the last hour of one subject, in one-minute buckets. Peak and
// Current are nil when the subject saw no request in the hour.
type RPMReport struct {
	FromMS        int64       `json:"from_ms"`
	ToMS          int64       `json:"to_ms"`
	ObservedAtMS  int64       `json:"observed_at_ms"`
	BucketWidthMS int64       `json:"bucket_width_ms"`
	Current       *RPMCounter `json:"current,omitempty"`
	Peak          *int64      `json:"peak_rpm,omitempty"`
	RPMCounter
	Points []RPMPoint `json:"points"`
}

// RPMStats records per-credential request rates and reads the AccessKey and
// credential statistics every instance writes to one Redis.
type RPMStats struct {
	client *Client
	now    func() time.Time
}

// NewRPMStats builds the shared RPM statistics.
func NewRPMStats(client *Client) *RPMStats {
	return &RPMStats{client: client, now: time.Now}
}

// RecordCredential counts one upstream attempt on the credential.
func (stats *RPMStats) RecordCredential(ctx context.Context, credentialID uint) {
	observeRPM(ctx, stats.client, RPMCredential, credentialID, stats.now())
}

// Report reads the subject's last hour.
func (stats *RPMStats) Report(ctx context.Context, kind RPMKind, id uint) (RPMReport, error) {
	now := stats.now()
	report := RPMReport{
		FromMS: now.Add(-rpmHistory).UnixMilli(), ToMS: now.UnixMilli(), ObservedAtMS: now.UnixMilli(),
		BucketWidthMS: rpmBucketWidth.Milliseconds(), Points: []RPMPoint{},
	}
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	keys := kind.keys(stats.client, id)
	pipe := stats.client.Pipeline()
	seconds := pipe.HGetAll(callCtx, keys[0])
	minutes := pipe.HGetAll(callCtx, keys[1])
	if _, err := pipe.Exec(callCtx); err != nil {
		return RPMReport{}, fmt.Errorf("read %s %d RPM statistics: %w", kind, id, err)
	}
	points, err := rpmPoints(minutes.Val(), now)
	if err != nil {
		return RPMReport{}, fmt.Errorf("decode %s %d RPM statistics: %w", kind, id, err)
	}
	if len(points) == 0 {
		return report, nil
	}
	report.Points = points
	peak := int64(0)
	for _, point := range points {
		report.Requests += point.Requests
		report.Rejected += point.Rejected
		peak = max(peak, point.Peak)
	}
	report.Peak = &peak
	current, err := rpmCurrent(seconds.Val(), now)
	if err != nil {
		return RPMReport{}, fmt.Errorf("decode %s %d RPM statistics: %w", kind, id, err)
	}
	report.Current = &current
	return report, nil
}

// Peaks returns each subject's highest rolling RPM in the last hour; subjects
// without a request in the hour are absent.
func (stats *RPMStats) Peaks(ctx context.Context, kind RPMKind, ids []uint) (map[uint]int64, error) {
	peaks := make(map[uint]int64, len(ids))
	if len(ids) == 0 {
		return peaks, nil
	}
	now := stats.now()
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	pipe := stats.client.Pipeline()
	replies := make([]*redis.MapStringStringCmd, len(ids))
	for index, id := range ids {
		replies[index] = pipe.HGetAll(callCtx, kind.keys(stats.client, id)[1])
	}
	if _, err := pipe.Exec(callCtx); err != nil {
		return nil, fmt.Errorf("read %s RPM peaks: %w", kind, err)
	}
	for index, id := range ids {
		points, err := rpmPoints(replies[index].Val(), now)
		if err != nil {
			return nil, fmt.Errorf("decode %s %d RPM statistics: %w", kind, id, err)
		}
		for _, point := range points {
			peaks[id] = max(peaks[id], point.Peak)
		}
	}
	return peaks, nil
}

// rpmPoints decodes the minutes that start within the hour before now.
func rpmPoints(fields map[string]string, now time.Time) ([]RPMPoint, error) {
	width := rpmBucketWidth.Milliseconds()
	first := (now.Add(-rpmHistory).UnixMilli() + width - 1) / width * width
	points := make([]RPMPoint, 0, len(fields))
	for field, value := range fields {
		minute, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, err
		}
		if minute < first || minute > now.UnixMilli() {
			continue
		}
		parts := strings.Split(value, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed minute %q", value)
		}
		point := RPMPoint{BucketStartMS: minute}
		for index, target := range []*int64{&point.Requests, &point.Rejected, &point.Peak} {
			if *target, err = strconv.ParseInt(parts[index], 10, 64); err != nil {
				return nil, err
			}
		}
		points = append(points, point)
	}
	slices.SortFunc(points, func(left, right RPMPoint) int { return cmp.Compare(left.BucketStartMS, right.BucketStartMS) })
	return points, nil
}

// rpmCurrent sums the rolling 60 s window ending at now.
func rpmCurrent(fields map[string]string, now time.Time) (RPMCounter, error) {
	var current RPMCounter
	second := now.Unix()
	for field, value := range fields {
		at, rejected := strings.CutSuffix(field, "r")
		parsedAt, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			return RPMCounter{}, err
		}
		if parsedAt <= second-60 || parsedAt > second {
			continue
		}
		count, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return RPMCounter{}, err
		}
		if rejected {
			current.Rejected += count
		} else {
			current.Requests += count
		}
	}
	return current, nil
}

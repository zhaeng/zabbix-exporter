package cache

import "time"

const longCycleThreshold = time.Minute

// PublishPolicy controls which timestamp is exposed to a publisher and whether
// an acknowledged value remains eligible in the next publish cycle.
type PublishPolicy uint8

const (
	// PublishSourceTimestamp is used for items with delay <= one minute. Each
	// ValueVersion is published at its original Zabbix clock/ns at most once.
	PublishSourceTimestamp PublishPolicy = iota
	// PublishCycleTimestamp is used for items with delay > one minute. A valid
	// cached value is eligible at each new cycle timestamp until ExpireAt.
	PublishCycleTimestamp
)

func (p PublishPolicy) String() string {
	switch p {
	case PublishSourceTimestamp:
		return "source_timestamp"
	case PublishCycleTimestamp:
		return "cycle_timestamp"
	default:
		return "unknown"
	}
}

// ValueState is the scalar, label-free state retained for one Zabbix item.
// Callers receive this structure by value; the cache never returns a mutable
// pointer to its internal state.
type ValueState struct {
	ItemID                 string
	Value                  float64
	SourceTimestamp        time.Time
	SourceNS               int64
	LastFetchAt            time.Time
	LastSuccessAt          time.Time
	Delay                  time.Duration
	ExpireAt               time.Time
	ConsecutiveMisses      uint32
	MetadataVersion        uint64
	ValueVersion           uint64
	LastPublishedVersion   uint64
	LastPublishedTimestamp time.Time
	PublishSlot            uint16
	PublishPolicy          PublishPolicy
	Valid                  bool
	scanSequence           uint64
}

// PublishPolicyForDelay maps the configured Zabbix delay to the fixed T04
// source-only/sample-and-hold boundary.
func PublishPolicyForDelay(delay time.Duration) PublishPolicy {
	if delay > longCycleThreshold {
		return PublishCycleTimestamp
	}
	return PublishSourceTimestamp
}

// ValueExpireAt calculates the hard TTL exclusively from the source timestamp
// and configured delay. Fetch, cache-write, and publish times never extend it.
func ValueExpireAt(sourceTimestamp time.Time, delay time.Duration) time.Time {
	if PublishPolicyForDelay(delay) == PublishSourceTimestamp {
		maxAge := 2 * delay
		if maxAge < 90*time.Second {
			maxAge = 90 * time.Second
		}
		return sourceTimestamp.Add(maxAge)
	}

	grace := delay / 5
	if grace < 30*time.Second {
		grace = 30 * time.Second
	}
	return sourceTimestamp.Add(2*delay + grace)
}

func missInvalidAfter(sourceTimestamp time.Time, delay time.Duration) time.Time {
	grace := delay / 5
	if grace < 30*time.Second {
		grace = 30 * time.Second
	}
	return sourceTimestamp.Add(delay + grace)
}

func sourceIsNewer(clock, ns int64, current ValueState) bool {
	currentClock := current.SourceTimestamp.Unix()
	return clock > currentClock || (clock == currentClock && ns > current.SourceNS)
}

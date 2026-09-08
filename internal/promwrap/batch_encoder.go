package promwrap

import (
	"fmt"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"

	"github.com/zhaeng/zabbix-exporter/internal/cache"
)

type publishKey struct {
	seriesID    string
	generation  uint64
	timestampMS int64
}

type encodeSeries struct {
	seriesID  string
	labels    []prompb.Label
	value     float64
	timestamp time.Time
	ack       *cache.PublishAck
	key       publishKey
}

// EncodedBatch is immutable after construction and is reused verbatim by all
// retry attempts. It contains at most one sample per TimeSeries.
type encodedBatch struct {
	body              []byte
	acks              []cache.PublishAck
	keys              []publishKey
	policy            cache.PublishPolicy
	slot              uint16
	cycleTimestampMS  int64
	lane              int
	samples           int
	uncompressedBytes int
	compressedBytes   int
}

type batchEncoder struct {
	maxSamples int
	maxBytes   int
}

func newBatchEncoder(maxSamples, maxBytes int) (batchEncoder, error) {
	if maxSamples <= 0 {
		return batchEncoder{}, fmt.Errorf("max samples per send must be positive: %d", maxSamples)
	}
	if maxBytes <= 0 {
		return batchEncoder{}, fmt.Errorf("max batch bytes must be positive: %d", maxBytes)
	}
	return batchEncoder{maxSamples: maxSamples, maxBytes: maxBytes}, nil
}

type batchBuilder struct {
	encoder      batchEncoder
	policy       cache.PublishPolicy
	slot         uint16
	cycleMS      int64
	lane         int
	series       []prompb.TimeSeries
	acks         []cache.PublishAck
	keys         []publishKey
	encodedBytes int
}

func (e batchEncoder) newBuilder(policy cache.PublishPolicy, slot uint16, cycleMS int64, lane int) *batchBuilder {
	capacity := e.maxSamples
	if capacity > 1024 {
		capacity = 1024
	}
	return &batchBuilder{
		encoder: e,
		policy:  policy,
		slot:    slot,
		cycleMS: cycleMS,
		lane:    lane,
		series:  make([]prompb.TimeSeries, 0, capacity),
		acks:    make([]cache.PublishAck, 0, capacity),
		keys:    make([]publishKey, 0, capacity),
	}
}

func (b *batchBuilder) empty() bool {
	return len(b.series) == 0
}

// add returns false without consuming sample when either configured boundary
// would be crossed. An individual sample larger than maxBytes is rejected so
// every emitted request honors both limits without exceptions.
func (b *batchBuilder) add(sample encodeSeries) (bool, error) {
	if len(b.series) >= b.encoder.maxSamples {
		return false, nil
	}
	series := prompb.TimeSeries{
		Labels: sample.labels,
		Samples: []prompb.Sample{{
			Value:     sample.value,
			Timestamp: sample.timestamp.UnixMilli(),
		}},
	}
	entryBytes := repeatedMessageSize(series.Size())
	if entryBytes > b.encoder.maxBytes {
		return false, fmt.Errorf("series %q encodes to %d bytes, exceeding max batch bytes %d", sample.seriesID, entryBytes, b.encoder.maxBytes)
	}
	if b.encodedBytes+entryBytes > b.encoder.maxBytes {
		return false, nil
	}

	b.series = append(b.series, series)
	if sample.ack != nil {
		b.acks = append(b.acks, *sample.ack)
	}
	b.keys = append(b.keys, sample.key)
	b.encodedBytes += entryBytes
	return true, nil
}

func (b *batchBuilder) encode() (*encodedBatch, error) {
	if len(b.series) == 0 {
		return nil, nil
	}
	request := prompb.WriteRequest{Timeseries: b.series}
	raw, err := request.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal remote write batch: %w", err)
	}
	if len(raw) > b.encoder.maxBytes {
		return nil, fmt.Errorf("encoded remote write batch is %d bytes, exceeding max %d", len(raw), b.encoder.maxBytes)
	}
	body := snappy.Encode(nil, raw)
	return &encodedBatch{
		body:              body,
		acks:              append([]cache.PublishAck(nil), b.acks...),
		keys:              append([]publishKey(nil), b.keys...),
		policy:            b.policy,
		slot:              b.slot,
		cycleTimestampMS:  b.cycleMS,
		lane:              b.lane,
		samples:           len(b.series),
		uncompressedBytes: len(raw),
		compressedBytes:   len(body),
	}, nil
}

// A WriteRequest encodes Timeseries as field 1: one tag byte, a varint length,
// then the TimeSeries message. WriteRequest currently has no other populated
// fields, making this an exact pre-marshal size calculation.
func repeatedMessageSize(messageBytes int) int {
	return 1 + varintSize(uint64(messageBytes)) + messageBytes
}

func varintSize(value uint64) int {
	size := 1
	for value >= 1<<7 {
		value >>= 7
		size++
	}
	return size
}

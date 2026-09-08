package zabbix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// HistoryQuery describes one history.get batch. Limit applies to the complete
// response, not independently to every ItemID.
type HistoryQuery struct {
	GroupID   string
	ItemIDs   []string
	ValueType int
	TimeFrom  time.Time
	TimeTill  time.Time
	Limit     int
}

// HistoryReader is the value-only Zabbix API contract used by the scheduled
// collector. Metadata refresh deliberately uses a different, item.get-only
// interface so metric values cannot accidentally flow through item.get.
type HistoryReader interface {
	QueryHistoryBatch(ctx context.Context, query HistoryQuery) BatchResult
}

// BatchStatus records whether a history batch is complete and usable.
type BatchStatus int

const (
	BatchSuccess BatchStatus = iota
	BatchEmpty
	BatchPossiblyTruncated
	BatchAPIError
	BatchDecodeError
)

func (s BatchStatus) String() string {
	switch s {
	case BatchSuccess:
		return "success"
	case BatchEmpty:
		return "empty"
	case BatchPossiblyTruncated:
		return "possibly_truncated"
	case BatchAPIError:
		return "api_error"
	case BatchDecodeError:
		return "decode_error"
	default:
		return "unknown"
	}
}

// BatchResult is the structured result of one history.get call. Records keeps
// the raw response for the compatible GetHistory entry point; Latest contains
// at most one clock/ns-latest point for each itemid.
type BatchResult struct {
	Query      HistoryQuery
	Status     BatchStatus
	Records    []HistoryItem
	Latest     map[string]HistoryItem
	Returned   int
	LimitHit   bool
	StartedAt  time.Time
	FinishedAt time.Time
	Err        error
}

// ReduceLatest keeps the greatest clock/ns point for each itemid. Response
// ordering is deliberately ignored because Zabbix only sorts the whole batch.
func ReduceLatest(records []HistoryItem) map[string]HistoryItem {
	latest := make(map[string]HistoryItem, len(records))
	for _, record := range records {
		current, ok := latest[record.ItemID]
		if !ok || record.Clock > current.Clock ||
			(record.Clock == current.Clock && record.NS > current.NS) {
			latest[record.ItemID] = record
		}
	}
	return latest
}

// QueryHistoryBatch performs one history.get and preserves empty, truncated,
// API and decode outcomes as distinct states. It intentionally does not split
// or retry a truncated batch; that belongs to the later scheduler work.
func (c *Client) QueryHistoryBatch(ctx context.Context, query HistoryQuery) (batch BatchResult) {
	query.ItemIDs = append([]string(nil), query.ItemIDs...)
	batch.Query = query
	batch.StartedAt = time.Now()
	defer func() {
		batch.FinishedAt = time.Now()
	}()

	params := struct {
		Output    []string `json:"output"`
		History   int      `json:"history"`
		ItemIDs   []string `json:"itemids"`
		TimeFrom  int64    `json:"time_from"`
		TimeTill  int64    `json:"time_till"`
		Limit     int      `json:"limit"`
		SortField string   `json:"sortfield"`
		SortOrder string   `json:"sortorder"`
	}{
		Output:    []string{"itemid", "clock", "ns", "value"},
		History:   query.ValueType,
		ItemIDs:   query.ItemIDs,
		TimeFrom:  query.TimeFrom.Unix(),
		TimeTill:  query.TimeTill.Unix(),
		Limit:     query.Limit,
		SortField: "clock",
		SortOrder: "DESC",
	}

	result, err := c.doRequest(ctx, "history.get", params, c.GetToken())
	if err != nil {
		batch.Status = BatchAPIError
		var decodeErr *responseDecodeError
		if errors.As(err, &decodeErr) {
			batch.Status = BatchDecodeError
		}
		batch.Err = err
		return batch
	}
	if result == nil {
		batch.Status = BatchDecodeError
		batch.Err = fmt.Errorf("history.get returned no result")
		return batch
	}

	if err := json.Unmarshal(*result, &batch.Records); err != nil {
		batch.Status = BatchDecodeError
		batch.Err = fmt.Errorf("failed to unmarshal history: %w", err)
		return batch
	}

	batch.Returned = len(batch.Records)
	batch.Latest = ReduceLatest(batch.Records)
	batch.LimitHit = query.Limit > 0 && batch.Returned >= query.Limit
	switch {
	case batch.LimitHit:
		batch.Status = BatchPossiblyTruncated
	case batch.Returned == 0:
		batch.Status = BatchEmpty
	default:
		batch.Status = BatchSuccess
	}
	return batch
}

var _ HistoryReader = (*Client)(nil)

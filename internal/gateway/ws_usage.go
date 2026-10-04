package gateway

import (
	"errors"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscoperealtime"
)

const maxWSUsageRecords = 4096

var (
	errWSUsageLimit    = errors.New("websocket usage: record limit reached")
	errWSUsageID       = errors.New("websocket usage: invalid correlation")
	errWSUsageFinished = errors.New("websocket usage: ledger already finished")
)

type wsUsageKey struct{ source, id string }

type wsUsageRecord struct {
	terminal      bool
	usage         canonical.Usage
	characters    int64
	hasCharacters bool
}

// wsUsage 只由上游观测 goroutine 调用；Finish 须在它退出后调用。
// 已结与未结共用容量，不淘汰旧键，否则一次迟到重发就会重新计费。
type wsUsage struct {
	metrics            *obs.Metrics
	protocol, outbound string
	records            map[wsUsageKey]wsUsageRecord
	sessionID          string
	finished           bool
}

func newWSUsage(metrics *obs.Metrics, protocol, outbound string) *wsUsage {
	return &wsUsage{metrics: metrics, protocol: protocol, outbound: outbound, records: make(map[wsUsageKey]wsUsageRecord)}
}

func (w *wsUsage) Observe(e dashscoperealtime.Event) error {
	if w.finished {
		return errWSUsageFinished
	}
	if !e.Started && !e.Terminal {
		w.observeFailure(e.Failure)
		return nil
	}
	if e.Source == "session" && e.Terminal && e.ID == "" {
		e.ID = w.sessionID
	}
	if e.ID == "" || len(e.ID) > dashscoperealtime.MaxIDBytes || (e.Source != "response" && e.Source != "transcription" && e.Source != "session") {
		w.diagnostic("invalid_event")
		return errWSUsageID
	}
	if e.Source == "session" && w.sessionID != "" && e.ID != w.sessionID {
		w.diagnostic("invalid_event")
		return errWSUsageID
	}
	key := wsUsageKey{e.Source, e.ID}
	previous, exists := w.records[key]
	if !exists && len(w.records) >= maxWSUsageRecords {
		w.diagnostic("ledger_limit")
		return errWSUsageLimit
	}
	if e.Source == "session" {
		w.sessionID = e.ID
	}
	record := wsUsageRecord{terminal: e.Terminal, usage: e.Usage, hasCharacters: e.Characters != nil}
	if e.Characters != nil {
		// 不借用调用方的字符指针，防外部复用内存使重复校验失效。
		record.characters = *e.Characters
	}
	if previous.terminal {
		if e.Terminal && (previous.usage != record.usage || previous.hasCharacters != record.hasCharacters || previous.characters != record.characters) {
			w.diagnostic("usage_conflict")
		}
		return nil
	}
	w.records[key] = record
	if e.Terminal {
		w.publish(e.Source, record)
		w.diagnostic(e.Diagnostic)
		w.observeFailure(e.Failure)
	}
	return nil
}

func (w *wsUsage) Finish() {
	if w.finished {
		return
	}
	w.finished = true
	for key, record := range w.records {
		if !record.terminal {
			record = wsUsageRecord{terminal: true, usage: canonical.UnavailableUsage()}
			w.records[key] = record
			w.publish(key.source, record)
			w.diagnostic("usage_unfinished")
		}
	}
}

func (w *wsUsage) publish(source string, record wsUsageRecord) {
	if w.metrics == nil {
		return
	}
	if record.hasCharacters {
		w.metrics.ObserveWSCharacters(w.protocol, source, record.characters)
	} else {
		w.metrics.ObserveWSUsage(w.protocol, source, record.usage)
		w.metrics.ObserveUsage(w.outbound, record.usage)
	}
}

func (w *wsUsage) diagnostic(reason string) {
	if w.metrics != nil && reason != "" {
		w.metrics.ObserveWSDiagnostic(w.protocol, reason)
	}
}

func (w *wsUsage) observeFailure(failure *canonical.Error) {
	if w.metrics != nil {
		w.metrics.ObserveError(w.outbound, failure)
	}
}

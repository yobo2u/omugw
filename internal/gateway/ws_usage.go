package gateway

import (
	"errors"
	"unicode/utf8"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/obs"
)

const (
	maxWSUsageRecords = 4096
	maxWSUsageIDBytes = 512
)

// wsUsageEvent 只承接协议已核验的小事实，不能持有原消息、全文或任意字段表。
type wsUsageEvent struct {
	Source, ID                     string
	Part                           int64
	HasPart                        bool
	Started, Terminal, ItemPending bool
	Usage                          canonical.Usage
	Details                        obs.WSTokenDetails
	Characters                     *int64
	Seconds                        *float64
	Diagnostic                     string
	Failure                        *canonical.Error
}

var (
	errWSUsageLimit    = errors.New("websocket usage: record limit reached")
	errWSUsageID       = errors.New("websocket usage: invalid correlation")
	errWSUsageFinished = errors.New("websocket usage: ledger already finished")
)

type wsUsageKey struct {
	source, id string
	part       int64
	hasPart    bool
}

type wsUsageRecord struct {
	terminal      bool
	usage         canonical.Usage
	characters    int64
	hasCharacters bool
	seconds       float64
	hasSeconds    bool
	details       obs.WSTokenDetails
	diagnostic    string
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

func (w *wsUsage) Observe(e wsUsageEvent) error {
	if w.finished {
		return errWSUsageFinished
	}
	if !e.Started && !e.Terminal && !e.ItemPending {
		w.observeFailure(e.Failure)
		return nil
	}
	if e.Source == "session" && e.Terminal && e.ID == "" {
		e.ID = w.sessionID
	}
	if e.ID == "" || len(e.ID) > maxWSUsageIDBytes || !utf8.ValidString(e.ID) ||
		(e.Source != "response" && e.Source != "transcription" && e.Source != "session") ||
		e.Part < 0 || e.Part > 2147483647 || !e.HasPart && e.Part != 0 ||
		e.HasPart && e.Source != "transcription" || e.ItemPending && (e.Source != "transcription" || e.HasPart || e.Terminal) {
		w.diagnostic("invalid_event")
		return errWSUsageID
	}
	if e.Source == "session" && w.sessionID != "" && e.ID != w.sessionID {
		w.diagnostic("invalid_event")
		return errWSUsageID
	}
	key := wsUsageKey{source: e.Source, id: e.ID, part: e.Part, hasPart: e.HasPart}
	if w.correlateItem(key, e.ItemPending, e.Started) {
		return nil
	}
	previous, exists := w.records[key]
	if !exists && len(w.records) >= maxWSUsageRecords {
		w.diagnostic("ledger_limit")
		return errWSUsageLimit
	}
	if e.Source == "session" {
		w.sessionID = e.ID
	}
	record := wsUsageRecord{
		terminal: e.Terminal, usage: e.Usage, details: e.Details,
		hasCharacters: e.Characters != nil, hasSeconds: e.Seconds != nil, diagnostic: e.Diagnostic,
	}
	if e.Characters != nil {
		// 不借用调用方的字符指针，防外部复用内存使重复校验失效。
		record.characters = *e.Characters
	}
	if e.Seconds != nil {
		record.seconds = *e.Seconds
	}
	if previous.terminal {
		if e.Terminal && previous != record {
			w.diagnostic("usage_conflict")
			// 缺单位与非法单位都会产生 unavailable；不能据此吞掉迟到的非法组诊断。
			if previous.diagnostic != record.diagnostic {
				w.diagnostic(record.diagnostic)
			}
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

// correlateItem 把 item 占位迁到首次明确的 part。只有一张有界表，没有额外 item 索引；
// 迁移不增加占用，已结 part 仍保留，迟到的无 index 事件不能另建幽灵 unavailable。
func (w *wsUsage) correlateItem(key wsUsageKey, itemPending, started bool) bool {
	if key.source != "transcription" {
		return false
	}
	if key.hasPart {
		pending := wsUsageKey{source: key.source, id: key.id}
		if record, ok := w.records[pending]; ok && !record.terminal {
			delete(w.records, pending)
		}
		return false
	}
	if !itemPending {
		return false
	}
	parts := 0
	for known := range w.records {
		if known.source == key.source && known.id == key.id && known.hasPart {
			parts++
		}
	}
	if parts > 1 && started {
		// 多 part 的无 index delta 无法选定归属，只诊断，不猜测新 part 或终态。
		w.diagnostic("usage_ambiguous")
	}
	return parts != 0
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
	}
	if record.hasSeconds {
		w.metrics.ObserveWSSeconds(w.protocol, source, record.seconds)
	}
	// 字符、秒与 token 独立；只有非 token 单位时不能另记不可知/零 token。
	if !record.hasCharacters && !record.hasSeconds || record.usage.Fidelity == canonical.FidelityAuthoritative {
		// 细分由 presence 单独发布，避免与 Canonical 中的兼容投影重复累计音频。
		totals := canonical.Usage{Fidelity: record.usage.Fidelity, InputTokens: record.usage.InputTokens, OutputTokens: record.usage.OutputTokens}
		w.metrics.ObserveWSUsage(w.protocol, source, totals)
		if record.usage.Fidelity == canonical.FidelityAuthoritative {
			w.metrics.ObserveWSTokenDetails(w.protocol, source, record.details)
		}
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

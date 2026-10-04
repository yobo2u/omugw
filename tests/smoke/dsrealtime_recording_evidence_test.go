//go:build smoke

package smoke_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
)

type dsAutomaticCommitState struct {
	nodes                   []int
	audioBytes              int
	terminal, finishSent    bool
	sessionFinished, closed bool
}

// finish 前必须已自动启动同实体音频；终态可在 finish 后到达，不能把结束排空当自动触发。
// 驱动只用未完成前缀准许 finish，见证/Coverage 则必须等完整链及真实正常 close。
func dsAutomaticCommitProgress(records []dsRecord) *dsAutomaticCommitState {
	s := &dsAutomaticCommitState{}
	configured, appended, created, closeSent := false, false, false, false
	id := ""
	for i, rec := range records {
		if s.closed {
			return nil
		}
		if rec.Kind == "close" {
			if !s.sessionFinished || rec.CloseCode != 1000 {
				return nil
			}
			if rec.Direction == "send" {
				if closeSent {
					return nil
				}
				closeSent = true
			} else if rec.Direction == "receive" {
				s.closed = true
			}
			continue
		}
		if rec.Kind != "message" {
			continue
		}
		if s.sessionFinished {
			return nil
		}
		var e map[string]any
		if json.Unmarshal(rec.Payload, &e) != nil {
			return nil
		}
		typ := dsString(e, "type")
		if rec.Direction == "send" {
			switch typ {
			case "input_text_buffer.commit", "response.create", "response.cancel":
				return nil
			case "session.finish":
				if s.audioBytes == 0 || s.finishSent {
					return nil
				}
				s.finishSent = true
				s.nodes = append(s.nodes, i)
			case "input_text_buffer.append":
				if !configured || appended || dsString(e, "text") != dsPublicText {
					return nil
				}
				appended = true
				s.nodes = append(s.nodes, i)
			}
			continue
		}
		switch typ {
		case "session.updated":
			if configured || appended || dsString(e, "session", "mode") != "server_commit" {
				return nil
			}
			configured = true
			s.nodes = append(s.nodes, i)
		case "response.created":
			if !appended || created || dsString(e, "response", "id") == "" {
				return nil
			}
			created = true
			id = dsString(e, "response", "id")
			s.nodes = append(s.nodes, i)
		case "response.audio.delta":
			b, err := base64.StdEncoding.DecodeString(dsString(e, "delta"))
			if !created || s.terminal || dsString(e, "response_id") != id || err != nil || len(b) == 0 || len(b) > dsMaxAudio-s.audioBytes {
				return nil
			}
			s.audioBytes += len(b)
			s.nodes = append(s.nodes, i)
		case "response.done":
			if !created || s.terminal || s.audioBytes == 0 || dsString(e, "response", "id") != id || dsString(e, "response", "status") != "completed" {
				return nil
			}
			s.terminal = true
			s.nodes = append(s.nodes, i)
		case "session.finished":
			if !s.finishSent || !s.terminal {
				return nil
			}
			s.sessionFinished = true
			s.nodes = append(s.nodes, i)
		case "error":
			return nil
		}
	}
	return s
}

func dsAutomaticCommitEvidence(records []dsRecord) []int {
	if s := dsAutomaticCommitProgress(records); s != nil && s.closed {
		return s.nodes
	}
	return nil
}

// 只在录制作者省略可选 id 时接受服务端分配；显式 id、角色、内容和 call_id 仍逐项核对。
func dsItemAckMatches(want, got map[string]any) bool {
	if dsString(got, "id") == "" {
		return false
	}
	switch dsString(want, "type") {
	case "message":
		if dsString(want, "role") != "user" {
			return false
		}
		wc, wok := want["content"].([]any)
		gc, gok := got["content"].([]any)
		if !wok || !gok || len(wc) == 0 || len(wc) != len(gc) {
			return false
		}
		for i, w := range wc {
			wm, wok := w.(map[string]any)
			gm, gok := gc[i].(map[string]any)
			if !wok || !gok || dsString(wm, "type") != "input_text" || dsString(wm, "text") == "" || !dsEchoMatches(wm, gm) {
				return false
			}
		}
		want = maps.Clone(want)
		delete(want, "content")
	case "function_call_output":
		if dsString(want, "call_id") == "" || dsString(want, "output") == "" {
			return false
		}
	default:
		return false
	}
	return dsEchoMatches(want, got)
}

// 单个 pending 的生命周期同时约束驱动和候选：不能跨过错 ack/另一次提交捡后面的同名事件。
type dsItemAcks struct {
	pending      map[string]any
	pendingIndex int
	seen         map[string]bool
	accepted     map[int]string
}

func (s *dsItemAcks) observe(direction string, e map[string]any, index int) bool {
	typ := dsString(e, "type")
	if direction == "send" {
		if s.pending != nil {
			return false
		}
		if typ == "conversation.item.create" {
			s.pending = dsMap(e, "item")
			s.pendingIndex = index
			return s.pending != nil
		}
		return true
	}
	if typ != "conversation.item.created" {
		return true
	}
	item := dsMap(e, "item")
	id := dsString(item, "id")
	if id == "" || s.seen[id] {
		return false
	}
	if s.pending != nil {
		if !dsItemAckMatches(s.pending, item) {
			return false
		}
		if s.accepted == nil {
			s.accepted = map[int]string{}
		}
		s.accepted[s.pendingIndex] = id
		s.pending = nil
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[id] = true
	return true
}

func dsItemRequestWitness(records []dsRecord, index int) bool {
	var acks dsItemAcks
	for i, rec := range records {
		if rec.Kind != "message" {
			continue
		}
		var e map[string]any
		if json.Unmarshal(rec.Payload, &e) != nil || !acks.observe(rec.Direction, e, i) {
			return false
		}
	}
	return acks.pending == nil && acks.accepted[index] != ""
}

// 节点索引来自原始证据链；发送选 authored，接收选 golden，不另找同名事件拼接。
func dsEvidenceNodes(records []dsRecord, indices []int) []string {
	var nodes []string
	for _, i := range indices {
		point := 1
		if records[i].Direction == "send" {
			point = 0
		}
		nodes = append(nodes, fmt.Sprintf("n%04d_%d", i, point))
	}
	return nodes
}

// 打断证据只收一条 created→非空音频→cancel→cancelled 链；类型集合不能连接两个响应。
// cancel 在线格式没有目标字段，目标由成功发送时唯一活跃且已有音频的响应确定。
func dsInterruptEvidence(records []dsRecord) ([]int, int) {
	id := ""
	ended := false
	cancelIndex := -1
	audioBytes := 0
	var nodes []int
	for i, rec := range records {
		if rec.Kind != "message" {
			continue
		}
		var e map[string]any
		if json.Unmarshal(rec.Payload, &e) != nil {
			return nil, -1
		}
		typ := dsString(e, "type")
		if rec.Direction == "send" {
			if typ == "response.cancel" {
				if id == "" || ended || audioBytes == 0 || cancelIndex >= 0 {
					return nil, -1
				}
				if target := dsString(e, "response_id"); target != "" && target != id {
					return nil, -1
				}
				cancelIndex = i
				nodes = append(nodes, i)
			}
			continue
		}
		switch typ {
		case "response.created":
			if id != "" || dsString(e, "response", "id") == "" {
				return nil, -1
			}
			id = dsString(e, "response", "id")
			nodes = append(nodes, i)
		case "response.audio.delta":
			b, err := base64.StdEncoding.DecodeString(dsString(e, "delta"))
			if id == "" || ended || dsString(e, "response_id") != id || err != nil || len(b) == 0 || len(b) > dsMaxAudio-audioBytes {
				return nil, -1
			}
			audioBytes += len(b)
			nodes = append(nodes, i)
		case "response.done":
			if id == "" || ended || cancelIndex < 0 || dsString(e, "response", "id") != id || dsString(e, "response", "status") != "cancelled" {
				return nil, -1
			}
			ended = true
			nodes = append(nodes, i)
		}
	}
	if !ended {
		return nil, -1
	}
	return nodes, cancelIndex
}

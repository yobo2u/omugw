//go:build smoke

package smoke_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// 自动提交必须在任何 finish/commit 之前完成同一响应，不能把结束排空当自动触发。
func dsAutomaticCommitResponse(records []dsRecord) []int {
	configured, appended, created, terminal := false, false, false, false
	id := ""
	audioBytes := 0
	var nodes []int
	for i, rec := range records {
		if rec.Kind != "message" {
			continue
		}
		var e map[string]any
		if json.Unmarshal(rec.Payload, &e) != nil {
			return nil
		}
		typ := dsString(e, "type")
		if rec.Direction == "send" {
			switch typ {
			case "session.finish", "input_text_buffer.commit", "response.create", "response.cancel":
				return nil
			case "input_text_buffer.append":
				if !configured || appended || dsString(e, "text") != dsPublicText {
					return nil
				}
				appended = true
				nodes = append(nodes, i)
			}
			continue
		}
		switch typ {
		case "session.updated":
			if appended || dsString(e, "session", "mode") != "server_commit" {
				return nil
			}
			configured = true
			nodes = append(nodes, i)
		case "response.created":
			if !appended || created || dsString(e, "response", "id") == "" {
				return nil
			}
			created = true
			id = dsString(e, "response", "id")
			nodes = append(nodes, i)
		case "response.audio.delta":
			b, err := base64.StdEncoding.DecodeString(dsString(e, "delta"))
			if !created || terminal || dsString(e, "response_id") != id || err != nil || len(b) == 0 || len(b) > dsMaxAudio-audioBytes {
				return nil
			}
			audioBytes += len(b)
			nodes = append(nodes, i)
		case "response.done":
			if !created || terminal || audioBytes == 0 || dsString(e, "response", "id") != id || dsString(e, "response", "status") != "completed" {
				return nil
			}
			terminal = true
			nodes = append(nodes, i)
		}
	}
	if !terminal {
		return nil
	}
	return nodes
}

func dsAutomaticCommitEvidence(records []dsRecord) []int {
	for i, rec := range records {
		var e map[string]any
		if rec.Direction != "send" || rec.Kind != "message" || json.Unmarshal(rec.Payload, &e) != nil || dsString(e, "type") != "session.finish" {
			continue
		}
		nodes := dsAutomaticCommitResponse(records[:i])
		if len(nodes) == 0 {
			return nil
		}
		for j := i + 1; j < len(records); j++ {
			if records[j].Direction != "receive" || records[j].Kind != "message" {
				continue
			}
			if json.Unmarshal(records[j].Payload, &e) != nil {
				return nil
			}
			switch dsString(e, "type") {
			case "response.created", "response.audio.delta", "response.done":
				return nil
			case "session.finished":
				return append(nodes, i, j)
			}
		}
		return nil
	}
	return nil
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

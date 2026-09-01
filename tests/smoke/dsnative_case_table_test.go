//go:build smoke

package smoke_test

import (
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// recordCases 是 Chat -> DashScope Native 路径本期必须交付举证的 11 个测试用例元数据表。
// 降级用例（expectDegraded=true）必须包含显式降级举证。
//
// audio_input 不在本表：设计处置仍是 PASS，但 qwen-audio-turbo 免费额度耗尽、
// 没有真实 fixture——无证据不兑现（ADR-0001）。未来恢复投放的条件见
// 2026-08-30 部分投放设计的「后续恢复条件」。
//
// multi_candidate_stream 不在本表：本期不承诺多候选流式，stream=true 搭 n>1
// 在出门前就被拒成 422。给它留一个录制格子，等于让录制器去打一个网关自己
// 都不会放行的请求——录不出任何证据，却先把这项能力写进了名单。
// 非流式的 multi_candidate_nonstream 不受影响，仍在本表。
var recordCases = []caseMeta{
	{
		name:           "basic",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         false,
		expectDegraded: false,
		note:           "text_generation 非流式基础文本生成",
		body:           bodyBasic,
	},
	{
		name:           "streaming",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         true,
		expectDegraded: false,
		note:           "streaming 流式生成（Native SSE 转 Chat SSE，包含 usage 与 [DONE]）",
		body:           bodyStreaming,
	},
	{
		name:           "tool_calling",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         true,
		expectDegraded: false,
		note:           "tool_calling 工具声明、历史、调用与跨帧 arguments",
		body:           bodyToolCalling,
	},
	{
		name:           "vision_input",
		door:           nativewire.DoorMultimodalGeneration,
		modelRole:      modelRoleVL,
		stream:         false,
		expectDegraded: false,
		note:           "vision_input 图像输入（URL/Data URI）",
		body:           bodyVisionInput,
	},
	{
		name:           "reasoning",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleReasoning,
		stream:         true,
		expectDegraded: false,
		note:           "reasoning 深度思考流式输出（reasoning_effort 与流式 reasoning_content）",
		body:           bodyReasoning,
	},
	{
		name:           "parallel_tool_calls",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         false,
		expectDegraded: true,
		note:           "parallel_tool_calls 并行工具调用降级（显式字段映射与降级头）",
		body:           bodyParallelToolCalls,
	},
	{
		name:           "structured_output",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         false,
		expectDegraded: true,
		note:           "structured_output 结构化输出降级（json_object/json_schema 保留与降级头）",
		body:           bodyStructuredOutput,
	},
	{
		name:           "web_search",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         false,
		expectDegraded: true,
		note:           "web_search 联网搜索降级（options 转 enable_search 与降级头）",
		body:           bodyWebSearch,
	},
	{
		name:           "combined",
		door:           nativewire.DoorMultimodalGeneration,
		modelRole:      modelRoleCombined,
		stream:         false,
		expectDegraded: true,
		note:           "combined 多能力组合（vision + tools + web_search + structured_output 证明可组合）",
		body:           bodyCombined,
	},
	{
		name:           "multi_candidate_nonstream",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         false,
		expectDegraded: false,
		note:           "multi_candidate_nonstream 多候选非流式（n=2 搭载在 text_generation 上的证据）",
		body:           bodyMultiCandidateNonStream,
	},
	{
		name:           "parallel_tool_calls_default",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         false,
		expectDegraded: false,
		note:           "parallel_tool_calls_default 缺省时出站体含 true",
		body:           bodyParallelToolCallsDefault,
	},
}

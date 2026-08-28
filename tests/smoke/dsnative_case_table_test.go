//go:build smoke

package smoke_test

import (
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// recordCases 是 Chat -> DashScope Native 路径必须交付举证的 13 个测试用例元数据表。
// 降级用例（expectDegraded=true）必须包含显式降级举证。
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
		name:           "audio_input",
		door:           nativewire.DoorMultimodalGeneration,
		modelRole:      modelRoleAudio,
		stream:         false,
		expectDegraded: false,
		note:           "audio_input 音频输入",
		body:           bodyAudioInput,
	},
	{
		name:           "reasoning",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         true,
		expectDegraded: false,
		note:           "reasoning 深度思考流式输出（enable_thinking 与流式 reasoning_content）",
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
		name:           "multi_candidate_stream",
		door:           nativewire.DoorTextGeneration,
		modelRole:      modelRoleText,
		stream:         true,
		expectDegraded: false,
		note:           "multi_candidate_stream 多候选流式（n=2 搭载在 streaming 上的证据）",
		body:           bodyMultiCandidateStream,
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

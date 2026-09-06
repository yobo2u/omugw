//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
)

// preflightClientModel 是本地预检时客户端侧提交的模型名。
//
// 刻意不取任何角色的真实模型名：出站体的 model 必须被改写成路由选定的上游
// 模型，两边同名时「没改写」与「改对了」看起来一模一样。
const preflightClientModel = "omugw-preflight-client-model"

// chatBody 把带一个 %s 占位符（模型名）的模板固化成请求体构造器。
//
// 模型名先经 JSON 编码再插入：角色模型名可由环境变量覆盖，直接拼接会让一个
// 带引号或反斜杠的名字产出语法非法的请求体，而这错误要等到上游 400 才现形。
func chatBody(template string) func(model string) json.RawMessage {
	return func(model string) json.RawMessage {
		// 字符串编码不会失败，因此这里的错误无需分支。
		name, _ := json.Marshal(model)
		return json.RawMessage(fmt.Sprintf(template, name))
	}
}

const (
	// tinyPNGDataURI 是 16×16 像素的合法 PNG，80 字节。
	// 用静态字节而不是读本地文件：录制器要能在任何检出目录里跑出同一份请求。
	tinyPNGDataURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAIAAACQkWg2" +
		"AAAAF0lEQVR4nGJiIBGMahjVMIw1AAIAAP//NyAAIT+Rv2wAAAAASUVORK5CYII="

	// preflightImageURL 是经 DashScope 官方文档验证的公开示例图片 URL。
	// 本地预检只验证 URL 语法在网关映射中的原样透传（网关不代下载，原则 2.6）；
	// 真实录制时由上游模型自行抓取此公开样本，验证远端 URL 图像输入端到端可用。
	preflightImageURL = "https://dashscope.oss-cn-beijing.aliyuncs.com/images/dog_and_girl.jpeg"

	// tinyWAVBase64 是 44 字节的 RIFF/WAVE 头（data 块长度为 0），合法且最小。
	tinyWAVBase64 = "UklGRiQAAABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YQAAAAA="
)

// 11 份举证请求体，另加一份音频负例请求体（bodyAudioInput，仅供 501 探针使用，
// 不进 recordCases）。每一份只做一件事：把该用例声称的那项能力真正用上，
// 且不夹带任何在 DashScope Native 无落点的字段（frequency_penalty / logit_bias /
// service_tier / store / user / metadata / audio）——夹带一个就会在出门前被
// rejectUnmappable 拦成 422，录到的将是网关的拒绝而不是上游的能力。
var (
	bodyBasic = chatBody(`{"model":%s,"messages":[` +
		`{"role":"system","content":"你是一个严谨的中文助手，回答尽量简短。"},` +
		`{"role":"user","content":"用一句话介绍杭州。"}]}`)

	bodyStreaming = chatBody(`{"model":%s,"stream":true,` +
		`"stream_options":{"include_usage":true},"messages":[` +
		`{"role":"user","content":"用一句话介绍杭州。"}]}`)

	// 工具续轮：声明 + 历史调用 + 工具结果 + 新指令，四段缺一不可。
	// 少了历史或结果，上游看到的就只是一次全新的首轮调用，跨轮关联根本没被考过。
	bodyToolCalling = chatBody(`{"model":%s,"stream":true,` +
		`"tools":[{"type":"function","function":{"name":"get_weather",` +
		`"description":"查询指定城市未来若干天的天气",` +
		`"parameters":{"type":"object","properties":{` +
		`"city":{"type":"string","description":"城市中文名"},` +
		`"unit":{"type":"string","enum":["celsius","fahrenheit"]},` +
		`"days":{"type":"integer","minimum":1,"maximum":7},` +
		`"include_hourly":{"type":"boolean"},` +
		`"note":{"type":"string","description":"附加说明，可以很长"}},` +
		`"required":["city","unit","days"]}}}],"tool_choice":"auto","messages":[` +
		`{"role":"user","content":"杭州今天天气怎么样？"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_smoke_0001",` +
		`"type":"function","function":{"name":"get_weather",` +
		`"arguments":"{\"city\":\"杭州\",\"unit\":\"celsius\",\"days\":1}"}}]},` +
		`{"role":"tool","tool_call_id":"call_smoke_0001",` +
		`"content":"{\"city\":\"杭州\",\"temp_c\":21,\"condition\":\"多云\"}"},` +
		`{"role":"user","content":"再帮我查一次：城市杭州，单位摄氏度，未来三天，` +
		`需要逐小时数据，并在备注里写明这是给周末骑行行程做参考的天气查询。"}]}`)

	bodyVisionInput = chatBody(`{"model":%s,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"这两张图分别是什么？"},` +
		`{"type":"image_url","image_url":{"url":"` + preflightImageURL + `"}},` +
		`{"type":"image_url","image_url":{"url":"` + tinyPNGDataURI + `"}}]}]}`)

	bodyAudioInput = chatBody(`{"model":%s,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"这段音频里说了什么？"},` +
		`{"type":"input_audio","input_audio":{"data":"` + tinyWAVBase64 + `","format":"wav"}}]}]}`)

	// 推理必须搭流式：非流式思考在 Native 是未定义行为，出门前就会被拒。
	bodyReasoning = chatBody(`{"model":%s,"stream":true,"reasoning_effort":"low",` +
		`"messages":[{"role":"user","content":"一个笼子里有鸡和兔共 8 只、腿共 26 条，各几只？"}]}`)

	bodyParallelToolCalls = chatBody(`{"model":%s,"parallel_tool_calls":true,` +
		`"tools":[{"type":"function","function":{"name":"get_weather",` +
		`"description":"查询指定城市的天气",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},` +
		`"required":["city"]}}},` +
		`{"type":"function","function":{"name":"get_time",` +
		`"description":"查询指定城市的当前时间",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},` +
		`"required":["city"]}}}],"messages":[` +
		`{"role":"user","content":"同时告诉我杭州的天气和当前时间。"}]}`)

	bodyStructuredOutput = chatBody(`{"model":%s,` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"city_brief",` +
		`"strict":true,"schema":{"type":"object","properties":{` +
		`"name":{"type":"string"},"province":{"type":"string"},` +
		`"population_million":{"type":"number"}},` +
		`"required":["name","province","population_million"],` +
		`"additionalProperties":false}}},"messages":[` +
		`{"role":"user","content":"用 JSON 描述杭州。"}]}`)

	bodyWebSearch = chatBody(`{"model":%s,` +
		`"web_search_options":{"search_context_size":"medium"},"messages":[` +
		`{"role":"user","content":"杭州今天有什么新闻？"}]}`)

	// 一个请求里同时压上视觉 / 工具 / 联网 / 结构化输出四族，且只用一个模型：
	// 拆成两个模型就证明不了「它们能在同一次调用里共存」。
	bodyCombined = chatBody(`{"model":%s,` +
		`"web_search_options":{"search_context_size":"medium"},` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"image_report",` +
		`"strict":true,"schema":{"type":"object","properties":{` +
		`"summary":{"type":"string"},"needs_tool":{"type":"boolean"}},` +
		`"required":["summary","needs_tool"],"additionalProperties":false}}},` +
		`"tools":[{"type":"function","function":{"name":"get_weather",` +
		`"description":"查询指定城市的天气",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},` +
		`"required":["city"]}}}],"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看图并结合最新资讯，必要时调用工具，最后按 schema 输出。"},` +
		`{"type":"image_url","image_url":{"url":"` + tinyPNGDataURI + `"}}]}]}`)

	// 多候选不带 tools、也不带 reasoning：Native 在这两种组合下会把 n 静默压回 1，
	// 带上它们等于把「n 生效了吗」这个问题换成一个没有答案的问题。
	//
	// 只有非流式一份：本期 stream=true 搭 n>1 在出门前就被拒成 422，
	// 留一份流式多候选请求体等于给一个永远发不出去的请求备着弹药。
	bodyMultiCandidateNonStream = chatBody(`{"model":%s,"n":2,"messages":[` +
		`{"role":"user","content":"给这家咖啡店起个名字。"}]}`)

	// 刻意不写 parallel_tool_calls：考的正是「客户端没提交时出站体是否被注入 true」。
	bodyParallelToolCallsDefault = chatBody(`{"model":%s,` +
		`"tools":[{"type":"function","function":{"name":"get_weather",` +
		`"description":"查询指定城市的天气",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},` +
		`"required":["city"]}}}],"messages":[` +
		`{"role":"user","content":"杭州今天天气怎么样？"}]}`)
)

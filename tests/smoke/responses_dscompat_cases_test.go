//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/protocol/openairesponses"
	"github.com/yobo2u/omugw/internal/testkit"
)

// 录制产物按 harness 的固定名字写入：request 里是逻辑模型名，upstream 断言里
// 是 harness 的默认上游模型名。真实模型名只在发给上游的那一份请求里出现。
const (
	rdcLogicalModel         = "logical-fast"
	rdcHarnessUpstreamModel = "upstream-model"
	rdcFixtureDir           = "../../testdata/routes/openai.responses__dashscope.compatible"
	rdcFixturePrefix        = "openai.responses__dashscope.compatible/"
)

// rdcFakeSeed 是离线校验用的假种子：形状与真实首轮响应一致，内容是桩。
var rdcFakeSeed = rdcSeed{
	ID: "resp_seed",
	Output: json.RawMessage(`[{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"3","annotations":[]}]}]`),
}

// TestResponsesDSCompatCasesAreWellFormed 在不触网的前提下校验用例表：
// 录制要花钱，表写错了应该在这里而不是在账单上发现。
func TestResponsesDSCompatCasesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range rdcCases() {
		t.Run(c.name, func(t *testing.T) {
			if seen[c.name] {
				t.Fatalf("用例名重复: %s", c.name)
			}
			seen[c.name] = true
			if c.note == "" {
				t.Error("缺少 note：半年后没人知道这份录制在证明什么")
			}
			client := c.build(rdcFakeSeed)
			// 客户端请求体必须过得了网关的严格解码，否则回放时连上游都到不了。
			if _, err := openairesponses.Decode([]byte(client)); err != nil {
				t.Fatalf("客户端请求体过不了网关解码: %v\n%s", err, client)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(client), &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["model"]) != `"`+rdcLogicalModel+`"` {
				t.Errorf("model = %s，期望逻辑模型名 %q", fields["model"], rdcLogicalModel)
			}
			if _, has := fields["stream"]; has != c.stream {
				t.Errorf("stream 字段存在 = %v，与用例声明 %v 不符", has, c.stream)
			}

			f, err := rdcFixture(c, client, c.status(), "application/json", []byte(`{"id":"resp_x"}`))
			if c.stream {
				f, err = rdcFixture(c, client, 200, "text/event-stream",
					[]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
			}
			if err != nil {
				t.Fatalf("构造 fixture 失败: %v", err)
			}
			if err := f.Validate(); err != nil {
				t.Fatalf("fixture 不合法: %v", err)
			}
			var up map[string]json.RawMessage
			if err := json.Unmarshal(f.Upstream.Body, &up); err != nil {
				t.Fatal(err)
			}
			if string(up["model"]) != `"`+rdcHarnessUpstreamModel+`"` {
				t.Errorf("upstream 断言的 model = %s，期望 %q", up["model"], rdcHarnessUpstreamModel)
			}
			if bytes.Contains(f.Upstream.Body, []byte("web_search_preview")) {
				t.Error("upstream 断言里不该还有 web_search_preview——适配器会把它改名")
			}
		})
	}

	// 有损格子按「文件名即能力名」举证（degrade 的 fixture 门槛），缺一份就过不了 CI。
	for _, name := range []string{"image_detail", "web_search", "structured_output"} {
		if !seen[name] {
			t.Errorf("缺少与能力同名的用例 %q", name)
		}
	}
}

// TestRDCFixtureRedactsRequestID 防的是上游工单号入库：它不是凭据，但能
// 关联到账号的调用记录，没有理由留在仓库里。
func TestRDCFixtureRedactsRequestID(t *testing.T) {
	c := rdcCases()[0]
	f, err := rdcFixture(c, c.build(rdcFakeSeed), 400, "application/json",
		[]byte(`{"request_id":"abc-123","code":"InvalidParameter","message":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.Response.Body, []byte("abc-123")) {
		t.Errorf("request_id 未脱敏: %s", f.Response.Body)
	}
	if !bytes.Contains(f.Response.Body, []byte(`"request_id"`)) {
		t.Error("request_id 键应保留——扁平信封的形状是证据的一部分")
	}
}

// TestRDCHasFunctionCall 防的是守卫自己失灵：它放过无调用的种子，或把坏 JSON
// 当成有调用，录制就会白花钱。
func TestRDCHasFunctionCall(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"含 function_call", `[{"type":"reasoning"},{"type":"function_call","call_id":"c1"}]`, true},
		{"只有 message", `[{"type":"message"}]`, false},
		{"空数组", `[]`, false},
		{"非法 JSON", `{not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rdcHasFunctionCall(json.RawMessage(tc.output)); got != tc.want {
				t.Errorf("rdcHasFunctionCall(%s) = %v，期望 %v", tc.output, got, tc.want)
			}
		})
	}
}

// rdcHasFunctionCall 报告种子轮的 output 是否含 function_call 条目。
//
// 防的是花钱录一份没有工具结果的 tool-result fixture：种子轮若被模型直接
// 文字作答，rdcToolOutputs 会拼出空串，随后的请求照样 200，录制成功却什么
// 也没证明，只能在审阅时才发现。output 解析失败按「没有」处理。
func rdcHasFunctionCall(output json.RawMessage) bool {
	var items []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(output, &items); err != nil {
		return false
	}
	for _, it := range items {
		if it.Type == "function_call" {
			return true
		}
	}
	return false
}

// rdcSeed 是种子轮（不入库的前一轮真实请求）留下的两样东西。
type rdcSeed struct {
	ID     string
	Output json.RawMessage
}

// rdcCase 是一份待录制的用例。
type rdcCase struct {
	name string
	note string

	// seed 非空时先把它发给上游，取回 id 与 output 交给 build。种子轮不入库。
	seed string
	// build 给出客户端发给网关的请求体（model 为逻辑名）。
	build func(seed rdcSeed) string
	// upstream 给出适配器修补后的期望形状；nil 表示与客户端请求体只差 model。
	upstream func(client string) string

	stream     bool
	wantStatus int
}

func (c rdcCase) status() int {
	if c.wantStatus == 0 {
		return 200
	}
	return c.wantStatus
}

func rdcStatic(body string) func(rdcSeed) string {
	return func(rdcSeed) string { return body }
}

// rdcSetModel 把请求体里的 model 换掉，其余字段保持。
func rdcSetModel(body, model string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %w", err)
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = m
	return json.Marshal(fields)
}

// rdcPNG 生成一张 64×64 的渐变图，足够让视觉模型说出主色调，又小到不值得
// 为它操心内联上限。
func rdcPNG() string {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(200 + x/2), G: uint8(40 + y), B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // 内存编码一张固定图片不会失败
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

const (
	rdcWeatherTool = `{"type":"function","name":"get_weather","description":"查询指定城市的当前天气",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}`
	rdcTimeTool = `{"type":"function","name":"get_time","description":"查询指定城市的当前时间",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}`
	rdcHard   = `一个数除以 7 余 3，除以 5 余 2，最小的正整数是多少？`
	rdcSchema = `{"type":"object","additionalProperties":false,` +
		`"properties":{"city":{"type":"string"},"population_millions":{"type":"number"}},` +
		`"required":["city","population_millions"]}`
)

func rdcBody(rest string) string {
	return `{"model":"` + rdcLogicalModel + `",` + rest + `}`
}

func rdcImageInput(detail string) string {
	part := `{"type":"input_image","image_url":"` + rdcPNG() + `"`
	if detail != "" {
		part += `,"detail":"` + detail + `"`
	}
	part += `}`
	return `"store":false,"input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"这张图的主色调是什么？一句话。"},` + part + `]}]`
}

// rdcCases 是这条路径的全部录制用例。文件名即用例名；有损格子的举证用例
// 必须与能力同名（image_detail / web_search / structured_output）。
func rdcCases() []rdcCase {
	return []rdcCase{
		{
			name:  "basic",
			note:  "最小闭环：只改写 model，其余字段原样到达上游；响应原样回传。",
			build: rdcStatic(rdcBody(`"input":"用一句话介绍杭州。","store":false`)),
		},
		{
			name:   "streaming",
			note:   "流式文本：事件名与 OpenAI 同名，终止事件 response.completed 携带 usage。",
			build:  rdcStatic(rdcBody(`"input":"Say hi in three words.","store":false,"stream":true`)),
			stream: true,
		},
		{
			name:  "tool_calling",
			note:  "function 工具调用（非流式）。",
			build: rdcStatic(rdcBody(`"input":"查一下北京天气。","store":false,"tools":[` + rdcWeatherTool + `]`)),
		},
		{
			name:   "tool_calling-streaming",
			note:   "function 工具调用（流式）：response.function_call_arguments.delta/done 原样转发。",
			build:  rdcStatic(rdcBody(`"input":"查一下北京天气。","store":false,"stream":true,"tools":[` + rdcWeatherTool + `]`)),
			stream: true,
		},
		{
			name: "tool_calling-result",
			note: "工具循环第二轮：把上一轮 output（含 function_call 项）原样回传并附 function_call_output。" +
				"证明网关解码器接受上游产出的输出项。",
			seed: rdcBody(`"input":"查一下北京天气。","store":false,"tools":[` + rdcWeatherTool + `]`),
			build: func(s rdcSeed) string {
				return rdcBody(`"store":false,"tools":[` + rdcWeatherTool + `],"input":` +
					rdcReplay(`{"role":"user","content":"查一下北京天气。"}`, s.Output,
						rdcToolOutputs(s.Output, `{"temp_c":21,"sky":"晴"}`)))
			},
		},
		{
			name: "parallel_tool_calls-true",
			note: "parallel_tool_calls 显式 true：上游并行发起多个调用。",
			build: rdcStatic(rdcBody(`"input":"请同时查询北京、上海、广州三个城市的天气，以及东京的当前时间。必须通过工具查询。",` +
				`"store":false,"parallel_tool_calls":true,"tools":[` + rdcWeatherTool + `,` + rdcTimeTool + `]`)),
		},
		{
			name: "parallel_tool_calls-false",
			note: "parallel_tool_calls 显式 false：上游一次只发起一个调用。与 -true 对照证明该字段被兑现。",
			build: rdcStatic(rdcBody(`"input":"请同时查询北京、上海、广州三个城市的天气，以及东京的当前时间。必须通过工具查询。",` +
				`"store":false,"parallel_tool_calls":false,"tools":[` + rdcWeatherTool + `,` + rdcTimeTool + `]`)),
		},
		{
			name:  "reasoning",
			note:  "推理：输出含 reasoning 项，usage 带 reasoning_tokens。",
			build: rdcStatic(rdcBody(`"input":"` + rdcHard + `","store":false,"reasoning":{"effort":"low"}`)),
		},
		{
			name:   "reasoning-streaming",
			note:   "推理（流式）：response.reasoning_text.delta/done 原样转发。",
			build:  rdcStatic(rdcBody(`"input":"` + rdcHard + `","store":false,"stream":true,"reasoning":{"effort":"low"}`)),
			stream: true,
		},
		{
			name: "reasoning-replay",
			note: "把上一轮 output（含 reasoning 项）原样回传后续问。#11 的录制项：思考历史在本路径原样到达上游。",
			seed: rdcBody(`"input":"` + rdcHard + `","store":false,"reasoning":{"effort":"low"}`),
			build: func(s rdcSeed) string {
				return rdcBody(`"store":false,"input":` + rdcReplay(
					`{"role":"user","content":"`+rdcHard+`"}`, s.Output,
					`{"role":"user","content":"把刚才的答案加 1 是多少？只回数字。"}`))
			},
		},
		{
			name:  "vision_input",
			note:  "图片输入，不带 detail：不应出现降级头。",
			build: rdcStatic(rdcBody(rdcImageInput(""))),
		},
		{
			name:  "image_detail",
			note:  "DEGRADE 举证（文件名即能力名）：detail 原样到达上游，但上游不区分档位——与 vision_input 的 input_tokens 相同。",
			build: rdcStatic(rdcBody(rdcImageInput("low"))),
		},
		{
			name: "web_search",
			note: "DEGRADE 举证（文件名即能力名）：内建 web_search 由上游执行，输出含 web_search_call。",
			build: rdcStatic(rdcBody(`"input":"今天杭州的天气如何？","store":false,` +
				`"tools":[{"type":"web_search"}]`)),
		},
		{
			name: "web_search-preview",
			note: "预览版类型：网关把 tools 与 tool_choice 里的 web_search_preview 改写为 web_search，上游因此真的执行搜索。",
			build: rdcStatic(rdcBody(`"input":"今天杭州的天气如何？","store":false,` +
				`"tools":[{"type":"web_search_preview"}],"tool_choice":{"type":"web_search_preview"}`)),
			upstream: func(client string) string {
				return strings.ReplaceAll(client, `"web_search_preview"`, `"web_search"`)
			},
		},
		{
			name: "structured_output",
			note: "DEGRADE 举证（文件名即能力名）：text.format=json_schema 原样到达上游，上游不兑现，返回自由文本。",
			build: rdcStatic(rdcBody(`"input":"上海是哪个国家的城市？人口大约多少？","store":false,` +
				`"text":{"format":{"type":"json_schema","name":"city_info","strict":true,"schema":` + rdcSchema + `}}`)),
		},
		{
			name: "structured_output-json_object",
			note: "text.format=json_object 原样到达上游；上游不保证输出是可直接解析的 JSON。",
			build: rdcStatic(rdcBody(`"input":"上海是哪个国家的城市？人口大约多少？请用 JSON 回答。","store":false,` +
				`"text":{"format":{"type":"json_object"}}`)),
		},
		{
			name: "structured_output-tool-strict",
			note: "function 工具带 strict:true：与 text.format 共用 structured_output 一格，工具调用照常可用，上游不校验 strict。",
			build: rdcStatic(rdcBody(`"input":"查一下北京天气。","store":false,` +
				`"tools":[` + strings.Replace(rdcWeatherTool, `"type":"function",`, `"type":"function","strict":true,`, 1) + `]`)),
		},
		{
			name: "stateful-continue",
			note: "会话续轮：只传 previous_response_id，上游答出首轮口令。previous_response_id 原样到达上游，response.id 不改写。",
			seed: rdcBody(`"input":"My secret codeword is MANGO42. Reply OK."`),
			build: func(s rdcSeed) string {
				return rdcBody(`"input":"What was my codeword? One word.","previous_response_id":"` + s.ID + `"`)
			},
		},
		{
			name: "stateful-store-false",
			note: "store:false 的一轮不可续：上游返回扁平信封的 400，网关映射为 bad_request，不重试、不冷却凭据。",
			seed: rdcBody(`"input":"My secret codeword is KIWI77. Reply OK.","store":false`),
			build: func(s rdcSeed) string {
				return rdcBody(`"input":"What was my codeword? One word.","previous_response_id":"` + s.ID + `"`)
			},
			wantStatus: 400,
		},
		{
			name:  "stateful-store-omitted",
			note:  "省略 store：网关不写入该字段，上游按默认值存储（响应回显 store:true，留存 7 天）。",
			build: rdcStatic(rdcBody(`"input":"Reply OK."`)),
		},
		{
			name: "error-required-two-tools",
			note: "上游更窄的语义：tool_choice=required 配多个工具时上游 400，网关原样映射。",
			build: rdcStatic(rdcBody(`"input":"你好。","store":false,"tool_choice":"required",` +
				`"tools":[` + rdcWeatherTool + `,` + rdcTimeTool + `]`)),
			wantStatus: 400,
		},
	}
}

// rdcReplay 拼出「前文 + 上一轮 output 的全部条目 + 后文」的 input 数组。
// output 条目逐字节保留：要证明的正是网关与上游都接受上游自己产出的形状。
func rdcReplay(before string, output json.RawMessage, after string) string {
	var items []json.RawMessage
	_ = json.Unmarshal(output, &items)
	parts := []string{before}
	for _, it := range items {
		parts = append(parts, string(it))
	}
	if after != "" {
		parts = append(parts, after)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// rdcToolOutputs 为 output 里的每个 function_call 生成一条 function_call_output。
// 没有任何调用时返回空串——用例随后会因上游答非所问而在录制审阅时被发现。
func rdcToolOutputs(output json.RawMessage, result string) string {
	var items []struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	_ = json.Unmarshal(output, &items)
	var outs []string
	for _, it := range items {
		if it.Type != "function_call" {
			continue
		}
		quoted, _ := json.Marshal(result)
		outs = append(outs, `{"type":"function_call_output","call_id":"`+it.CallID+`","output":`+string(quoted)+`}`)
	}
	return strings.Join(outs, ",")
}

// rdcFixture 把一次真实交互装成 fixture。
//
// request 是客户端发给网关的内容；upstream 是适配器修补后应当发出的形状；
// response 是真实上游对后者（model 换成真实模型名）的回答。响应头只留
// content-type：其余头要么是工单号、要么随时间变化，进了 golden 只会制造噪音。
func rdcFixture(c rdcCase, clientBody string, status int, contentType string, respBody []byte) (testkit.Fixture, error) {
	expect := clientBody
	if c.upstream != nil {
		expect = c.upstream(clientBody)
	}
	upstreamBody, err := rdcSetModel(expect, rdcHarnessUpstreamModel)
	if err != nil {
		return testkit.Fixture{}, err
	}

	f := testkit.Fixture{
		Name: rdcFixturePrefix + c.name,
		Note: c.note,
		Request: testkit.Request{
			Method:  "POST",
			Path:    "/v1/responses",
			Headers: map[string]string{"authorization": "<redacted>", "content-type": "application/json"},
			Body:    json.RawMessage(clientBody),
		},
		Upstream: &testkit.UpstreamExpectation{
			Method: "POST",
			Path:   "/v1/responses",
			Body:   upstreamBody,
		},
		Response: testkit.Response{Status: status},
	}

	if strings.HasPrefix(contentType, "text/event-stream") {
		events, err := testkit.ParseSSE(bytes.NewReader(respBody))
		if err != nil {
			return testkit.Fixture{}, fmt.Errorf("解析上游 SSE 失败: %w", err)
		}
		if len(events) == 0 {
			return testkit.Fixture{}, fmt.Errorf("上游 SSE 没有任何事件")
		}
		f.Response.Headers = map[string]string{"content-type": "text/event-stream"}
		f.Response.SSE = &testkit.SSEBody{Events: events}
		return f, nil
	}

	if !json.Valid(respBody) {
		return testkit.Fixture{}, fmt.Errorf("上游响应体不是 JSON（status %d）", status)
	}
	f.Response.Headers = map[string]string{"content-type": "application/json"}
	f.Response.Body = rdcRedactRequestID(respBody)
	return f, nil
}

// rdcRedactRequestID 把顶层 request_id 的值换成占位，键保留。
func rdcRedactRequestID(body []byte) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	if _, ok := fields["request_id"]; !ok {
		return body
	}
	fields["request_id"] = json.RawMessage(`"<redacted>"`)
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}

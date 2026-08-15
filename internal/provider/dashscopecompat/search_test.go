package dashscopecompat

import "testing"

// 本文件只钉一件事：web_search_options → enable_search 这处语义映射。
// 它是本适配器与同源直通的唯一实质差别，值得与线格式断言分开看。

// searchInput 固定住与搜索无关的输入，让每个用例的字面量里只剩下区分它的请求体。
func searchInput(raw string) callInput {
	return callInput{raw: raw, upstreamModel: "m", path: ChatCompletionsPath}
}

// TestNoSearchNoEnableSearch：客户端没要搜索时不得替它开启。
//
// 替客户端开搜索不是「更好用」，是静默改语义：搜索会改变输出内容、增加计费，
// 而客户端从响应里看不出网关做过这件事。
func TestNoSearchNoEnableSearch(t *testing.T) {
	srv, got := okServer(t)

	if _, err := call(t, srv, searchInput(
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatal(err)
	}

	if _, ok := upstreamFields(t, got)["enable_search"]; ok {
		t.Errorf("无 web_search_options 时不得注入 enable_search: %s", got.body)
	}
}

// TestSearchOptionsMappedToEnableSearch：非空 web_search_options 整体删除，
// 换写 enable_search: true。
//
// 整体删除而不是逐字段搬运：search_context_size / user_location 在 DashScope
// 没有落点，猜一个映射等于替客户端编造行为。损失登记在降级矩阵里，
// 随响应头明说，比悄悄猜一个更诚实。
func TestSearchOptionsMappedToEnableSearch(t *testing.T) {
	srv, got := okServer(t)

	raw := `{"model":"m","messages":[{"role":"user","content":"新闻"}],` +
		`"web_search_options":{"search_context_size":"high",` +
		`"user_location":{"type":"approximate","approximate":{` +
		`"country":"CN","city":"上海","timezone":"Asia/Shanghai"}}}}`
	if _, err := call(t, srv, searchInput(raw)); err != nil {
		t.Fatal(err)
	}

	fields := upstreamFields(t, got)
	if _, ok := fields["web_search_options"]; ok {
		t.Errorf("web_search_options 应被删除: %s", got.body)
	}
	if string(fields["enable_search"]) != "true" {
		t.Errorf("enable_search = %s，期望 true", fields["enable_search"])
	}
}

// TestEmptySearchOptionsStillEnables：{} 正是客户端的显式搜索请求。
func TestEmptySearchOptionsStillEnables(t *testing.T) {
	srv, got := okServer(t)

	if _, err := call(t, srv, searchInput(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":{}}`,
	)); err != nil {
		t.Fatal(err)
	}

	fields := upstreamFields(t, got)
	if string(fields["enable_search"]) != "true" {
		t.Errorf("enable_search = %s，期望 true", fields["enable_search"])
	}
	if _, ok := fields["web_search_options"]; ok {
		t.Errorf("web_search_options 应被删除: %s", got.body)
	}
}

// TestNullSearchOptionsStayUntouched：null 与缺省同义——不开搜索，也不动字段。
func TestNullSearchOptionsStayUntouched(t *testing.T) {
	srv, got := okServer(t)

	if _, err := call(t, srv, searchInput(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":null}`,
	)); err != nil {
		t.Fatal(err)
	}

	fields := upstreamFields(t, got)
	if _, ok := fields["enable_search"]; ok {
		t.Errorf("null 等同缺省，不得注入 enable_search: %s", got.body)
	}
	if string(fields["web_search_options"]) != "null" {
		t.Errorf("null 的 web_search_options 应原样保留: %s", got.body)
	}
}

// TestUnrelatedFieldsPreserved：除两处修补点外全部字段保持原语义——
// 包括网关没有建模的字段。这是不经 Canonical 往返的理由：当前 IR 不承载
// n / penalty / logprobs 等参数，往返一次它们就悄悄消失，而客户端收不到任何提示。
func TestUnrelatedFieldsPreserved(t *testing.T) {
	srv, got := okServer(t)

	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"n":2,"presence_penalty":0.5,"frequency_penalty":-0.5,"logprobs":true,"top_logprobs":3,` +
		`"tools":[{"type":"function","function":{"name":"f"}}],` +
		`"stream_options":{"include_usage":true},` +
		`"response_format":{"type":"json_object"},` +
		`"web_search_options":{},` +
		`"brand_new_param":{"nested":[1,2,3]}}`
	in := searchInput(raw)
	in.upstreamModel = "upstream"
	if _, err := call(t, srv, in); err != nil {
		t.Fatal(err)
	}

	fields := upstreamFields(t, got)
	want := map[string]string{
		"n":                 "2",
		"presence_penalty":  "0.5",
		"frequency_penalty": "-0.5",
		"logprobs":          "true",
		"top_logprobs":      "3",
		"tools":             `[{"type":"function","function":{"name":"f"}}]`,
		"stream_options":    `{"include_usage":true}`,
		"response_format":   `{"type":"json_object"}`,
		"brand_new_param":   `{"nested":[1,2,3]}`,
	}
	for k, w := range want {
		if string(fields[k]) != w {
			t.Errorf("字段 %s = %s，期望原样保留 %s", k, fields[k], w)
		}
	}
	if string(fields["enable_search"]) != "true" {
		t.Errorf("enable_search = %s，期望 true", fields["enable_search"])
	}
	if _, ok := fields["web_search_options"]; ok {
		t.Errorf("web_search_options 应被删除: %s", got.body)
	}
}

// TestClientEnableSearchPreservedWithoutOptions：客户端自己带的 enable_search
// （DashScope 原生参数）与搜索映射无关，不得被改动。
func TestClientEnableSearchPreservedWithoutOptions(t *testing.T) {
	srv, got := okServer(t)

	if _, err := call(t, srv, searchInput(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"enable_search":false}`,
	)); err != nil {
		t.Fatal(err)
	}

	if v := string(upstreamFields(t, got)["enable_search"]); v != "false" {
		t.Errorf("客户端自带的 enable_search=false 应原样保留: %s", got.body)
	}
}

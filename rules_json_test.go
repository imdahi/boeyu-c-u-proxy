package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestJsonPatch(t *testing.T) {
	patch, ok := parseJsonPatch(json.RawMessage(`{"json":{"status":200,"msg":"","data.token":"xyz","arr.0.id":9}}`))
	if !ok {
		t.Fatal("parse failed")
	}
	body := []byte(`{"status":101,"msg":"密码有误","data":null,"arr":[{"id":1}]}`)
	out, ok2 := applyJsonPatch(body, patch)
	if !ok2 {
		t.Fatal("apply failed")
	}
	want := `{"status":200,"msg":"","data":{"token":"xyz"},"arr":[{"id":9}]}`
	var gotV, wantV any
	json.Unmarshal(out, &gotV)
	json.Unmarshal([]byte(want), &wantV)
	ga, _ := json.Marshal(gotV)
	wa, _ := json.Marshal(wantV)
	if string(ga) != string(wa) {
		t.Fatalf("got %s want %s", ga, wa)
	}
	notJSON, ok3 := applyJsonPatch([]byte(`<html>oops</html>`), patch)
	if ok3 {
		t.Fatal("expected no patch on non-json")
	}
	if string(notJSON) != "" {
		t.Fatalf("expected nil bytes, got %s", notJSON)
	}
	noMatch, ok4 := applyJsonPatch([]byte(`{"a":1}`), map[string]json.RawMessage{"a.b.c": json.RawMessage(`1`)})
	if ok4 {
		t.Fatal("expected no change when path descends through a scalar")
	}
	if string(noMatch) != "" {
		t.Fatalf("expected nil bytes, got %s", noMatch)
	}
	createLeaf, ok5 := applyJsonPatch([]byte(`{"a":1}`), map[string]json.RawMessage{"b": json.RawMessage(`2`)})
	if !ok5 || string(createLeaf) != `{"a":1,"b":2}` {
		t.Fatalf("expected missing leaf to be created, got %s ok=%v", createLeaf, ok5)
	}
}

func TestJsonPatchAppend(t *testing.T) {
	// flagList append (+): add "1004" to a comma-separated string field
	patchFlags, ok := parseJsonPatch(json.RawMessage(`{"json":{"data.strategyDeviceInterface.deviceInterfaceFlagList+":"1004,1004,1099"}}`))
	if !ok {
		t.Fatal("parse flags append patch failed")
	}
	out, ok2 := applyJsonPatch([]byte(`{"data":{"strategyDeviceInterface":{"deviceInterfaceFlagList":"1003,1004,1005"}}}`), patchFlags)
	if !ok2 {
		t.Fatal("apply flags append failed")
	}
	if string(out) != `{"data":{"strategyDeviceInterface":{"deviceInterfaceFlagList":"1003,1004,1005,1099"}}}` {
		t.Fatalf("flags append got %s", out)
	}

	// whitelist append (+): objects deduplicated by packageName
	patchWL, ok3 := parseJsonPatch(json.RawMessage(`{"json":{"data.strategySoftwareWhiteList+":[` +
		`{"whiteId":"26031664FYXAFDGR","logo":"software/25101483F2BPM5AW.png","name":"all","isWhole":0,"whiteList":"*","packageName":"*"}]}}`))
	if !ok3 {
		t.Fatal("parse whitelist append patch failed")
	}
	outWL, ok4 := applyJsonPatch([]byte(`{"data":{"strategySoftwareWhiteList":[`+
		`{"whiteId":"26031664FYXAFDGC","logo":"software/25101483F2BPM5AW.png","name":"all","isWhole":0,"whiteList":"*","packageName":"com.foo"}]}}`), patchWL)
	if !ok4 {
		t.Fatal("apply whitelist append failed")
	}
	var got any
	json.Unmarshal(outWL, &got)
	list := got.(map[string]any)["data"].(map[string]any)["strategySoftwareWhiteList"].([]any)
	if len(list) != 2 {
		t.Fatalf("whitelist append should yield 2 entries, got %s", outWL)
	}
	pkg0 := list[0].(map[string]any)["packageName"].(string)
	pkg1 := list[1].(map[string]any)["packageName"].(string)
	if pkg0 != "com.foo" || pkg1 != "*" {
		t.Fatalf("whitelist append entries wrong: %s", outWL)
	}

	// appending an already-present entry is a no-op (patch not applied)
	outWL2, ok5 := applyJsonPatch([]byte(`{"data":{"strategySoftwareWhiteList":[{"whiteId":"1","name":"all","packageName":"*"}]}}`), patchWL)
	if ok5 || string(outWL2) != "" {
		t.Fatalf("duplicate whitelist entry must be a no-op, got ok=%v out=%s", ok5, outWL2)
	}

	// missing target gets created as-is
	outNew, ok6 := applyJsonPatch([]byte(`{"data":{}}`), map[string]json.RawMessage{"data.strategyDeviceInterface.deviceInterfaceFlagList+": json.RawMessage(`"1004"`)})
	if !ok6 || string(outNew) != `{"data":{"strategyDeviceInterface":{"deviceInterfaceFlagList":"1004"}}}` {
		t.Fatalf("missing append target should be created, got %s", outNew)
	}
}

func TestNormalizeResponseBodies(t *testing.T) {
	raw := []byte(`[{"name":"strat","enabled":true,"url":"https://x/foo","type":"responseReplace","items":[` +
		`{"enabled":true,"type":"replaceResponseBody","values":{"body":"{\"status\":200,\"data\":{\"a\":\"line1\nline2\"},\"n\":1.0}"}}` +
		`]},` +
		`{"name":"plain","enabled":true,"url":"https://x/bar","type":"responseReplace","items":[` +
		`{"enabled":true,"type":"replaceResponseBody","values":{"body":"not-json-text"}}` +
		`]}]`)
	var rules []Rule
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	normalizeResponseBodies(rules)
	// the decoded body string must now be valid JSON (control chars escaped)
	data, err := json.Marshal(rules)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	var reload []Rule
	if err := json.Unmarshal(data, &reload); err != nil {
		t.Fatalf("reload: %v", err)
	}
	var bv ruleBodyValue
	json.Unmarshal(reload[0].Items[0].Values, &bv)
	var s string
	json.Unmarshal(bv.Body, &s)
	if !json.Valid([]byte(s)) {
		t.Fatalf("served body is still invalid JSON: %q", s)
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("served body parse: %v", err)
	}
	m := v.(map[string]any)
	if m["status"].(float64) != 200 {
		t.Fatalf("status wrong: %v", m["status"])
	}
	if d := m["data"].(map[string]any); d["a"] != "line1\nline2" {
		t.Fatalf("escaped value mangled: %q", d["a"])
	}
	// non-JSON string body must not be rewritten
	json.Unmarshal(reload[1].Items[0].Values, &bv)
	json.Unmarshal(bv.Body, &s)
	if s != "not-json-text" {
		t.Fatalf("non-json body was rewritten: %q", s)
	}
	// valid JSON body must round-trip byte-for-byte
	valid := []byte(`{"status":200,"data":{"a":"line1\nline2"},"n":1}`)
	if got := escapeJSONControl(valid); !bytes.Equal(got, valid) {
		t.Fatalf("escapeJSONControl changed valid input: %q -> %q", valid, got)
	}
}

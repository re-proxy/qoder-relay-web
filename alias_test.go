package main

import "testing"

func TestResolveModelRequest(t *testing.T) {
	cases := []struct{ in, key, effort string }{
		{"auto", "auto", ""},
		{"lite", "lite", ""},
		{"qwen3.8-max", "qmodel_38max", ""},
		{"qwen-3.8-max", "qmodel_38max", ""},
		{"qwen3.8-flash", "qfmodel", ""},
		{"qwen3.7-max", "qmodel_latest", ""},
		{"qwen3.7-plus", "qmodel", ""},
		{"qwen3.7-flash", "q37fmodel", ""},
		{"deepseek-v4-pro", "dmodel", ""},
		{"deepseek-chat", "dfmodel", ""},
		{"glm-5.3", "gmodel", ""},
		{"glm-5.2", "gm51model", ""},
		{"kimi-k3", "kmodel_latest", ""},
		{"kimi-k2.8-preview", "kmodel", ""},
		{"step-5-preview", "jmodel", ""},
		{"Step-5-Preview-high", "jmodel", "high"},
		{"minimax", "mmodel", ""},
		{"qoder/qwen3.8-max", "qmodel_38max", ""},
		{"QWEN 3.8 MAX", "qmodel_38max", ""},
		{"qmodel_38max", "qmodel_38max", ""},
		{"unknown-model", "unknown-model", ""},
		{"auto-high", "auto", "high"},
		{"qwen3.8-max-xhigh", "qmodel_38max", "xhigh"},
		{"qwen3.8-max-low", "qmodel_38max", "low"},
		{"qoder:glm-5.3-high", "gmodel", "high"},
	}
	for _, c := range cases {
		key, effort := resolveModelRequest(c.in)
		if key != c.key || effort != c.effort {
			t.Errorf("resolveModelRequest(%q) = (%q,%q), want (%q,%q)", c.in, key, effort, c.key, c.effort)
		}
	}
}

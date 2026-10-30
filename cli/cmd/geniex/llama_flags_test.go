// Copyright 2024-2026 Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"slices"
	"testing"
)

func TestExtractLlamaArgs(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
		rest []string
	}{
		{
			name: "key=value and separate-value forms",
			argv: []string{"infer", "m", "--llama-ctxk=q4_0", "--llama-flash-attn", "--nctx", "8192", "--verbose"},
			want: []string{"ctxk=q4_0", "flash-attn"},
			rest: []string{"infer", "m", "--nctx", "8192", "--verbose"},
		},
		{
			name: "value token followed by another long flag stays valueless",
			argv: []string{"infer", "m", "--llama-no-perf", "--nctx", "4096"},
			want: []string{"no-perf"},
			rest: []string{"infer", "m", "--nctx", "4096"},
		},
		{
			name: "value token not starting with dashes is consumed",
			argv: []string{"infer", "m", "--llama-ctx-size", "16384", "--llama-ctxv=pq2_0"},
			want: []string{"ctx-size=16384", "ctxv=pq2_0"},
			rest: []string{"infer", "m"},
		},
		{
			name: "unknown --llama keys pass through as-is",
			argv: []string{"infer", "m", "--llama-split-mode", "layer"},
			want: []string{"split-mode=layer"},
			rest: []string{"infer", "m"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRest := extractLlamaArgs(tt.argv)
			if !slices.Equal(llamaPassthrough, tt.want) {
				t.Errorf("llamaPassthrough = %v, want %v", llamaPassthrough, tt.want)
			}
			if !slices.Equal(gotRest, tt.rest) {
				t.Errorf("rest = %v, want %v", gotRest, tt.rest)
			}
		})
	}
}

// The raw --llama-arg form must not be intercepted by the pre-pflag
// extractor: the token itself stays in argv so pflag can bind it, and the
// raw llama.cpp token it carries must reach the plugin verbatim.
func TestExtractLlamaArgsKeepsExplicitLlamaArg(t *testing.T) {
	argv := []string{"infer", "m", "--llama-arg", "--split-mode=layer", "--nctx", "8192"}
	rest := extractLlamaArgs(argv)

	if !slices.Equal(llamaPassthrough, []string(nil)) {
		t.Fatalf("llamaPassthrough = %v, want empty", llamaPassthrough)
	}
	want := []string{"infer", "m", "--llama-arg", "--split-mode=layer", "--nctx", "8192"}
	if !slices.Equal(rest, want) {
		t.Fatalf("rest = %v, want %v", rest, want)
	}
}

func TestExtractLlamaArgsKeepsExplicitLlamaArgWithEquals(t *testing.T) {
	argv := []string{"infer", "m", "--llama-arg=--split-mode=layer"}
	rest := extractLlamaArgs(argv)

	if len(llamaPassthrough) != 0 {
		t.Fatalf("llamaPassthrough = %v, want empty", llamaPassthrough)
	}
	if !slices.Equal(rest, argv) {
		t.Fatalf("rest = %v, want %v", rest, argv)
	}
}

// A raw token passed through --llama-arg keeps its leading dashes and
// reaches the plugin untouched.
func TestEffectiveLlamaArgsMergesRawTokens(t *testing.T) {
	extractLlamaArgs([]string{"infer", "m", "--llama-ctxk=q4_0"})
	llamaFlagValues = []string{"--split-mode=layer", "-sm", "layer", "--override-tensor", "a=b,c=d"}

	want := []string{"ctxk=q4_0", "--split-mode=layer", "-sm", "layer", "--override-tensor", "a=b,c=d"}
	if got := EffectiveLlamaArgs(); !slices.Equal(got, want) {
		t.Fatalf("EffectiveLlamaArgs() = %v, want %v", got, want)
	}

	llamaFlagValues = nil
}

// The raw-token spelling must normalize to the same bare option name as the
// "--llama-<key>" spelling, so the plugin resolves both to the same upstream
// option.
func TestSplitLlamaArgToken(t *testing.T) {
	tests := []struct {
		token    string
		wantKey  string
		wantVal  string
		wantHasV bool
	}{
		{token: "--split-mode=layer", wantKey: "split-mode", wantVal: "layer", wantHasV: true},
		{token: "-sm", wantKey: "sm", wantHasV: false},
		{token: "--tensor-split=3,1", wantKey: "tensor-split", wantVal: "3,1", wantHasV: true},
		{token: "split-mode=layer", wantKey: "split-mode", wantVal: "layer", wantHasV: true},
		{token: "--", wantKey: "", wantHasV: false},
		{token: "", wantKey: "", wantHasV: false},
	}
	for _, tt := range tests {
		key, val, hasV := splitLlamaArgToken(tt.token)
		if key != tt.wantKey || val != tt.wantVal || hasV != tt.wantHasV {
			t.Errorf("splitLlamaArgToken(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.token, key, val, hasV, tt.wantKey, tt.wantVal, tt.wantHasV)
		}
	}
}

// A raw --llama-arg token must also be visible to the CLI-level overrides
// (e.g. the multimodal projector path).
func TestLlamaArgValueFromRawToken(t *testing.T) {
	extractLlamaArgs([]string{"infer", "m"})
	llamaFlagValues = []string{"--mmproj=/tmp/mm.gguf"}

	if got := llamaArgValue("mmproj"); got != "/tmp/mm.gguf" {
		t.Errorf("llamaArgValue(mmproj) = %q, want /tmp/mm.gguf", got)
	}

	llamaFlagValues = nil
}

// The device option must be lifted out of the generic passthrough list and
// carried on the dedicated LlamaDevice field: the llama_cpp plugin refuses
// llama.cpp's own --device/-dev (its upstream parser pushes a null device for
// the value "none", which aborted the host process).
func TestLlamaDeviceOverride(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantValue string
		wantRest  []string
	}{
		{"no device", []string{"ctxk=q4_0"}, "", []string{"ctxk=q4_0"}},
		{"inline", []string{"device=none"}, "none", []string{}},
		{"inline dashes", []string{"--device=none"}, "none", []string{}},
		{"short inline", []string{"-dev=none"}, "none", []string{}},
		{"separate token", []string{"dev", "none"}, "none", []string{}},
		{"separate raw token", []string{"-dev", "none"}, "none", []string{}},
		{"keeps the others", []string{"device=none", "split-mode=layer"}, "none", []string{"split-mode=layer"}},
		{"first wins", []string{"device=HTP0", "device=none"}, "HTP0", []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value, rest := LlamaDeviceOverride(tc.args)
			if value != tc.wantValue {
				t.Errorf("LlamaDeviceOverride(%v) value = %q, want %q", tc.args, value, tc.wantValue)
			}
			if len(rest) != len(tc.wantRest) {
				t.Fatalf("LlamaDeviceOverride(%v) rest = %v, want %v", tc.args, rest, tc.wantRest)
			}
			for i := range rest {
				if rest[i] != tc.wantRest[i] {
					t.Errorf("LlamaDeviceOverride(%v) rest = %v, want %v", tc.args, rest, tc.wantRest)
				}
			}
		})
	}
}

func TestLlamaArgValue(t *testing.T) {
	extractLlamaArgs([]string{"infer", "m", "--llama-mmproj", "/tmp/mm.gguf", "--llama-ctxk=pq2_0"})
	if got := llamaArgValue("mmproj"); got != "/tmp/mm.gguf" {
		t.Errorf("llamaArgValue(mmproj) = %q, want /tmp/mm.gguf", got)
	}
	if got := llamaArgValue("ctxk"); got != "pq2_0" {
		t.Errorf("llamaArgValue(ctxk) = %q, want pq2_0", got)
	}
	if got := llamaArgValue("nope"); got != "" {
		t.Errorf("llamaArgValue(nope) = %q, want empty", got)
	}
}

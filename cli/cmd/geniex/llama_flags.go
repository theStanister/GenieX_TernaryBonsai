// Copyright 2024-2026 Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: BSD-3-Clause

package main

import "strings"

// llamaPassthrough holds the normalized --llama-* tokens collected from
// os.Args before cobra/pflag parses them (pflag would otherwise reject
// unknown flags). Each element is "key" or "key=value" where `key` is the
// llama.cpp long-option name with the leading "--llama-" stripped, e.g.
// "ctxk=pq2_0", "flash-attn=off", "no-perf".
//
// The llama_cpp plugin resolves these tokens against llama.cpp's own option
// table, so any llama.cpp option works — not just a curated subset.
var llamaPassthrough []string

// EffectiveLlamaArgs returns the complete llama.cpp passthrough list: the
// "--llama-*" flags recovered from os.Args plus any explicit "--llama-arg"
// tokens. Order preserves use-side precedence (explicit --llama-arg wins).
func EffectiveLlamaArgs() []string {
	merged := make([]string, 0, len(llamaPassthrough)+len(llamaFlagValues))
	merged = append(merged, llamaPassthrough...)
	merged = append(merged, llamaFlagValues...)
	return merged
}

// splitLlamaFlagToken splits a "--llama-X[=value]" token into (key, value,
// hasValue), stripping the "--llama-" prefix from the key.
func splitLlamaFlagToken(token string) (key, value string, hasValue bool) {
	const prefix = "--llama-"
	if !strings.HasPrefix(token, prefix) {
		return "", "", false
	}
	k := strings.TrimPrefix(token, prefix)
	if k == "" || strings.HasPrefix(k, "-") {
		return "", "", false
	}
	if i := strings.IndexByte(k, '='); i >= 0 {
		return k[:i], k[i+1:], true
	}
	return k, "", false
}

// isExplicitLlamaArg reports whether the token is the pflag-bound --llama-arg
// flag (which must be left for pflag rather than intercepted as a passthrough).
func isExplicitLlamaArg(token string) bool {
	return token == "--llama-arg" || strings.HasPrefix(token, "--llama-arg=")
}

// extractLlamaArgs moves every "--llama-*" flag (plus, when a flag has no
// "=", its following value token) into llamaPassthrough and returns the
// remaining argv for pflag. A following token is treated as the flag's value
// unless it itself begins with "--" (another long flag), so "--llama-ngl -1"
// yields "ngl=-1" while "--llama-flash-attn --nctx 8192" yields "flash-attn"
// with no value.
func extractLlamaArgs(argv []string) []string {
	llamaPassthrough = nil
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "--llama-") || isExplicitLlamaArg(a) {
			out = append(out, a)
			continue
		}
		key, val, hasVal := splitLlamaFlagToken(a)
		if key == "" {
			continue // syntactically invalid ("--llama-" or "--llama--x"); drop it
		}
		if hasVal {
			llamaPassthrough = append(llamaPassthrough, key+"="+val)
			continue
		}
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			llamaPassthrough = append(llamaPassthrough, key+"="+argv[i+1])
			i++
			continue
		}
		llamaPassthrough = append(llamaPassthrough, key)
	}
	return out
}

// llamaArgValue returns the value of the first passthrough entry whose key is
// `key`, or "" when absent. Used for CLI-level overrides (e.g. --llama-mmproj)
// that must be applied before the model create input is built, since the
// llama_cpp plugin only understands model/context params, not file paths.
//
// Both the "--llama-<key>=<value>" spelling and the raw "--<key>=<value>" /
// "--<key>" spelling are recognised.
func llamaArgValue(key string) string {
	for _, a := range EffectiveLlamaArgs() {
		name, value, hasValue := splitLlamaArgToken(a)
		if name != key {
			continue
		}
		if hasValue {
			return value
		}
		return ""
	}
	return ""
}

// splitDeviceOverride extracts llama.cpp's device option ("device" / "dev")
// from a passthrough token list and returns it separately.
//
// The device selection is owned by the GenieX CLI (--compute) and by the
// GenieX offload planner, so the llama_cpp plugin REFUSES llama.cpp's own
// --device/-dev option (see sdk/plugins/llama_cpp/src/passthrough.cpp). The
// user's --llama-device=… value is therefore carried to the plugin through
// the dedicated, typed ModelConfig.LlamaDevice field instead of through the
// generic passthrough list.
//
// Returns the extracted value ("" when there was none) and the remaining
// tokens (the device tokens removed).
func splitDeviceOverride(args []string) (string, []string) {
	value := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		tok := args[i]
		key, val, hasValue := splitLlamaArgToken(tok)
		if key != "device" && key != "dev" {
			rest = append(rest, tok)
			continue
		}
		if hasValue {
			if value == "" {
				value = val
			}
			continue
		}
		// "device" / "dev" with the value in the next token.
		if i+1 < len(args) {
			_, nextVal, nextHas := splitLlamaArgToken(args[i+1])
			if nextHas {
				if value == "" {
					value = nextVal
				}
				i++
				continue
			}
			// A bare next token without "=" is the value as well (e.g.
			// "--llama-arg -dev none" arrives as two separate tokens).
			if !strings.HasPrefix(args[i+1], "--") {
				if value == "" {
					value = args[i+1]
				}
				i++
				continue
			}
		}
	}
	return value, rest
}

// LlamaDeviceOverride returns the device override carried by the current
// passthrough tokens ("" when none), and strips the device tokens from the
// generic passthrough list.
func LlamaDeviceOverride(args []string) (string, []string) {
	return splitDeviceOverride(args)
}

// splitLlamaArgToken normalizes one passthrough token into (key, value,
// hasValue). The key never carries leading dashes, so "split-mode=layer",
// "--split-mode=layer" and "-sm" all normalize to their bare option name.
func splitLlamaArgToken(token string) (key, value string, hasValue bool) {
	t := strings.TrimSpace(token)
	if t == "" {
		return "", "", false
	}
	// A leading "--" or "-" is part of the raw-token form and is stripped.
	t = strings.TrimLeft(t, "-")
	if t == "" {
		return "", "", false
	}
	if i := strings.IndexByte(t, '='); i >= 0 {
		return t[:i], t[i+1:], true
	}
	return t, "", false
}

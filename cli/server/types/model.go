// Copyright 2024-2026 Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: BSD-3-Clause

package types

import geniex_sdk "github.com/qualcomm/GenieX/bindings/go"

// ModelParam holds the model-load knobs the keep-alive cache keys instances on.
// NCtx / NGpuLayers are llama_cpp-only; DeviceID is the compute unit resolved by
// the SDK (empty = the SDK's own default). PowerMode is resolved from the
// request's alias string by ResolveModelParam.
type ModelParam struct {
	NCtx        int32
	NGpuLayers  int32
	DeviceID    string
	VitDeviceID string
	PowerMode   geniex_sdk.PowerMode
	Spec        SpecParam
	// LlamaArgs is the llama.cpp passthrough ("key"/"key=value" tokens) applied
	// at model load. Populated from the request body / server --llama-arg
	// defaults; ignored by non-llama_cpp runtimes.
	LlamaArgs []string
	// LlamaDevice is the typed offload-device override ("none", or a
	// comma-separated device list). It is carried on its own because the
	// llama_cpp plugin refuses llama.cpp's own --device/-dev passthrough option;
	// the value is split out of LlamaArgs before they reach the plugin.
	LlamaDevice string
}

// SpecParam bundles the speculative-decoding knobs sourced from a request; all
// zero-values mean "spec disabled". Only llama_cpp consumes these fields.
type SpecParam struct {
	Type       string
	DraftModel string
	NMax       int32
	NMin       int32
	PMin       float32
}

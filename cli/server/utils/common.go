// Copyright 2024-2026 Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: BSD-3-Clause

package utils

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"

	_ "golang.org/x/image/webp"
)

// SplitDeviceOverride extracts llama.cpp's device option ("device" / "dev")
// from a passthrough token list and returns it separately.
//
// The llama_cpp plugin refuses llama.cpp's own --device/-dev passthrough option
// (see sdk/plugins/llama_cpp/src/passthrough.cpp), so the device selection is
// carried on the dedicated, typed ModelConfig.LlamaDevice field instead.
// Returns the extracted value ("" when there was none) and the remaining
// tokens with the device tokens removed.
func SplitDeviceOverride(args []string) (string, []string) {
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

// splitLlamaArgToken normalizes one passthrough token into (key, value,
// hasValue). The key never carries leading dashes, so "split-mode=layer",
// "--split-mode=layer" and "-sm" all normalize to their bare option name.
func splitLlamaArgToken(token string) (key, value string, hasValue bool) {
	t := strings.TrimSpace(token)
	if t == "" {
		return "", "", false
	}
	t = strings.TrimLeft(t, "-")
	if t == "" {
		return "", "", false
	}
	if i := strings.IndexByte(t, '='); i >= 0 {
		return t[:i], t[i+1:], true
	}
	return t, "", false
}

func SaveURIToTempFile(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}

	var data []byte
	// windows drive letter
	if len(u.Scheme) == 1 && (u.Scheme[0] >= 'a' && u.Scheme[0] <= 'z' || u.Scheme[0] >= 'A' && u.Scheme[0] <= 'Z') {
		u.Scheme = "file"
		u.Path = uri
	}
	switch u.Scheme {
	case "http", "https":
		resp, err := http.Get(uri)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", errors.New("http download failed: " + resp.Status)
		}
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
	case "data":
		parts := strings.SplitN(u.Opaque, ",", 2)
		if len(parts) != 2 {
			return "", errors.New("invalid data URI format")
		}
		// format: data:[<mediatype>][;base64],<data>
		if strings.Contains(parts[0], ";base64") {
			data, err = base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				return "", err
			}

		} else {
			// format: data:[<mediatype>],<data>
			decoded, err := url.QueryUnescape(parts[1])
			if err != nil {
				return "", err
			}
			data = []byte(decoded)
		}
	case "file", "":
		data, err = os.ReadFile(u.Path)
		if err != nil {
			return "", err
		}
	default:

		return "", errors.New("unsupported scheme: " + u.Scheme)
	}

	// Detect content type
	contentType := http.DetectContentType(data)

	// Convert WebP to PNG for compatibility with native SDK
	if strings.HasPrefix(contentType, "image/webp") || strings.HasSuffix(strings.ToLower(u.Path), ".webp") {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return "", errors.New("failed to decode WebP image: " + err.Error())
		}

		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return "", errors.New("failed to encode image as PNG: " + err.Error())
		}

		data = buf.Bytes()
		contentType = "image/png"
	}

	fileExt := ""
	if exts, err := mime.ExtensionsByType(contentType); err == nil && len(exts) > 0 {
		fileExt = exts[0]
	}

	tmpFile, err := os.CreateTemp("", "uri-*"+fileExt)
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	_, err = tmpFile.Write(data)
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}

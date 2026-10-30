# Ternary-Bonsai / PrismML llama.cpp Integration (GenieX)

> **Scope of this document.** This file is the engineering record for the work that
> replaced the upstream llama.cpp submodule with **PrismML's "Ternary Bonsai" fork**
> and wired ternary / quantized-KV / passthrough support through the GenieX SDK, C ABI,
> Go bindings, and CLI. It is written so a rebuild can be reproduced on a **Snapdragon
> ARM64 (Windows) PC** — the primary target — as well as on a plain Windows x64 host
> (what was built and verified during this work). Every path and API name below was
> read from the real source at the time of writing; do not edit it without re-checking
> the code.

---

## 1. Overview

This change swaps the llama.cpp backend of GenieX from the upstream
[`ggml-org/llama.cpp`](https://github.com/ggml-org/llama.cpp) submodule to PrismML's
**Ternary Bonsai** fork at [`https://github.com/PrismML-Eng/llama.cpp`](https://github.com/PrismML-Eng/llama.cpp).
That fork is a ternary-inference branch of llama.cpp; it adds a ternary GGUF weight
type, a quantized/ternary KV cache, and BF16 multimodal projector support.

Concretely, the work adds three capabilities to GenieX's `llama_cpp` plugin and CLI:

1. **Ternary model weights** — the fork registers `GGML_TYPE_PQ2_0` (group-128 Q2_0,
   ggml type **142**) and `GGML_TYPE_PTQ1_0` (Prism-private ternary, group-128, ggml
   type **143**), used by the `output.weight` tensor of ternary GGUF models.
2. **Optional KV-cache quantization for K and V** — via the fork's `type_k` / `type_v`
   context params (`--llama-ctxk` / `--llama-ctxv`), accepting `pq2_0`, `ptq1_0`, and
   the standard `f32`/`f16`/`bf16`/`q8_0`/… names.
3. **A generic `--llama-*` CLI passthrough** — *any* llama.cpp option can be forwarded
   to the `llama_cpp` plugin, either as `--llama-<key>=<value>` or as a raw token through
   `--llama-arg`. The tokens are resolved against **llama.cpp's own option table**
   (`common_params_parser_init(..., LLAMA_EXAMPLE_COMMON, ...)`), run against a scratch
   `common_params` seeded from the GenieX defaults, and applied as a **diff-based
   override layer** on top of the model/context params GenieX already computed. Options
   that cannot be applied produce a WARN naming the key — never an abort.
   `--llama-mmproj` is additionally used by the CLI to override the VLM projector path
   (so a BF16/ternary mmproj can be supplied at runtime).
4. **A default NPU → GPU → CPU offload policy** — with no `--compute` flag the plugin
   fills the Hexagon NPU first (up to a unified-memory budget = 50 % of the total system
   RAM), spills the remainder to the Adreno GPU, and keeps the lowest-index layers on the
   CPU as a resident base tier. See §9.

All of this is **llama_cpp-only**; the `qairt` plugin (QNN/QAIRT) ignores the new
`geniex_ModelConfig` fields and passthrough tokens.

> **New in this revision.** §3.4 has been rewritten for the *generic* resolver (it used to
> describe a curated allowlist), §9 is new (default offload policy), §7.1 records the
> current green test results, and §8.5 lists the caveats that could not be validated
> without Snapdragon hardware.

---

## 2. Key facts / ground truth

- **The integration is CMake-based, not Bazel.** The native SDK + plugins are built
  with CMake (entry point [`sdk/CMakeLists.txt`](../sdk/CMakeLists.txt)); Bazel builds
  only the Go CLI/bindings and links against the CMake-installed SDK bridge.
- **`sdk/plugins/llama_cpp/` is the plugin.** It is pulled in by
  [`sdk/plugins/CMakeLists.txt`](../sdk/plugins/CMakeLists.txt) under
  `if(GENIEX_PLUGIN_LLAMA_CPP) add_subdirectory(./llama_cpp)` and consumed by the SDK
  plugin registry ([`sdk/src/registry.cpp`](../sdk/src/registry.cpp)) at runtime.
  - [`sdk/plugins/llama_cpp/CMakeLists.txt`](../sdk/plugins/llama_cpp/CMakeLists.txt)
    calls `add_subdirectory(../../../third-party/llama.cpp …)` (with
    `LLAMA_BUILD_COMMON ON`, `LLAMA_BUILD_TOOLS ON`, `BUILD_SHARED_LIBS ON`,
    `GGML_BLAS OFF`, `GGML_NATIVE OFF`, `GGML_BACKEND_DL OFF`), then
    `add_subdirectory(./src)`.
  - [`sdk/plugins/llama_cpp/src/CMakeLists.txt`](../sdk/plugins/llama_cpp/src/CMakeLists.txt)
    builds the shared library `geniex_llama_cpp` with `OUTPUT_NAME "geniex_plugin"`,
    linking `llama llama-common` and `geniex mtmd`. The output lands in
    `plugins/llama_cpp/` and installs to `lib/llama_cpp/`.
  - `sdk/src/registry.cpp` scans `GENIEX_PLUGIN_PATH` (defaulting to the SDK shared-lib
    dir) for the per-plugin shared library named `geniex_plugin.dll` /
    `libgeniex_plugin.so`, loads it with `LoadLibraryExW`/`dlopen`, resolves
    `create_plugin` and `plugin_id`, and registers the `llama_cpp` plugin.
- **`third-party/llama.cpp` is the submodule.** Its remote is recorded in
  [`.gitmodules`](../.gitmodules):
  ```ini
  [submodule "third-party/llama.cpp"]
      path = third-party/llama.cpp
      url = https://github.com/PrismML-Eng/llama.cpp
  ```
  The previously-pinned ggml-org URL is what this change replaces. A second submodule
  (`third-party/geniex-qairt`) is unrelated.
- **Fork commit currently checked out.**
  - Commit: `9a9394a895b96003ca842a6041cb28ac49a108f7`
  - Date/msg: `2026-09-18 15:24:35 -0700  build: fix Ubuntu ARM64 and Windows Vulkan release jobs (#198)`
  - Remote (origin): `https://github.com/PrismML-Eng/llama.cpp`
  - The submodule working tree is **clean** (no local fork edits applied to it).

---

## 3. Data flow for `--llama-*`

The full path from a shell flag to a llama.cpp struct field is:

```
CLI --llama-<key>[=<value>] / --llama-arg "<raw token>"
        │
        ▼  cli/cmd/geniex/llama_flags.go  (pre-pflag extraction)
Go ModelConfig.LLamaArgs []string          (bindings/go/common.go)
        │
        ▼  bindings/go/common.go fillC()
C ABI geniex_ModelConfig.llama_args / llama_args_count   (sdk/include/geniex.h)
        │
        ▼  sdk/plugins/llama_cpp/src/passthrough.cpp
resolve_passthrough()  — generic, diff-based resolver
        │
        ▼
llama.cpp model/context params (type_k / type_v / flash_attn_type / n_ctx / …)
```

### 3.1 CLI: pre-pflag extraction (`cli/cmd/geniex/llama_flags.go`)

`main.go` runs the extraction **before** cobra/pflag parses args, because pflag would
otherwise reject the unknown `--llama-*` flags:

- [`main.go`](../cli/cmd/geniex/main.go:156) — `os.Args = extractLlamaArgs(os.Args)`.
- [`extractLlamaArgs()`](../cli/cmd/geniex/llama_flags.go:55) — walks `os.Args`, pulls
  every `--llama-*` token (plus a following value token when there is no `=` and the
  next token doesn't start with `--`) into the package-level `llamaPassthrough`, and
  returns the remaining argv for pflag. The `--llama-` prefix is stripped so the stored
  key is the llama.cpp long-option name (e.g. `ctxk=pq2_0`, `flash-attn=off`, `no-perf`).
  `--llama-arg` itself is left for pflag (it's the explicit form, bound in
  [`infer.go`](../cli/cmd/geniex/infer.go:107) via `StringArrayVarP(&llamaFlagValues, "llama-arg", …)`).
- [`EffectiveLlamaArgs()`](../cli/cmd/geniex/llama_flags.go:19) — concatenates
  `llamaPassthrough` (from `--llama-*`) + `llamaFlagValues` (from explicit
  `--llama-arg`), so explicit `--llama-arg` wins on use-side precedence.
- [`llamaArgValue(key)`](../cli/cmd/geniex/llama_flags.go:86) — returns the value of the
  first passthrough token for `key`; used for CLI-level overrides that must apply
  **before** the model create input is built (notably `--llama-mmproj`, see §3.5).

**Two syntaxes are accepted:**

| Form | Example | Notes |
|---|---|---|
| `--llama-<key>[=<value>]` | `--llama-split-mode=layer`, `--llama-tensor-split=3,1` | Only long options; the prefix is stripped. The value must be attached with `=` (a separate value token cannot be expressed this way). |
| `--llama-arg "<raw token>"` | `--llama-arg "--split-mode=layer"`, `--llama-arg "-sm" --llama-arg "layer"`, `--llama-arg "--override-tensor" --llama-arg "a=b,c=d"` | The token is forwarded **verbatim, including its leading dashes**, so short flags and repeated / multi-value options are expressible. Repeat the flag for multiple tokens. |

`--llama-arg` is bound to pflag (it is *not* intercepted by the pre-pflag extractor), so
`--llama-arg "--split-mode=layer"` is passed through untouched; the raw tokens are also
visible to the CLI-level overrides through `llamaArgValue()`.

The [`infer.go`](../cli/cmd/geniex/infer.go) command builds `geniex_sdk.ModelConfig{
LLamaArgs: EffectiveLlamaArgs(), … }`. `serve.go` merges `llamaPassthrough` into the
server's per-model `llamaargs` defaults so `geniex serve --llama-ctxk=pq2_0` behaves
like an explicit `--llama-arg`.

### 3.2 Go binding (`bindings/go/common.go`)

[`bindings/go/common.go`](../bindings/go/common.go) defines `ModelConfig` with the new
ternary/passthrough fields:

```go
CacheTypeK string
CacheTypeV string
LLamaArgs  []string   // "key" / "key=value" tokens forwarded verbatim
```

`ModelConfig.fillC()` maps them onto the C struct: `cache_type_k` / `cache_type_v`
via `cStringIfSet`, and `LLamaArgs` via a C array of `char*` stored in `llama_args` /
`llama_args_count`. `freeCModelConfig()` releases both.

### 3.3 C ABI (`sdk/include/geniex.h`)

The [`geniex_ModelConfig`](../sdk/include/geniex.h:480) struct carries (among others):

```c
// Ternary Bonsai KV-cache quantization (llama_cpp only; ignored by qairt).
geniex_Path cache_type_k;  // KV cache data type for K ("" = default F16)
geniex_Path cache_type_v;  // KV cache data type for V ("" = default F16)

// Generic llama.cpp CLI passthrough (llama_cpp only; ignored by qairt).
const char** llama_args;       // NULL when llama_args_count == 0
int32_t      llama_args_count;
```

These are documented in the header as "llama_cpp only; ignored by qairt."

### 3.4 Plugin: the generic resolver (`sdk/plugins/llama_cpp/src/passthrough.cpp`)

> This section used to describe a **curated allowlist** (`ngl`, `no-mmap`, `flash-attn`,
> `cache-type-k/v`, `ctx-size`, …) that logged-and-dropped everything else. The plugin now
> resolves **every** llama.cpp option generically.

The resolver lives in [`passthrough.cpp`](../sdk/plugins/llama_cpp/src/passthrough.cpp)
and is exposed through [`passthrough.h`](../sdk/plugins/llama_cpp/include/passthrough.h):

- **Option table** — [`option_table()`](../sdk/plugins/llama_cpp/src/passthrough.cpp:152)
  builds the upstream option list once with
  `common_params_parser_init(scratch, LLAMA_EXAMPLE_COMMON, nullptr)` and indexes every
  long and short spelling. `LLAMA_EXAMPLE_COMMON` structurally excludes the
  llama-server-only options, so `--host`, `--port`, `--api-key`, `--slots` are not even
  in the table — they are still refused explicitly (§3.4.2).
- **Tokenizer** — [`tokenize()`](../sdk/plugins/llama_cpp/src/passthrough.cpp:124) accepts
  `key`, `key=value`, `--key`, `--key=value`, `-short`, `-short=value`; a bare token with
  no dash and no `=` becomes the **value of the preceding option**, so
  `--llama-arg "-sm" --llama-arg "layer"` works.
- **Dispatch** — [`dispatch()`](../sdk/plugins/llama_cpp/src/passthrough.cpp:350) first
  applies the **pre-dispatch refusal** (§3.4.2) against the token the *user* typed, then
  looks the option up and invokes the **upstream handler**
  (`handler_void` / `handler_bool` / `handler_int` / `handler_string` / `handler_str_str`)
  so the upstream semantics (and upstream validation) apply.
- **Diff-based resolution** — [`resolve_passthrough()`](../sdk/plugins/llama_cpp/src/passthrough.cpp:334)
  seeds a scratch `common_params` from the GenieX base model/context params
  ([`seed_scratch()`](../sdk/plugins/llama_cpp/src/passthrough.cpp:188)), runs the
  handlers, and reports **only the fields that changed**. The result is a
  `PassthroughOverrides` struct with one `has_*` flag per model/context field.
- **Application** — [`apply_passthrough_overrides()`](../sdk/plugins/llama_cpp/src/passthrough.cpp:586)
  writes those overrides onto the real `llama_model_params` / `llama_context_params`.
  Because only diffs are applied, **anything the user did not set keeps the GenieX
  behaviour** (device policy, threadpool, HTP settings).
- **Sampling** — [`resolve_sampling_passthrough()`](../sdk/plugins/llama_cpp/src/passthrough.cpp:544)
  does the same for `common_params_sampling`, so `--llama-temp`, `--llama-top-k`,
  `--llama-top-p`, `--llama-min-p`, `--llama-seed`, `--llama-repeat-penalty`,
  `--llama-presence-penalty`, `--llama-frequency-penalty`, `--llama-typical-p`,
  `--llama-mirostat*` and `--llama-dry-*` reach `geniex_SamplerConfig` /
  `build_sampling_params()`. The values are carried on the LLM object and take effect at
  generation time.

#### 3.4.1 Curated pre-pass (still present)

Two things stay plugin-side because upstream's parser does not know them:

- **KV-cache types** — `cache-type-k` / `cache-type-v` (aliases `ctxk` / `ctk`,
  `ctxv` / `ctv`) are resolved by
  [`parse_cache_type()`](../sdk/plugins/llama_cpp/src/params.cpp:110), which is
  case-insensitive, strips a leading `ggml_type_`, and resolves against a table listing
  the PrismML ternary additions first:
  ```
  pq2_0 → GGML_TYPE_PQ2_0   (142)
  ptq1_0 → GGML_TYPE_PTQ1_0 (143)
  f32 / f16 / bf16 / q8_0 / q4_0 / q4_1 / q5_0 / q5_1 / iq4_nl → corresponding ggml_type
  ```
  These keys are **never** dispatched to the upstream handler, so a `pq2_0` value never
  trips upstream's "unknown cache type" error.
- **`mmproj`** — a file path, not a model/context param; handled by the CLI (§3.6).

#### 3.4.2 Out of scope — refused **before** dispatch

The following are **refused** with a WARN naming the option; they are never applied.
The check runs **before** any upstream lookup and **before** any upstream handler runs,
and it is made against the token the **user typed** (the bare name with the leading
dashes stripped). Therefore `--cache-list`, `-cl` and `cache-list` all hit the same
refusal, and an option that *does* exist in llama.cpp's COMMON table
(`--model`, `--hf-repo`, `--hf-file`, `--hf-token`, `--mmproj`, `--help`, `--usage`, …)
is refused exactly like one that does not (`--host`, `--port`, `--api-key`, `--slots`,
`--rpc`, …).

The warning wording is always:

```
WARN llama.cpp option '<name>' is out of scope for GenieX and was ignored
```

| Group | Options (all spellings: long **and** short) |
|---|---|
| **Fatal — upstream handler calls `exit(0)`** | `--cache-list` / `-cl`, `--version`, `--list-devices` |
| Usage / help | `--help` / `-h`, `--usage`, `--completion`, `--completion-bash` |
| Model acquisition (owned by the GenieX CLI) | `--model` / `-m`, `--model-url` / `-mu`, `--docker-repo` / `-dr`, `--hf-repo` / `-hf` / `-hfr`, `--hf-file` / `-hff`, `--hf-token` / `-hft` |
| Files the GenieX CLI owns | `--mmproj` / `-mm`, `--mmproj-device` / `-mmdev`, `--mmproj-url` / `-mmu`, `--session-file`, `--prompt-cache-file`, `--logits-file`, `--logits-out` |
| llama-server networking / auth | `--host`, `--port`, `--api-key`, `--api-prefix`, `--slots`, `--no-slots`, `--timeout` / `-to`, `--metrics`, `--props` |
| Remote backends / routing metadata | `--rpc`, `--alias` / `-a`, `--tags` |
| Adapters / control vectors | `--lora`, `--lora-scaled`, `--control-vector`, `--control-vector-scaled`, `--control-vector-layer-range` |
| Grammar / schema constraining | `--grammar`, `--grammar-file`, `--json-schema` / `-j`, `--json-schema-file` / `-jf` |
| Prompt-cache files | `--prompt-cache`, `--prompt-cache-all`, `--prompt-cache-ro`, `--slots-prompt-cache` |
| Device selection (owned by the GenieX CLI / offload planner) | `--device` / `-dev` |

> **Note.** `--device` / `-dev` is refused **before** dispatch. llama.cpp's own device
> parser deliberately pushes a *null* device for the value `none`, and the post-dispatch
> diffing would then dereference it (`ggml_backend_dev_name()` asserts on a null
> pointer) — an `abort()` that no `try`/`catch` can intercept, which would kill the
> whole host process. The device selection is therefore a **GenieX-level** concern:
> use `--compute cpu|gpu|npu` (the typed `input->device_id` route) or the dedicated
> `--llama-device=…` override, which is carried to the plugin on the typed
> `geniex_ModelConfig.llama_device` field and never travels through the generic
> passthrough list.

> **Note.** `-v` is **not** refused: in the PrismML fork `-v` is the short spelling of
> `--verbose` (a harmless log-level option), and the fatal `--version` option has no
> short spelling. `--mmproj` is refused on the *plugin* side, but `--llama-mmproj` still
> works at the CLI level: the CLI maps it onto the VLM projector path (§3.6) before the
> model is created.

##### Fatal-handler audit

The upstream option table was audited for handlers that can terminate the process
(`exit(`, `std::exit(`, `abort(`, or an escaping `throw`) in
`third-party/llama.cpp/common/arg.cpp`. Exactly **three** upstream COMMON handlers
terminate the process, and all three are refused by name:

| Upstream option | Handler | Source |
|---|---|---|
| `--version` | `llama_print_build_info(); exit(0);` | `common/arg.cpp:1447-1454` |
| `-cl` / `--cache-list` | prints the cached model list, then `exit(0);` | `common/arg.cpp:1455-1466` |
| `--list-devices` | `common_print_available_devices(); exit(0);` | `common/arg.cpp:2734-2741` |

Every other upstream handler either mutates `common_params` or throws; the throw is
caught by the dispatch wrapper and turned into a warning. A second, independent gate
(`is_fatal_upstream()`) refuses any token that resolves to one of those three upstream
entries, whatever spelling reached it.

##### Credential handling

`--hf-token` and `--api-key` are refused **before** dispatch, so their values are never
written into the scratch `common_params` buffer and are never retained beyond the
warning.

#### 3.4.3 Failure behaviour

Every failure mode is **non-fatal**:

| Condition | Result |
|---|---|
| Unknown option name (not in llama.cpp at all) | `WARN` "unknown llama.cpp option '<name>'" — the option is skipped |
| Out-of-scope option (refused before dispatch) | `WARN` "llama.cpp option '<name>' is out of scope for GenieX and was ignored" |
| Fatal option (`--cache-list`, `--version`, `--list-devices`) | same "out of scope" warning — the upstream handler is **never** invoked, so the process is **not** terminated |
| Invalid value | `WARN` "invalid value for llama.cpp option '<name>': …" — the field keeps its GenieX value |
| Upstream handler throws | caught; the field keeps its GenieX value |

The model load is **never** aborted by a bad passthrough token. Precisely:

* Every handler invocation is wrapped, so an upstream handler that **throws** is caught
  and turned into a warning — the field keeps its GenieX value.
* The three upstream handlers that call `exit(0)` are refused **by name**, so they are
  never invoked and the process is not terminated by them.
* Upstream's own parser (`common_params_parse`, which `exit(0)`s on usage and
  `exit(1)`s on error) is never called.
* The post-dispatch diffing never dereferences a null device pointer, and an
  unresolvable device name is refused with a warning instead of being applied.

What is **not** claimed: an upstream `GGML_ASSERT` failure calls `abort()`, which is not
an exception and therefore cannot be caught. The passthrough layer cannot make the
upstream library assertion-free; what it guarantees is that the *options GenieX
refuses* are never dispatched, and that the device path specifically (the one place
where an upstream handler deliberately produced a null device) is both refused
pre-dispatch and null-guarded after dispatch.

### 3.5 Example: `--llama-ctxk=q4_0`

Concrete trace for `geniex infer … --llama-ctxk=q4_0`:

1. `extractLlamaArgs` sees `--llama-ctxk=q4_0`, stores `ctxk=q4_0` in
   `llamaPassthrough`.
2. `infer.go` sets `ModelConfig{ LLamaArgs: EffectiveLlamaArgs() }` →
   `[]string{"ctxk=q4_0"}`.
3. `fillC` writes `llama_args = {"ctxk=q4_0"}`, `llama_args_count = 1` into
   `geniex_ModelConfig`.
4. `apply_llama_context_args` parses key `ctxk` → matches `a.key == "cache-type-k" ||
   "ctxk" || "ctk"` → `parse_cache_type("q4_0")` → `GGML_TYPE_Q4_0` →
   `cpar.type_k = GGML_TYPE_Q4_0`.

So `--llama-ctxk=q4_0` → `ctxk=q4_0` → `cpar.type_k = Q4_0`. Similarly
`--llama-ctxv=pq2_0` sets `cpar.type_v = GGML_TYPE_PQ2_0`.

### 3.6 `--llama-mmproj` (VLM projector override)

`mmproj` is a file path, not a model/context param, so the plugin can't consume it
directly (it's explicitly a no-op key in
[`apply_llama_context_args`](../sdk/plugins/llama_cpp/src/params.cpp:242)). Instead the
CLI reads it via `llamaArgValue("mmproj")` and overrides the catalog projector path
before constructing `geniex_sdk.VlmCreateInput{ MmprojPath: … }` in
[`infer.go`](../cli/cmd/geniex/infer.go:560). The VLM plugin then loads it through the
fork's mtmd path in [`vlm.cpp`](../sdk/plugins/llama_cpp/src/vlm.cpp:122)
(`mtmd_init_from_file(input->mmproj_path, model, mparams)`). This is how a **BF16 /
ternary mmproj** can be supplied at runtime.

---

## 4. Ternary support specifics

### 4.1 Fork constants

Read from the fork's headers:

- [`third-party/llama.cpp/ggml/include/ggml.h`](../third-party/llama.cpp/ggml/include/ggml.h:433):
  ```c
  // Prism-private Q2_0 at group size 128 (upstream Q2_0 is group 64). High id so it
  // slots above upstream types; type_traits is sized to COUNT (143) with 43..141 unused.
  GGML_TYPE_PQ2_0  = 142,
  GGML_TYPE_PTQ1_0 = 143, // Prism-private ternary, group 128
  GGML_TYPE_COUNT  = 144,
  ```
- [`third-party/llama.cpp/include/llama.h`](../third-party/llama.cpp/include/llama.h:159):
  ```c
  LLAMA_FTYPE_MOSTLY_PQ2_0        = 141, // except 1d tensors (Prism group-128 Q2_0; matches published PQ2_0 ggufs)
  LLAMA_FTYPE_MOSTLY_PQ2_0_LEGACY = 142, // pre-rename value for the same format, still found in published ggufs
  LLAMA_FTYPE_MOSTLY_PTQ1_0       = 143, // except 1d tensors (Prism group-128 ternary, 1.75 bpw)
  ```
- KV-cache type fields on `llama_context_params`
  ([`third-party/llama.cpp/include/llama.h`](../third-party/llama.cpp/include/llama.h:388)):
  ```c
  enum ggml_type type_k; // data type for K cache [EXPERIMENTAL]
  enum ggml_type type_v; // data type for V cache [EXPERIMENTAL]
  ```

### 4.2 Type 142 handling (ternary `output.weight`)

`GGML_TYPE_PQ2_0 = 142` is the tensor type used by the ternary `output.weight` of
PrismML ternary GGUFs. The plugin does **not** need any special code for type 142
itself: it is a standard `ggml_type` in the fork's `ggml.h`, so tensors of that type
load and compute through the normal llama.cpp tensor pipeline (the fork supplies the
type traits, quantize/unquantize, and dequant shims). GenieX's job is only to (a) ship
the fork, and (b) let the user pick the quantized KV-cache types through
`parse_cache_type()`. Type 142 appears both as the raw `GGML_TYPE_PQ2_0` ggml type and,
in the GGUF file-format enum, as `LLAMA_FTYPE_MOSTLY_PQ2_0_LEGACY = 142` (the pre-rename
value still found in published GGUFs); the current canonical ftype is
`LLAMA_FTYPE_MOSTLY_PQ2_0 = 141`.

### 4.3 PQ2_0 / PTQ1_0 KV cache via `type_k` / `type_v`

The fork exposes the K and V cache data types directly on `llama_context_params`
(`type_k` / `type_v`, §4.1). `apply_llama_context_args` writes them from
`--llama-ctxk` / `--llama-ctxv` (aliases `cache-type-k`/`ctk`, `cache-type-v`/`ctv`)
through `parse_cache_type()`. So:

- `--llama-ctxk=pq2_0 --llama-ctxv=pq2_0` → `cpar.type_k = GGML_TYPE_PQ2_0`,
  `cpar.type_v = GGML_TYPE_PQ2_0` (group-128 Q2_0 K/V cache).
- `--llama-ctxk=ptq1_0 --llama-ctxv=ptq1_0` → `GGML_TYPE_PTQ1_0` (ternary K/V cache).
- `--llama-ctxk=bf16` → `GGML_TYPE_BF16`.

`build_context_params` also reads the structured `config.cache_type_k` /
`config.cache_type_v` fields the same way, so both routes land on `cpar.type_k` /
`cpar.type_v`.

### 4.4 BF16 mmproj (multimodal)

The fork's mtmd multimodal stack loads the projector through
[`mtmd_init_from_file`](../third-party/llama.cpp/tools/mtmd/mtmd.h:126) in
[`vlm.cpp`](../sdk/plugins/llama_cpp/src/vlm.cpp:122). BF16 projector support comes
from the fork (which handles BF16 weights in the mtmd/mmproj path); GenieX contributes
the runtime override via `--llama-mmproj` (§3.6) so a BF16/ternary mmproj file can be
swapped in without re-downloading the catalog. `GGML_TYPE_BF16` is one of the standard
cache types already resolved by `parse_cache_type()` (so `bf16` is accepted as a
K/V-cache type too).

---

## 5. Build instructions — Windows x64 (what was done & verified)

> This is the build that was actually executed during this work, on a **Windows x64**
> host. It validated the fork compiles and the plugin links with
> `GENIEX_PLUGIN_QAIRT=OFF` (see §5.3 for why).

### 5.1 Toolchain

- **Go** — installed via **scoop** (`scoop install go`). Verified `go version go1.27.1
  windows/amd64`.
- **CMake + Ninja** — installed via **pip** (`pip install cmake ninja`).
- **bazelisk** — for the CLI builds (`winget install --id Bazel.Bazelisk`, or scoop).
- **MSVC 19.44** — the Microsoft C/C++ toolchain used by the CMake default preset on
  Windows.

### 5.2 Commands

From the repo root, `cd sdk`, then:

```powershell
# Configure: default (Release) preset, llama_cpp ON, QAIRT OFF (see 5.3)
cmake --preset default -DGENIEX_PLUGIN_QAIRT=OFF -B build

# Build just the llama_cpp plugin (fast iteration)
cmake --build build --target geniex_llama_cpp -j

# Full build (SDK bridge + plugins)
cmake --build build -j

# Install into the SDK staging dir that Bazel links against
cmake --install build --prefix pkg-geniex
```

The plugin output is `build/plugins/llama_cpp/geniex_plugin.dll` (with
`OUTPUT_NAME "geniex_plugin"`), installed to `sdk/pkg-geniex/lib/llama_cpp/`. The SDK
plugin registry loads it from `GENIEX_PLUGIN_PATH` (default = the SDK shared-lib dir,
§2).

### 5.3 Why `GENIEX_PLUGIN_QAIRT=OFF` on x64

`GENIEX_PLUGIN_QAIRT` (the QAIRT/QNN backend, `sdk/plugins/qairt`) defaults to `ON`
([`sdk/CMakeLists.txt`](../sdk/CMakeLists.txt:31)). It is **Snapdragon/QNN-only** — it
targets the Hexagon NPU through Qualcomm's QNN runtime and its sentencepiece build step
needs **symlink / Developer Mode** support that isn't available on the plain x64 CI
host. On this x64 Windows machine there is no Snapdragon NPU or QNN toolchain to target,
so QAIRT is disabled for the build:

```powershell
cmake --preset default -DGENIEX_PLUGIN_QAIRT=OFF -B build
```

The llama_cpp plugin (the one this work is about) builds and installs normally with
QAIRT off, which is all that's needed for the ternary/Bonsai work on x64.

### 5.4 The single source fix for the fork

The fork's mtmd **helper** API changed one call signature relative to the version the
plugin was written against: `mtmd_helper_bitmap_init_from_file` gained an extra
`mtmd_helper_init_opt_default()` argument upstream. On the PrismML fork the helper keeps
the **3-arg** form. The plugin's [`vlm.cpp`](../sdk/plugins/llama_cpp/src/vlm.cpp:250)
was therefore reverted from the 4-arg call back to the 3-arg call:

```cpp
mtmd_bitmap* bmp = mtmd_helper_bitmap_init_from_file(
    this->ctx_vision, input->config->image_paths[i], false)
                       .bitmap;
```

This is the one source edit needed to compile the plugin against the fork. The fork's
own header ([`mtmd-helper.h`](../third-party/llama.cpp/tools/mtmd/mtmd-helper.h:43))
declares `mtmd_helper_bitmap_init_from_file(mtmd_context*, const char*, bool)`.

### 5.5 Patch-application behavior

At configure time [`sdk/CMakeLists.txt`](../sdk/CMakeLists.txt:171) `git apply`s the
patches under `sdk/patches/` (see §8 for the current state). The relevant knobs:

- `GENIEX_REQUIRE_LLAMA_PATCHES` — **defaults to `OFF`**. When a patch fails to apply,
  the build logs a warning and continues (graceful degradation) instead of failing.
  Set it `ON` to restore a hard failure (for CI that runs the upstream ggml-org tree).
- The 6 patches in `sdk/patches/` are grouped by condition:
  - Under `GGML_HEXAGON`: `llama-hexagon-release-sessions.patch`,
    `llama-hexagon-power-mode.patch`, `llama-hexagon-power-mode-setter.patch`.
  - Under `GGML_OPENCL`: `llama-opencl-adreno-cpy-constreg.patch`,
    `llama-zero-free-mem-even-split.patch`, `llama-opencl-adreno-xmem-gemm-off.patch`.
- On this x64 build (`GGML_HEXAGON`/`GGML_OPENCL` off by default) **none** of the
  conditional patches are selected, so nothing needs to apply against the fork.

---

## 6. Build instructions — Snapdragon ARM64 (the primary target)

> This section is the primary rebuild recipe: an **ARM64 Windows host with a Snapdragon
> SoC** (e.g. Snapdragon X Elite / X1E80100, X2 Elite / SM8850, …). On that hardware the
> Hexagon NPU (via `ggml-hexagon`), Adreno GPU (via OpenCL), and QAIRT/QNN backends are
> all available.

### 6.1 Existing ARM64 presets / toolchains

[`sdk/CMakePresets.json`](../sdk/CMakePresets.json) and the files in
[`sdk/cmake/`](../sdk/cmake) define the ARM64 cross toolchains:

| Preset family | Toolchain file | Target / ISA |
|---|---|---|
| `arm64-windows-snapdragon` | [`sdk/cmake/arm64-windows-llvm.cmake`](../sdk/cmake/arm64-windows-llvm.cmake) | `arm64-pc-windows-msvc`, clang, `-march=armv8.7-a -fvectorize` |
| `arm64-android-snapdragon` | [`sdk/cmake/arm64-android-llvm.cmake`](../sdk/cmake/arm64-android-llvm.cmake) | NDK, `armv8.7a+fp16+dotprod+i8mm` |
| `arm64-android-cpu` | [`sdk/cmake/arm64-android-llvm-cpu.cmake`](../sdk/cmake/arm64-android-llvm-cpu.cmake) | NDK, baseline `armv8-a` (CPU-only) |
| `arm64-linux-snapdragon` | [`sdk/cmake/arm64-linux-gnu.cmake`](../sdk/cmake/arm64-linux-gnu.cmake) | `aarch64-linux-gnu-gcc-13`, `armv8.2-a+fp16+dotprod` |
| `arm64-linux-cpu` | [`sdk/cmake/arm64-linux-gnu-cpu.cmake`](../sdk/cmake/arm64-linux-gnu-cpu.cmake) | gcc-13, baseline `armv8-a` (CPU-only) |

The composite presets you actually invoke inherit from two hidden groups:

- **`snapdragon`** (full NPU/GPU) sets
  `GENIEX_PLUGIN_LLAMA_CPP=ON, GGML_HEXAGON=ON, GGML_OPENCL=ON, GGML_OPENMP=OFF,
  GENIEX_PLUGIN_QAIRT=ON, GENIEX_BENCHMARK=ON` plus the Hexagon/OpenCL environment roots
  (`OPENCL_SDK_ROOT`, `HEXAGON_SDK_ROOT`, `HEXAGON_TOOLS_ROOT`, `HEXAGON_HTP_CERT`,
  `WINDOWS_SDK_BIN`).
- **`cpu-only`** sets `GENIEX_PLUGIN_LLAMA_CPP=ON, GENIEX_PLUGIN_QAIRT=OFF,
  GGML_HEXAGON=OFF, GGML_OPENCL=OFF, GGML_OPENMP=OFF, GENIEX_BENCHMARK=ON,
  GENIEX_CPU_ONLY=ON`.

Concrete preset names (debug/release): `arm64-windows-snapdragon-debug|release`,
`arm64-android-snapdragon-debug|release`, `arm64-android-cpu-debug|release`,
`arm64-linux-snapdragon-debug|release`, `arm64-linux-cpu-debug|release`.

### 6.2 Prerequisites on the ARM64 Windows host

- **Developer Mode** enabled (Settings → Privacy & Security → For developers) so
  **symlinks** work — required by Bazel (`startup --windows_enable_symlinks` in
  [`.bazelrc`](../.bazelrc:23)) and by the Hexagon sentencepiece build. Also grant
  **Create symbolic links** rights (gpedit) and enable **long paths** (see
  [`notes/build.md`](../notes/build.md)).
- **Hexagon SDK** — `HEXAGON_SDK_ROOT`, `HEXAGON_TOOLS_ROOT` (for `ggml-hexagon` HTP
  backends and QAIRT).
- **OpenCL SDK** — `OPENCL_SDK_ROOT` (headers + `OpenCL.lib`; the runtime ICD ships with
  the Snapdragon GPU driver).
- **Windows Driver Kit** — provides `inf2cat.exe` for HTP catalog signing.
- **Self-signed HTP cert** (`.pfx`) + Windows test-signing — `HEXAGON_HTP_CERT`; see
  [`notes/run.md`](../notes/run.md) § Self-signed fallback for cert generation
  (`makecert`/`pvk2pfx`) and `bcdedit /set TESTSIGNING ON`.

> **Hexagon path length.** The Hexagon toolchain has a 250-char path limit. Shorten the
> source path with `subst G: C:\path\to\geniex` before building (see
> [`notes/build.md`](../notes/build.md)).

### 6.3 Enabling `GGML_HEXAGON` / OpenCL for Snapdragon

`GGML_HEXAGON` and `GGML_OPENCL` are the two switches that bring up the Snapdragon
compute paths in the llama_cpp plugin:

- `GGML_HEXAGON=ON` — builds `ggml-hexagon` (the HTP/NPU backend) and, in
  [`sdk/plugins/llama_cpp/CMakeLists.txt`](../sdk/plugins/llama_cpp/CMakeLists.txt:33),
  triggers the **HTP session/power-mode handling**: it adds the `install-htp-skels`
  custom target (installs `libggml-htp-v{73,75,79,81}.so`), and, when the corresponding
  patches apply, the power-mode/session patches. The plugin reads the patched
  `ggml_backend_hexagon_release_sessions` / `ggml_backend_hexagon_set_power_mode`
  proc-address entries at runtime and no-ops when absent (see §8).
- `GGML_OPENCL=ON` — builds `ggml-opencl` (Adreno GPU backend) and selects the three
  Adreno OpenCL workaround patches.

The **`snapdragon`** preset sets both (plus `GENIEX_PLUGIN_QAIRT=ON`).

### 6.4 Enabling `GENIEX_PLUGIN_QAIRT` on real Snapdragon HW

`GENIEX_PLUGIN_QAIRT=ON` builds the QAIRT/QNN backend, which now makes sense because a
real Snapdragon ARM64 host has both the **hardware** (Hexagon NPU) and the **QNN
toolchain** present. On x64 (no NPU/QNN, no symlink/Developer-Mode for its
sentencepiece build) it must be forced `OFF` (§5.3). On the ARM64 Snapdragon build you
**keep it ON** (it's the `snapdragon` preset default) so both `llama_cpp` (GGUF) and
`qairt` (QAIRT `.bin`) runtimes build.

### 6.5 Expected commands (ARM64 Windows Snapdragon)

```powershell
cd sdk

# Configure: ARM64 Windows + Snapdragon (Hexagon + OpenCL + QAIRT), Ninja
cmake --preset arm64-windows-snapdragon-release -B build

# Build the llama_cpp plugin alone for fast iteration
cmake --build build --target geniex_llama_cpp -j

# Full build
cmake --build build -j

# Install into the SDK staging dir
cmake --install build --prefix pkg-geniex
```

Then build/run the CLI with Bazelisk (which links against `sdk/pkg-geniex/lib`):

```powershell
# from repo root
bazelisk run //cli -- infer Qwen/Qwen3-0.6B-GGUF --device hybrid
```

> **On ARM64 the Bazel CLI build should work.** The hermetic C++ toolchain in
> [`MODULE.bazel`](../MODULE.bazel:61) registers `@zig_sdk//toolchain:windows_arm64`
> (alongside `linux_arm64_gnu.2.28` / `linux_amd64_gnu.2.28`). Bazel also needs
> Developer Mode for symlinks on Windows (`startup --windows_enable_symlinks` in
> [`.bazelrc`](../.bazelrc:23)); `--config=windows_arm64` selects the
> `@io_bazel_rules_go//go/toolchain:windows_arm64_cgo` platform
> ([`.bazelrc`](../.bazelrc:14)). A caveat: on an **x64 host** the
> `windows_amd64` platform is **not** registered in `MODULE.bazel`, so the Bazel CLI
> build on x64 can fail to resolve a toolchain — on ARM64 (native host == target) it
> resolves normally.

For a GPU/CPU-only build without the Hexagon/QNN SDKs, use the `cpu-only` presets
(`arm64-windows-snapdragon-release` is the full NPU+GPU+QAIRT stack). The CPU-only
variant is baseline `armv8-a` and needs none of the Hexagon/OpenCL/QNN env vars
([`sdk/cmake/arm64-linux-gnu-cpu.cmake`](../sdk/cmake/arm64-linux-gnu-cpu.cmake),
[`sdk/cmake/arm64-android-llvm-cpu.cmake`](../sdk/cmake/arm64-android-llvm-cpu.cmake)).

---

## 7. Testing

### 7.1 Green results already obtained (Windows x64)

#### 7.1.1 Plugin unit tests (ctest)

Two standalone C++ test executables live under
[`sdk/plugins/llama_cpp/tests/`](../sdk/plugins/llama_cpp/tests) and are registered with
CTest by [`sdk/plugins/llama_cpp/CMakeLists.txt`](../sdk/plugins/llama_cpp/CMakeLists.txt)
(gated on an x86-64 host toolchain, because the test executables link the SDK bridge and
the llama.cpp targets and are executed by `ctest`):

- [`passthrough_test.cpp`](../sdk/plugins/llama_cpp/tests/passthrough_test.cpp) —
  `geniex_llama_cpp_passthrough_test`: arbitrary options land in the model params
  (`split-mode=layer`, `tensor-split=3,1`), short spellings, raw-dash spellings,
  `gpu-layers` / `main-gpu` / `ctx-size` overrides, unknown key → warn only, server-only
  option → refused, sampling passthrough, curated cache type does not warn.
- [`offload_test.cpp`](../sdk/plugins/llama_cpp/tests/offload_test.cpp) —
  `geniex_llama_cpp_offload_test`: budget from fraction, explicit budget overrides the
  fraction, NPU gets priority and the CPU keeps a base layer, `tensor_split` is never
  all-zero, NPU fills before the GPU, GPU-only when there is no NPU, graceful degradation
  without devices / without a known layer count, small budget offloads nothing, and
  per-device layer counts sum to the offloaded layer count.

```text
sdk\build-x64.cmd
ctest --test-dir sdk/build-x64-windows-msvc-release -C Release --output-on-failure
  1/2 Test #1: geniex_llama_cpp_offload_test .......   Passed    0.22 sec
  2/2 Test #2: geniex_llama_cpp_passthrough_test .....  Passed    0.55 sec
100% tests passed, 0 tests failed out of 2
```

> The plugin tests need the runtime DLLs on `PATH`:
> `sdk/build-x64-windows-msvc-release/bin` (llama.dll, llama-common.dll, ggml*.dll)
> and `sdk/build-x64-windows-msvc-release/src` (geniex.dll). `ctest` comes from the
> pip-installed CMake (`%APPDATA%\Python\Python312\site-packages\cmake\data\bin`).

#### 7.1.2 Go tests

```text
cd cli && go test ./...            # every package ok
cd bindings/go && go test ./...    # ok github.com/qualcomm/GenieX/bindings/go
```

[`llama_flags_test.go`](../cli/cmd/geniex/llama_flags_test.go) was extended with the
raw-token cases: `TestExtractLlamaArgsKeepsExplicitLlamaArg`,
`TestExtractLlamaArgsKeepsExplicitLlamaArgWithEquals`,
`TestEffectiveLlamaArgsMergesRawTokens`, `TestSplitLlamaArgToken`,
`TestLlamaArgValueFromRawToken` — all passing.

> The `cli` and `bindings/go` tests need the cgo SDK bridge: build
> `build-cli\libgeniex3.a` with `scripts\make_import_lib.py` + `dlltool`, build with
> `CGO_LDFLAGS=-L../build-cli -lgeniex3` (from `cli/`) or `-L../../build-cli -lgeniex3`
> (from `bindings/go/`), and put `build-cli` on `PATH` so `geniex.dll` resolves.

### 7.2 Re-running on ARM64

On the ARM64 Snapdragon build, run the same tests under Bazel (which has the SDK cdeps
and the hermetic `windows_arm64` toolchain):

```powershell
bazelisk test //cli/cmd/geniex:geniex_test --config=windows_arm64 --test_output=errors
bazelisk test //bindings/go:geniex_sdk_go_test --config=windows_arm64 --test_output=errors
```

Or, once `sdk/pkg-geniex` is built and the SDK bridge is present, `cd cli && go test
./cmd/geniex/ -run 'TestExtractLlamaArgs|TestLlamaArgValue'`.

### 7.3 Smoke-run note

A quick end-to-end smoke is `bazelisk run //cli -- infer Qwen/Qwen3-0.6B-GGUF` (from
[`notes/build.md`](../notes/build.md)) — this verifies the plugin loads and the CLI
plumbs a model through.

### 7.4 Caveat: a real inference run needs a network GGUF

A **real inference run needs a network GGUF model**. The `infer` flow pulls the model
from a hub (Hugging Face / AI Hub / ModelScope) first; there is no baked-in test GGUF,
so without network access (or a locally-cached model via `--model-hub localfs
--local-path …`) the LLM path can only be smoke-tested up to model load. The ternary /
KV-cache-quant / passthrough paths are exercised by the unit tests above plus a
load-time smoke; actual ternary-`output.weight` inference requires downloading a
PrismML ternary GGUF and running it on a machine with the fork built in.

---

## 8. Known limitations / TODOs / decisions

### 8.1 The 4 non-applying patches (verified against the fork)

The `git apply --check` results against the currently-checked-out fork commit
`9a9394a` are:

| Patch | Applies on fork? |
|---|---|
| `sdk/patches/llama-hexagon-release-sessions.patch` | **NO** |
| `sdk/patches/llama-hexagon-power-mode.patch` | **NO** |
| `sdk/patches/llama-hexagon-power-mode-setter.patch` | **NO** |
| `sdk/patches/llama-opencl-adreno-cpy-constreg.patch` | **YES** |
| `sdk/patches/llama-opencl-adreno-xmem-gemm-off.patch` | **NO** |
| `sdk/patches/llama-zero-free-mem-even-split.patch` | **YES** |

So **4 of the 6 patches do not apply** to the PrismML fork. Because
`GENIEX_REQUIRE_LLAMA_PATCHES` defaults to `OFF`, the CMake configure step logs a
warning for each failed apply and **continues** — the plugin degrades gracefully
(`htp_session.cpp` reads the patched proc-address entries at runtime and no-ops when
absent), losing only the per-model HTP power/session management and the Adreno OpenCL
workarounds those patches add.

**TODO:** if Snapdragon HTP/OpenCL is needed with the fork, **regenerate the 4
non-applying patches against the PrismML tip** (or fold their changes into the fork),
then re-verify with `git apply --check`. The two OpenCL patches that do apply
(`llama-opencl-adreno-cpy-constreg.patch`, `llama-zero-free-mem-even-split.patch`) are
already safe to keep feeding to git-apply.

### 8.2 QAIRT off on x64

`GENIEX_PLUGIN_QAIRT` is forced `OFF` for the x64 build (§5.3) because QAIRT is
Snapdragon/QNN-only and its sentencepiece build needs symlink/Developer Mode. It stays
`ON` on the real Snapdragon ARM64 target via the `snapdragon` preset.

### 8.3 Bazel `windows_amd64` toolchain gap on x64 hosts

`MODULE.bazel` registers `@zig_sdk//toolchain:windows_arm64` (and Linux variants) but
**not** a `windows_amd64` hermetic C++ toolchain. On an x64 Windows host the Bazel CLI
build may therefore fail to resolve a C++ toolchain (CGO cdeps). On ARM64 (host ==
target) it resolves normally via `windows_arm64`. Workaround for x64: rely on the
CMake-built SDK + `go build`/`go test` for the passthrough tests, as done here.

### 8.4 Other decisions / caveats

- **`--llama-*` is llama_cpp-only.** `qairt` ignores `cache_type_k` / `cache_type_v` /
  `llama_args` (documented in [`sdk/include/geniex.h`](../sdk/include/geniex.h:511)).
- **Unknown `--llama-*` keys are logged and dropped** (never fatal) — see
  [`dispatch`](../sdk/plugins/llama_cpp/src/passthrough.cpp:243).
- **`--llama-mmproj` is a CLI-level override**, not a plugin param; the plugin treats
  `mmproj` as a no-op passthrough key (§3.6).
- **mtmd helper API drift.** The single source fix (§5.4) — reverting the 4-arg
  `mtmd_helper_bitmap_init_from_file` to 3-arg — may need re-checking if the fork's
  mtmd helper API changes again.
- **`GENIEX_REQUIRE_LLAMA_PATCHES`** exists to restore a hard failure for CI that runs
  the upstream `ggml-org` tree (where the patches still apply); on the fork it stays
  `OFF`.

### 8.5 What could NOT be validated without Snapdragon hardware

The following were **not** validated on real hardware; the x64 host has neither a Hexagon
NPU nor an Adreno GPU:

- **The NPU → GPU → CPU split itself.** The plan was only validated through the pure
  planner math with synthetic inputs (`offload_test.cpp`). The real layer placement,
  the HTP session/power-mode handling and the actual offload speedup were not measured.
- **The device enumeration.** `enumerate_offload_devices()` looks for the `HTP` backend
  registry and the `GPUOpenCL` device; on x64 neither exists, so the planner always
  degrades to "no devices → CPU only".
- **The unified-memory budget in practice.** The budget is derived from the *total
  system RAM*, which is only a proxy for the unified pool. On the target the actual
  usable pool (minus the OS, the GPU carve-out and the 1 GiB reserve) is smaller, so
  the plan may be optimistic; `--unified-budget-mb` / `--offload-budget-fraction` exist
  to correct it.
- **The `tensor_split` proportions under a real split.** The non-zero requirement is
  unit-tested, but the *distribution* between the NPU and the GPU was not measured.
- **Sampling passthrough at generation time.** The resolver is unit-tested; the values
  were not run through a real decode loop.

Everything else (the generic resolver, the CLI plumbing, the build, the packaging) was
verified end-to-end on the x64 host.

---

## 9. Default offload policy — NPU → GPU → CPU

> New in this revision. When the user does **not** pick a compute unit, the plugin no
> longer relies on llama.cpp's auto-selection (which skips ACCEL devices and therefore
> never picks the Hexagon NPU). It computes an explicit plan.

### 9.1 Where it lives

- [`sdk/plugins/llama_cpp/include/offload.h`](../sdk/plugins/llama_cpp/include/offload.h)
  — the plan/inputs structures and the tuning constants.
- [`sdk/plugins/llama_cpp/src/offload.cpp`](../sdk/plugins/llama_cpp/src/offload.cpp) —
  the implementation. The pure math is
  [`compute_offload_plan()`](../sdk/plugins/llama_cpp/src/offload.cpp:159), which takes
  synthetic inputs and therefore is unit-testable on any host.
- The plan is applied in [`llm.cpp`](../sdk/plugins/llama_cpp/src/llm.cpp:84) and
  [`vlm.cpp`](../sdk/plugins/llama_cpp/src/vlm.cpp:62) (same shape in both).

### 9.2 The algorithm

1. **Device enumeration** — [`enumerate_offload_devices()`](../sdk/plugins/llama_cpp/src/offload.cpp:118)
   walks the backend registry: the **HTP** registry
   (`ggml_backend_reg_by_name("HTP")` → `HTP0..HTPn`) first, then the OpenCL GPU device
   named exactly `GPUOpenCL` (matching `kDeviceGPUOpenCL` in
   [`sdk/src/device.cpp`](../sdk/src/device.cpp)). The list is in **priority order**.
2. **Budget** — the NPU and the GPU share one unified memory pool, so the budget is
   self-imposed from the **total system RAM** (`GlobalMemoryStatusEx` on Windows,
   `/proc/meminfo` on Linux) × the configured fraction:
   `budget = total_ram × fraction`, default **fraction = 0.50** ⇒ **16 GB on a 32 GB
   Snapdragon X Elite**. `ggml_backend_dev_memory()` is *not* used for the NPU because the
   HTP backend reports 0/0.
   The NPU takes the larger share (`kOffloadNpuBudgetShare = 0.60`); the GPU share is
   clamped to `gpu_total - 1 GiB` (`kOffloadGpuReserveBytes`), matching the 1 GiB margin
   the OpenCL backend already subtracts from `global_mem_size`.
3. **Model footprint** — a **sizing pass** reads the GGUF header without allocating the
   model ([`read_model_meta()`](../sdk/plugins/llama_cpp/src/offload.cpp:69) →
   `gguf_init_from_file` with `no_alloc = true`) to learn `n_layer_all`; the model is then
   loaded for real.
4. **Bytes → layers** — `per_layer_bytes = (gguf_size + kv_bytes) / (n_layer_all + 2)`.
   The NPU is filled first with as many whole layers as its share allows, then the GPU
   takes the next ones, and the remainder stays on the CPU.
5. **Apply** — [`apply_offload_plan()`](../sdk/plugins/llama_cpp/src/offload.cpp:308)
   sets `llama_model_params::devices` (NULL-terminated, **order = priority**),
   `split_mode = LLAMA_SPLIT_MODE_LAYER`, `n_gpu_layers` and `tensor_split`
   (`llama_max_devices()` entries).

### 9.3 The CPU-is-the-base-tier caveat

llama.cpp's layer split is **not** "layer N goes to device M": with
`LLAMA_SPLIT_MODE_LAYER` the **CPU owns the LOW-index layers** and the offload tier owns
the **HIGH-index layers** (`i_gpu_start = max(n_layer_all + 1 - n_gpu_layers, 0)` in
[`llama-model.cpp`](../third-party/llama.cpp/src/llama-model.cpp:1564)). The plan
therefore keeps at least `kOffloadMinCpuBaseLayers = 1` layer on the CPU and sets
`n_gpu_layers < n_layer_all + 1`, so the CPU always holds a resident base tier and the
priority order applies to the offloaded (high-index) layers. The device list order is
the priority order *inside* the offload tier, and `tensor_split` expresses the share of
the offloaded layers each device takes. **Do not invert this.**

### 9.4 `tensor_split` must never be all-zero

An all-zero `tensor_split` makes the fork fall back to its broken free-memory default
path (the NaN crash that
[`sdk/patches/llama-zero-free-mem-even-split.patch`](../sdk/patches/llama-zero-free-mem-even-split.patch)
defuses). The planner always writes a non-zero split, and the unit test
`test_tensor_split_is_never_all_zero` pins that.

### 9.5 Precedence

From lowest to highest:

1. **llama.cpp defaults**
2. **The GenieX default offload policy** (this section) — applied only when the user did
   **not** pick devices explicitly
3. **The user's `--llama-*` passthrough** — applied as an override layer on top
4. **An explicit `--compute cpu|gpu|npu|hybrid` (or an explicit device list)** — highest

In code ([`llm.cpp`](../sdk/plugins/llama_cpp/src/llm.cpp:87), mirrored in
[`vlm.cpp`](../sdk/plugins/llama_cpp/src/vlm.cpp:58)): the device choice is resolved
first; the default plan is built and applied **only if** the user did not pick devices
and did not give a device override; the passthrough is resolved and applied last, so it
can override the plan (e.g. `--llama-tensor-split=3,1`, `--llama-split-mode=layer`,
`--llama-ngl`).

`--llama-device=…` is **not** part of the generic passthrough: the CLI strips the
device token out of the passthrough list and hands it to the plugin on the typed
`geniex_ModelConfig.llama_device` field, where `resolve_device_override()` turns it
into a device list (`"none"` disables offloading, an unknown device name is refused
with a warning and leaves the plan intact). This keeps the documented
`--llama-device=…` override behaviour while llama.cpp's own `--device` / `-dev`
option stays refused.

### 9.6 Tuning knobs

| Knob | CLI | Go binding | C ABI | Default |
|---|---|---|---|---|
| Unified budget (MiB) | `--unified-budget-mb` | `ModelConfig.UnifiedBudgetMB` | `geniex_ModelConfig.unified_budget_mb` | `0` = derive from the fraction |
| Budget fraction | `--offload-budget-fraction` | `ModelConfig.OffloadBudgetFraction` | `geniex_ModelConfig.offload_budget_fraction` | `0.50` |

An explicit budget wins over the fraction. The whole plan can be overridden through the
passthrough (`--llama-tensor-split`, `--llama-split-mode`, `--llama-ngl`) or through the
typed device override (`--llama-device`).

### 9.7 Logging and graceful degradation

The computed plan (NPU/GPU/CPU layer counts, budget bytes, split fractions, device order)
is logged at **INFO**. The planner degrades gracefully:

- no HTP and no OpenCL device → the plan is disabled and the model stays on the CPU;
- the layer count cannot be determined → the plan is disabled;
- the budget is smaller than one layer → nothing is offloaded;
- an unknown / missing device → that device is skipped.

### 9.8 Collateral fixes

- `classify_device()` ([`params.cpp`](../sdk/plugins/llama_cpp/src/params.cpp:42)) is now
  **multi-device aware** — an explicit list such as `HTP0,HTP1` is classified as an NPU
  device list rather than an unknown device.
- `hybrid` (and the default / empty compute unit) now actually **includes the NPU**.
  llama.cpp's own auto-selection skips ACCEL devices, so the default path had to be
  replaced by the explicit plan above.
- `--compute cpu|gpu|npu` behaves exactly as before.

---

## 10. References

- **Fork (submodule):** `https://github.com/PrismML-Eng/llama.cpp`
  - Current commit: `9a9394a895b96003ca842a6041cb28ac49a108f7`
    (2026-09-18, "build: fix Ubuntu ARM64 and Windows Vulkan release jobs (#198)")
  - Recorded in [`.gitmodules`](../.gitmodules) (replaces the ggml-org URL).
  - Ternary symbols: [`ggml.h`](../third-party/llama.cpp/ggml/include/ggml.h:433)
    (`GGML_TYPE_PQ2_0=142`, `GGML_TYPE_PTQ1_0=143`),
    [`llama.h`](../third-party/llama.cpp/include/llama.h:159)
    (`LLAMA_FTYPE_MOSTLY_PQ2_0=141`, `_LEGACY=142`, `_PTQ1_0=143`, `type_k`/`type_v`).
  - mtmd helper header:
    [`tools/mtmd/mtmd-helper.h`](../third-party/llama.cpp/tools/mtmd/mtmd-helper.h:43)
    (`mtmd_helper_bitmap_init_from_file` 3-arg), entry point
    [`tools/mtmd/mtmd.h`](../third-party/llama.cpp/tools/mtmd/mtmd.h:126)
    (`mtmd_init_from_file`).

**Key GenieX file paths:**

- [`sdk/CMakeLists.txt`](../sdk/CMakeLists.txt) — plugin options, patch application.
- [`sdk/CMakePresets.json`](../sdk/CMakePresets.json) — `snapdragon` / `cpu-only` /
  ARM64 presets.
- [`sdk/cmake/`](../sdk/cmake) — ARM64 toolchain files
  (`arm64-windows-llvm.cmake`, `arm64-android-llvm.cmake`,
  `arm64-linux-gnu.cmake`, + `-cpu` variants).
- [`sdk/plugins/CMakeLists.txt`](../sdk/plugins/CMakeLists.txt) — plugin subdirs.
- [`sdk/plugins/llama_cpp/CMakeLists.txt`](../sdk/plugins/llama_cpp/CMakeLists.txt) —
  fork submodule add, HTP skel/install.
- [`sdk/plugins/llama_cpp/src/CMakeLists.txt`](../sdk/plugins/llama_cpp/src/CMakeLists.txt)
  — `geniex_llama_cpp` target.
- [`sdk/plugins/llama_cpp/src/params.cpp`](../sdk/plugins/llama_cpp/src/params.cpp) —
  `parse_cache_type`, `apply_llama_model_args`, `apply_llama_context_args`,
  `classify_device`.
- [`sdk/plugins/llama_cpp/src/passthrough.cpp`](../sdk/plugins/llama_cpp/src/passthrough.cpp)
  + [`include/passthrough.h`](../sdk/plugins/llama_cpp/include/passthrough.h) — the generic
  llama.cpp option resolver (`resolve_passthrough`, `resolve_sampling_passthrough`,
  `apply_passthrough_overrides`).
- [`sdk/plugins/llama_cpp/src/offload.cpp`](../sdk/plugins/llama_cpp/src/offload.cpp)
  + [`include/offload.h`](../sdk/plugins/llama_cpp/include/offload.h) — the default
  NPU → GPU → CPU offload planner.
- [`sdk/plugins/llama_cpp/tests/`](../sdk/plugins/llama_cpp/tests) — the plugin unit
  tests (`passthrough_test.cpp`, `offload_test.cpp`).
- [`sdk/plugins/llama_cpp/src/llm.cpp`](../sdk/plugins/llama_cpp/src/llm.cpp) /
  [`vlm.cpp`](../sdk/plugins/llama_cpp/src/vlm.cpp) — the offload-plan + passthrough
  precedence; the mtmd projector load; the 3-arg `mtmd_helper_bitmap_init_from_file` fix.
- [`sdk/include/geniex.h`](../sdk/include/geniex.h:480) — `geniex_ModelConfig`
  (`cache_type_k`, `cache_type_v`, `llama_args`, `llama_args_count`,
  `unified_budget_mb`, `offload_budget_fraction`).
- [`sdk/src/registry.cpp`](../sdk/src/registry.cpp) — plugin registry / loading.
- [`sdk/patches/`](../sdk/patches) — the 6 conditional llama.cpp patches.
- [`bindings/go/common.go`](../bindings/go/common.go:152) — Go `ModelConfig`
  (`CacheTypeK`, `CacheTypeV`, `LLamaArgs`) and `fillC`.
- [`cli/cmd/geniex/llama_flags.go`](../cli/cmd/geniex/llama_flags.go) —
  `extractLlamaArgs`, `EffectiveLlamaArgs`, `llamaArgValue`.
- [`cli/cmd/geniex/llama_flags_test.go`](../cli/cmd/geniex/llama_flags_test.go) — the
  passthrough tests.
- [`cli/cmd/geniex/infer.go`](../cli/cmd/geniex/infer.go) — wires `LLamaArgs` and
  `--llama-mmproj`.
- [`cli/cmd/geniex/serve.go`](../cli/cmd/geniex/serve.go) — merges `--llama-*` into
  server defaults.
- [`cli/cmd/geniex/BUILD.bazel`](../cli/cmd/geniex/BUILD.bazel) — `geniex_test`
  `go_cgo_test` includes `llama_flags_test.go`.
- [`bindings/go/BUILD.bazel`](../bindings/go/BUILD.bazel) — `geniex_sdk_go` /
  `geniex_sdk_go_test` cdeps.
- [`MODULE.bazel`](../MODULE.bazel:61) — hermetic `windows_arm64` toolchain
  registration.
- [`notes/build.md`](../notes/build.md) / [`notes/run.md`](../notes/run.md) — build
  recipes, HTP signing, `--llama-*` usage examples.
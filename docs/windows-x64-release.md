# Windows x64 Release Build & `output/` Layout (GenieX)

This document records the **Windows x64 desktop release** of GenieX built against the
PrismML Ternary-Bonsai llama.cpp fork, and the layout convention of the top-level
`output/` folder. It complements
[`docs/ternary-bonsai-prismml.md`](ternary-bonsai-prismml.md), which documents the
fork integration itself (ternary weights, quantized KV cache, `--llama-*` passthrough).

- Fork: `https://github.com/PrismML-Eng/llama.cpp` @ `9a9394a895b96003ca842a6041cb28ac49a108f7`
  (branch `prism`, tag `prism-b10709-9a9394a`)
- ggml `0.21.0`, llama.cpp `0.2.0-dev`
- Host used: Windows 11 x64, MSVC 19.44 (`14.44.35207`), Go 1.27.1, Ninja, CMake 4.4.4

---

## 1. Why there is a separate x64 preset

The SDK is normally built for **Snapdragon ARM64 Windows** through
[`sdk/cmake/arm64-windows-llvm.cmake`](../sdk/cmake/arm64-windows-llvm.cmake), which
targets the LLVM/clang-cl cross toolchain plus the HTP/OpenCL backends.

HTP (Hexagon Tensor Processor) and the QNN/QAIRT runtime are **Snapdragon-only**. On a
plain x64 host those backends do not exist, so the desktop build uses:

- MSVC (`cl.exe`) as the compiler,
- the CPU-only ggml backend,
- `GENIEX_PLUGIN_QAIRT=OFF`,
- `GENIEX_PLUGIN_LLAMA_CPP=ON`.

A minimal, self-contained toolchain file was added:

### [`sdk/cmake/x64-windows-msvc.cmake`](../sdk/cmake/x64-windows-msvc.cmake)

```cmake
set( CMAKE_SYSTEM_NAME Windows )
set( CMAKE_SYSTEM_PROCESSOR x86_64 )
set( CMAKE_C_COMPILER   cl )
set( CMAKE_CXX_COMPILER cl )
```

It intentionally mirrors the shape of the arm64 file (system name / processor / compilers
only). Note there is no `-march` / `-ffp-model` equivalent under MSVC, and the HTP and
OpenCL backends are not built.

### [`sdk/CMakePresets.json`](../sdk/CMakePresets.json) additions

| Preset | Kind | Notes |
| --- | --- | --- |
| `x64-windows-msvc` | configure (hidden) | Ninja generator, toolchain file `cmake/x64-windows-msvc.cmake`, `PREBUILT_LIB_DIR=windows_x86_64` |
| `x64-windows-msvc-release` | configure (visible) | inherits `x64-windows-msvc` + `cpu-only` + `release` + `base` |
| `x64-windows-msvc-debug` | configure (visible) | inherits `x64-windows-msvc` + `debug` |
| `x64-windows-msvc-release` / `-debug` | build | matching build presets |

---

## 2. Building the SDK (Release)

Helper script: [`sdk/build-x64.cmd`](../sdk/build-x64.cmd) — it locates Visual Studio via
`vswhere`, calls `vcvarsall.bat x64`, puts `cmake`/`ninja` on `PATH` and then runs the
CMake configure/build/install steps.

```bat
sdk\build-x64.cmd install
```

which expands to:

```bat
cmake --preset x64-windows-msvc-release -DGENIEX_PLUGIN_QAIRT=OFF -DGENIEX_PLUGIN_LLAMA_CPP=ON
cmake --build --preset x64-windows-msvc-release
cmake --install build-x64-windows-msvc-release --prefix "<repo>\sdk\pkg-geniex"
```

Resulting install tree (`sdk/pkg-geniex`):

```
lib/geniex.dll
lib/llama_cpp/geniex_plugin.dll
lib/llama_cpp/llama.dll
lib/llama_cpp/llama-common.dll
lib/llama_cpp/ggml.dll
lib/llama_cpp/ggml-base.dll
lib/llama_cpp/ggml-cpu.dll
lib/llama_cpp/mtmd.dll
include/geniex.h
include/geniex_model.h
include/power_mode_alias.h
bin/geniex-bench.exe
```

The Rust `model-manager` crate is built by CMake via `cargo`; on Windows the FFI symbols
are exported explicitly with `/INCLUDE:` + `/EXPORT:` (not `/WHOLEARCHIVE`).

### 2.1 Plugin unit tests (ctest)

The x86-64 build also produces two plugin-level unit-test executables (the tests live in
[`sdk/plugins/llama_cpp/tests/`](../sdk/plugins/llama_cpp/tests) and are registered with
CTest by [`sdk/plugins/llama_cpp/CMakeLists.txt`](../sdk/plugins/llama_cpp/CMakeLists.txt)):

```bat
sdk\build-x64.cmd
set PATH=%CD%\sdk\build-x64-windows-msvc-release\bin;%CD%\sdk\build-x64-windows-msvc-release\src;%PATH%
ctest --test-dir sdk/build-x64-windows-msvc-release -C Release --output-on-failure
```

> `ctest` comes from the pip-installed CMake
> (`%APPDATA%\Python\Python312\site-packages\cmake\data\bin`); add it to `PATH` if
> `where ctest` finds nothing.

Expected: `geniex_llama_cpp_offload_test` and `geniex_llama_cpp_passthrough_test` both
pass (2/2). The `bin` / `src` entries on `PATH` provide `llama.dll`, `llama-common.dll`,
`ggml*.dll` and `geniex.dll`.

---

## 3. Building the CLI

### 3.1 Bazel path (blocked on this host)

`bazelisk.exe build //cli/cmd/geniex:geniex` fails on Windows x64 with:

```
Failed to prepare the symlink forest: createSymbolicLinkW failed (permission denied).
Either Windows developer mode or admin privileges are required.
```

Enabling Windows Developer Mode via `reg add HKLM\SOFTWARE\Microsoft\Windows\
CurrentVersion\AppModelUnlock /v AllowDevelopmentWithoutDevLicense` also fails with
"Access is denied" on a non-admin shell. This is a **symlink-forest** limitation of
Bazel on Windows, not a code problem.

Additionally, [`MODULE.bazel`](../MODULE.bazel) registers hermetic C++ toolchains for
`linux_amd64`, `linux_arm64` and `windows_arm64` only — there is no `windows_amd64`
toolchain, so the cgo toolchain would not resolve either.

### 3.2 Fallback: `go build` + MinGW import library (used here)

The Go CLI is built with cgo against the MSVC-produced `geniex.dll`. MinGW cannot link
against a `.lib` produced by MSVC, so an import library is generated from the DLL's PE
export table:

```bat
python scripts\make_import_lib.py sdk\pkg-geniex\lib\geniex.dll build-cli\geniex.def
dlltool -D geniex.dll -d build-cli\geniex.def -l build-cli\libgeniex3.a
```

Two details are load-bearing:

1. The generated `.def` must contain a `LIBRARY geniex.dll` directive, otherwise the
   resulting import table names the DLL `(null)` and the exe dies with
   `STATUS_DLL_NOT_FOUND` (3221225781).
2. `dlltool` must be given `-D geniex.dll` — without it the import table again ends up
   unnamed. Passing `-l:geniex.dll` to the linker or handing the `.def` straight to `ld`
   does **not** work (the latter is interpreted as an export definition for the exe).

Linking then proceeds with:

```bat
cd cli
set PATH=C:\Users\Stanford\scoop\apps\gcc\current\bin;%PATH%
set CGO_ENABLED=1
set CGO_LDFLAGS=-L../build-cli -lgeniex3
set CC=gcc
go build -ldflags "-X main.Version=v0.0.0-prism-bonsai" -o ..\build-cli\geniex.exe ./cmd/geniex
```

`bindings/go/crash_handler.c` was adjusted to be MinGW-friendly: `_set_abort_behavior`
only exists in the MSVC CRT, so under MinGW the handler now uses
`SetErrorMode(SEM_FAILCRITICALERRORS | SEM_NOGPFAULTERRORBOX | SEM_NOOPENFILEERRORBOX)`
instead (the MSVC branch is unchanged).

Verification: `dumpbin /imports build-cli\geniex.exe` shows `geniex.dll`, `KERNEL32.dll`,
`msvcrt.dll`.

---

## 4. The `output/` folder convention

`output/` is a **build artifact directory** (it is git-ignored; it is not a source
folder). It is produced by [`scripts/stage-output.cmd`](../scripts/stage-output.cmd):

```bat
scripts\stage-output.cmd
```

Layout:

```
output/
  geniex.exe              CLI (go build, cgo -> geniex.dll)
  geniex.dll              SDK bridge
  llama_cpp/              <-- canonical runtime plugin folder
    geniex_plugin.dll
    llama.dll llama-common.dll ggml.dll ggml-base.dll ggml-cpu.dll mtmd.dll
  plugins/
    llama_cpp/            <-- drop-in deliverable (identical content)
      geniex_plugin.dll
      llama.dll llama-common.dll ggml.dll ggml-base.dll ggml-cpu.dll mtmd.dll
  lib/                    llama/ggml/mtmd runtime only (NO geniex_plugin.dll)
  include/                geniex.h, geniex_model.h, power_mode_alias.h
  run.cmd                 launcher: cd /d "%~dp0" && geniex.exe %*
  geniex.ico
  geniex-cli-setup.exe    Windows installer (built afterwards)
  geniex-cli-setup.iss    staged copy of the installer script
```

### 4.1 Plugin discovery (why there are two copies)

[`sdk/src/registry.cpp`](../sdk/src/registry.cpp) resolves the **plugin root** as
`get_shared_lib_dir()` — the directory that contains `geniex.dll` — unless
`GENIEX_PLUGIN_PATH` is set. It then walks the *immediate child directories* of that
root and, for each child, looks for a file named **exactly `geniex_plugin.dll`**
(`get_geniex_plugin_name()`). The plugin id is the child directory's name, so:

```
<root>/llama_cpp/geniex_plugin.dll   ->  plugin id "llama_cpp"
```

This is why the staging script emits `output/llama_cpp/` **in addition to**
`output/plugins/llama_cpp/`:

- `output/llama_cpp/` is the location the running CLI actually scans, so the shipped
  tree works out of the box.
- `output/plugins/llama_cpp/` is the **drop-in deliverable** the user copies into an
  existing GenieX installation's plugin folder.

`output/lib/` deliberately contains only the `llama*` / `ggml*` / `mtmd` runtime DLLs
and **no** `geniex_plugin.dll`. The registry stores plugins in a `std::map` populated
with `emplace`, which does not overwrite an existing key — so a duplicate
`geniex_plugin.dll` in a scanned folder could shadow the real one.

`GENIEX_PLUGIN_PATH` overrides the root entirely and is the knob to use when dropping
the plugin into a third-party layout.

### 4.2 Verification

```bat
cd output
run.cmd version
```

```
GenieX CLI Version:     v0.0.0-prism-bonsai
QAIRT Runtime Version:
LlamaCPP Runtime Hash:  9a9394a
```

The `LlamaCPP Runtime Hash` line is the fork commit the plugin was built from; an empty
value means the plugin was **not** discovered. With `GENIEX_TRACE=1` the SDK logs
`Registered plugin: llama_cpp`.

`run.cmd run --help` describes the **generic** passthrough: any llama.cpp option is
accepted, either as `--llama-<key>=<value>` (`--llama-split-mode=layer`,
`--llama-tensor-split=3,1`) or as a raw token through `--llama-arg`
(`--llama-arg "--split-mode=layer"`, `--llama-arg "-sm" --llama-arg "layer"`). The
tokens are resolved against llama.cpp's own option table and applied as an override layer
on top of the GenieX defaults. The llama-server options (`--host`, `--port`,
`--api-key`, `--slots`, …) plus `--model`, `--hf-*`, `--rpc`, `--list-devices`, the
logit/session/adapter file options and llama.cpp's own `--device` / `-dev` are out of
scope and are refused with a warning. The device selection stays under GenieX's
control: use `--compute cpu|gpu|npu` or the dedicated `--llama-device=…` override
(`none` disables offloading), which is handled by GenieX itself and never forwarded
to llama.cpp. The
help text also lists the default offload policy and the `--unified-budget-mb` /
`--offload-budget-fraction` knobs. See
[`docs/ternary-bonsai-prismml.md`](ternary-bonsai-prismml.md) §3.4 and §9 for the full
design.

---

## 5. Windows installer

### 5.1 In-repo script (Bazel path)

[`cli/release/windows/geniex-cli-setup.iss`](../cli/release/windows/geniex-cli-setup.iss)
keeps the existing structure and pulls the CLI + SDK from `ArtifactIss`, which
[`cli/release/windows/iss_manifest.bzl`](../cli/release/windows/iss_manifest.bzl)
generates from `//cli:_artifact` (= `//cli:_artifact_bin` + `//sdk:artifact`).
`//sdk:artifact` carries `pkg-geniex/lib/**` with
`strip_prefix.from_pkg("pkg-geniex/lib")`, so the llama.cpp plugin folder is already
emitted as

```
Source: "...\llama_cpp\geniex_plugin.dll"; DestDir: "{app}\llama_cpp"
```

together with its `llama` / `ggml` / `mtmd` runtime. **No explicit `Source:` lines for
the plugin were added**, because hard-coded `{#SrcRoot}\sdk\pkg-geniex\...` paths would
break the Bazel flow (Bazel stages into a sandbox tree, not the source checkout).

The hermetic InnoSetup repo rule is
[`cli/release/windows/iscc.bzl`](../cli/release/windows/iscc.bzl) (InnoSetup 6.7.1),
driven by [`cli/release/windows/BUILD.bazel`](../cli/release/windows/BUILD.bazel)
(`genrule windows_installer`).

### 5.2 Staged script (manual path)

Because Bazel cannot run on this host, the installer was compiled manually against the
staged tree using
[`cli/release/windows/geniex-cli-setup-staged.iss`](../cli/release/windows/geniex-cli-setup-staged.iss).
It carries the same `[Setup]`, `[Registry]`, `[Tasks]`, `[Icons]`, `[UninstallRun]`,
`[UninstallDelete]` and `[Code]` sections as the in-repo script, but references the
staged files directly. `__STAGED_ROOT__` / `__OUTPUT_DIR__` placeholders are rewritten
to `.` by the staging script.

```bat
scripts\stage-output.cmd
cd output
"C:\Users\Stanford\AppData\Local\Programs\Inno Setup 6\ISCC.exe" ^
    /DVersion=v0.0.0-prism-bonsai geniex-cli-setup.iss
```

> The local `iscc.exe` came from `winget install --id JRSoftware.InnoSetup`
> (InnoSetup 6.7.3, per-user install at
> `%LOCALAPPDATA%\Programs\Inno Setup 6\ISCC.exe`).

### 5.3 Silent-install verification

```bat
output\geniex-cli-setup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /DIR="C:\Temp\geniex-test"
C:\Temp\geniex-test\run.cmd version
```

The installed tree contains `geniex.exe`, `geniex.dll`, `llama_cpp\*`,
`plugins\llama_cpp\*`, `lib\*`, `include\*`, `run.cmd`; the installed launcher reports
`LlamaCPP Runtime Hash: 9a9394a`. The temporary installation was then removed.

---

## 6. How to use the drop-in plugin

To add the PrismML Ternary-Bonsai llama.cpp backend to an **existing** GenieX
installation:

1. Copy the whole folder `output/plugins/llama_cpp/` into the installation directory,
   so it becomes `<install>\llama_cpp\` next to `geniex.dll`.
2. The folder must contain `geniex_plugin.dll` **and** the matching
   `llama.dll`, `llama-common.dll`, `ggml.dll`, `ggml-base.dll`, `ggml-cpu.dll`,
   `mtmd.dll` (the plugin loads its backend from its own directory).
3. Alternatively, set `GENIEX_PLUGIN_PATH=<dir>` where `<dir>` contains a
   `llama_cpp\geniex_plugin.dll` subtree — this overrides the default scan root.
4. Check with `geniex version`: `LlamaCPP Runtime Hash` must show the fork hash
   (`9a9394a` for this build). An empty value means the folder name or the DLL name is
   wrong.
5. Use the CLI passthrough for the fork-specific knobs, e.g.
   `geniex run --llama-arg ctx-size=4096 --llama-arg cache-type-k=pq2_0 ...`, or the
   raw-token form for short / multi-value options:
   `geniex run --llama-arg "--split-mode=layer" --llama-arg "--tensor-split=3,1"`.

Do **not** place a second `geniex_plugin.dll` in a sibling folder of the same plugin
root — the registry keeps the first plugin registered for a given id.

---

## 7. Caveats

- **QAIRT/QNN is disabled** (`GENIEX_PLUGIN_QAIRT=OFF`): it targets Snapdragon Hexagon
  only. `geniex version` therefore prints an empty `QAIRT Runtime Version`, and the SDK
  logs a harmless `failed to get plugin version for qairt` line.
- **No HTP signing / Hexagon DSP offload on x64.** The HTP backend and its signed
  firmware path are arm64-Snapdragon features; the x64 build uses the ggml CPU backend
  only.
- **arm64-only features are absent**: Snapdragon power modes, HTP delegation, QAIRT
  device enumeration.
- **Bazel remains unusable on this host** (symlink forest needs Developer Mode or
  admin). The Bazel release path (`bazelisk build //cli/release/windows`) is
  unverified here; the in-repo `.iss` / `iss_manifest.bzl` were kept semantically
  correct for it, but only the manual `iscc` path was actually compiled and tested.
- `output/` is git-ignored: regenerate it with `scripts\stage-output.cmd` rather than
  committing it.
